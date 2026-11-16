# ADR 0033: The admin web UI is a React SPA in `web/`, embedded in the binary; users sign in with GitHub, and their role from `ui.access` is one of the three API scopes

- **Status:** Proposed (the maintainer accepted D1–D8 on 2026-11-16; this record is reviewed in its PR)
- **Date:** 2026-11-16
- **Story:** angelia-dev/angelia#99 (UI-0); implemented by #261, #262, #100, #263–#268

## Context

[prd.md](../prd.md) F14 asks for a web UI ("watches, events timeline, deliveries, DLQ", *Could*, v1.x),
and [architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions) planned
"SvelteKit or React, static build served by the binary. Separate repo." Epic #23 held two outline
stories: #99 (create the `ui` repository, choose the stack) and #100 (UI v1).

Since v0.2.0 everything the UI shows already has an API: watches with `WatchStatus`, versions, the
event log and its SSE stream, deliveries with replay, sinks, subscriptions, secrets, consumers,
plugins and tokens. Operators reach it today through the CLI, curl or Prometheus; on a PaaS such as
Coolify that means a shell in the container. The maintainer asked (2026-11-16) for an admin front to
configure Angelia and follow what is happening, with GitHub sign-in and admin/user roles from a list.

Constraints: one binary and one container ([ADR 0002](0002-one-binary-several-roles.md)); the binary
under 40 MB and the image under 50 MB (architecture §6); scopes are declared per operation in the
OpenAPI spec and checked before routing ([ADR 0030](0030-api-tokens.md) T5); rows from the watch file
(`managed_by: file`) are read-only through the API (`409 managed-by-file`).

## Decision

Maintainer decisions of 2026-11-16 (comment on the plan): D1 now, ahead of Phase 3; D2 a reader, an
admin, and writers who add their own watches; D3 the access list in a static file to start; D4 GitHub
users, with `mmornati` as the first admin; D5 GitHub first, adding providers must stay simple; D6–D8
left to the plan's recommendations.

### 1. The UI is a client of `/v1`, nothing else

The UI calls only the public REST API, with the same scopes, validation, access log and audit as the
CLI and MCP. There is no UI-only backend. The few things a browser needs that the API lacks are added
to the spec, additively: `GET /v1/me` (`read`), `GET /v1/sessions` and `DELETE /v1/sessions/{id}`
(`admin`). Sign-in routes (`/ui/auth/...`) are browser redirects, outside the spec.

### 2. Roles are the three scopes

| Role in `ui.access` | Scope | Can do (from the spec's `security` requirements) |
|---|---|---|
| `viewer` | `read` | Every `GET`: watches, events, deliveries, sinks (redacted), subscriptions, consumers, plugins |
| `editor` | `write` | Plus watches, subscriptions, replays, consumers: the "writers who add their own watch" of D2 |
| `admin` | `admin` | Plus sinks, secrets, `*_ref` keys, API tokens and UI sessions |

A session resolves to an `api.Principal` with that scope, so the route-to-scope table of the spec,
`TestAuthEveryRoute` and the audit log apply unchanged. There is no second permission system.
Per-resource ownership ("an editor edits only their own watches") is **not** in v1: editors share the
API-managed watches, as tokens with `write` do. It needs a contract change and its own ADR.

### 3. The access list lives in the instance config

```yaml
ui:
  enabled: true
  public_url: https://angelia.example.com
  github:
    client_id: Ov23li...                      # an OAuth App's client id
    client_secret_env: ANGELIA_UI_GITHUB_CLIENT_SECRET   # or client_secret_file
    # api_url / web_url: GitHub Enterprise Server, optional
  access:
    - github_user: mmornati
      role: admin
    - github_id: 139323                        # same person, by numeric id (preferred)
      role: admin
  session:
    idle_timeout: 12h
    max_lifetime: 168h
```

- The file is the source of truth (D3): versioned with the deployment, read at start, shown
  read-only in the UI. It also bootstraps the first admin, which tokens need a bootstrap file for.
- `github_user` matches the login case-insensitively and is **pinned** to the numeric id seen at the
  first sign-in (`ui_identity_pin`); a later sign-in with that login and another id is refused and
  logged at `WARN`. GitHub logins can be renamed and re-registered, ids cannot. `github_id` needs no
  pin. Teams and organisations (`read:org`) are left out until asked for (D4).
- `config check` refuses an enabled UI without `public_url`, a client id, a client secret reference,
  or at least one `admin` entry, and refuses an inline `client_secret`.
- The role is re-read from the loaded config on **every request**, so removing an entry takes effect at
  the next start (there is no hot reload today).

### 4. Sign-in: OAuth authorization code with PKCE, behind an `IdentityProvider` interface

```go
// internal/webauth
type Identity struct {
    Provider, Subject, Login, Name, AvatarURL string // Subject: the provider's stable id
}
type IdentityProvider interface {
    Name() string                                     // "github"
    AuthCodeURL(state, codeChallenge string) string
    Exchange(ctx context.Context, code, codeVerifier string) (Identity, error)
}
```

- GitHub is an **OAuth App** (not OIDC): `GET /login/oauth/authorize` with `state`, `code_challenge`
  (S256) and no scope; `POST /login/oauth/access_token`; `GET /user`. The access token is used for
  that one call and dropped: Angelia never stores a GitHub credential. Standard library only, through
  netguard's HTTP client; hosts `github.com` and `api.github.com` (public, already reached by the
  GitHub sources).
