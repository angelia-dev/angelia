# ADR 0034: The admin UI signs in with any OpenID Connect provider through `go-oidc`, with PKCE, state and nonce; access entries match a subject or a verified email pinned to its subject

- **Status:** Proposed (dependencies accepted by the maintainer, 2026-11-16)
- **Date:** 2026-11-16
- **Story:** angelia-dev/angelia#268 (UI-9)

## Context

[ADR 0033](0033-admin-web-ui.md) §4 put the admin UI's sign-in behind an `IdentityProvider` interface,
shipped GitHub (an OAuth App, standard library only) and left generic OIDC to #268, noting that
ID-token verification "needs JOSE code or a dependency, which #268 decides". Self-hosters run
Authentik, Keycloak or GitLab, or use Google accounts, and want to sign in with them.

The maintainer decided on 2026-11-16 to use `github.com/coreos/go-oidc/v3` (Apache-2.0), which brings
`github.com/go-jose/go-jose/v4` (Apache-2.0) and `golang.org/x/oauth2` (BSD-3-Clause, already in the
binary through the MCP SDK, [ADR 0029](0029-mcp-server-go-sdk-rest-client.md)). Writing JOSE in-house
(JWK parsing, RSA-PSS, ECDSA, EdDSA, key rotation) is security code with no product value.

## Decision

### 1. Configuration: `ui.oidc`

```yaml
ui:
  oidc:
    - name: authentik                 # slug: /ui/auth/login/authentik, ui.access, metrics, principal ids
      display_name: Company SSO       # the button label; default: name
      issuer: https://auth.example.com/application/o/angelia/
      client_id: angelia
      client_secret_file: /run/secrets/angelia-oidc   # or client_secret_env; never inline
      scopes: [openid, email, profile]                # the default; must contain openid
      allowed_algs: [RS256]                           # default: every asymmetric algorithm
  access:
    - provider: authentik
      subject: 5f1c0a7e-...           # the ID token's sub (preferred)
      role: admin
    - provider: authentik
      email: alice@example.com        # a verified email, pinned to its sub at the first sign-in
      role: editor
```

- `name` is `^[a-z][a-z0-9-]{0,31}$`, unique; `github` is reserved for `ui.github`. `ui.github` is
  now optional when `ui.oidc` lists a provider; an enabled UI needs at least one of them.
- `issuer` is `https://`, or `http://` on a loopback host (local development), without credentials,
  query or fragment; it is used as written, since discovery compares it with the document's `issuer`.
- The client secret follows the GitHub rules: `client_secret` is refused inline (even with the UI
  disabled), exactly one of `client_secret_env` and `client_secret_file`, the same file checks.
  Public clients (PKCE without a secret) are not supported.
- `allowed_algs` accepts only `RS256 RS384 RS512 PS256 PS384 PS512 ES256 ES384 ES512 EdDSA`. `none`
  and `HS*` are refused: an HMAC ID token would be signed with the client secret, which Angelia holds.
