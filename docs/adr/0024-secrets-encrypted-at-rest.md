# ADR 0024: Secret values are named, write-only rows sealed with AES-256-GCM under a key from a file; configuration references them by name

- **Status:** Accepted (2026-11-05, maintainer decision on #195: "go with what proposed")
- **Date:** 2026-11-05
- **Story:** angelia-dev/angelia#75

> Written as the design note for a **Human-led** story; the maintainer accepted it, with every open
> question settled as recommended, before the implementation.

## Context

[architecture.md §8](../architecture.md#8-security) and [prd.md §7](../prd.md#7-non-functional-requirements)
require secrets encrypted at rest with a key from env or file ("age or AES-GCM") and never returned by
the API. Issue #75 adds: a key rotation command, write-only output in API and CLI, a clear error and no
plaintext fallback when the key is missing, and a test that tampered ciphertext fails.

What the code does today (main at `3e36650`):

- **No secret value is stored anywhere.** The watch file and the instance config only accept
  *references* to environment variables (`*_env` keys: `sources.<kind>.token_env`,
  `plugins.<kind>.credentials.<host>.token_env` from #145, a webhook's `secret_env`).
  `internal/watchfile` rejects token-looking values, credentials in URLs and secret-named query
  parameters. The API (spec `docs/api.md` decisions 24–26) accepts only `*_env` names, allowed by
  `api.allowed_env`, and refuses any value a kind's `config_schema` marks `x-angelia-secret: true`.
- `sink.config_json_encrypted` (migration 00001) holds **plaintext JSON**: the dispatcher's
  `Options.DecryptConfig` is a pass-through (maintainer decision on #107) and `internal/api/sinks.go`
  carries `TODO(#75)`. The watch-file apply and `PUT /v1/sinks` detect no-op updates with
  `bytes.Equal` on that column.
- Env values are resolved in two places: instance-config tokens at startup into `config.Tokens` /
  `PluginTokens` (type `config.secret`, which prints `[REDACTED]` under every `fmt` verb, `slog` and
  JSON), and the CLI's `ANGELIA_TOKEN` through `os.Getenv` in `cmd/angelia/clientconfig.go`, which also
  refuses a client config file holding a token that group or others can read.
- `internal/api` writes audit records (`Server.audit`, `audit=true`, with the principal) for replays.
- `internal/secrets` is an empty package. Only SQLite migrations exist (next free: `00015`); the
  Postgres store is not written yet, so the design must be portable but only SQLite ships with it.

Upcoming stories that need stored secrets: the webhook sink's `whsec_` signing secret (#108,
ADR 0023), github-dispatch tokens, chat sinks whose URL carries a token (Slack, Discord, shoutrrr), and
private-registry credentials (#111: "auth referenced by secret name (stored with S-secrets)"). Env
references stay useful, but they put every secret into the environment of every node and cannot be
managed through the API.

## Decision

### 1. What is a secret

| Item | Stored by this ADR? | How |
|---|---|---|
| Webhook signing secret `whsec_…` (#108) | yes | `secret_ref` or `secret_env`, **at most one**; neither = unsigned, still allowed (ADR 0023) |
| github-dispatch / GitHub App tokens | yes | `token_ref` / `token_env` |
| Chat sinks (Slack, Discord, shoutrrr) | yes | a **plain base URL** (host visible and checked at write) plus a referenced token or path segment (`token_ref`); no whole-URL secret |
| Registry credentials (#111): bearer, basic password, npm `_authToken` | yes | `token_ref` / `password_ref` on the per-watch or per-source auth block; **host binding mandatory** (§2) |
| Plugin host credentials (#145) | later, additive | `token_ref` beside `token_env` |
| Custom credential headers (`Authorization`, `X-Api-Key`) | **future work** | deferred out of #108; a later story adds a reference shape |
| API tokens (#72) | **no** | stored as a **hash** (SHA-256 of 256 random bits, #72's call): the server never needs the value back |
| Bootstrap admin token, client config token (#74) | no | printed once / client-side file with the 0600 check |
| The master key, DB DSN password, `OTEL_*` headers | no | instance config or env, outside the database |
| `*_env` and `*_ref` names, sink names, URL hosts | no | references and metadata |

A secret is an **opaque byte string** of at most 64 KiB, stored once under a **name**, with optional
**`hosts`** metadata (§2), and referenced from configuration by its name. Its format belongs to the
consumer. For the webhook, ADR 0023 owns it: one value holding one to three space-separated `whsec_`
secrets, the first signing and all verifying. Rotation is a new write of that value. There are no
per-secret key ids or expiry, and the value is never returned.

If a future kind truly needs a whole URL as a secret, its schema marks the property, and the core
validates the **resolved** URL at delivery before calling the sink: scheme `https` (or `http` only where
the SSRF allowlist allows it), no userinfo, host allowed by netguard and by the secret's `hosts`. A
failure is a retryable error whose message carries only the scheme and host.

**Marking.** A configuration key whose name ends in `_ref` names a stored secret, exactly as a key ending
in `_env` names an environment variable. The server validates the name (`^[a-z0-9][a-z0-9._-]{0,62}$`)
and never treats it as a value. This needs code: `maskStrings` in `internal/api/sinks.go` skips only
`_env` keys today and must skip `_ref` too, and the watch-file parser must accept `*_ref` keys through
`secretFree`. `x-angelia-secret: true` in a kind's `config_schema` keeps its meaning from spec
decision 26 (a literal secret value: masked in every response, refused in API writes) as a fail-closed
backstop; no shipped kind needs it. The secret write request's `value` and `value_base64` carry
`writeOnly: true`. The spec's existing Spectral rule matches only secret-*named* properties and does not
fire on `value`, so the spec PR adds an **explicit rule**: those two properties must be `writeOnly` and
must not appear in any response schema or example.

### 2. Who may reference a secret

A reference sends a secret to wherever its config points, so `*_ref` gets two guards. Spec decision 25
(`api.allowed_env`) does the same job for `*_env`.

- **Admin scope for references.** An API write that **adds, removes or changes the value of any
  `*_ref` key** needs `admin`. That covers watches and subscriptions as well as sinks. On an endpoint whose
  scope is `write`, such a request from a `write` token gets `403` with `required_scope: admin`. Writes
  that leave every `*_ref` unchanged keep the endpoint's scope. Watch-file apply goes through the
  same rule (the apply token must be `admin` when the diff touches a `*_ref`).
- **Host binding.** A secret may carry `hosts`: a list in the syntax of #145's credential keys, `host`
  (port 443) or `host:port`, the host a lower-case hostname (IDNA as punycode) or an IP literal, with no
  wildcards. `hosts` is **mandatory** for a secret referenced from a source or registry credential
  (#111 `token_ref`, `password_ref`); the write of such a reference is refused with
  `422 secret-host-binding-required` if the secret has none. It is optional for sink secrets. With no
  `hosts`, the admin-only rule above is the guard.
- **The check.** Resolution happens before any request is built. A bound secret is resolved only for a
  destination whose canonical host (lower-cased, punycode, default port made explicit: 443 for https,
  80 for http) **equals** one entry of `hosts`. For a sink the destination is the kind's URL property,
  marked `x-angelia-destination: true` in its `config_schema`. For a source it is the target's base URL.
  netguard then re-checks **every outgoing request that carries the secret**, including each redirect hop
  a source follows. Sinks follow none (ADR 0017 §7). A request whose `URL.Host` does not match is not
  sent. A mismatch is an error naming the secret and the host, never the value. For a sink it is
  retryable, since the operator can fix the binding. For a source it is a fetch error.

### 3. Crypto

- **AES-256-GCM from the Go standard library** (`crypto/aes`, `crypto/cipher`, `crypto/rand`,
  `crypto/hkdf`, `crypto/sha256`). No new dependency.
- The data key is `HKDF-SHA256(master, salt = nil, info = "angelia secrets v1 aes-256-gcm")`, so a later
  use of the same master (for example a MAC) gets an independent key.
- **Key id**: the first 8 bytes of `HKDF-SHA256(master, info = "angelia secrets v1 key id")`, in hex.
- **Nonce**: 12 bytes from `crypto/rand` per seal, never a counter, never derived from the row. The NIST
  bound of 2^32 random-nonce seals per key is far beyond this workload.
- **Envelope** (stored in `secret.ciphertext`): byte 0 version `0x01`; bytes 1–8 key id; bytes 9–20
  nonce; then GCM ciphertext and 16-byte tag.
- **AAD** binds the ciphertext to its row and its header:
  `"angelia/secret/v1" 0x00 || envelope[0:9] || 0x00 || secret.id || 0x00 || secret.name` (neither a ULID
  nor a valid name contains NUL). Copying a ciphertext into another row, renaming a row in SQL, or
  editing the version or key-id byte makes `Open` fail. A rename through the API re-seals.
- Decryption errors are one sentinel (`secrets: cannot decrypt secret <name> (key id <hex>)`), never the
  bytes or the GCM detail. An unknown version or key id has its own error naming the key id.
- Plaintext lives in `secrets.Value` with the redacting `String`/`GoString`/`Format`/`LogValue`/
  `MarshalJSON` of `config.secret` (moved into `internal/secrets` and shared). `Reveal()` returns a
  **copy** of the bytes, so callers cannot mutate the held value, and is used only where a request is built.

**age is not chosen**: `filippo.io/age` (BSD-3) is not in [§6](../architecture.md#6-language-and-framework-phase-0-assumptions),
adds ≈200 bytes per value, and has **no associated data**, so binding a ciphertext to its row would need
a MAC of our own. Its public-key recipients do not help: only the server seals.

### 4. Key source

- `secrets.key_file` (already parsed, env `ANGELIA_SECRETS_KEY_FILE`): **one standard-base64 encoding
  of 32 random bytes** (44 characters), optionally followed by one newline. Anything else is a startup
  error that never echoes the content.
- `ANGELIA_SECRETS_KEY`: the same value inline, for platforms that only inject env. Setting both is a
  startup error. After reading, the server calls `os.Unsetenv`. That only keeps the key out of children
  spawned later: it does not scrub `/proc/self/environ` or the container runtime's record. Any exec-based
  sink or plugin must pass an explicit environment anyway. The file is the documented default.
- `secrets.previous_key_files` (env `ANGELIA_SECRETS_PREVIOUS_KEY_FILES`, comma-separated): keys used
  **only to decrypt**, during rotation and to read restored backups.
- **Permissions** (not on Windows). The server opens the file, following symlinks (Kubernetes secret
  volumes are symlinks), and checks the mode with `f.Stat()` on the **opened file**, so there is no
  time-of-check gap. It must be a regular file. It is **refused** when other users can read it or when
  group or others can write it; the error says `chmod 600`. It is **accepted with a warning** when only
  the group can read it. Ownership is not checked. The docs give working settings:
  - Kubernetes: `defaultMode: 0400` on the secret volume, or `0440` with `fsGroup` (the default `0644`
    is refused);
  - Docker Swarm: `secrets: [{source: …, mode: 0400}]` (the default `0444` is refused), with `uid` set
    to the container user;
  - Compose file secrets are **bind mounts that keep host mode and ownership**: `chown` the host file to
    the container UID (65532 for the distroless nonroot image) and `chmod 0400` it.
- `angelia secrets keygen` writes a new key to stdout or to `--out FILE` (created `0600`, refusing to
  overwrite). It is the only command that ever prints key material.

**Behaviour without a key** (checked at startup by every role, since all roles share the store):

| State | Outcome |
|---|---|
| No key, `secret` table empty | Starts. `config check` reports "secrets: no key configured (0 stored)". |
| No key, rows exist | **Refuses to start**: `secrets: 3 stored secrets but no key configured; set secrets.key_file`. No plaintext fallback exists in code. |
| A row's key id is not among the configured keys | Refuses to start, listing the unknown key ids and their row counts. |
| No key, a secret is written | `422 secrets-key-missing`; nothing stored. |
| A reference names a secret that does not exist | Apply/`PUT` warning, as for an unset `*_env`; deliveries fail before `Deliver`, retryable (§6). |

### 5. Key rotation

1. `angelia secrets keygen --out new.key`.
2. Configure `key_file: new.key`, `previous_key_files: [old.key]`, restart. With several nodes, first
   roll out `old.key` as the primary and `new.key` as a previous key everywhere, then swap them, so every
   node can read what any other node writes.
3. `angelia secrets rotate` (instance-side, reads the same config, needs the store; not an API call)
   re-seals every row whose `key_id` is not the primary's in **one transaction**, and audits the run.
   - **Crash safety.** A crash rolls the transaction back, leaving every row under a configured key.
     Rerunning is idempotent.
   - **SQLite.** `BEGIN IMMEDIATE` serialises the run against every writer, so no write can interleave.
   - **Postgres.** Each row is updated with `WHERE id = ? AND revision = ?` and its `revision` incremented.
     If a concurrent API write wins, `rotate` re-reads that row and tries again, **at most 3 times**. A
     row that still loses is reported and left alone, and the command exits non-zero, to be rerun.
   - **Large stores.** `--batch N` gives each batch its own transaction, with the same per-row guarantees.
4. `angelia secrets status` prints the row count per key id and marks which ids are configured. When
   the old id has 0 rows, remove it from `previous_key_files`. **Keep the old key file archived** as
   long as backups taken before the rotation are kept: restoring one needs it.

`rotate --dry-run` reports what it would re-seal. Neither command prints values.

### 6. Storage and resolution

Migration `00015_secret.sql` (SQLite now; the Postgres twin uses `BYTEA`, `BIGINT`, `TEXT[]`/JSON):

```sql
CREATE TABLE secret (
    id         TEXT    PRIMARY KEY,         -- ULID, immutable, part of the AAD
    name       TEXT    NOT NULL UNIQUE,     -- part of the AAD; referenced by *_ref keys
    hosts_json TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(hosts_json)), -- §2, not secret
    key_id     TEXT    NOT NULL,            -- hex, copy of envelope bytes 1–8
    ciphertext BLOB    NOT NULL,            -- envelope, §3
    revision   INTEGER NOT NULL DEFAULT 1,  -- optimistic lock (Postgres), §5
    created_at INTEGER NOT NULL,            -- unix ms UTC (ADR 0008)
    updated_at INTEGER NOT NULL
) STRICT;
CREATE INDEX secret_key_id ON secret (key_id);
```

- A **separate table**, referenced by name. The alternatives were encrypted columns on `sink` or a
  fully encrypted `config_json_encrypted`. With the separate table:
  - sink configuration stays plaintext and diffable (random nonces would break the `bytes.Equal` no-op
    detection);
  - an instance with no secrets needs no key;
  - rotation touches one table;
  - one secret can serve many sinks and watches.
- References are not foreign keys (they live in JSON). Deleting a referenced secret is refused
  (`409 secret-in-use`, listing referrers by name).
- **No data rewrite**: no stored secret value exists today. The migration only creates a table. Its
  `Down` drops it, which destroys the secrets; like the other migrations, it is for development only. As
  a fail-closed upgrade check, startup refuses to start if any sink config holds a literal at an
  `x-angelia-secret` path.
- `sink.config_json_encrypted` is a misnomer. A follow-up migration renames it `config_json`
  (decided question 4). `dispatch.Options.DecryptConfig` is replaced by the resolution below.

**Resolution (`sink.Request.Secrets`, additive to ADR 0017).** When the dispatcher has leased a delivery
and loaded its sink, the core walks the sink config and resolves every `*_env` and `*_ref` key:

- **Keys.** `Request.Secrets` is a `map[string]secrets.Value` keyed by the **RFC 6901 JSON Pointer of the
  reference key** (`/secret_ref`, `/auth/token_ref`).
- **Arrays.** Arrays are walked, and an element's key uses its index (`/targets/0/token_ref`), so every
  reference in an array resolves separately. A sink looks values up by the pointer of the key it read.
- **Failure.** If any reference fails to resolve (missing secret, unset or empty env variable, host
  mismatch §2, decryption error), the delivery fails **before `Deliver` is called** and is **retried** with
  the normal backoff. `last_error` names the pointer and the secret or variable name, never a value.
  Sinks (ADR 0023 is being amended accordingly) read secrets only from `Request.Secrets` and never touch
  the environment or the store. Values are not cached across attempts.
- **Scope of core resolution.** For now it covers **sink config only**:
  - instance-config tokens (`sources.<kind>.token_env`, `plugins.<kind>.credentials.*.token_env`) keep
    being resolved once at startup into `config.Tokens` / `PluginTokens`;
  - the CLI's `ANGELIA_TOKEN` stays a client-side `os.Getenv` in `apiclient`/`cmd/angelia`, outside the
    server;
  - source `*_ref` credentials (#111) use the same resolver in the fetch path when that story lands;
  - moving startup env tokens to the resolver is later work.

### 7. Write-only everywhere

- **API**. The new endpoints, all **admin** scope (names, hosts and referrers map the instance's
  credentials):
  - `PUT /v1/secrets/{name}` with `{"value": …}` or `{"value_base64": …}` (`writeOnly`) and an optional
    `"hosts": [...]`;
  - `GET /v1/secrets[/{name}]` returns `name, hosts, key_id, created_at, updated_at, referenced_by` and
    **never** a value;
  - `DELETE` as in §6.

  No endpoint, scope or flag returns a value. Problem details never echo it: a too-long value reports its
  size only. Bodies of this route are never logged.
- **Audit**. Every secret create, update (value or `hosts`), rename and delete, and every `rotate` run,
  writes an audit record through the existing `Server.audit` path (`audit=true`, the principal, the
  action, the secret name, the key id), never the value.
- **Sinks in responses**: masking is unchanged and stays fail-closed. `*_ref` values are names and are
  returned like `*_env` once `maskStrings` learns the suffix.
- **CLI**:
  - `angelia secret set NAME --from-file PATH|- [--host H]…`, plus `list` and `delete`.
  - The value is never accepted as an argument, never printed.
  - `--from-file` and stdin strip **exactly one trailing newline** (`\n` or `\r\n`), because editors
    end files with one; anything else, including other whitespace, is stored as is. This is documented,
    and ADR 0023's parser applies its own trimming on top.
  - `secrets keygen|rotate|status` are the instance-side commands.
- **Watch file**: references only, as today, now with `*_ref` beside `*_env`. Inline values stay
  refused. `angelia apply` uploads no values (decided question 5).
- **Logs**: `secrets.Value` redacts itself. Tests capture `slog` output (and `telemetrytest` spans) on
  the API write path, the dispatcher with a webhook-style sink, `rotate` and the startup errors. They
  assert that a canary plaintext, its base64 and the key never appear.
- **Required tests** (acceptance "tampered ciphertext fails"):
  - flipping each byte of an envelope fails;
  - swapping ciphertexts between rows fails;
  - renaming a row in SQL fails;
  - a wrong or unknown key id fails, naming it;
  - a truncated envelope fails;
  - seal → open round trips, and two seals of one plaintext differ;
  - startup is refused with rows and no key, and a write with no key gets `422`;
  - a `write` token changing a `*_ref` gets `403`;
  - a bound secret is refused for another host, including on a redirect hop;
  - `--from-file` strips one newline only.

### 8. Threat model

Protects against:
- theft or leak of the **database file, a backup, a dump, a Postgres replica or a DBA's read access**
  without the key file;
- accidental disclosure through the API, the CLI, logs, traces, `last_error` and events;
- a `write`-scope API caller redirecting a secret: references need `admin`, and bound secrets only go to
  their hosts.

Does **not** protect against:
- an attacker on the host or in the process who can read the key file, the environment or memory;
- an attacker with database **write** access. They cannot read or forge secrets. They can delete them,
  roll a row back to an older ciphertext of the same row, or point an **unbound** sink secret at their own
  host. `hosts` defeats that last attack for bound secrets;
- an `admin` token holder, who is trusted;
- loss of the key, which makes the stored secrets unrecoverable. Back the key up separately from the
  database.

## Consequences

### Positive

- Stdlib-only crypto, no new dependency; small, testable envelope with row binding.
- Instances that only use `*_env` references need no key and see no change.
- Configuration stays plaintext and diffable; watch files stay free of secret values; hosts stay visible.
- One credential serves many sinks and watches. Host binding limits where it can go.

### Negative

- New public contract to add to the spec and the config docs:
  - `/v1/secrets`;
  - `*_ref` keys and the admin rule for them;
  - `x-angelia-destination`;
  - the explicit Spectral rule;
  - the instance config keys **`secrets.previous_key_files`**, **`ANGELIA_SECRETS_KEY`** and
    `ANGELIA_SECRETS_PREVIOUS_KEY_FILES`;
  - CLI verbs (`secret set|list|delete`, `secrets keygen|rotate|status`);
  - problem types.
- A lost key loses every stored secret. A missing key stops every role from starting.
- Two places can hold a credential (env and store), and two env models (startup and per-delivery)
  coexist for now. The docs must explain them.
- Changes `sink.Request` (ADR 0017), the dispatcher hook, `maskStrings`, the watch-file parser and
  netguard (a re-check on requests that carry a bound secret: a sensitive path).

### Decided questions

The maintainer accepted every recommendation on #195 (2026-11-05).

1. **Named secrets** referenced with `*_ref`, not a per-sink `secrets` member. The spec change lands as
   a spec PR with the implementation.
2. **The `_ref` suffix**, mirroring `_env`; no structured value, no interpolation.
3. **Raw key in env** (`ANGELIA_SECRETS_KEY`) is supported; the key file is the documented default, and
   the docs carry the `os.Unsetenv` caveat.
4. **Renaming `sink.config_json_encrypted`** is a separate small story; this story's migration only
   adds a table.
5. **`angelia apply` uploads no values** in v0.1; operators run `angelia secret set`.
6. **Host matching** is exact host (and port) only in v0.1; no wildcards.
7. **Problem types** as proposed: `secrets-key-missing` (422), `secret-in-use` (409),
   `secret-host-binding-required` (422); a missing reference at apply time is a warning.
8. **`GET /v1/secrets`** needs the `admin` scope.
9. **A host-binding mismatch** on a sink is retryable; the delivery goes dead after the normal attempts.

## Alternatives considered

- **age** (`filippo.io/age`). New dependency, no associated data, recipient model not needed (§3).
- **Encrypt the whole `sink.config_json_encrypted` blob.** Every sink would need the key, random nonces
  break the no-op diff, the read API would need the key, and hosts would be hidden.
- **Whole-URL secrets (`url_ref`) for chat sinks.** They hide the host from write-time validation, from
  the diff and from `url_redacted`, and make host binding impossible. Replaced by a plain base URL and a
  referenced token.
- **`api.allowed_refs`** (an allowlist like `api.allowed_env`) instead of admin scope plus `hosts`. It
  limits *which* secrets but not *where* they go; host binding addresses the actual exfiltration.
- **Encrypted columns per secret field.** Spreads ciphertexts over many tables; rotation becomes a
  schema-wide walk.
- **Envelope encryption with per-row data keys.** Re-wrapping saves nothing at this size; one layer is
  simpler to audit.
- **Deterministic or counter nonces.** AES-GCM-SIV is not in the stdlib; counters need state shared
  across nodes.
- **`angelia secrets rotate --new-key-file`** editing config itself. The config-driven flow keeps the
  command a pure re-seal that is safe to rerun.
- **Env references only, no stored secrets.** Meets nothing in the issue's API and rotation criteria, and
  cannot serve per-watch registry credentials (#111) managed through the API.

## References

- [architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions),
  [§7](../architecture.md#7-deployment), [§8](../architecture.md#8-security);
  [prd.md §7](../prd.md#7-non-functional-requirements)
- Issues #75, #72 (API tokens, hashed), #74 (CLI, client config), #108 / ADR 0023 (webhook `whsec_`),
  #111 (registry credentials), #145 (plugin credentials)
- [ADR 0005](0005-standard-webhooks-signing.md), [ADR 0008](0008-store-history-rows-and-event-cursor.md),
  [ADR 0017](0017-dispatcher-delivery-leases-and-retries.md), [ADR 0022](0022-rest-api-in-house-spec-driven-validation.md)
- Spec `docs/api.md` decisions 22, 24, 25, 26 and the Secrets section
- NIST SP 800-38D (GCM, nonce limits); RFC 5869 (HKDF); RFC 6901 (JSON Pointer)

## Amendment (2026-11-05, #75 review)

The implementation review found places where the text above says more than v0.1 does. The accepted
text is left as is; this amendment states what holds.

- **No rename in v0.1** (§3, §7). The API has no rename: a secret's name is immutable
  (`UpdateSecret` never writes it). So "A rename through the API re-seals" and the rename audit record
  describe a future operation. Renaming a row in SQL still makes `Open` fail, as §3 says. A rename, if
  one is added, re-seals and is audited.
- **A missing reference** (§4 table). The watch-file apply warns. The API accepts the reference without
  a warning, because it has no channel for warnings yet.
- **Resolution** (§6), wired after #108 merged. On every attempt the dispatcher resolves each `*_env` key
  from the environment and each `*_ref` key through `secrets.Resolver` (`dispatch.Options.Secrets`), into
  `sink.Request.Secrets`, before `Deliver`. A missing secret, an invalid name, no key, a host mismatch or a
  decryption error is a retried error naming the pointer and the secret (and the host), never the value.
  A dispatcher with no resolver fails every `*_ref` the same way, so a stored-secret sink is never sent
  unsigned.
  - **Destination.** `api.SinkDestinations` reads the key each sink kind's `config_schema` marks
    `x-angelia-destination: true` (the spec's `WebhookSinkConfig.url` for the built-in kind) into
    `dispatch.Options.Destinations`, kind → JSON Pointer. A bound secret is resolved only for the URL at
    that pointer; a kind that marks none gets unbound secrets only. A kind that marks several is an error.
  - **Redirects.** Sinks follow no redirect (ADR 0017 §7; the webhook sink's client returns the `3xx`), so
    the URL checked at resolution is the only one the value is sent to. `Bound.CheckRedirect` stays for the
    source and registry credentials of #111, which follow redirects; `422 secret-host-binding-required`
    takes effect there too. A sink kind that sends a bound secret anywhere other than its destination
    (a redirect, a second request) needs netguard's per-request re-check (§2) before it may do so.
  - **Scheme.** The binding compares host and port only: `http://h:443/` matches a binding of `h`. Plain
    http needs the host in `http.allow_http_hosts`, and the webhook sends only an HMAC of the secret.
  - **`ANGELIA_` at delivery.** `*_env` resolution refuses `ANGELIA_` names too, as a retried error, so
    a sink row written before that rule cannot read the instance's own settings.
  - **Type.** `Request.Secrets` is `map[string]secrets.Value`, as §6 says, not the `map[string][]byte`
    #108 shipped (ADR 0023 §4). A `[]byte` prints its content under `%s` and `%q`, so a `Request` passed
    to `fmt` or slog would leak it; a `Value` prints `[REDACTED]` under every verb, and a sink calls
    `Reveal` only where it signs. The change touches only the webhook sink and its tests.
  - **`DecryptConfig` removed.** `dispatch.Options.DecryptConfig` was a pass-through; the sink
    configuration is plaintext and the resolution above replaces it (§6). The column rename stays decided
    question 4.
  - **`_ref` and `_env` are reserved suffixes** at any depth of a sink config, for plugin kinds too: a key
    with either suffix is always resolved, and a delivery fails when it does not resolve.
- **Scope of the stored-sinks startup check** (§6). The check runs in the role that opens the store,
  and today only the `api` role does: the scheduler and dispatcher roles of `serve` are stubs that
  don't open it. When they replace their stubs, they must run the same check before they start. The
  stored-secrets key check (§4) already runs in every role, before any role starts.
- **`ANGELIA_` names** (§2, §4). Like `api.allowed_env`, the instance config (`sources.<kind>.token_env`,
  a plugin's `credentials.<host>.token_env`) and the watch file (`secret_env`) refuse every name that
  starts with `ANGELIA_`. Otherwise `token_env: ANGELIA_SECRETS_KEY` would read the key before `serve`
  unsets it, and send it as a token.
- **A lost fenced update on `PUT /v1/secrets/{name}`** (§5). The request runs again on the new row, up
  to three times (last writer wins, as for any `PUT`). This cannot happen on SQLite, whose API
  transactions lock.

## Amendment (2026-11-06, #198)

The dispatcher role of `angelia serve` now runs with `dispatch.Options.Secrets` set to
`&secrets.Resolver{Store, Keys}` and `Options.Destinations` from `api.SinkDestinations`. The stored-sinks
check (§6) runs once per process, when any role that opens the store (`api`, `scheduler`, `dispatcher`)
is selected, before any role starts (`api.CheckStoredSinks`, the package-level form of
`Server.CheckStoredSinks`).