- `state` and the PKCE verifier travel in a short-lived (10 min) `__Host-angelia_oauth` cookie,
  `HttpOnly`, `SameSite=Lax`, compared in constant time on callback.
- A new provider (generic OIDC, #268) implements the same interface; `ui.access` entries name their
  provider. D5 is met by the interface, not by shipping OIDC now: ID-token verification needs JOSE
  code or a dependency, which #268 decides.

### 5. Sessions are hashed rows, like API tokens

- Migration `00022` on both engines, additive: `ui_session` (ULID id, SHA-256 hash, provider,
  subject, login, name, avatar URL, user agent, `created_at`, `last_seen_at`, `expires_at`,
  `revoked_at`, `revoked_by`) and `ui_identity_pin` (provider, login, subject).
- Cookie value `angs_<ulid>_<secret>`, 256 random bits, stored as
  `SHA-256("angelia ui session v1" || 0x00 || value)` (ADR 0030 T2's reasoning applies). Cookie
  `__Host-angelia_session`: `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`. When `public_url` is a
  loopback `http://` URL the cookie is `angelia_session` without `Secure`, for local development only.
- A session ends at `idle_timeout` without use, at `max_lifetime`, on sign-out, on revocation by an
  admin (`DELETE /v1/sessions/{id}`), or when its identity no longer matches `ui.access`. Last-seen
  writes are coalesced at most once per 5 minutes per session (ADR 0030 T6).
- `Principal.ID` is `github:<numeric id>`, `Principal.Name` the login: audit records survive a rename.

### 6. Request authentication and CSRF

- `Authorization: Bearer` wins when present. Without it, the server reads the session cookie and calls
  a `SessionAuthenticator`; the failure reasons and the `401` are the same as for tokens.
- Unsafe methods (`POST`, `PUT`, `PATCH`, `DELETE`) authenticated **by cookie** need both an `Origin`
  equal to `public_url`'s origin and the header `X-Angelia-CSRF: 1`, else `403` with slug `csrf`.
  Bearer requests are unaffected. `/mcp` never accepts the cookie.
- Event streams (`EventSource` sends cookies, not headers) re-check the session on the existing
  60-second schedule.

### 7. Frontend stack and location

- **React 19 + TypeScript (strict) + Vite**, single-page app; **TanStack Router** and **TanStack Query**;
  **Tailwind** with **shadcn/ui** components copied into the tree (Radix primitives); API types
  generated from `api/openapi/angelia.v1.yaml` with **openapi-typescript**, calls through
  **openapi-fetch**; tests with **Vitest** and **Playwright**. Runtime packages and licences (#261 pins
  the versions): React, React DOM, TanStack Router and Query, openapi-fetch, Radix Slot, clsx and
  tailwind-merge (MIT), lucide-react (ISC), class-variance-authority (Apache-2.0). TanStack Router pulls
  in `isbot` (Unlicense), which is outside the AGENTS.md licence list; the maintainer accepted it on 2026-11-16 (public-domain
  dedication, used only by TanStack's server rendering code, not shipped in the bundle).
- **`web/` in this repository** (D7), not a separate `ui` repository: an endpoint and its screen change in
  one PR, and the UI version is always the binary's.
- **Embedded**: Vite builds into `internal/webui/dist/app/` (git-ignored) and `internal/webui` embeds it
  with `go:embed`. A plain `CGO_ENABLED=0 go build` without Node embeds the committed placeholder page, so `go build` and `go test` never need Node. CI has a `web`
  job; the release's GoReleaser `before` hook builds `web/` first. Built assets are not committed.
- **Served by the `api` role** under `/ui/` (SPA fallback), hashed assets cached a year, `index.html`
  `no-cache`, CSP `default-src 'self'` (plus GitHub avatars), `frame-ancestors 'none'`. `ui.enabled`
  defaults to `false`.

### 8. File-managed rows stay read-only

Rows with `managed_by: file` show a badge and no edit controls (D8). The UI offers "Show as YAML" on
API-managed watches so that users can move them to GitOps.

### Story split

| Plan id | Issue | Story | Complexity | Model |
|---|---|---|---|---|
| UI-0 | #99 | This ADR | Medium | Opus |
| UI-1 | #261 | `web/` scaffold embedded in the binary | Medium | Sonnet |
| UI-2 | #262 | GitHub sign-in, sessions, roles, `/v1/me`, `/v1/sessions` | High | Opus |
| UI-3 | #100 | Read-only screens | Medium | Sonnet |
| UI-4 | #263 | Delivery replay and dead letters | Low | Sonnet |
| UI-5 | #264 | Create and edit watches | Medium | Sonnet |
| UI-6 | #265 | Sinks, subscriptions, secrets | Medium | Sonnet |
| UI-7 | #266 | Tokens, sessions and access pages | Medium | Sonnet |
| UI-8 | #267 | Docs: setup, GitHub OAuth App, Coolify | Low | Haiku |
| UI-9 | #268 | Generic OIDC provider (needs-spec) | High | Opus |

## Consequences

### Positive

- One authorisation model for tokens and people; every UI action is in the audit log with a stable id.
- No passwords and no GitHub credential stored; the first admin comes from the config file.
- One container, no Node at run time; a UI change and its API change ship together.
- The UI exercises the public contract; gaps it finds become spec changes, not private endpoints.

### Negative

- Node and an npm dependency tree enter CI and the release build (lockfile, `npm ci --ignore-scripts`,
  Renovate). The SBOM must include the frontend packages.
- A browser session is a new credential type: cookie handling, CSRF and session revocation must be
  right. #262 is a sensitive path.
- Changing who has access needs a config change and a restart.
- A public `public_url` exposes the sign-in page; the OAuth App and `ui.access` are the gate.

### Open questions

- Teams and organisations in `ui.access`, database-managed members, per-label permissions, and a
  queryable audit log: each later, each with its own decision.
- Generic OIDC: in-house JOSE subset or a dependency (#268).

## Alternatives considered

- **Go templates + htmx.** No JS toolchain, but the pages would read the store through server-side
  handlers: a second surface to authorise and test, against "the UI is a generated client" (§3.1).
- **SvelteKit (static adapter).** Smaller bundles; agents still mix Svelte 4 and 5 syntax, and the
  table and form ecosystem is smaller. Bundle size does not matter at this scale.
- **A separate UI container (Next.js).** A Node server to run, patch and secure; breaks one binary.
- **A separate `ui` repository** (architecture §9). Version skew between UI and API, two PRs per change.
- **Access list in the database, edited in the UI.** Needs its own bootstrap and lets a compromised UI
  admin add accomplices without a trace in version control. Can come later with the file as
  break-glass admins.
- **Roles beyond the scopes.** A second permission table to keep in sync with the spec; nothing in D2
  needs it.
- **Trusted reverse-proxy headers (oauth2-proxy, Authelia).** Popular in homelabs, but a header that a
  misconfigured proxy passes through is a full bypass. Not in v1.

## References

- [prd.md F14](../prd.md#6-functional-requirements), [architecture.md §3.1, §6, §9](../architecture.md)
- [ADR 0002](0002-one-binary-several-roles.md), [ADR 0024](0024-secrets-encrypted-at-rest.md),
  [ADR 0030](0030-api-tokens.md)
- GitHub: authorizing OAuth apps (web application flow, PKCE)
- OWASP cheat sheets: session management, CSRF prevention
