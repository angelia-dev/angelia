# ADR 0024: Secret values are named, write-only rows sealed with AES-256-GCM under a key from a file; configuration references them by name

- **Status:** Proposed
- **Date:** 2026-11-05
- **Story:** angelia-dev/angelia#75

> Design note for a **Human-led** story: nothing here is implemented. Every public-contract choice
> (API resource, config keys, problem types, CLI verbs) and every crypto or key-handling choice waits
> for the maintainer. The numbered open questions at the end each carry a recommendation.

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
- Resolved env values live in `config.secret`, a type that prints `[REDACTED]` under every `fmt` verb,
  `slog` and JSON. `cmd/angelia/clientconfig.go` refuses a client config file that holds a token and
  is readable by group or others.
- `internal/secrets` is an empty package. Only SQLite migrations exist (next free: `00015`); the
  Postgres store is not written yet, so the design must be portable but only SQLite ships with it.

Upcoming stories that need stored secrets: the webhook sink's `whsec_` signing secret (#108,
ADR 0023 drafted in parallel), github-dispatch tokens, chat sinks whose URL *is* the credential
(Slack, Discord, shoutrrr URLs), and private-registry credentials (#111: "auth referenced by secret
name (stored with S-secrets)"). Env references stay useful, but they force every secret into the
process environment of every node and cannot be managed through the API.

## Decision

### 1. What is a secret

| Item | Stored by this ADR? | How |
|---|---|---|
| Webhook signing secret `whsec_…` (#108) | yes | `secret_ref` next to `secret_env` (exactly one) |
| github-dispatch / GitHub App tokens | yes | `token_ref` / `token_env` |
| Chat URLs with an embedded token (Slack, Discord, shoutrrr) | yes | the **whole URL** is the secret: `url_ref` |
| Sensitive custom header values (`Authorization`, …) | yes | per-header reference; shape decided by ADR 0023 |
| Registry credentials (#111): bearer, basic password, npm `_authToken` | yes | `token_ref` / `password_ref` on the per-watch or per-source auth block |
| Plugin host credentials (#145) | later, additive | `token_ref` beside `token_env` |
| API tokens (#72) | **no** | stored as a **hash** (SHA-256 of 256 random bits, #72's call): the server never needs the value back |
| Bootstrap admin token, client config token (#74) | no | printed once / client-side file with the 0600 check |
| The master key, DB DSN password, `OTEL_*` headers | no | instance config or env, outside the database |
| `*_env` and `*_ref` names, sink names, URL hosts | no | references and metadata, returned by the API |

A secret is an **opaque byte string** of at most 64 KiB, stored once under a **name** and referenced
from configuration by that name. Its format (for example, several space-separated `whsec_` values for
signature rotation) belongs to the consumer: ADR 0023 owns the webhook secret format.

**Marking.** A configuration key whose name ends in `_ref` names a stored secret, exactly as a key ending
in `_env` names an environment variable. The server validates the name (`^[a-z0-9][a-z0-9._-]{0,62}$`),
never masks it and never treats it as a value. `x-angelia-secret: true` in a kind's `config_schema`
keeps its meaning from spec decision 26 (a literal secret value: masked in every response, refused in
API writes) as a fail-closed backstop; with references, no shipped kind needs it. `writeOnly: true` is
used on the `value` property of the secret write request (the spec's Spectral rule already requires it
for secret-named properties). See open question 1 for the alternative the spec anticipated.

### 2. Crypto

- **AES-256-GCM from the Go standard library** (`crypto/aes`, `crypto/cipher`, `crypto/rand`,
  `crypto/hkdf`, `crypto/sha256`). No new dependency; FIPS-140-capable through Go's native module.
- The configured 32-byte **master key** is not used directly: the data key is
  `HKDF-SHA256(master, salt = nil, info = "angelia secrets v1 aes-256-gcm")`, so a later use of the same
  master (for example a MAC) gets an independent key.
- **Key id**: the first 8 bytes of `HKDF-SHA256(master, info = "angelia secrets v1 key id")`, shown in
  hex. It identifies a key without revealing it and lets the server pick the right key during rotation.
- **Nonce**: 12 bytes from `crypto/rand` per seal. Random nonces are safe far beyond this workload (the
  NIST bound is 2^32 seals per key; an instance writes a handful of secrets per day). Never a counter,
  never derived from the row.
- **Envelope** (stored in `secret.ciphertext`):

  ```
  byte 0      version = 0x01
  bytes 1–8   key id
  bytes 9–20  nonce
  bytes 21–   GCM ciphertext || 16-byte tag
  ```

- **AAD** binds the ciphertext to its row and to its header:
  `"angelia/secret/v1" 0x00 || envelope[0:9] || 0x00 || secret.id || 0x00 || secret.name`. Copying a
  ciphertext into another row, renaming a row in SQL, or editing the version or key-id byte makes `Open`
  fail. A rename through the API re-seals with the new name.
- Decryption errors are one sentinel (`secrets: cannot decrypt secret <name> (key id <hex>)`), never the
  bytes, never the GCM detail. An unknown version byte or key id is its own error naming the key id, so
  the operator knows which key file is missing.
- Plaintext is held in a `secrets.Value` type with the same redacting `String`/`GoString`/`Format`/
  `LogValue`/`MarshalJSON` as `config.secret` (moved into `internal/secrets` and shared), and an explicit
  `Reveal() []byte` used only where a sink builds its request.

**age is not chosen**: `filippo.io/age` (BSD-3) is not in [§6](../architecture.md#6-language-and-framework-phase-0-assumptions)
and would need approval; it is a file format with per-recipient X25519 stanzas (≈200 bytes overhead per
value), and it has **no associated data**, so it cannot bind a ciphertext to its row without a second MAC
we would design ourselves. Its strength (public-key recipients, so a CI job can encrypt without being
able to decrypt) does not apply: only the server seals.

### 3. Key source

- `secrets.key_file` (already parsed, env `ANGELIA_SECRETS_KEY_FILE`): the path of a file holding
  **one standard-base64 encoding of 32 random bytes** (44 characters), optionally followed by one newline.
  Anything else (wrong length, other content, URL-safe alphabet) is a startup error that never echoes
  the content.
- `ANGELIA_SECRETS_KEY`: the same base64 value inline, for platforms that only inject env. Setting both
  is a startup error. After reading, the server calls `os.Unsetenv` so plugin subprocesses do not
  inherit it. The file is the documented default (open question 3).
- `secrets.previous_key_files` (env `ANGELIA_SECRETS_PREVIOUS_KEY_FILES`, comma-separated): extra keys
  used **only to decrypt**, during rotation. New seals always use the primary key.
- **Permissions** (not on Windows, like the client config check): the file, after following symlinks
  (Kubernetes secret volumes are symlinks), must be a regular file; it is refused when writable by group
  or others or readable by others (`chmod 600` hint in the error). Group-readable (`0440`, Kubernetes
  `fsGroup`) is accepted with a warning. Ownership is not checked (containers run under arbitrary UIDs).
- `angelia secrets keygen` writes a new key to stdout or `--out FILE` (created `0600`, refusing to
  overwrite). It is the only command that ever prints key material.

**Behaviour without a key** (checked at startup by every role, since all roles share the store):

| State | Outcome |
|---|---|
| No key, `secret` table empty | Starts. `config check` reports "secrets: no key configured (0 stored)". |
| No key, rows exist | **Refuses to start**: `secrets: 3 stored secrets but no key configured; set secrets.key_file`. No plaintext fallback exists in code. |
| Key configured, a row's key id is not among the configured keys | Refuses to start, listing the unknown key ids and their row counts. |
| No key, a secret is written (API/CLI) | `422` problem `secrets-key-missing`; nothing stored. |
| A reference names a secret that does not exist | Apply/`PUT` warning, as for an unset `*_env`; the delivery fails with a retryable error naming the reference until the secret is created. |

### 4. Key rotation

Config-driven, two commands, crash-safe by construction:

1. `angelia secrets keygen --out new.key`.
2. Configure `key_file: new.key`, `previous_key_files: [old.key]`, restart. With several nodes, first
   roll out `old.key` primary + `new.key` previous everywhere, then swap: every node can then read
   whatever any other node writes.
3. `angelia secrets rotate` (instance-side command, reads the same config, needs the store; not an API
   call): re-seals every row whose `key_id` is not the primary's, in **one transaction**. Each row is
   updated with `WHERE id = ? AND revision = ?` and its `revision` incremented, so a concurrent API write
   wins and the row is simply re-read. A crash rolls the transaction back: every row is still under a
   configured key, and running `rotate` again is idempotent. Secrets are few (hundreds at most), so one
   transaction is fine on SQLite (one writer; `BEGIN IMMEDIATE`) and on Postgres (default isolation plus
   the revision guard). `--batch N` exists for very large stores, each batch its own transaction, with
   the same guarantees per row.
4. `angelia secrets status` prints counts per key id and marks which ids are configured. When the old
   id has 0 rows, remove `previous_key_files` and restart.

`rotate --dry-run` reports what it would re-seal. Neither command prints values. Rotation of an
individual secret's *value* is just a new write (`angelia secret set`), and the webhook's overlapping
signature rotation is a value holding two `whsec_` entries (ADR 0023).

### 5. Storage

Migration `00015_secret.sql` (SQLite now; the Postgres twin uses `BYTEA` and `BIGINT`):

```sql
CREATE TABLE secret (
    id         TEXT    PRIMARY KEY,         -- ULID, immutable, part of the AAD
    name       TEXT    NOT NULL UNIQUE,     -- part of the AAD; referenced by *_ref keys
    key_id     TEXT    NOT NULL,            -- hex, copy of envelope bytes 1–8, for status/rotate
    ciphertext BLOB    NOT NULL,            -- envelope, §2
    revision   INTEGER NOT NULL DEFAULT 1,  -- optimistic lock for rotate vs writes
    created_at INTEGER NOT NULL,            -- unix ms UTC (ADR 0008)
    updated_at INTEGER NOT NULL
) STRICT;
CREATE INDEX secret_key_id ON secret (key_id);
```

- A **separate table** referenced by name, not encrypted columns on `sink` or a fully encrypted
  `config_json_encrypted`: sink configuration stays plaintext and diffable (the watch-file apply and
  `PUT` no-op detection compare bytes, which random nonces would break); an instance without secrets
  needs no key; rotation touches one table; one secret can serve many sinks and watches (#111).
- References are **not** foreign keys (they live inside JSON). Deleting a secret that a sink, watch or
  source config references is refused (`409 secret-in-use`, listing the referrers by name); `--force`
  is not offered.
- **No data rewrite.** No stored secret value exists today (§Context): the migration only creates a
  table, and the `Down` drops it (which destroys secrets; the Down is for development only, as for the
  other migrations). As a fail-closed upgrade check, startup scans sink configs for any `x-angelia-secret`
  path holding a literal and refuses to start if one is found, rather than silently re-encrypting.
- `sink.config_json_encrypted` is a misnomer from 00001. A follow-up migration renames it `config_json`
  and adds `CHECK (json_valid(config_json))`, and `dispatch.Options.DecryptConfig` is replaced by the
  reference resolution below (open question 5). Renaming is data-preserving but touches a migration.

**Resolution.** The core, not each sink, resolves references. When the dispatcher leases a delivery it
resolves every `*_env` and `*_ref` key of the sink config into `sink.Request.Secrets`, a
`map[string]secrets.Value` keyed by the config key's JSON path (an additive field on the request struct
of ADR 0017). Sinks read secrets only from there and never touch env or the store. Sources (#111) get
the same through their fetch context. Decryption costs microseconds; values are not cached across
deliveries, so a changed secret applies to the next attempt.

### 6. Write-only everywhere

- **API** (additions to the spec, admin scope): `PUT /v1/secrets/{name}` with
  `{"value": "<string>"}` (`writeOnly`) or `{"value_base64": …}`; `GET /v1/secrets[/{name}]` returns
  `name, key_id, created_at, updated_at, referenced_by` and **never** a value; `DELETE` as in §5. There is
  no endpoint, scope or flag that returns a value. Problem details never echo the value (a too-long value
  says its size only). The request-body logger, if any, is disabled for this route.
- **Sinks in responses**: unchanged masking (`sinkJSONOf`, fail-closed when a schema is incomplete);
  `*_ref` values are names and are returned like `*_env`.
- **CLI**: `angelia secret set NAME --from-file PATH` (or `-` for stdin), `list`, `delete`. The value is
  never accepted as an argument (visible in `ps` and shell history) and never printed; `set` echoes
  `secret NAME stored (key id …)`. `secrets keygen|rotate|status` are the instance-side commands of §3–4.
- **Watch file**: references only, as today. It gains `*_ref` keys beside `*_env`; inline values remain
  refused, so watch files stay safe to commit. `angelia apply` does not upload secret values
  (open question 6).
- **Logs**: `secrets.Value` redacts itself; tests run the API write path, the dispatcher with a
  webhook-style sink, `rotate` and the startup errors with a captured `slog` handler (and
  `telemetrytest` spans) and assert that a canary plaintext, its base64 and the key never appear.
- **Required tests** (acceptance "tampered ciphertext fails"): flipping each byte of an envelope fails;
  swapping ciphertexts between two rows fails; renaming a row in SQL fails; a wrong or unknown key id
  fails with the key id in the message; a truncated envelope fails; seal → open round trips; two seals
  of the same plaintext differ; startup refusal with rows and no key; `422` on write without a key.

### 7. Threat model

Protects against: theft or leak of the **database file, a backup, a dump, a Postgres replica or a
DBA's read access** without the key file; accidental disclosure through the API, the CLI, logs, traces,
`last_error` and events.

Does **not** protect against: an attacker on the host or in the process who can read the key file, the
environment or memory (root, a debugger, a compromised plugin host); an attacker with **write** access
to the database, who cannot forge or read secrets but can delete them, roll a row back to an older
ciphertext of the same row, or edit a sink's plaintext URL so that a token-bearing request goes to their
host (open question 7); loss of the key, which makes the stored secrets unrecoverable: operators back
up the key separately from the database.

## Consequences

### Positive

- Stdlib-only crypto, no new dependency; small, testable envelope with row binding.
- Instances that only use `*_env` references need no key and see no change.
- Configuration stays plaintext and diffable; watch files stay free of secret values.
- One credential serves many sinks and watches; rotation of the key and of a value are both simple.

### Negative

- A new public surface: `/v1/secrets`, `*_ref` keys, `secrets.*` config keys, CLI verbs, problem types.
- A lost key loses every stored secret; a missing key stops every role from starting.
- Two places can hold a credential (env and store); docs must explain when to use which.
- The sink request struct (ADR 0017) and the dispatcher hook change.

### Open questions

1. **Named secrets vs a per-sink `secrets` member.** Spec decision 24 anticipated a `secrets` member on
   the sink resource. *Recommendation:* named secrets with `*_ref`, because #111 already asks for
   credentials "referenced by secret name", one token often serves several sinks, and watch files can
   reference a name but cannot hold a value. Needs a spec PR either way.
2. **Suffix `_ref`** (`secret_ref`, `token_ref`, `url_ref`) vs a structured value
   (`{"secret": "name"}`) vs `${secret:name}` interpolation. *Recommendation:* the suffix, mirroring
   `_env`, which the masking code, the watch-file parser and `api.allowed_env` already understand.
   Interpolation invites partial-string secrets that masking cannot see.
3. **Raw key in env** (`ANGELIA_SECRETS_KEY`). *Recommendation:* support it (the issue and §8 say "env or
   file") but document the file as preferred; env leaks through `/proc/*/environ`, `docker inspect` and
   crash reports.
4. **Group-readable key file**: warn (proposed) or refuse? *Recommendation:* warn, so Kubernetes
   `fsGroup` mounts work; refuse world-readable.
5. **Rename `sink.config_json_encrypted` to `config_json`** in a migration of this story or a separate
   one? *Recommendation:* separate small story after this ADR is accepted, so this story's migration
   only adds a table.
6. **`angelia apply` uploading values** (`value_from: {file: …}` resolved client-side). *Recommendation:*
   no for v0.1; operators run `angelia secret set` once. Revisit with the UI.
7. **Binding token secrets to hosts** (like #145's per-host plugin credentials) so a tampered sink URL
   cannot redirect a token. *Recommendation:* add optional `hosts` metadata to a secret later; for v0.1
   rely on admin-only sink writes and netguard. Note it in the docs' threat model.
8. **Problem types** `secrets-key-missing` (422) and `secret-in-use` (409), and whether a missing
   reference at apply time is a warning (proposed, like `*_env`) or an error. *Recommendation:* as
   proposed.
9. **Who resolves secrets for sinks**: the core into `Request.Secrets` (proposed, amends ADR 0017
   additively) or each sink via a resolver interface. *Recommendation:* the core, so sinks cannot reach
   the store or the environment; ADR 0023 should read secrets from the request.

## Alternatives considered

- **age** (`filippo.io/age`). New dependency, no associated data, recipient model not needed (§2).
- **Encrypt the whole `sink.config_json_encrypted` blob.** Every sink would need the key, random nonces
  break the byte-equality no-op diff of apply and `PUT`, the read API would need the key, and secrets
  could not be shared across sinks and watches.
- **Encrypted columns per secret field.** Spreads ciphertexts over many tables and makes rotation a
  schema-wide walk; AAD per column is easy but the rotation and status queries are not.
- **Envelope encryption with a per-row data key wrapped by the master.** Rotation would re-wrap only the
  32-byte data keys, but the values are already small; one layer is simpler to audit.
- **Deterministic nonces (SIV) or nonce counters.** Not in the stdlib (AES-GCM-SIV) or need persistent
  state across nodes; random 96-bit nonces are sufficient at this volume.
- **`angelia secrets rotate --new-key-file`** writing the new key into config itself. The command would
  have to edit config files and coordinate nodes; the config-driven flow keeps the command a pure
  re-seal that is safe to rerun.
- **Env references only, no stored secrets.** Meets nothing in the issue's API/rotation criteria and
  cannot serve per-watch registry credentials (#111) managed through the API.

## References

- [architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions),
  [§8](../architecture.md#8-security); [prd.md §7](../prd.md#7-non-functional-requirements)
- Issues #75, #72 (API tokens, hashed), #74 (CLI, client config), #108 / ADR 0023 (webhook `whsec_`),
  #111 (registry credentials), #145 (plugin credentials)
- [ADR 0005](0005-standard-webhooks-signing.md), [ADR 0008](0008-store-history-rows-and-event-cursor.md),
  [ADR 0017](0017-dispatcher-delivery-leases-and-retries.md), [ADR 0022](0022-rest-api-in-house-spec-driven-validation.md)
- Spec `docs/api.md` decisions 22, 24, 25, 26 and the Secrets section
- NIST SP 800-38D (GCM, nonce limits); RFC 5869 (HKDF); Go `crypto/hkdf` (Go 1.24+)
