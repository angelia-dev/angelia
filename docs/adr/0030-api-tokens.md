# ADR 0030: API tokens are `ang_` bearer tokens stored as SHA-256 hashes in an `api_token` table on both engines; the first admin token comes from a bootstrap file or the instance-side CLI

- **Status:** Proposed
- **Date:** 2026-11-15
- **Story:** angelia-dev/angelia#72

> Written as the design note for a **Human-led** story. No implementation code is written until the
> maintainer has decided the items under "Questions for the maintainer". Decisions are numbered
> T1–T12 so that the review can answer them one by one.

## Context

[architecture.md §3.1](../architecture.md#31-api) says the API uses "API tokens with scopes
(read / write / admin). OIDC later." [§8](../architecture.md#8-security) asks for secrets encrypted at
rest, and [ADR 0024](0024-secrets-encrypted-at-rest.md) §1 already lists API tokens as **not** a stored
secret but a **hash** ("SHA-256 of 256 random bits, #72's call"), because the server never needs the
value back. Issue #72 asks for: tokens stored hashed (argon2id or SHA-256, decided here), a bootstrap
token printed once or read from a file and never logged again, constant-time comparison, scope checks
on every route with a table test over all routes, and `angelia token create|list|revoke`.

What exists on main (`2374cb3`):

- **`internal/api/auth.go`.** `Scope` (`none < read < write < admin`, ordered), `ParseScope`,
  `Principal{ID, Scope}` (the ID "names the token in logs and the audit trail" and must not be derived
  reversibly from the token), `ErrUnauthenticated`, and the `Authenticator` interface
  (`Authenticate(ctx, token) (Principal, error)`). `bearerToken` parses `Authorization: Bearer`; the
  `401` carries the RFC 6750 challenge.
- **`cmd/angelia/api.go`** wires `api.DenyAll{}`, so every API request gets `401` today
  ([configuration.md, "REST API"](../configuration.md#rest-api)). The MCP tools are unusable for the
  same reason ([mcp.md](../mcp.md)).
- **Scopes come from the spec.** Each operation's `security: - bearerAuth: [<scope>]` in the embedded
  OpenAPI document sets `op.Scope`; `internal/api/server.go` checks `401` (no or bad token, or a
  principal below `read`) and `403` (scope too low, with `required_scope`) **before** routing to a
  resource, so `401`/`403` never reveal existence (spec `docs/api.md` decisions 22–23).
  `auth_test.go` holds `wantScopes`, a hand-written copy of the table that `TestScopesFromSpec`
  compares with the spec, and `TestAuthEveryRoute`, which calls every operation with each scope below
  and at or above its own. ADR 0024 §2 / spec decision 35 add a body-dependent rule: a write that
  touches a `*_ref` key needs `admin` (`checkRefScope`).
- **Event streams** re-authenticate: `AuthRecheck` is capped so that a revoked token is noticed within
  `MaxAuthRecheck` = 60 s (spec decision 15).
- **MCP over HTTP** ([ADR 0029](0029-mcp-server-go-sdk-rest-client.md), `internal/mcp/http.go`) builds
  one MCP server per request and forwards exactly that request's `Authorization` values on each
  in-memory REST call: nothing is cached, every tool call authenticates anew. MCP over stdio is a REST
  client with the CLI's connection settings.
- **Client side** (`cmd/angelia/clientconfig.go`): `--token`, `ANGELIA_TOKEN`, or `token:` in the client
  config file (refused when group or others can read it); the token is never sent over plain `http`
  to a non-loopback host unless `--insecure-http`. `Connection.String` never shows the token.
- **Logging.** The access log records `principal` and `scope`, never headers, and `safePath` redacts a
  path segment that looks like the token. `Server.audit` writes `audit=true` records with the
  principal. `internal/config` refuses token-looking values (`tokenStart`: `ghp_`, `glpat-`, `whsec_`,
  …) in the instance configuration.
- **Instance-side commands.** `angelia secrets keygen|rotate|status` read `--config` and open the store
  directly (`openInstance`), unlike `angelia secret …`, which is an API client.
- **Store.** Both engines ([ADR 0028](0028-postgres-event-cursor-and-write-concurrency.md)) use the same
  migration numbers; the head is `00020` on both, so the next free number is **`00021`**. On Postgres
  `InConfigTx`, `InAPITx` and `InSecretTx` take the config advisory lock first (ADR 0028 §2, D15). The
  `store` interfaces are contracts (AGENTS.md).

Constraints: no new dependency outside [architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions)
without the maintainer; the OpenAPI spec, the YAML config format and the store interfaces are public
or internal contracts, so every change to them is listed in T9.

## Decision

### T1. Token format

```
ang_<id>_<secret><check>
     │    │       └─ 7 chars: CRC-32 (IEEE) of everything before it, lower-case base32
     │    └───────── 52 chars: 32 random bytes (crypto/rand), lower-case base32, no padding
     └────────────── 26 chars: the token's ULID (upper-case Crockford base32, internal/ulid)
```

Example shape (not a real token): `ang_01JCZ8W3T6Q9N2H4K7M5R0V8XA_` followed by 59 characters from
`a–z2–7`. Total length 90. Regular expression for scanners and redaction:

```
\bang_[0-9A-HJKMNP-TV-Z]{26}_[a-z2-7]{59}\b
```

| Part | Options | Recommendation |
|---|---|---|
| Prefix | (a) `ang_`; (b) `angelia_`; (c) none | **(a) `ang_`.** Short, greppable, and with the fixed length and alphabets the regex above has practically no false positives. A visible prefix is what GitHub, GitLab and npm secret scanning key on, and what our own redaction (`tokenStart`, `safePath`) needs. (b) is more unique but longer for no gain once the body is structured. |
| Visible id | (a) the row's ULID; (b) a separate random lower-case id; (c) none, look up by hash | **(a).** It is the token's public `id` in the API and the audit log (`Principal.ID`), so a leaked token found by a scanner or in a log can be revoked by id without anyone holding admin access to the hash. ULIDs are the id convention of the whole API ("names lower case, ULIDs upper case, never overlap"), so `/v1/tokens/{id}` cannot be confused with a name. It reveals the creation time, which `GET /v1/tokens` lists anyway. |
| Random part | 128, 192 or 256 bits | **256 bits** (32 bytes from `crypto/rand`), as ADR 0024 §1 anticipated. Lower-case RFC 4648 base32 (`encoding/base32` with a custom alphabet): no `-`/`+`/`/`, so a double-click selects the whole token and no URL or shell escaping is needed. |
| Checksum | (a) CRC-32 suffix; (b) none | **(a).** It lets the server reject typos and random garbage without a database read (T11), and lets a scanner discard look-alikes offline (the reason GitHub added one in 2021). It is not a security control: CRC-32 is public and keyed by nothing. |

Parsing is strict: wrong prefix, length, alphabet, invalid ULID or a checksum mismatch is
`ErrUnauthenticated` before any store access. The format is versioned by its prefix: a later format
gets a new prefix (for example `ang2_`), and the table keeps a `hash_version` column (T3).

### T2. Hashing at rest

| Option | Cost per request | Protects against | Dependency |
|---|---|---|---|
| (a) **SHA-256** of the full token string | ~0.3 µs | A read of the database (backup, replica, SQL injection) yields hashes of 256-bit random values: preimage search is infeasible | standard library (`crypto/sha256`) |
| (b) **argon2id** (for example m = 64 MiB, t = 1, p = 4) | ~50–100 ms CPU and 64 MiB memory per request, and per stream re-check | Same, plus brute force of **low-entropy** secrets, which we never issue | `golang.org/x/crypto/argon2` (BSD-3, `golang.org/x`; not in `go.mod` today, so a new module) |
| (c) HMAC-SHA-256 with a server-side pepper (for example HKDF from the secrets key) | ~0.5 µs | Same as (a), plus a database-only leak without the key | standard library; but couples tokens to `secrets.key_file` |

**Recommendation: (a) SHA-256**, as ADR 0024 §1 anticipated. Slow hashes exist to make guessing
low-entropy passwords expensive; a 256-bit random secret cannot be guessed at any hash speed. argon2id
would put 50–100 ms and 64 MiB on every request, every stream re-check (T6) and every MCP tool call, and
turn each unauthenticated request into a memory-cost amplifier (an attacker sending garbage tokens with
valid-looking ids makes the server allocate 64 MiB each). A pepper (c) adds nothing against an attacker
who cannot guess 256 bits, and it would make every token depend on a key that is optional today
(ADR 0024: "an instance with no secrets needs no key") and that rotation would then have to cover.

Exact definition: `hash = SHA-256("angelia api token v1" || 0x00 || token)`, where `token` is the whole
`ang_…` string. The domain-separation prefix keeps the hash distinct from any other SHA-256 of the same
bytes; hashing the whole string binds the id to the secret, so a row's hash cannot be paired with
another row's id. Stored as 32 raw bytes, `hash_version = 1`.

### T3. Storage: `api_token`, migration `00021` on both engines

```sql
-- SQLite: internal/store/migrations/sqlite/00021_api_token.sql (STRICT, INTEGER ms, BLOB)
-- Postgres: internal/store/migrations/postgres/00021_api_token.sql (TEXT COLLATE "C", BIGINT ms, BYTEA)
CREATE TABLE api_token (
    id            TEXT    PRIMARY KEY,               -- ULID, the visible id of T1
    name          TEXT    NOT NULL,                  -- operator label, ^[a-z0-9][a-z0-9._-]{0,62}$
    scope         TEXT    NOT NULL CHECK (scope IN ('read', 'write', 'admin')),
    hash          BLOB    NOT NULL CHECK (length(hash) = 32),   -- Postgres: octet_length
    hash_version  INTEGER NOT NULL DEFAULT 1,
    created_at    INTEGER NOT NULL,
    created_by    TEXT    NOT NULL,                  -- token id, 'bootstrap' or 'local'
    expires_at    INTEGER,                           -- NULL: no expiry
    last_used_at  INTEGER,                           -- coalesced, see T6
    revoked_at    INTEGER,                           -- NULL: active
    revoked_by    TEXT
);
CREATE UNIQUE INDEX api_token_active_name ON api_token (name) WHERE revoked_at IS NULL;
```

- **Additive only**: the migration creates a table and rewrites no row. `Down` drops it (development
  only, like the other migrations).
- **Soft revocation.** A revoked row stays, so the audit trail can still name it and an id is never
  reused; names are unique among active tokens only (partial index, supported by both engines).
- **No foreign keys**, like the other history-like tables (ADR 0008).

**Store interfaces** (new file `internal/store/token.go`, embedded in `APIStore` the way `SecretStore`
is; this ADR is the record the contract rule asks for):

```go
type APIToken struct {
    ID, Name, Scope string
    Hash            []byte
    HashVersion     int
    CreatedAt       time.Time
    CreatedBy       string
    ExpiresAt, LastUsedAt, RevokedAt *time.Time
    RevokedBy       string
}

type TokenReader interface {
    // APITokenByID returns the row with this id (revoked or not), or ErrNotFound.
    APITokenByID(ctx context.Context, id string) (APIToken, error)
    // ListAPITokens pages by id; includeRevoked adds revoked rows.
    ListAPITokens(ctx context.Context, afterID string, limit int, includeRevoked bool) ([]APIToken, error)
}

type TokenTx interface {
    TokenReader
    CountAPITokens(ctx context.Context) (int64, error)            // every row, revoked included (T4)
    CreateAPIToken(ctx context.Context, t APIToken) error           // ErrConflict: id, or active name, taken
    RevokeAPIToken(ctx context.Context, id string, at time.Time, by string) (bool, error) // false: already revoked
}

type TokenStore interface {
    TokenReader
    // InTokenTx takes the config lock first on Postgres (ADR 0028 D15), BEGIN IMMEDIATE on SQLite.
    InTokenTx(ctx context.Context, fn func(tx TokenTx) error) error
    // TouchAPITokens sets last_used_at = max(last_used_at, t) per id, outside the config lock (T6).
    TouchAPITokens(ctx context.Context, used map[string]time.Time) error
}
```

`storetest` gets the shared cases (create, active-name conflict, revoke twice, touch never moves
backwards, count includes revoked rows) and both engines run them.

### T4. Bootstrap: the first admin token

Who can create the first token when token management itself needs an admin token?

| Option | Restart | Several replicas | Leak surface | Recovery of a lost admin token |
|---|---|---|---|---|
| (a) **Generate and print** on first start when the table is empty | Nothing printed again; a crash between insert and print loses the token | Whichever replica wins prints; the operator must find which | The token lands in container logs, journald and log shippers: "printed once" becomes "logged forever" | Needs (c) anyway |
| (b) **Bootstrap file**: the operator puts a token (made with `angelia token generate`) in a file; the server inserts its hash as `admin` **only if the table has never held a row** | Idempotent: the table is non-empty, the file is ignored | Same file everywhere; the insert runs under the config lock, exactly one replica inserts, the others see a row and skip | The file only (a Kubernetes or Docker secret mount); the server never prints or logs the value | Needs (c) |
| (c) **Instance-side CLI**: `angelia token create --local --scope admin --name NAME` opens the store from `--config`, like `angelia secrets rotate`, and prints the new token on its own stdout | Unaffected | Unaffected (one row, one transaction) | The operator's terminal only | **Is** the recovery path: whoever can read the instance config and reach the database is already all-powerful |

**Recommendation: (b) for unattended installs plus (c) for interactive installs and recovery, and no
automatic printing (a).** The issue's "printed once" is met by (c): the token is printed exactly once,
on the stdout of the command that created it, never by the server.

- On start, the `api` role checks the table. While it holds **no active admin token** it logs one
  `WARN` per start, without any secret: `no active admin API token; create one with "angelia token
  create --local --scope admin --name admin" (docs/configuration.md#api-tokens)`. The quickstart becomes
  one extra command: `docker compose exec angelia angelia token create --local --scope admin --name me`.
- **Bootstrap file.** New key `api.bootstrap_token_file`, env `ANGELIA_API_BOOTSTRAP_TOKEN_FILE` (the
  loader's `ANGELIA_<SECTION>_<KEY>` rule; the issue's `ANGELIA_BOOTSTRAP_TOKEN_FILE` would break that
  rule). The file holds one token in the T1 format and an optional newline. On `api` role start, in one
  `InTokenTx`: if `CountAPITokens` is **0**, insert the token's id and hash as `admin`, name
  `bootstrap`, `created_by = 'bootstrap'`, and log `INFO bootstrap admin token <id> stored` (the id, not
  the token); otherwise do nothing and, if the file's id is not in the table, log at `INFO` that the
  file is ignored. Counting **every** row, revoked included, means a revoked bootstrap token never comes
  back on the next restart. A malformed file or a bad checksum fails the start. A file that group or
  others can read gets a `WARN`, not a refusal (Kubernetes secret volumes default to `0644`).
- `angelia token generate [--out FILE]` (offline, no store, no config) prints or writes a fresh
  token for the bootstrap file, like `angelia secrets keygen` (`--out` creates the file `0600` and
  refuses to overwrite).
- `--local` works on both engines and while `serve` runs (SQLite takes its write lock briefly; Postgres
  takes the config lock). Its rows have `created_by = 'local'`.

### T5. Scopes and the route-to-scope table

Confirmed as they are: one scope per operation, ordered, **declared in the OpenAPI spec** (each
operation's `security` requirement), read by the server from the embedded document, and checked by
`TestScopesFromSpec` against the hand-written `wantScopes` in `internal/api/auth_test.go`, so a spec
change that moves a scope fails a test. `TestAuthEveryRoute` is the issue's "table test over all routes"
and already covers every operation; it switches from its fixed test tokens to real tokens from the new
authenticator in the implementation. The authoritative human-readable table stays in the spec
(`docs/api.md`, "Resources" and "Auth"); this ADR adds no copy.

New operations (T9) are all **admin**: `listTokens`, `createToken`, `getToken`, `revokeToken`. Reading
token metadata is admin too (like secrets), because the list shows who can do what. The `*_ref` rule of
spec decision 35 is unchanged. No MCP tool manages tokens: an agent with an admin token should not be
able to mint more tokens through a tool whose call the user may not inspect.

### T6. Revocation, expiry, last use and caching

- **Revocation** is `DELETE /v1/tokens/{id}` or `angelia token revoke`: it sets `revoked_at` and
  `revoked_by`. A token may revoke itself. Revoking the last active admin token is allowed (the CLI asks
  for confirmation unless `--yes`); (c) of T4 recovers.
- **Effect time.** With no cache (below), the next request on any replica fails. An open event stream
  fails at its next re-check, at most 60 s later (spec decision 15, already implemented). MCP over HTTP
  forwards the header on every call, so it fails at once.
- **Expiry**: optional `expires_at` set at creation (`--expires-in 90d` or an RFC 3339 time); no expiry
  by default. An expired token is `401` exactly like an unknown one. Expired rows are not deleted.
- **Caching: none in v0.1.** Authentication is one primary-key read (`APITokenByID`) plus a hash: tens of
  microseconds on SQLite, one round trip on Postgres. If a benchmark later shows it matters, a
  positive-only cache with a TTL of at most 10 s, cleared locally on revoke, can be added without a
  contract change; the stream re-check bound stays 60 s regardless.
- **Last use**: writing `last_used_at` on every request would turn every read into a write (on SQLite,
  a writer-lock acquisition per GET). Instead the authenticator records uses in memory and a background
  flush calls `TouchAPITokens` **at most once per token per 5 minutes**, and once on graceful shutdown.
  The value is documented as "accurate to about 5 minutes"; a crash loses at most the last interval.
  The flush takes no config lock (a plain `UPDATE … SET last_used_at = max(…)` per id), so it never
  waits behind config writes.

### T7. CLI

| Command | Path | Output |
|---|---|---|
| `angelia token create --name NAME --scope read\|write\|admin [--expires-in D \| --expires-at T]` | API (`POST /v1/tokens`, admin token) | The token alone on **stdout**, one line, so `$(angelia token create …)` works; id, name, scope and "shown once, store it now" on **stderr**. `-o json` prints the full created object, including `token`, on stdout. |
| `angelia token list [--all] [-o table\|json\|yaml]` | API | ID, NAME, SCOPE, CREATED, EXPIRES, LAST USED, and REVOKED with `--all`. Never a hash. |
| `angelia token revoke ID\|NAME [--yes]` | API | A name is resolved to the active token's id first. |
| `angelia token create\|list\|revoke --local` | Instance-side: `--config`, store opened directly | Same output. |
| `angelia token generate [--out FILE]` | Offline | A new token for the bootstrap file (T4). |

**Recommendation: one `token` command group with a `--local` flag**, not a separate `tokens` group.
The existing `secret`/`secrets` split separates different operations (values through the API, keys on
the instance); here the operations are the same and only the path differs, so a flag says that more
clearly than one letter. `--local` and `--server`/`--token` are mutually exclusive. The CLI never
writes the token anywhere by itself (no `--save` into the client config in v0.1).

### T8. Format of responses carrying the secret

The secret appears in exactly one place: the `201` response of `POST /v1/tokens` (member `token`) and
the stdout of `create`/`generate`. `GET` never returns it, the access log never logs bodies, and the
audit record of a create carries id, name, scope and creator only.

### T9. Public-contract impact

**Spec repository PR** (angelia-dev/spec, minor version, additive):

1. Paths `GET /v1/tokens` (`listTokens`, cursor paging like the other lists, `include_revoked`
   boolean query parameter), `POST /v1/tokens` (`createToken`, `201`), `GET /v1/tokens/{id}`
   (`getToken`), `DELETE /v1/tokens/{id}` (`revokeToken`, `204`; revoking a revoked token is `204`
   too). All `security: - bearerAuth: [admin]`.
2. Schemas `Token` (response: `id`, `name`, `scope`, `created_at`, `created_by`, `expires_at`,
   `last_used_at`, `revoked_at`, `revoked_by`), `TokenCreate` (request: `name`, `scope`, optional
   `expires_at`), `TokenCreated` (`Token` plus `token`, response-only).
3. **A Spectral exception**: the existing rule forbids secret-named properties in response schemas.
   Allow exactly `TokenCreated.token`, and add a rule that no other response schema or example has a
   `token` member.
4. Errors: an active-name clash is `422 validation-failed` with `errors[{pointer: "/name"}]` (no new
   problem type). `404 not-found` for an unknown id, after auth as usual.
5. `docs/api.md`: a Resources row (Tokens, admin/admin), new decisions recording T1 (the token format
   and regex, so scanners can rely on it), T5 (admin for token metadata) and T6 (revocation effect
   time), and the Auth section's "How tokens are created … is the core repository's API tokens story"
   replaced with a link to this ADR. The `bearerAuth` description names the `ang_` format.

**Core repository** (no spec PR, but public or internal contracts, reviewed here):

- Instance config key `api.bootstrap_token_file` and env `ANGELIA_API_BOOTSTRAP_TOKEN_FILE` (new YAML
  key: the YAML format is a public contract).
- CLI commands of T7.
- `ang_` added to `internal/config` `tokenStart` and to the watch-file token check, so a pasted API
  token is refused in configuration.
- Migrations `00021_api_token.sql` on both engines; `store.TokenStore` embedded in `store.APIStore`.
- Metric `angelia_api_auth_failures_total{reason}` with the closed label set `missing`, `malformed`,
  `unknown`, `revoked`, `expired` (ADR 0019).
- `api.Principal` gains `Name` (internal type; the access log adds `token_name`).
- Docs: `configuration.md` ("API tokens", replacing "Token storage is not built yet"), `cli.md`,
  `mcp.md` (drop "until API tokens exist"), the compose quickstart.

### T10. Constant-time comparison and lookup timing

- `Authenticate` parses the token (T1), reads the row by id, computes the T2 hash and compares it with
  `crypto/subtle.ConstantTimeCompare`. Then it checks `revoked_at` and `expires_at`. Every failure
  returns the same `ErrUnauthenticated` and the same `401` body.
- **Unknown id.** The handler still computes the hash and compares it with a fixed dummy, so the
  CPU path is the same; the store read is faster for a miss than for a hit. That difference can only
  tell an attacker whether a given **id** exists. Ids are not secrets (admins list them, logs carry
  them), and knowing one gives nothing toward the 256-bit part, so this is accepted rather than padded
  with artificial delay.
- **Malformed or bad checksum** is rejected before the store read. That timing difference reveals only
  whether the string is well-formed, which anyone can compute.

### T11. Failed authentication: rate limiting and logging

- **No per-client `429` limiter in v0.1.** The token cannot be brute-forced, malformed tokens cost no
  store access (T1 checksum), and a well-formed unknown token costs one indexed read. A per-IP limiter
  would also be harmful today: the instance has no trusted-proxy setting, so behind a reverse proxy
  every client shares the proxy's address and one attacker would lock everyone out. Revisit with a
  `X-Forwarded-For` trust setting (a later story).
- **Metric** `angelia_api_auth_failures_total{reason}` (T9) for alerting.
- **Log**: at most one `WARN api authentication failed` per reason per minute per process, with the
  count since the last line and, for `revoked`/`expired`, the token **id** (useful: "a revoked token is
  still in use by something"). Never the token, never a hash.

### T12. Audit

`Server.audit` records (`audit=true`, actor principal id and name) for `token.create`, `token.revoke`,
and, from the CLI and the server start, `token.create` with `created_by = local` or `bootstrap`. The
`--local` commands write the same record through the instance logger. The target's id, name and scope
are logged; the token and its hash never are.

## Consequences

### Positive

- Angelia becomes usable: `angelia serve` accepts real tokens, and the CLI, `apply`, the MCP server and
  the event stream work end to end.
- No new dependency: SHA-256, CRC-32, base32 and `crypto/subtle` are all in the standard library, and
  the token code is independent of the secrets key.
- A leaked token is recognisable (prefix, regex, checksum) and revocable by the id it carries.
- Bootstrap is deterministic on restarts and with several replicas, and a token is never written to a
  server log.

### Negative

- One store read per request (no cache). Fine for the expected load; measured in the implementation.
- `last_used_at` is approximate (about 5 minutes) and can lose the last interval on a crash.
- Interactive installs need one extra command (`token create --local`) instead of reading a token from
  the startup log.
- No brute-force throttling; relies on token entropy until a trusted-proxy setting exists.
- Revoked rows accumulate; a purge command is left for later.

### Open questions

- See "Questions for the maintainer" below.
- Registering the `ang_` format with GitHub's secret-scanning partner programme needs a public
  verification endpoint; left for after v0.1.
- OIDC (architecture §3.1 "later") would add a second `Authenticator`; nothing here blocks it.

## Alternatives considered

- **argon2id hashes** (T2 b): rejected for cost and DoS amplification with no gain for random tokens.
- **Peppered HMAC** (T2 c): rejected, couples tokens to the optional secrets key.
- **Lookup by hash instead of id** (T1 visible id c): equally safe (an index lookup on a SHA-256 value
  leaks nothing useful) and shorter tokens, but the token would carry no revocable id and the
  "constant-time comparison" criterion would be met only implicitly.
- **Print the bootstrap token on first start** (T4 a): the issue allows it, but in containers the
  console is the log, so the token would be logged; a crash between insert and print loses it; with
  several replicas the operator must find the right log.
- **Inline `ANGELIA_API_BOOTSTRAP_TOKEN` env** beside the file: like `ANGELIA_SECRETS_KEY`, convenient on
  platforms that only inject env, but env leaks into `docker inspect`, `/proc/<pid>/environ` and crash
  reports. Left out; can be added later without breaking anything (question 4).
- **Tokens in the watch file / `apply`**: rejected; a token is a credential and the watch file lives in
  Git.
- **Hard delete on revoke**: rejected; the audit trail would name ids that no longer exist, and ids could
  be reused.
- **Separate `angelia tokens` group** for instance-side commands (mirroring `secrets`): see T7.

## Questions for the maintainer

1. **T1 prefix and format**: `ang_<ULID>_<52 base32><7 CRC-32>` (90 chars). Approve, or prefer
   `angelia_` / no checksum? (Recommendation: approve as is.)
2. **T2 hashing**: SHA-256 with the `"angelia api token v1"` domain prefix, no argon2id, no pepper.
   (Recommendation: approve; it matches ADR 0024 §1.)
3. **T4 bootstrap**: no automatic printing; bootstrap file plus `token create --local`, with a `WARN`
   while no active admin token exists. Is that acceptable for the issue's "printed once", or do you want
   (a) as well, printed only when stderr is a terminal?
4. **T4 config surface**: new key `api.bootstrap_token_file` / env `ANGELIA_API_BOOTSTRAP_TOKEN_FILE`
   (loader convention, not the issue's `ANGELIA_BOOTSTRAP_TOKEN_FILE`). File only, or an inline env too?
   (Recommendation: file only.)
5. **T7 CLI shape**: one `token` group with `--local`, versus a separate instance-side `tokens` group.
   (Recommendation: `--local`.)
6. **T6 expiry default**: none (recommended) or a default such as 365 days?
7. **T11**: accept no `429` limiter until a trusted-proxy setting exists?
8. **T9 spec PR**: OK to add the Spectral exception for `TokenCreated.token` and the four admin
   operations as a minor spec release?

## Implementation split proposal

| Sub-story | Content | Size / complexity / model | Executor | Blocked by | ACs (issue #72) |
|---|---|---|---|---|---|
| **72-A** ADR 0030 | This note; maintainer answers the questions; status Accepted. | XS / High / Opus | Human-led (agent drafts) | – | Design note |
| **72-B** Spec | Spec PR of T9 (paths, schemas, Spectral exception, `docs/api.md` decisions); vendor the new spec into `api/openapi/`, regenerate, add the four ids to `wantScopes`. | S / Medium / Sonnet | Agent + human check (public contract) | 72-A | Scope checks on every route |
| **72-C** Token core and store | `internal/apitoken` (generate, parse, checksum, hash, `Authenticator` with constant-time compare and `last_used_at` coalescing); migrations `00021` on both engines; `store.TokenStore` on SQLite and Postgres; `storetest` cases; `ang_` in `tokenStart`. | M / High / Opus | Agent + human check (migrations, auth) | 72-A | Stored hashed; constant-time comparison |
| **72-D** API and wiring | `/v1/tokens` handlers, audit records, failure metric and rate-limited log, `serve` wires the real authenticator instead of `DenyAll`, bootstrap file at `api` role start; `TestAuthEveryRoute` on real tokens. | M / High / Opus | Agent + human check (API auth) | 72-B, 72-C | Bootstrap never logged; scope table test |
| **72-E** CLI and docs | `angelia token create\|list\|revoke [--local]` and `generate`; golden tests; `configuration.md`, `cli.md`, `mcp.md`, compose quickstart. | S / Medium / Sonnet | Agent | 72-D | `angelia token create\|list\|revoke` |

| | |
|---|---|
| Size | M (whole story, ≈ 1.5× M) → sub-stories XS + S + M + M + S |
| Complexity | Very high (design), High (implementation) |
| Suggested model | Opus (`claude-opus-5-5`) for 72-A, 72-C, 72-D; Sonnet (`claude-sonnet-5-5`) for 72-B, 72-E |
| Executor | 72-A Human-led; 72-B, 72-C, 72-D Agent + human check; 72-E Agent |
| Risk class | Security |
| Human estimate | **3.5 person-days** with review and tests (0.5 + 0.5 + 1 + 1 + 0.5) |
| Expected first-pass success (agent) | 72-B 60%, 72-C 45%, 72-D 40%, 72-E 60% |
| Expected agent wall-clock | ≈ 9 h (0.5 + 1.5 + 3 + 3 + 1.5 h, rounded down for overlap) |
| Expected human supervision | ≈ 6 h (decisions, migration and auth reviews) |

72-B and 72-C can run in parallel once 72-A is accepted.

## References

- Issue angelia-dev/angelia#72; epic angelia-dev/angelia#12.
- [architecture.md §3.1](../architecture.md#31-api), [§6](../architecture.md#6-language-and-framework-phase-0-assumptions),
  [§8](../architecture.md#8-security).
- [ADR 0024](0024-secrets-encrypted-at-rest.md) §1 (API tokens are hashed, not sealed), §5 (instance-side
  commands); [ADR 0028](0028-postgres-event-cursor-and-write-concurrency.md) §2 (config lock), D19
  (shared migration numbers); [ADR 0029](0029-mcp-server-go-sdk-rest-client.md) D10 (MCP forwards the
  caller's token); [ADR 0019](0019-prometheus-metrics-on-the-listen-address.md) (closed label sets).
- `internal/api/auth.go`, `internal/api/auth_test.go` (`TestScopesFromSpec`, `TestAuthEveryRoute`),
  `internal/api/server.go` (`MaxAuthRecheck`), `internal/mcp/http.go`, `cmd/angelia/api.go`,
  `cmd/angelia/clientconfig.go`, `cmd/angelia/secrets.go`.
- Spec `docs/api.md` decisions 15, 22, 23, 35 and "Auth"; `openapi/angelia.v1.yaml` `bearerAuth`.
- RFC 6750 (bearer tokens); GitHub, "Behind GitHub's new authentication token formats" (2021) for the
  prefix-and-checksum design.