- Group or role claims are out of scope (#268).

### 2. Flow

- `GET /ui/auth/login/{name}`: discovery (`<issuer>/.well-known/openid-configuration`) on the first
  sign-in, through netguard's HTTP client (SSRF guard: a private issuer needs `http.allow_networks`),
  redirects not followed, retried at the first sign-in 10 s after a failure (sign-ins in between fail
  without a request; a sign-in waiting for another's discovery gives up with its own request); the api role starts without
  reaching the issuer. `authorization_endpoint`, `token_endpoint` and `jwks_uri` must be https (http on
  loopback only with a loopback issuer); a provider that lists `code_challenge_methods_supported`
  without `S256` is refused. The client authenticates with `client_secret_basic`, or
  `client_secret_post` when the provider lists only that.
- The browser goes to the provider with `state`, the PKCE S256 challenge and a `nonce`: three random
  256-bit values in the 10-minute `__Host-angelia_oauth` cookie (now `provider.time.state.verifier.nonce`).
- `GET /ui/auth/callback/{name}`: `state` compared in constant time (unchanged), the code exchanged
  with the verifier, then the ID token verified by go-oidc against the issuer's JWKS: signature with an
  allowed algorithm, `iss`, `aud` contains the client id, `exp`, `nbf`; then by Angelia: `nonce` equal
  to the cookie's (constant time), `azp` equal to the client id when present or when `aud` has several
  values (OIDC Core §3.1.3.7), `sub` printable ASCII of 1 to 255 characters. The access token and any
  refresh token are dropped; the userinfo endpoint is not called.

### 3. Identity and principal

- Principal id `<name>:<sub>` (such as `authentik:5f1c0a7e-...`): it fits the API's `PrincipalId` and
  `TokenActor` patterns (`[a-z][a-z0-9-]{0,31}:[!-~]{1,255}`), so the OpenAPI document is unchanged.
  The provider name alone is the prefix, not `oidc-<name>`: names are already distinct from `github`.
- `/v1/me` `user`: `provider` the name, `subject` the sub, `login` the `preferred_username`, else the
  `email`, else the sub; `name` the `name` claim; `avatar_url` the `picture` claim if https.
- `email` and `email_verified` (a boolean, or the string `"true"`) are read at sign-in only and not
  stored with the session. An `email` that is not a plain address (control characters, space, several
  `@`, over 254 characters) is dropped, never trimmed.

### 4. Access entries

- `{provider, subject, role}` matches the sub.
- `{provider, email, role}` matches a **verified** email, ASCII letters case-insensitively (no
  Unicode folding, under which `\u212A` would match `k`), and pins the address to
  the sub at its first match, in `ui_identity_pin` (`provider` = the name, `login` =
  `email:<address in lower case>`), as `github_user` pins a login. Another sub with that address later
  gets nothing from the entry (`identity_mismatch`). On every request, and when the email has changed
  at the provider, the entry grants through its pin: it follows the account it first matched.
- An entry has either GitHub keys or `provider`, never both; `provider` must name a `ui.oidc` entry.
- The start-up check for API tokens whose creator `ui.access` no longer lists now covers every
  `<provider>:<subject>` creator, including providers removed from the configuration.

### 5. Interface and metrics

- `IdentityProvider.AuthCodeURL(ctx, state, challenge, nonce) (string, error)` (discovery can fail)
  and `Exchange(ctx, code, verifier, nonce)`; GitHub ignores the nonce. This interface is internal to
  `internal/webauth`, not one of the architecture §3 contracts.
- `angelia_ui_logins_total{provider}` takes `github` and the configured names; anything else is
  `other`, so the label set stays closed.

## Consequences

### Positive

- Any certified OpenID provider works with configuration only; adding one is a config change.
- No credential from the provider is stored; the security of the session (hashed cookie, CSRF) is
  unchanged.
- No spec, schema or migration change.

### Negative

- Three modules in the binary (`go-oidc`, `go-jose`; `x/oauth2` was already there) and their CVEs to
  follow.
- A sign-in page that cannot reach the issuer shows an error; unauthenticated visits to
  `/ui/auth/login/{name}` can make Angelia call the configured issuer (discovery until it succeeds,
  then only the JWKS when a key id is unknown). Only configured hosts are reached.
- An email entry depends on the provider's `email_verified`. A provider that does not send it as
  true (some Authentik mappings) needs subject entries.
- `login` (and so the "Not authorised" log line) may be an email address.

### Open questions

- The login page needs the list of providers (name and label) to show one button each; no API
  exposes it yet (the UI stack is still merging). See the PR of #268.
- Group claims, public clients and `private_key_jwt` are not supported.

## Alternatives considered

- **In-house JOSE subset (RS256 and ES256 only).** Less code in the binary, but key rotation, JWK
  parsing and algorithm confusion are easy to get wrong; refused by the maintainer.
- **Principal `oidc-<name>:<sub>`.** Marks the kind of provider, but costs five characters of the
  32-character provider budget and repeats what the configuration says.
- **Calling the userinfo endpoint.** Needed only when claims are missing from the ID token; Google,
  GitLab, Keycloak and Authentik put them there with the `email` and `profile` scopes.

## References

- [ADR 0033](0033-admin-web-ui.md) §3–§5; [Admin UI sign-in](../configuration.md#admin-ui-sign-in).
- OpenID Connect Core 1.0 §3.1.3.7 (ID token validation), RFC 7636 (PKCE), RFC 9700 (OAuth security BCP).
