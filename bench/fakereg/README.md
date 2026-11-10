# fakereg: fake registries for benchmarks

One HTTP server that answers like five registries, so the scale benchmark ([bench/load](../load/README.md), story
angelia-dev/angelia#62) never touches a real registry.
Packages are synthetic and their versions follow from the configuration and the time since the
server started, so a run is reproducible and the server knows when every release was published.

```
go run ./bench/fakereg/cmd/fakereg -config bench/fakereg/fakereg.yaml   # standalone, 127.0.0.1:8099
```

## Endpoints

Each ecosystem lives under its own prefix, which is the source's base URL
(`sources.<kind>.base_url` in the [instance configuration](../../docs/configuration.md)).

| Ecosystem | Base URL | Request served | Source that reads it |
|---|---|---|---|
| `pypi` | `http://HOST/pypi` | `GET /pypi/pypi/{name}/json` (PyPI JSON API) | `internal/source/pypi` |
| `npm` | `http://HOST/npm` | `GET /npm/{name}` (packument) | `internal/source/npm` |
| `crates` | `http://HOST/crates` | `GET /crates/{nn}/{nn}/{name}` (sparse index file, one JSON line per version); `GET /crates/api/v1/crates/{name}/versions` (crates.io API) | `internal/source/crates` (index; with a custom index it reads no API, so versions carry no publication time), crates plugin (`bench/plugins/crates`, API) |
| `github` | `http://HOST/github` | `GET /github/repos/{owner}/{name}/releases?per_page=&page=` | `internal/source/githubrelease` |
| `oci` | `http://HOST/oci` | `GET /oci/v2/{owner}/{name}/tags/list?n=&last=` (distribution spec, lexical order, `Link` for the next page), `GET /oci/v2/` | none yet |

Package names are `pkg-00000` to `pkg-<packages-1>`. GitHub and OCI repositories are
`{owner}/pkg-NNNNN`; any owner is accepted. Unknown names get 404.
Version *k* (from 0) of every package is `1.k.0` (`v1.k.0` as a GitHub tag).
`GET /_fakereg/stats` returns the statistics below as JSON. In Go, `Server.Versions(eco, i)` gives a
package's version count by the server's clock (with `WithClock` a harness can freeze it) and
`Server.FirstServed(eco, i)` the count in the first 200 that listed it (a client's baseline);
benchmark 5 ([crash](../crash/README.md)) reconciles angelia's database with them. With `primary_limit` set,
`GET /github/rate_limit` answers like GitHub's `GET /rate_limit` (`resources.core` and `rate`: limit,
remaining, used, reset), without counting as a request; without it, 404.

## Behaviour file

YAML, every key optional. `defaults` applies to every ecosystem; `ecosystems.<name>` overrides
keys for one ecosystem (nested maps such as `rate_limit` are merged key by key). Unknown keys and
unknown ecosystems are errors. [fakereg.yaml](fakereg.yaml) is the file `make bench-scale` uses; [github.yaml](github.yaml) the one of the
benchmark 3 dry run (`BENCH_GITHUB_FAKE=1 make bench-github`), with GitHub's primary limit on.

| Key | Default | Meaning |
|---|---|---|
| `seed` (top level) | `1` | Changes every package's release phase. |
| `packages` | `2000` | Packages per ecosystem. A load generator usually sets it to its watch count. |
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
| `primary_limit.requests` | `0` (off) | github only: GitHub's primary rate limit, requests per window. Used up: 403 with `x-ratelimit-remaining: 0` and `x-ratelimit-reset` (Unix seconds of the window's end). Every response carries `x-ratelimit-limit`, `-remaining`, `-used`, `-reset` and `-resource`. A 304 does not count, as on GitHub. |
| `primary_limit.window` | | Fixed window, starting with the first request (GitHub: `1h`). |

Order of checks per request: `random_429` (takes no token), then the `rate_limit` bucket (every
request that passes `random_429` takes a token, 304s included), then `primary_limit`, then the
latency, then 404. Timestamps have a fixed precision per ecosystem, as on the real registries:
PyPI microseconds, npm milliseconds, GitHub and crates.io whole seconds.

## Statistics and check lag

Per ecosystem and in total: requests, 200, 304, 429, 403 and 404 counts, `cancelled` (the
client gave up during the latency); `published` (releases published in the window); `seen`
(those sent to a client at least once); and check lag.

The lag of a seen release is the time from its publication to the first 200 that contained it
(for GitHub, page 1; for OCI, the page holding the newest tag; for crates, either the index file or
the API). An unseen release counts with
the time since its publication, a lower bound of its eventual lag. Percentiles use the
nearest-rank method.

The end of a run censors the data: a release published a few seconds before the snapshot is
unseen and counts with a lag of a few seconds, which flatters the percentiles. `Server.StatsIn`
takes a window of publication times; a benchmark should end it at least one target (for
benchmark 1, two minutes) before the snapshot, so that every unseen release in it counts with at
least that lag, and start it after the client's first full check. `GET /_fakereg/stats` has no
window.

This is measured at the registry, so it is a lower bound of angelia's detection lag: it ends
when angelia *received* the release, not when it emitted the event. The gap is the processing
time of one response, small next to a two-minute target.

## Known approximations

- **Release timing** is periodic: every package of an ecosystem releases at the same interval,
  with a fixed phase. Real releases are bursty (Poisson at best, release trains at worst).
- **Versions** are plain `1.k.0`: no prereleases, yanks, deprecations, unpublished versions,
  drafts or build metadata, so version parsing and policy are exercised only lightly.
- **Document shape**: one body per package. npm serves the same document for the abbreviated
  and the full packument; crates.io always fits in one page, and the sparse index has no
  `config.json` and no dependencies or features; GitHub sends no `Link` header (the
  source pages by count); OCI has no token handshake; there is no authentication anywhere.
- **Validators**: the ETag changes only when a release is added (and with the query string);
  `Last-Modified` has one-second resolution.
- **Rate limits** are one bucket per ecosystem, not per client or token, and the token bucket
  counts 304s, as GitHub's secondary limits do.
- **Lag is measured at the registry**: see above.
