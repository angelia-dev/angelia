# fakereg: fake registries for benchmarks

One HTTP server that answers like five registries, so the scale benchmark
([bench/load](../load/README.md), story angelia-dev/angelia#62) never touches a real registry.
Packages are synthetic and their versions follow from the configuration and the time since the
server started, so a run is reproducible and the server knows when every release was published.

```
go run ./bench/fakereg/cmd/fakereg -config bench/fakereg/fakereg.yaml   # standalone, 127.0.0.1:8099
```

`bench/load` embeds the same server; it does not need the standalone binary.

## Endpoints

Each ecosystem lives under its own prefix, which is the source's base URL
(`sources.<kind>.base_url` in the [instance configuration](../../docs/configuration.md)).

| Ecosystem | Base URL | Request served | Source that reads it |
|---|---|---|---|
| `pypi` | `http://HOST/pypi` | `GET /pypi/pypi/{name}/json` (PyPI JSON API) | `internal/source/pypi` |
| `npm` | `http://HOST/npm` | `GET /npm/{name}` (packument) | `internal/source/npm` |
| `crates` | `http://HOST/crates` | `GET /crates/api/v1/crates/{name}/versions` | crates plugin (`bench/plugins/crates`); no built-in source yet |
| `github` | `http://HOST/github` | `GET /github/repos/{owner}/{name}/releases?per_page=&page=` | `internal/source/githubrelease` |
| `oci` | `http://HOST/oci` | `GET /oci/v2/{owner}/{name}/tags/list?n=&last=` (distribution spec, lexical order, `Link` for the next page), `GET /oci/v2/` | none yet |

Package names are `pkg-00000` to `pkg-<packages-1>`. GitHub and OCI repositories are
`{owner}/pkg-NNNNN`; any owner is accepted (`bench/load` uses `bench`). Unknown names get 404.
Version *k* (from 0) of every package is `1.k.0` (`v1.k.0` as a GitHub tag).
`GET /_fakereg/stats` returns the statistics below as JSON.

## Behaviour file

YAML, every key optional. `defaults` applies to every ecosystem; `ecosystems.<name>` overrides
keys for one ecosystem (nested maps such as `rate_limit` are merged key by key). Unknown keys and
unknown ecosystems are errors. [fakereg.yaml](fakereg.yaml) is the file `make bench-scale` uses.

| Key | Default | Meaning |
|---|---|---|
| `seed` (top level) | `1` | Changes every package's release phase. |
| `packages` | `2000` | Packages per ecosystem. `bench/load` overrides it to `ceil(N/5)`. |
| `history` | `20` | Versions each package has at start (published one day apart before the start). |
| `releases_per_hour` | `200` | New releases per hour over the whole ecosystem. Each package releases every `packages / releases_per_hour` hours with its own phase derived from `seed`. `0`: no releases. |
| `latency` | `30ms` | Added to every response (Go duration). |
| `latency_jitter` | `70ms` | Plus a uniform random part in `[0, latency_jitter)`. |
| `etag` | `strong` | `strong`: `"hash"` and 304 on a matching `If-None-Match`. `weak`: `W/"hash"`, weak comparison. `none`: no `ETag`, `If-None-Match` ignored. `ignore`: `ETag` sent, never a 304 (a broken CDN). The hash changes when a package gets a release. |
| `last_modified` | `true` | Send `Last-Modified` (publication of the newest version) and answer `If-Modified-Since` with 304 when nothing is newer. Ignored when the request has `If-None-Match`, and never honoured with `etag: ignore`. |
| `rate_limit.rps` | `0` (off) | Token bucket over the ecosystem: requests beyond it get 429. |
| `rate_limit.burst` | `0` | Bucket size; at least 1 when `rps` is set. |
| `rate_limit.retry_after` | `1s` | Sent as `Retry-After`, rounded up to whole seconds. |
| `random_429` | `0` | Probability (0 to 1) that a request gets 429 anyway. |
| `body_padding` | `0` | Extra bytes in every 200 body (at most 16 MiB), to model real document sizes. |

429s are decided before the latency, 404s after it, as on a real registry behind a gateway.

## Statistics and check lag

Per ecosystem and in total: requests, 200, 304, 429 and 404 counts; `published` (releases since
the start); `seen` (releases sent to a client at least once); and check lag.

The lag of a seen release is the time from its publication to the first 200 that contained it
(for GitHub, page 1; for OCI, the page holding the newest tag). An unseen release counts with
the time since its publication, a lower bound of its eventual lag, so missed releases make the
percentiles worse, never better. Percentiles use the nearest-rank method.

This is measured at the registry, so it is a lower bound of angelia's detection lag: it ends
when angelia *received* the release, not when it emitted the event. The gap is the processing
time of one response, small next to a two-minute target.
