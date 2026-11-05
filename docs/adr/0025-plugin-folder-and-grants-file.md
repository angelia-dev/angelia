# ADR 0025: Each installed plugin has a folder in `plugins_dir`; the operator's grants live in its `grants.yaml`

- **Status:** Accepted
- **Date:** 2026-11-05
- **Story:** angelia-dev/angelia#184

## Context

[ADR 0011](0011-wasm-plugin-host-prototype.md) (amendment of #145) let the operator extend a WASM plugin's
`allowed_hosts` and bind credentials that the host injects. Those settings went into the instance config as
**draft** keys, `plugins.<kind>.allowed_hosts` and `plugins.<kind>.credentials.<host[:port]>.{token_env,
scheme}`. Question 1 of #184 asked whether they should stay there or move next to the plugin before release.

The maintainer answered (2026-11-04, #184) that they move **to the plugin folder**. Nobody uses the draft
keys yet, so the move breaks no one. But Angelia had no plugin folder: `angelia serve` does not load plugins,
and loading them from disk is [#88](https://github.com/angelia-dev/angelia/issues/88) (Phase 2). The
maintainer then chose (2026-11-05, via the project coordinator) to define the layout now, in this story,
rather than leave the keys as draft until #88.

## Decision

**Instance key `plugins_dir`** (`ANGELIA_PLUGINS_DIR`): an absolute path, by default `<data dir>/plugins`.
The data directory is the directory of the sqlite database file (`/var/lib/angelia` with the default
`database.dsn`), or `/var/lib/angelia` (`config.DefaultDataDir`) when the database is Postgres. An empty
environment value means the default. A missing directory means no plugins are installed.

**One folder per plugin**, named after its kind (`^[a-z][a-z0-9-]{0,62}$`, the manifest's rule):

```
<plugins_dir>/<kind>/
  manifest.json   # the author's manifest (ADR 0011); its kind must equal the folder name
  plugin.wasm     # the module; reading and running it is #88
  grants.yaml     # optional: what the operator grants beyond the manifest
```

Entries whose name starts with `.` are skipped. Plain files are ignored with a warning.

**`grants.yaml`** (`config.PluginGrants`, read by `config.LoadPlugins`). Unknown keys are rejected, and the
file holds one YAML document:

```yaml
allowed_hosts: [crates-mirror.example.com]
credentials:
  crates-mirror.example.com: {token_env: CRATES_MIRROR_TOKEN, scheme: Bearer}   # port 443
  crates-mirror.example.com:8443: {token_env: CRATES_MIRROR_TOKEN}
  gitlab.example.com: {token_env: GITLAB_TOKEN, header: PRIVATE-TOKEN}
```

- The keys keep the meaning and rules of the #145 amendment, plus `header` (ADR 0011 amendment of #184).
- A credential's host must be in the manifest's or the file's `allowed_hosts`.
- `token_env` names a variable. The file never holds a secret.
- **File mode.** A `grants.yaml` that the group or others can write is refused (`mode & 0o022`), and so is
  anything that is not a regular file. The mode is checked on the opened file. The file holds no secret
  values, so it may be readable by others. But it grants network access and binds tokens to hosts, so the
  risk is tampering, not reading. This differs from the #74 client config check, which refuses a file
  *readable* by others because that file holds a token. The check is skipped on Windows, as #74 does.

**The old keys are refused.** An instance config with a `plugins` key fails to load with `<file>: line N:
plugins: moved to the plugin's folder: write allowed_hosts and credentials in
<plugins_dir>/<kind>/grants.yaml`. The message names each valid kind it found.

**`angelia config check`** reads `plugins_dir` and prints each plugin:

- its kind;
- its allowed hosts, each marked `manifest` or `grants`;
- each credential's `host:port`, header and scheme, with the name of the variable that holds the token and
  whether that variable is set.

It never prints a token value. An invalid folder fails the check (`invalid plugins: ...`).

**#88 inherits this layout.** The production plugin host reads `manifest.json` and `plugin.wasm` from the
same folder. It turns each `grants.yaml` credential into a `wasm.Credential`, with `Token` reading
`InstalledPlugin.Token`, and passes netguard's `ExtraAuthHeaders` through `wasm.Options.ExtraAuthHeaders`.
Until #88 lands, `plugin.wasm` is neither read nor required.

## Consequences

### Positive

- Everything about one plugin lives in one folder: what the author ships and what the operator grants.
  Removing the folder uninstalls the plugin and revokes its grants at the same time.
- `grants.yaml` is a stable, public format before release, so #88 starts from a settled contract.
- The instance config holds no per-plugin settings, only where plugins live.

### Negative

- One more file whose permissions an operator must get right. The refusal message gives the `chmod`.
- `grants.yaml` has no environment overrides. A container deployment mounts the file instead.
- The check covers only the file's mode. A `<kind>` folder or `plugins_dir` that others can write would
  still let them replace the file. Checking the folders is left to #88, which will also verify
  `plugin.wasm`.

### Open questions

- A manifest digest pinned at install time, and an install command that writes the folder: #88.
- Whether a secret-store reference (#75) becomes an extra credential key (additive) next to `token_env`.

## Alternatives considered

- **Keep the keys in the instance config** (the #145 draft). This was the maintainer's first option to
  decide, and it was rejected in #184: per-plugin settings belong with the plugin.
- **A `grants` section in `manifest.json`.** Rejected: the manifest belongs to the author and will be
  digest-pinned, while grants come from the operator and have a different trust level.
- **Wait for #88 and keep the keys as draft until then.** Rejected by the maintainer, so that the format is
  settled before release.

## References

- angelia-dev/angelia#184 (questions and answers), #145 / PR #182, #88, #74 (client config mode check)
- [ADR 0011](0011-wasm-plugin-host-prototype.md) and its amendments; [configuration.md](../configuration.md#installed-plugins)
- `internal/config/plugins.go`, `cmd/angelia/config.go`
