# ADR 0037: oci source: registry selection, token flow and tags as versions

- **Status:** Proposed
- **Date:** 2026-11-25
- **Story:** angelia-dev/angelia#94 (sub-story 94-A), epic #21

## Context

The `oci` source lists the tags of an image repository on Docker Hub, GHCR or any registry implementing the OCI
Distribution API. The event contract already knows `oci` (`data.source`, `data.package` such as
`ghcr.io/org/image`, purl type `oci`). Three security questions had no ADR: which host a watch may make the
source contact, how the registry token is obtained and kept, and which URLs the source follows. Decisions D40 to
D49 of the story were accepted as recommended; this ADR records D40 to D43, D45 and D48. D44 (credentials) is
added by sub-story 94-C; D46 (digests) and D49 (URLs) by 94-B.

## Decision

- **D40 Standard library only.** No `go-containerregistry` or `oras-go`: both send credentials themselves and
  would bypass the per-hop host binding of `registryauth` (ADR 0024).
- **D41 The image names the registry.** `image` carries the host (`ghcr.io/org/app`; none means Docker Hub). The
  built-in `oci` fetches from that host, anonymously only. A source with `base_url` is pinned: `Validate` accepts
  only images served by that origin (`docker.io` matches `https://registry-1.docker.io`), and the requests go to
  `base_url` (so its scheme applies). `base_url` of an `oci` source has no path.
- **D42 Canonical form.** Lower-case host; no host is `docker.io`; `index.docker.io` and `registry-1.docker.io`
  are `docker.io`; a one-segment Docker Hub path gets `library/`. `Validate` refuses a tag, a digest, a scheme,
  upper case in the path and anything outside the distribution reference grammar (255 characters at most). The
  API endpoint of `docker.io` is `https://registry-1.docker.io`, of any other host `h` it is `https://h`.
- **D43 Token flow.** `GET /v2/<name>/tags/list?n=1000`; on a 401 with `Bearer realm, service, scope`, one
  request to the realm (no `Authorization`), then one retry with `Authorization: Bearer <token>`. The realm must
  be `https`, or `http` only for a host of `http.allow_http_hosts`; it goes through the netguard client like every
  request. The token lives for one `List` call (no cache, no cursor), is an `apiclient.Secret` and is sent only
  to the registry endpoint. A `Link: rel="next"` URL is resolved against the request URL and must have the origin
  of the endpoint, else the call fails without requesting it. A 401 after a token means "not found or access
  denied" (Docker Hub answers 401 for a missing repository). A `Basic` challenge fails with a message naming
  `sources.<name>.auth` (94-C).
- **D45 Tags are versions.** Every tag is returned except the referrers tag schema (`sha256-<64 hex>` with an
  optional `.sig`, `.att`, `.sbom`). Filtering is the watch policy's job (recipes in docs/watch-file.md).
- **D48 Bounds.** `n=1000`, at most 50 pages; more fails the call, because the missing tail of a sorted list
  would look removed. No conditional requests (no registry measured sends an `ETag`). No new built-in host limit:
  the fallback limit, 429 and `Retry-After` handling of netguard apply.

The source uses `hc.Do` and not `netguard.Fetch`, because `Fetch` drops the headers of a 401 and the challenge
is in `WWW-Authenticate`; the SSRF check, rate limits and body cap are in the transport and still apply.

## Consequences

### Positive

- No dependency, no change to `Source`, the event schema, the OpenAPI spec or the store.
- A watch choosing a public host is no new risk: every hop is checked by the SSRF guard and the built-in source
  sends no credential.
- `node` and `docker.io/library/node` are one target.

### Negative

- Two extra requests per check (401 and token) for registries that need a token.
- Without a filter, tags such as `latest` produce events when first seen (known limitation of D45).
- A repository above 50,000 tags cannot be watched.

### Open questions

- Docker's current pull and `HEAD` rules could not be re-checked from the development sandbox; a live test
  (`go test -tags live ./internal/source/oci`) exists, and the measurements of 2026-11-25 in the story remain
  the reference: a manifest `HEAD` carried the pull-limit headers without spending the budget. To be
  re-verified by the maintainer before 94-B relies on it for anonymous Docker Hub watches.
- Token caching and remembering the challenge are left out (D43 b, c).

## Alternatives considered

- **A library (`go-containerregistry`, `oras-go`).** Large module graph or its own credential handling; neither
  is in architecture.md section 6.
- **One registry per source (as npm).** Loses the host in `data.package`, contradicting the spec examples.
- **A closed list of public hosts.** Safe but surprising.
- **A token cache per (realm, service, scope).** Saves almost nothing at 5 minute intervals and caches a
  credential-derived value (ADR 0024 does not cache secret values).

## References

- angelia-dev/angelia#94, #21; [ADR 0006](0006-source-target-and-fetchstate.md), [ADR 0021](0021-source-canonical-target.md),
  [ADR 0024](0024-secrets-encrypted-at-rest.md)
- OCI Distribution Specification; Docker Registry token authentication specification
