# Docker Compose examples

Two stacks, both on the published image `ghcr.io/angelia-dev/angelia` (built by the release
pipeline, see [docs/operations/release.md](../../docs/operations/release.md)):

| Directory | What it runs | Status |
|---|---|---|
| [`sqlite/`](sqlite/compose.yaml) | One Angelia container, SQLite on a named volume (the "homelab" mode of [architecture.md §7](../../docs/architecture.md#7-deployment)) | Ready, once the image is published |
| [`full/`](full/compose.yaml) | Angelia with Postgres, plus an apprise-api sidecar | **Placeholder**: see below |

## Before you start

- **The image is not published yet.** The first release is a human decision (see the release
  doc). Until `ghcr.io/angelia-dev/angelia:0.1.0` exists, `docker compose up` fails with a pull
  error. The compose files pin a version tag. Add the digest of the released image once it is
  published and verified (`cosign verify`, see the release doc).
- **Pins are refreshed by hand** until Dependabot covers compose files. Third-party images
  (postgres, busybox, apprise) carry a tag and a `@sha256:` digest. The digest moves with the
  tag, so re-pin the digest when you bump a tag, and check that it matches the registry.
- Compose v2 (`docker compose`).

## SQLite (homelab)

```sh
cd deploy/compose/sqlite
docker compose up -d
docker compose ps          # "healthy" after the first probe
docker compose logs -f angelia
```

- `angelia.yaml` is the instance configuration and `watches.yaml` the watch file (format:
  [docs/watch-file.md](../../docs/watch-file.md)). Both are mounted read-only. The container
  applies the watch file at each start; edit it and run `docker compose restart angelia`.
- The database is in the named volume `angelia-data`. The `volume-init` job gives that volume
  to the image user (uid 65532) before Angelia starts, because a new volume is owned by root.
  Remove the volume with `docker compose down -v` only if you want to lose the data.
- The port is published on `127.0.0.1:8080` only. The REST API needs a bearer token, and token
  storage is not built yet, so every API request is refused for now (see
  [docs/configuration.md](../../docs/configuration.md#rest-api)).
- The healthcheck runs `angelia healthcheck`. The image has no shell or curl, so the binary is
  its own probe. Exit 0 means a 2xx answer; a redirect, a timeout (3s) or a refused connection
  is exit 1.
- **The probe URL.** Without `--url`, it is built from `ANGELIA_LISTEN`: the port is taken from
  it, and an empty or `0.0.0.0` / `::` host becomes `127.0.0.1`. Without `ANGELIA_LISTEN`, it is
  `http://127.0.0.1:8080/healthz`. If `listen` is set only in the config file, the probe does not
  see it: give the URL explicitly with `--url`, for example `angelia healthcheck --url
  http://127.0.0.1:9000/healthz`, and override the healthcheck in the compose file with the same
  command. A `--url` with credentials is refused.

Check the two files without starting anything:

```sh
angelia config check --config angelia.yaml --watches watches.yaml
```

### Not covered here

- **Delivery to a webhook receiver.** The example has no sink, so events are stored but not
  delivered. Add one with a [webhook sink](../../docs/webhooks.md); the CI check below does,
  in its own copy of the example.

## Full stack (placeholder)

```sh
cd deploy/compose/full
printf 'POSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 24)" > .env
docker compose up -d       # starts postgres only
```

- **postgres** runs, but Angelia cannot use it yet. The store has a SQLite engine only, and
  `angelia serve` refuses a `postgres://` DSN. Tracked in #85.
- **angelia** is in the `placeholder` profile, so the default `up` does not start it. The
  service shows the intended wiring (DSN, healthcheck, depends on postgres). Start it with
  `docker compose --profile placeholder up` only to see the wiring; it exits until #85 lands.
- **apprise** (`caronc/apprise`, the apprise-api image) is in the `not-wired` profile. Nothing
  calls it: the chat sink that would use it does not exist yet (#120).
- The password is interpolated into `ANGELIA_DATABASE_DSN` from `.env`, unescaped. Use hex only
  (the `openssl rand -hex` above), or URL-encode it: characters such as `@ : / #` break the DSN.
  The password is visible in `docker inspect` of the angelia container. That is acceptable for
  a placeholder; secret references for DSNs are not decided (#75). Do not commit `.env`.

## Checks

The Go tests check the files without starting anything. `go test ./deploy/compose/` checks the
structure of both files: services, volumes, mounts, healthchecks, profiles, digest pins, and the
Dockerfile HEALTHCHECK. When the `docker` CLI and its compose plugin are available, it also runs
`docker compose config --quiet` on each file with all profiles. `go test ./cmd/angelia/` loads
the example configs with the real loaders. Neither needs a daemon or the network.

## Quickstart check (CI)

The SQLite example is verified end to end by the `quickstart` job of the CI workflow (part of
**CI OK**; it runs when the code that goes into the image, `deploy/compose/` or the workflow
changes). The job runs [`ci/run.sh`](ci/run.sh), which:

1. builds the image from the checkout and a small test tool, `ciharness` ([`ci/`](ci/main.go),
   standard library plus `bench/fakereg` and the webhook verifier);
2. starts `sqlite/compose.yaml` with the overlay [`ci/compose.override.yaml`](ci/compose.override.yaml)
   (`docker compose up --wait`). The overlay adds a fake PyPI (`bench/fakereg`, one package,
   two versions) and a webhook receiver that verifies the Standard Webhooks signature. The
   overlay's [`angelia.yaml`](ci/angelia.yaml) and [`watches.yaml`](ci/watches.yaml) point the
   `pypi` source at the fake registry and add one webhook sink, subscription and watch;
3. waits for the first check (it records the versions known at start and sends nothing);
4. has the fake registry publish one more version (`POST /_ci/publish`);
5. polls the receiver until it holds a `release.published` event for that version, or 9 minutes
   have passed. The shortest watch interval is 5 minutes (plus up to 10% jitter), so a passing
   run takes about 6 minutes after the stack is up;
6. on failure prints `compose ps` and the logs of all services; always runs `docker compose down -v`.

To run the same check locally you need Docker with Compose v2, Go, `curl` and `jq`:

```sh
deploy/compose/ci/run.sh                    # builds angelia:ci and ciharness, then runs the check
ANGELIA_IMAGE=my/image:tag deploy/compose/ci/run.sh   # test an image you already built
AWAIT_TIMEOUT=12m deploy/compose/ci/run.sh  # longer wait on a slow machine
```

It publishes `127.0.0.1:8080`, `:8099` and `:9000`, and uses the network `172.28.77.0/24`; free
those first. The test uses the instance configuration keys `http.allow_networks` and
`http.allow_http_hosts` ([Outgoing HTTP](../../docs/configuration.md#outgoing-http-and-private-networks))
to let Angelia reach the two containers. They are set to that one /24 and to the host name
`receiver`, in the overlay's `angelia.yaml` only. The shipped examples set neither, and no code
or default of the SSRF guard changed. The signing secret is generated for each run and is
never written to disk.

`go test ./deploy/compose/ci/` tests the tool and the overlay without Docker.
