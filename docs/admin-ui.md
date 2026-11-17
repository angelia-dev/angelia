# Admin UI

The admin UI is a web front for Angelia. You use it to see watches, events and deliveries, and to change
what Angelia watches and sends. It is part of the binary: the `api` role serves it under `/ui/`. People
sign in with GitHub. The design is in [ADR 0033](adr/0033-admin-web-ui.md); the keys are in
[configuration.md](configuration.md#admin-ui) and [Admin UI sign-in](configuration.md#admin-ui-sign-in).

The UI calls the same REST API as the CLI and tokens. Each action has the same scope check and the same
audit record. A browser session is a credential of its own: it stops at the sign-out, at its timeout, or
when an admin revokes it.

> **Status.** The sign-in and the read-only pages are in place. Replay, editing and the admin pages
> come in follow-up stories (#263-#266). Check the release notes for what your version has.

## What the UI shows

| Page | Shows |
|---|---|
| Overview | Angelia's state at a glance, with the exact number of events of the last 24 hours. |
| Watches | Each watch with its status and versions. Editors create, edit and delete API-managed watches. |
| Events | The event log, newest first, with live updates. |
| Deliveries | Each delivery with its sink, filtered by status, subscription, sink and time range, and the dead letters. Editors replay. |
| Sinks | Each sink. Secret values are never shown. Admins create and edit API-managed sinks and preview template-valued keys. |
| Subscriptions | Which sink gets which events. Editors create and edit API-managed ones; admins preview the template on a sample event. |
| Consumers | Each consumer. |
| Plugins | The installed plugins. |
| Admin | API tokens, sessions (the current one marked) and the `ui.access` list (admins only). |

Saving a watch, a sink or a subscription is conditional: an edit sends the `ETag` it read (`If-Match`) and a
create sends `If-None-Match: *`. If someone else saved first, or the name is taken, nothing is overwritten and
the form keeps your input. A template preview renders on the server's built-in sample event; its output is
shown as plain text, never as HTML.

Rows that come from the watch file (`managed_by: file`) show a badge and have no edit controls. Change
them in the watch file and restart, or apply the file with `angelia apply`.

## Design

The UI uses the same visual system as the documentation site, "The Release Wire": midnight chrome, canary
paper for anything that is a release, gold for the path and the one primary action per block, and the
gold Baudot tape as the rule. Status uses the wire's priority stamps: FLASH (a failure, a dead letter, a
major release), BULLETIN (a minor release, a delivery in flight), ROUTINE (a patch). Headings are Archivo,
condensed through its width axis; data, slugs and code are JetBrains Mono. Light and dark follow the
operating system. The tokens and rules are in [web/DESIGN.md](../web/DESIGN.md).

- **Overview: the sorting rack.** One pigeonhole per watch: its source, its latest release with the bump
  and the previous version, a meter to the next check, and a paper slip with the last release and, when
  one needs attention, its pending or dead delivery. Failing watches are outlined in Flash red. Past 24
  watches the rack opens in its compact form, grouped by source; the Rack / Compact switch is kept per
  browser.
- **Events: the punched tape.** The last 24 hours, 7 days or 30 days as a gold tape with one hole per
  release (solid: release, ring: prerelease, red: yank). Pick a hole with the mouse or the arrow keys
  (Home and End jump to the ends): the release opens below as a bulletin slip next to its CloudEvent JSON.
  The ledger under it lists the same events; its sequence number moves the tape to that event. A range
  with no release says so and names the newest one.

## Roles

Each entry of `ui.access` gives a role. A role is the same as an API scope.

| Role | API scope | What it can do |
|---|---|---|
| `viewer` | `read` | Read every page: watches, events, deliveries, sinks, subscriptions, consumers, plugins. |
| `editor` | `write` | Everything a viewer can, and manage watches, subscriptions, replays and consumers. |
| `admin` | `admin` | Everything an editor can, and manage sinks, secrets, `*_ref` keys, API tokens and UI sessions. |

If several entries match one person, the highest role applies. The config must have at least one `admin`
entry.

## Who can sign in: `ui.access`

```yaml
ui:
  access:
    - github_id: 139323      # preferred: the numeric account id
      role: admin
    - github_user: octocat   # a login, pinned to its account id at the first sign-in
      role: editor
```

Each entry names one of these, and not both:

- **`github_id`** (preferred). The numeric id of the GitHub account. It does not change when the login
  changes. To find it, open `https://api.github.com/users/<login>` in a browser and read the `id` field.
- **`github_user`**. A login, matched without case. At the first sign-in that matches, Angelia pins the
  login to that account's id. A later sign-in with the same login from another account gets no access, and
  Angelia logs a `WARN` line with both ids. To move a login to a new account, use a `github_id` entry.

Anyone else who signs in sees a "Not authorised" page. Their attempt is logged at `INFO`.

Angelia reads `ui.access` once, at start. After a restart with a changed file, the new list applies to open
sessions at their next request: a user whose entry was removed is refused. Until the restart, open
sessions keep the old list.

## Sessions

- A session ends after `ui.session.idle_timeout` without a request. The default is `12h`.
- A session ends at `ui.session.max_lifetime` after the sign-in, whatever its use. The default is `168h`
  (7 days). Idle timeout must not be longer than the max lifetime.
- **Sign-out** ends the current session: `POST /ui/auth/logout`.
- The **sign-in page** shows one button per configured provider (GitHub, then each `ui.oidc` provider). It
  reads the list from `GET /ui/auth/providers`, which needs no session and returns each provider's `name`
  and `label` only (`Cache-Control: no-store`), never a client id, an issuer or a secret.
- **Admins revoke** sessions through the API: `GET /v1/sessions` lists them, and
  `DELETE /v1/sessions/{id}` ends one. The Admin page does the same.

Each request reads the session from the database. A revoked session stops working on every replica at once.

## CSRF and cookies

The session is a cookie, `__Host-angelia_session`. It is `Secure`, `HttpOnly` and `SameSite=Lax`, and it
holds a random secret. The database keeps only a hash of it. A request that changes data with the cookie
must send an `Origin` header equal to `public_url`, and the header `X-Angelia-CSRF: 1`. Otherwise Angelia
answers `403` with the slug `csrf`. Requests with an `Authorization` header do not use the cookie. With a
loopback `http://` `public_url`, for local development, the cookie is named `angelia_session` and is not
`Secure`.

## Troubleshooting

- **GitHub reports a callback URL error.** The OAuth App's "Authorization callback URL" must equal
  `<public_url>/ui/auth/callback/github` exactly: same scheme, host, port, and no trailing slash. GitHub
  stops the sign-in before it reaches Angelia.
- **"Sign-in failed: The sign-in expired or was started in another browser."** The sign-in took longer than
  10 minutes, or its cookie was not kept. Start again. Check that the browser accepts cookies for the host.
- **"Sign-in failed: The identity provider could not confirm who you are."** GitHub returned an error. Check
  the client id and the client secret, and the `WARN` line in the log.
- **"Not authorised".** The account is not in `ui.access`, its login is pinned to another account (see
  `WARN` in the log), or the file was changed and Angelia was not restarted.
- **The sign-in does not stick, or actions fail with `403 csrf`.** `public_url` must be the URL the browser
  opens, with `https://`. If `public_url` says `https://` but the browser uses `http://` on a host other than
  localhost, the browser does not keep the `Secure` cookie. Angelia compares the `Origin` header with the
  origin of `public_url`, so a different host or port fails too.
- **`config check` refuses the UI section.** The message names the key. A client secret written inline is
  refused and never printed. Use `client_secret_env` or `client_secret_file`.

## Create a GitHub OAuth App

Do this once, for each Angelia instance. You need a GitHub account that can create OAuth Apps in your
user account or organisation.

1. On GitHub, open **Settings**, then **Developer settings**, then **OAuth Apps**, and choose **New OAuth App**.
2. **Application name**: any name, such as `Angelia (example.com)`.
3. **Homepage URL**: your `public_url`, such as `https://angelia.example.com`.
4. **Application description**: optional.
5. **Authorization callback URL**: `<public_url>/ui/auth/callback/github`, such as
   `https://angelia.example.com/ui/auth/callback/github`.
6. Choose **Register application**. Copy the **Client ID** into `ui.github.client_id`.
7. Choose **Generate a new client secret**, and copy it now. GitHub shows it once.
8. Put the secret in one place only:
   - an environment variable of the Angelia process, named in `ui.github.client_secret_env`, such as
     `ANGELIA_UI_GITHUB_CLIENT_SECRET`; or
   - a file, named in `ui.github.client_secret_file`. The file holds the secret on one line. Use mode
     `0600`. Angelia refuses the file if group or others can write it, and warns if they can read it.

Angelia asks GitHub for no scope. It reads the public profile with the access token and drops the token.

## Deploy notes

The `api` role must be reachable by your users over `https://`. Angelia itself serves plain HTTP. Put a
reverse proxy or a platform proxy in front of it, and let that proxy hold the certificate.

### Docker Compose

Add the UI to the SQLite stack in [deploy/compose/sqlite](../deploy/compose/sqlite/). Set the variables in
`.env` next to `compose.yaml`:

```sh
ANGELIA_UI_GITHUB_CLIENT_SECRET=paste-the-client-secret
```

```yaml
services:
  angelia:
    # ... as in deploy/compose/sqlite/compose.yaml ...
    environment:
      ANGELIA_UI_GITHUB_CLIENT_SECRET: ${ANGELIA_UI_GITHUB_CLIENT_SECRET:?set it in .env}
    ports:
      - "127.0.0.1:8080:8080"   # loopback: the proxy on this host reaches it, the internet does not
```

The `ui` keys are in `angelia.yaml`, which is already mounted. Give it `public_url` and the `ui` section of
[examples/admin-ui.yaml](../examples/admin-ui.yaml). The secret is in the container's environment, so
`docker inspect` shows it to anyone who can run it. The compose files pin `0.2.0`, which has no admin UI and rejects the `ui:` key. The UI needs the first
release that includes it, which is not released yet. See
[deploy/compose/README.md](../deploy/compose/README.md) for the tag.

### Coolify

These steps describe the general flow. Coolify's labels can change between versions.

1. **Domain.** Set the service's domain to `angelia.example.com`. Coolify's proxy obtains the certificate
   and terminates TLS. Angelia listens on plain HTTP, on port `8080` by default. Set the port that Coolify
   forwards to `8080`.
2. **`public_url`.** Set it to `https://angelia.example.com`, the URL users open. It is used for the cookies,
   the `Origin` check and the callback URL, so it must not be the internal address.
3. **Environment variables.** Add `ANGELIA_UI_GITHUB_CLIENT_SECRET` with the client secret, and mark it as
   secret if Coolify offers that option.
4. **Config file.** The `ui` keys have no environment variables, and `access` is a list. Mount your
   `angelia.yaml` (the file from [examples/admin-ui.yaml](../examples/admin-ui.yaml), extended with the
   instance keys) at `/etc/angelia/angelia.yaml`, and start the container with
   `serve --config /etc/angelia/angelia.yaml`.
5. **Persistent storage.** Add a volume for `/var/lib/angelia`. The SQLite database is there, with the
   sessions and the login pins. Without the volume, a redeploy loses them. The image runs as uid `65532`,
   and a new volume may be owned by root, so Angelia could not write the database. Make the volume writable
   by uid `65532`. The compose files do it with a one-shot `volume-init` job (a pinned `busybox` image that
   runs `chown -R 65532:65532 /var/lib/angelia` on the volume, and that `angelia` waits for). In Coolify,
   run the same command once against the volume, for example with a one-off container that mounts it.
6. **Forwarded headers.** At this version Angelia does not read `X-Forwarded-Proto` or any other forwarded
   header. The `Secure` flag of the cookie comes from the scheme of `public_url`, not from the request. So the
   scheme of `public_url` must be `https`, and the proxy's scheme does not change anything.

## Related

- [Install and first run](install.md#next-steps)
- [Instance configuration: Admin UI](configuration.md#admin-ui)
- [ADR 0033: the admin web UI](adr/0033-admin-web-ui.md)
