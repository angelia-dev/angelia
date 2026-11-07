# Private registries and mirrors

Angelia can watch packages on a private registry or a mirror: Artifactory, Nexus, Verdaccio, devpi, a
GitHub Enterprise Server, a self-managed GitLab. You set the registry's `base_url` and its credential once per source kind in the
[instance configuration](configuration.md). Design and security rules:
[ADR 0024, amendment of #111](adr/0024-secrets-encrypted-at-rest.md).

- One registry per source kind per instance. A watch is an ordinary watch of that kind
  (`source: npm`); it cannot choose its own base URL or credential
  ([watch-file.md](watch-file.md) decision D9).
- Events from a registry other than the public one carry its URL as the `repository_url` qualifier of
  the purl.

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

Angelia reads the PyPI JSON API (`/pypi/<name>/json`). Check that your repository serves it at that path;
a server that offers only the Simple API, or the JSON API elsewhere, cannot be watched yet (pypiserver is one).

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
`pypiserver/pypiserver:latest` (the credential reaches the server; pypiserver does not serve the JSON API at the path the pypi
source reads, so the source ends in a 404, not a 401). Without a Docker daemon they skip.
