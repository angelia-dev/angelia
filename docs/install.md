# Install and first run

This guide takes you from nothing to a first delivered event: install the binary (or the container), run it with
SQLite, and receive one signed webhook. It is a guide, not a reference; the links lead to the references.

## What works today

Angelia is an installable **pre-release**. Push delivery is usable: you describe sources, sinks and subscriptions in a
[watch file](watch-file.md), and Angelia detects new versions and delivers signed events to your webhooks, GitHub
Actions dispatches or chat services.

Everything that goes through the REST API needs a bearer token (`401 unauthorized` without one). The first admin token
is made on the instance, in [section 4](#4-create-your-first-api-token). These need a token:

- the REST API under `/v1/`, including SSE event streams;
- the CLI client commands: `watch`, `sink`, `sub`, `event`, `delivery`, `replay`, `secret`, `apply`, `token create`,
  `token list` and `token revoke`;
- `angelia mcp` and the `/mcp` endpoint.

These work without a token: `/healthz`, `/readyz` and `/metrics` on the listen address, `angelia serve`,
`angelia config check`, `angelia healthcheck`, `angelia webhook secret new`, `angelia token generate`,
`angelia token create --local`, `angelia version`, and push delivery configured from the watch file. To change what
is watched, edit the watch file and restart the process, or use the API with a token.

**Coming in 0.2.0 (on `main`, not in v0.1.2; the version is planned, not confirmed):**

- the pull API (consumers), which needs a token;
- `angelia mcp`, the MCP server (a client of the API, so it needs a token too);
- API tokens and the `angelia token` commands;
- the Postgres store (`postgres://` DSN) and the `full/` compose stack.

## 1. Install the binary

Each [GitHub release](https://github.com/angelia-dev/angelia/releases) has one archive per platform
(`angelia_<version>_<os>_<arch>.tar.gz`, `.zip` on Windows; linux, darwin and windows on amd64 and arm64), a
`checksums.txt`, its cosign bundle `checksums.txt.sigstore.json`, and SBOMs. The binary is static: nothing else to install.

```sh
TAG=v0.1.2
VERSION=${TAG#v}
BASE=https://github.com/angelia-dev/angelia/releases/download/$TAG
curl -fsSLO "$BASE/angelia_${VERSION}_linux_amd64.tar.gz"
curl -fsSLO "$BASE/checksums.txt"
curl -fsSLO "$BASE/checksums.txt.sigstore.json"
```

Verify before you run it. The checksums file is signed with cosign (keyless), and the archive has a GitHub build
attestation:

```sh
IDENTITY="https://github.com/angelia-dev/angelia/.github/workflows/release.yml@refs/tags/${TAG}"
ISSUER="https://token.actions.githubusercontent.com"
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" checksums.txt
sha256sum --ignore-missing -c checksums.txt        # macOS: shasum -a 256 -c --ignore-missing checksums.txt
gh attestation verify "angelia_${VERSION}_linux_amd64.tar.gz" --repo angelia-dev/angelia
```

All three must succeed. What each one proves, the image checks and the SBOMs are in
[operations/release.md](operations/release.md#verify-a-release).

```sh
tar -xzf "angelia_${VERSION}_linux_amd64.tar.gz" angelia
sudo install -m 0755 angelia /usr/local/bin/angelia
angelia version
```

## 2. Run it with SQLite

You need two files: the instance configuration ([configuration.md](configuration.md)) and the watch file
([watch-file.md](watch-file.md)). The smallest useful pair:

<!-- install-check: config -->
```yaml
# /etc/angelia/angelia.yaml
instance:
  name: homelab
listen: "127.0.0.1:8080"
database:
  dsn: sqlite:///var/lib/angelia/angelia.db
```

<!-- install-check: watches -->
```yaml
# /etc/angelia/watches.yaml
version: 1
watches:
  - name: pypi-requests
    source: pypi
    coordinates:
      name: requests
    interval: 1h
```

Check them, then start:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin angelia
sudo mkdir -p /etc/angelia /var/lib/angelia     # put the two files in /etc/angelia
sudo chown angelia: /var/lib/angelia            # the database lives here
sudo -u angelia angelia config check --config /etc/angelia/angelia.yaml --watches /etc/angelia/watches.yaml
sudo -u angelia angelia serve --config /etc/angelia/angelia.yaml --watches /etc/angelia/watches.yaml
```

`serve` creates the database, applies the watch file and records the versions that exist today as the baseline, without
events: you only hear about releases made after that. Then, from another terminal:

```sh
curl -i http://127.0.0.1:8080/healthz        # 200
curl -i http://127.0.0.1:8080/v1/watches     # 401: the API needs a token (next step)
```

`angelia serve` reads the watch file once, at startup. Edit it and restart the process to apply a change. The
default database is `sqlite:///var/lib/angelia/angelia.db` and the default listen address is `:8080`.

### As a systemd service

Use the `angelia` user created above and install this unit as `/etc/systemd/system/angelia.service`:

<!-- install-check: unit -->
```ini
[Unit]
Description=Angelia release event bus
After=network-online.target
Wants=network-online.target

[Service]
User=angelia
Group=angelia
# Creates /var/lib/angelia owned by the service user.
StateDirectory=angelia
# Holds the signing secrets, for example CI_WEBHOOK_SECRET=whsec_... (mode 0600, owner root).
EnvironmentFile=-/etc/angelia/angelia.env
ExecStart=/usr/local/bin/angelia serve --config /etc/angelia/angelia.yaml --watches /etc/angelia/watches.yaml
Restart=on-failure
# Longer than the 25 s drain timeout of `angelia serve`.
TimeoutStopSec=30
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
LockPersonality=true
RestrictNamespaces=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
CapabilityBoundingSet=
# MemoryDenyWriteExecute is left off on purpose: WASM plugins may need JIT.

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now angelia
journalctl -u angelia -f
```

The logs are JSON lines by default; set `log.format: text` for reading by eye. `angelia serve` stops cleanly on
SIGTERM.

Commands of [exec sinks](exec.md) run inside this sandbox too, as the `angelia` user: the file system is
read-only except `/var/lib/angelia` (add `ReadWritePaths=` for a directory a hook writes to), and the address
families and capabilities above apply to them. In the container image (section 3) an exec sink needs a derived
image; a webhook to a sidecar is usually simpler.

## 3. Or run the container

The image is `ghcr.io/angelia-dev/angelia:<version>` (linux/amd64 and arm64, distroless, no shell, uid 65532). The
Docker Compose examples in [deploy/compose](../deploy/compose/README.md) run it for you: one container with SQLite on a
volume (`sqlite/`), and a stack with Postgres (`full/`).

```sh
cd deploy/compose/sqlite
# The first admin token (section 4), made once with the image itself. The container user is uid 65532,
# not you, so it must be able to read the file: a mode of 0644 is accepted with a warning in the log.
docker run --rm ghcr.io/angelia-dev/angelia:0.2.0 token generate > bootstrap-token && chmod 0644 bootstrap-token
docker compose up -d
docker compose ps                 # "healthy" after the first probe
```

`bootstrap-token` holds an admin token (git ignores it): read it with `cat bootstrap-token` and use it as in
section 4. Once you have made a named admin token, revoke `bootstrap` (`angelia token revoke bootstrap`).

Run from a clone of the repository. To pass a signing secret (section 5) into the container, add it under the
`angelia` service's `environment:` in `compose.yaml` (for example `MY_WEBHOOK_SECRET: ${MY_WEBHOOK_SECRET}`) and
export it before `docker compose up`.

Notes:

- The GitHub Container Registry package is still private. Pulling it works without a login **once the package is
  public**; until then, `docker login ghcr.io` with a token that can read the package, or use the binary above.
- The compose files pin `0.2.0`, the first image with Postgres support. v0.2.0 is planned, not released yet. For the
  SQLite example alone, you may set the tag to `0.1.2`, which exists.
- To verify the image, see [operations/release.md](operations/release.md#verify-a-release); for production, deploy by
  digest after verifying it.
- The port is published on `127.0.0.1` only. Put a reverse proxy in front before you expose it.

## 4. Create your first API token

The REST API, the CLI client and MCP take a bearer token (`ang_…`) with one scope: `read`, `write` or `admin`. The
instance stores only a hash, so a token is shown once, when it is made. The first one is an `admin` token made on the
instance; with it you make the others through the API. Two ways, one is enough:

- **A command on the instance** (it writes the token to the database and needs no running server):

  ```sh
  angelia token create --local --name me --config /etc/angelia/angelia.yaml     # prints the token alone on stdout
  # container: docker compose exec angelia angelia token create --local --name me
  ```

- **A bootstrap file**, for unattended installs, as the compose examples do. Make it once, before the first start, and
  name it in `api.bootstrap_token_file`: `angelia token generate --out bootstrap-token`. The first start with an empty
  token table stores it as the admin token `bootstrap`; the token is the file's content. See
  [configuration.md](configuration.md#api-tokens) before you rely on it: it is an admin credential until revoked.

Then use the token (here `ang_…` stands for what the command printed; keep it out of shell history and logs):

```sh
export ANGELIA_TOKEN=ang_...                                      # or a client config file, see cli.md
curl -s -H "Authorization: Bearer $ANGELIA_TOKEN" http://127.0.0.1:8080/v1/watches   # 200 with the watches
angelia watch list --server http://127.0.0.1:8080                 # the CLI reads ANGELIA_TOKEN
angelia token create --name ci --scope write --expires-in 90d     # more tokens, through the API
angelia token list
angelia token revoke bootstrap                                    # once you have a named admin token
```

For [MCP](mcp.md), run `angelia mcp --server http://127.0.0.1:8080` (it reads `ANGELIA_TOKEN` too), or point an HTTP
client at `/mcp` with the same bearer header. Give agents a `read` or `write` token, not `admin`.

## 5. Your first delivered event

A webhook sink, one watch and one subscription. The signing secret never goes into a file: you generate it, keep it in an
environment variable of the Angelia process, and give the same value to the receiver.

**1. Make a secret.** It is printed once and stored nowhere:

```sh
angelia webhook secret new
# whsec_3q2+7w...=
```

**2. Add a sink and a subscription to the watch file.**

<!-- install-check: first-event-watches -->
```yaml
# /etc/angelia/watches.yaml
version: 1

sinks:
  - name: my-webhook
    kind: webhook
    config:
      url: http://127.0.0.1:9000/hook
      secret_env: MY_WEBHOOK_SECRET   # the name of the variable, never the value

watches:
  - name: pypi-requests
    source: pypi
    coordinates:
      name: requests
    interval: 1h

subscriptions:
  - name: requests-to-my-webhook
    sink: my-webhook
```

A subscription without a `filter` receives every event of every watch. Filters, templates and policies:
[watch-file.md](watch-file.md).

**3. Allow the local receiver (test only).** Angelia refuses to send webhooks to loopback and private addresses (its SSRF guard), and
to plain `http://`. Real receivers use `https://` on a public name and need nothing more. For this local test, allow
the one address in the instance configuration, which the watch file cannot do for you
([why](configuration.md#outgoing-http-and-private-networks)):

<!-- install-check: first-event-config -->
```yaml
# /etc/angelia/angelia.yaml
instance:
  name: homelab
listen: "127.0.0.1:8080"
database:
  dsn: sqlite:///var/lib/angelia/angelia.db
http:
  allow_networks: ["127.0.0.1/32"]
  allow_http_hosts: ["127.0.0.1"]
```

**Remove the whole `http:` block once the sink points at a real `https://` receiver.** `allow_networks: 127.0.0.1/32`
lets every sink and every source reach every service on loopback, including Angelia's own API, not only your receiver.

**4. Start a receiver that checks the signature.** Save this as `receiver.py` (Python 3.9 or newer, standard library only; it accepts one or several space-separated secrets, as during a rotation). It
verifies the [Standard Webhooks](https://www.standardwebhooks.com/) signature as
[webhooks.md](webhooks.md#for-receivers) describes, and prints the event. Use a Standard Webhooks library in a real receiver.

```python
import base64, hashlib, hmac, json, os, time
from http.server import BaseHTTPRequestHandler, HTTPServer

SECRETS = [base64.b64decode(v.removeprefix("whsec_")) for v in os.environ["MY_WEBHOOK_SECRET"].split()]

class Hook(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        msg_id = self.headers.get("webhook-id", "")
        ts = self.headers.get("webhook-timestamp", "")
        signed = f"{msg_id}.{ts}.".encode() + body
        wants = [base64.b64encode(hmac.new(k, signed, hashlib.sha256).digest()).decode() for k in SECRETS]
        given = [s.removeprefix("v1,") for s in self.headers.get("webhook-signature", "").split() if s.startswith("v1,")]
        fresh = ts.isdigit() and abs(time.time() - int(ts)) <= 300
        ok = fresh and any(hmac.compare_digest(w, g) for w in wants for g in given)
        print("signature", "OK" if ok else "INVALID", msg_id, flush=True)
        if ok:
            print(json.dumps(json.loads(body), indent=2), flush=True)
        self.send_response(204 if ok else 503)  # 503: Angelia retries; a 401 would be final
        self.end_headers()

HTTPServer(("127.0.0.1", 9000), Hook).serve_forever()
```

```sh
export MY_WEBHOOK_SECRET=whsec_...        # the value from step 1
python3 receiver.py
```

**5. Start Angelia with the same secret** (in another terminal):

```sh
export MY_WEBHOOK_SECRET=whsec_...        # the same value
angelia config check --config /etc/angelia/angelia.yaml --watches /etc/angelia/watches.yaml
angelia serve --config /etc/angelia/angelia.yaml --watches /etc/angelia/watches.yaml
```

With systemd, put `MY_WEBHOOK_SECRET=whsec_...` in `/etc/angelia/angelia.env` and restart the service.

**6. Wait for a release.** The first check only records the versions that exist (the baseline), so the first event is
the next new version of the package. A busy package releases often; a quiet one can take weeks. To see the whole path
now, without waiting, run the repository's own end-to-end check, which uses a fake registry:
`deploy/compose/ci/run.sh` (needs Docker and a checkout; see [deploy/compose/README.md](../deploy/compose/README.md#quickstart-check-ci)).

When a release is detected, the receiver prints `signature OK` and the `release.published` CloudEvent. A delivery
that fails is retried with backoff; the reasons are in the Angelia log and in [webhooks.md](webhooks.md#responses-and-retries).

## Next steps

- [Watch file reference](watch-file.md), [policies](policy.md) (prerelease, cooldown, constraints), [templates](templates.md)
  for chat messages.
- Other sinks: [GitHub dispatch](github-dispatch.md), [Shoutrrr](shoutrrr.md) (chat).
- [Instance configuration](configuration.md), [metrics](operations/metrics.md), [private registries](private-registries.md).
- [Admin UI](admin-ui.md): the web UI, its GitHub sign-in and roles, with Docker Compose and Coolify notes.
- [Command-line reference](cli.md) and [MCP](mcp.md): both use the API token of section 4.
