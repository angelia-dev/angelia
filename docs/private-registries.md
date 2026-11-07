# Private registries and mirrors

Angelia can watch packages on a private registry or a mirror: Artifactory, Nexus, Verdaccio, devpi, a
GitHub Enterprise Server, a self-managed GitLab. You set the registry's `base_url` and its credential once per
source (or named source instance) in the [instance configuration](configuration.md). Design and security rules:
[ADR 0024, amendment of #111](adr/0024-secrets-encrypted-at-rest.md).

- A watch cannot choose its own base URL or credential ([watch-file.md](watch-file.md) decision D9).
  It chooses a **source**: the built-in kind (`source: npm`, one registry per kind) or a
  [named instance](#several-registries-of-one-kind) (`source: npm-internal`) that the instance
  configuration defines.
- Events from a registry other than the public one carry its URL as the `repository_url` qualifier of
  the purl. It is the configured `base_url` as written, with any API path (`/api/v3`, `/api/v4`) and
  without a trailing slash.

## Credentials

Put the credential in a [stored secret](configuration.md#stored-secrets) **bound to the registry's host**,
and name it from the source:

```
angelia secret set nexus-npm --from-file ./token.txt --host nexus.example.com
```

```yaml
sources:
  npm:
    base_url: https://nexus.example.com/repository/npm-group
    auth:
      type: npm_auth_token        # or bearer; both send "Authorization: Bearer <token>"
      token_ref: nexus-npm
```

| `auth.type` | Keys | Header |
|---|---|---|
| `bearer` (default) | `token_ref` | `Authorization: Bearer <token>` |
| `npm_auth_token` | `token_ref` | the same; npm's `_authToken` |
| `basic` | `username`, `password_ref` | `Authorization: Basic base64(username:password)` |

- `hosts` is mandatory. A secret without it, or bound to another host, is refused when the source fetches,
  and no request is sent. The binding is host and port: for a registry on 8443 write `--host nexus.example.com:8443`.
- The secret is read from the store on every request and checked again on every redirect hop. A redirect
  to a host outside the binding (a registry that redirects downloads to a CDN, say) is refused; bind the
  secret to both hosts if you want it followed.
- `token_env` (the name of an environment variable) still works, for `pypi`, `npm`, `github-release`, `github-tag` and
  `gitlab-release` (which sends it as `PRIVATE-TOKEN`). It
  is sent only to the host of `base_url`. Set `token_env` or `auth`, not both.
- No value appears in `config check`, logs, errors, events or API responses. A value written in a `*_ref`
  key is refused as not being a secret name, without being printed.

## Several registries of one kind

To watch the same kind of registry at several URLs (the public npm and an internal Nexus, say), define a
named instance. The key is the name; `kind` selects the implementation:

```yaml
sources:
  npm-internal:
    kind: npm
    base_url: https://nexus.example.com/repository/npm-group
    auth: { token_ref: nexus-npm-token }
```

```yaml
# watch file
watches:
  - {name: widget, source: npm-internal, coordinates: {name: "@acme/widget"}}
```

- The instance has its own `base_url`, `token_env` and `auth`, all as for a built-in kind. The built-in
  `npm` (the public registry, or whatever `sources.npm` configures) stays available next to it. A
  `token_env` is sent only by the source it is set on.
  `http.ca_file` and the proxy rules are instance-wide and apply to every instance.
- The name is lower-case letters, digits and single hyphens. A name that is a built-in kind (`npm`) cannot
  select another kind. `kind` is one of `pypi`, `npm`, `crates`, `github-release`, `github-tag`,
  `gitlab-release`; anything else stops `angelia serve` and `config check`.
- A watch with a name that is neither a built-in kind nor a configured instance is refused by the watch file,
  by the API and by `angelia config check --watches`.
- Events keep the kind: the purl type, `data.source`, the subject and the version scheme are those of
  `npm`, whatever the instance is called. The purl's `repository_url` is the instance's `base_url`. A
  subscription `filter.source` therefore names kinds, not instances.
- The watch's `source` (watch file, API) is the instance name. The same package on two instances is two
  targets, each fetched from its own registry.
- Secrets named by an instance's `auth` are checked against the instance's own host.

### What a name is bound to

A target remembers the kind it was created for (`target.source_kind`, migration 00017; empty means the kind
equals the name, which is every target of a built-in kind and every target stored before instances). The
registry now behind the name must still be of that kind:

- If you change `kind` of an instance (or a future built-in kind takes the name of one of your instances), its
  stored targets are **not** reinterpreted. Their checks fail with a permanent error, are retried only
  once a day, fetch nothing and emit no events; `angelia apply` and `PUT /v1/watches/<name>` refuse a watch
  that would use them, with an error that names both kinds.
- To change a registry's kind, give the instance a new name and re-create its watches. To keep the name, put
  `kind` back.
- Removing or renaming an instance leaves its stored targets without a source: their checks fail until the
  name is configured again. `angelia config check` and the log at `serve` start warn about stored targets
  whose source is not configured or changed kind (when the database can be read).
- The migration only adds a nullable column; it does not rewrite any row.

### Naming rules

- 1 to 63 characters, lower-case letters, digits and single hyphens.
- An instance cannot take the name of an installed plugin's kind (`config check` fails). A `sources` key that is
  neither a built-in kind, an installed plugin nor an instance (no `kind`) is reported by `config check`
  as ignored, usually a typo.
- Logs and traces of a check carry `source=<name>` and `kind=<kind>` (`angelia.source.name`,
  `angelia.source.kind`). Metrics are per kind, never per instance, so their cardinality stays bounded.
- `?source=` means the name in `GET /v1/watches` (it filters the watches by the `source` they have) and the
  kind in `GET /v1/events` and in subscription filters (events carry `data.source`, the kind).

## Examples

### Artifactory

PyPI remote repository `pypi-remote` and npm remote repository `npm-remote`, authenticated with an access
token (bearer) or a user and password (basic). The URL is the repository's API path; Angelia appends
`/pypi/<name>/json` for PyPI and `/<name>` for npm.

```yaml
sources:
  pypi:
    base_url: https://artifactory.example.com/artifactory/api/pypi/pypi-remote
    auth: {type: basic, username: angelia, password_ref: artifactory-password}
  npm:
    base_url: https://artifactory.example.com/artifactory/api/npm/npm-remote
    auth: {type: bearer, token_ref: artifactory-token}
```

```
angelia secret set artifactory-password --from-file ./pw.txt --host artifactory.example.com
angelia secret set artifactory-token    --from-file ./token.txt --host artifactory.example.com
```

### Nexus

An npm group and a PyPI proxy repository, with a user token as basic credentials for PyPI.

```yaml
sources:
  npm:
    base_url: https://nexus.example.com/repository/npm-group
    auth: {type: npm_auth_token, token_ref: nexus-npm}
  pypi:
    base_url: https://nexus.example.com/repository/pypi-proxy
    auth: {type: basic, username: angelia, password_ref: nexus-pypi-password}
```

By default Angelia reads the PyPI JSON API (`/pypi/<name>/json`). Check that your repository serves it at that
path. A server that offers only the Simple API needs `api: simple` (next section); a JSON API elsewhere cannot
be watched yet.

### pypiserver and other Simple-API-only indexes

pypiserver (and some mirrors) serve only the Simple API, not `/pypi/<name>/json`. Set `api: simple` on the pypi
source:

```yaml
http:
  allow_networks: [10.0.0.0/8]
  allow_http_hosts: [pypi.internal]                 # only if the index is plain http and has a credential
sources:
  pypiserver:
    kind: pypi
    base_url: http://pypi.internal:8080
    api: simple
    auth: {type: basic, username: angelia, password_ref: pypiserver-password}
```

```
angelia secret set pypiserver-password --from-file ./pw.txt --host pypi.internal:8080
```

```yaml
# watch file
watches:
  - {name: demo, source: pypiserver, coordinates: {name: demo}}
```

- `api` is `json` (the default: nothing changes) or `simple`. Any other value is an error, and so is the key
  on a source that is not pypi; `config check` reports both. There is no automatic fallback: with `json`, a
  404 on `/pypi/<name>/json` still means "package not found".
- With `simple` the releases come from `<base_url>/simple/<name>/`, with the name normalized as in PEP 503
  (lower case, runs of `-`, `_` and `.` become `-`). Angelia asks for PEP 691 JSON
  (`Accept: application/vnd.pypi.simple.v1+json`) and reads the PEP 503 HTML page when the server answers
  with HTML, as pypiserver does. A 404 is "package not found".
- The versions come from the file names (`name-1.0.0.tar.gz`, `.zip`, `name-1.0.0-py3-none-any.whl`) and, in
  JSON, from the `versions` key. As on the JSON path, a version is yanked when every one of its files is
  (`yanked` in JSON, `data-yanked` in HTML), and the reason is kept.
- The publish time is the PEP 700 `upload-time` (the earliest of the version's files) when the server
  sends it; otherwise it is left unset. pypiserver sends none, so releases listed from it have no publish
  time.
- Authentication, the custom CA bundle, the proxy and the SSRF guard are those of the JSON path.

### Verdaccio and devpi

```
angelia secret set verdaccio-token --from-file ./token.txt --host verdaccio.internal:4873
```

```yaml
http:
  allow_networks: [10.0.0.0/8]
  allow_http_hosts: [verdaccio.internal]            # required for a credential over plain http
sources:
  npm:
    base_url: http://verdaccio.internal:4873
    auth: {type: npm_auth_token, token_ref: verdaccio-token}
```

The secret's host must carry the port (`:4873`): a bare host means port 443.

For devpi, set `base_url` to the index, for example `https://devpi.example.com/root/pypi`, and use
`basic` if the index needs a login.

### GitHub Enterprise Server

```yaml
sources:
  github-release:
    base_url: https://ghes.example.com/api/v3
    auth: {token_ref: ghes-token}
```

### GitHub tags and GitLab (self-managed)

`github-tag` takes the same `base_url` and credential as `github-release`. `gitlab-release` takes the root
URL of the instance (a trailing `/api/v4` is accepted):

```yaml
sources:
  gitlab-release:
    base_url: https://gitlab.example.com
    token_env: GITLAB_TOKEN          # sent as "PRIVATE-TOKEN: <token>"
  # or a stored secret, sent as "Authorization: Bearer <token>" (basic is not supported by GitLab)
  #  auth: {token_ref: gitlab-token}
```

`crates` takes the sparse index URL of a mirror as `base_url` (default `https://index.crates.io`). It sends no
credential.

## Private networks

A registry on a private address needs its network in `http.allow_networks`
([Outgoing HTTP](configuration.md#outgoing-http-and-private-networks)).

A credential is sent over plain `http://` only to a host listed in `http.allow_http_hosts`, as for a webhook.
A source with `token_env` or `auth` and an `http://` `base_url` outside that list fails `config check`, and a
request to such a host (a redirect, say) is refused before any secret is read. A token from `token_env` is
also never sent to a redirect that changes the scheme or host. Prefer `https://` outside a trusted network.

## Custom CA bundle

For a registry whose certificate is signed by a private CA, point `http.ca_file` at a PEM file with the CA
certificate(s):

```yaml
http:
  ca_file: /etc/angelia/ca.pem      # or ANGELIA_HTTP_CA_FILE
```

The certificates are added to the system roots (they do not replace them) for every HTTPS request, sources and
sinks. `angelia config check` fails if the file is unreadable or holds no certificate. There is no option
to skip certificate verification.

## Proxy

`HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` are read once at start. Through a proxy, netguard checks the
URL's host and the proxy resolves the target, so rebinding protection is the proxy's job
([Outgoing HTTP](configuration.md#outgoing-http-and-private-networks)). A request that carries a credential
is refused when it would go through a proxy over plain `http://`: use an `https://` registry, which goes
through a `CONNECT` tunnel. `http.ca_file` also verifies an `https://` proxy.

## Checks at start

`angelia serve` and `angelia config check` look up each secret named by `sources.<kind>.auth` (name and
hosts only; nothing is decrypted) and fail when it does not exist or its hosts do not include the registry's
host and port. `config check` reads the database read-only and only warns when it does not exist yet or is
not sqlite. `auth` or `token_env` on a kind no built-in source reads (a typo such as `nmp`) is a warning.

## Testing against real servers

`go test` uses `httptest` fakes that mimic Verdaccio's bearer auth and a JSON-API mirror's basic auth.
Tests against the real servers need Docker and sit behind the `integration` build tag:

```
go test -tags integration -run Integration ./cmd/angelia
```

They start `verdaccio/verdaccio:5` (publish a package, read it with a token) and
`pypiserver/pypiserver:v2.3.2` (a package in its directory is listed through `api: simple` with the credential).
Without a Docker daemon they skip.
