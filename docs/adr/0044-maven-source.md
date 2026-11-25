# ADR 0044: The maven source reads maven-metadata.xml and dates new versions from their POM

- **Status:** Accepted (maintainer, 2026-11-25: the recommended D95–D99 of #96)
- **Date:** 2026-11-25
- **Story:** angelia-dev/angelia#96

## Context

PRD F2 lists Maven among the built-in sources and epic #21 requires `maven` to pass the contract suite.
The event spec already lists `maven` in `data.source`, documents `data.package` as `org.slf4j:slf4j-api`
and the purl `pkg:maven/<groupId>/<artifactId>@<version>`, with `repository_url` for a repository other
than Maven Central. A Maven repository has no listing API: each artifact has a `maven-metadata.xml` with
its versions in deploy order and no per-version date. Maven Central (measured 2026-11-25) sends an `ETag`
and honours `If-None-Match` (304), not `If-Modified-Since`; a `HEAD` of a version's `.pom` gives its upload
time as `Last-Modified`; `<release>` is the last deployed version, which can be a pre-release. Ordering is
the `maven` version scheme (ADR 0007 addendum).

## Decision

1. **Coordinates (D95).** One key, `package`, `groupId:artifactId`, equal to `data.package`. Exactly one
   `:`; groupId `[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*`; artifactId `[A-Za-z0-9_][A-Za-z0-9_.-]*`; each at most
   255 characters; any other key refused. No `Canonicalizer`: repository paths are case-sensitive. The rules
   keep the request path inside the artifact's directory.
2. **Listing (D96).** `GET <base>/<groupId, . as />/<artifactId>/maven-metadata.xml` through the injected
   netguard client, with the stored `ETag` and `Last-Modified`; a 304 is `NotModified`. Parsed with
   `encoding/xml` (`Strict`; `CharsetReader` for `US-ASCII` and `ISO-8859-1`, anything but UTF-8 otherwise
   refused; `UTF8` without the hyphen is read as UTF-8). A `<groupId>` or `<artifactId>` that differs from
   the coordinate fails the call. Versions are
   trimmed and deduplicated; an empty one, `.`, `..`, one over 256 bytes or with `/`, `\`, white space or a
   control character is skipped (one DEBUG record per call). They are returned newest first by the `maven`
   scheme, ties by raw string. At most 10,000 distinct versions are kept per call: past that, a WARN, and
   the last 10,000 in metadata (deploy) order are kept. 404 and 410 fail the call.
3. **Dates (D96).** For versions new since the cursor (JSON with the known version names; none on the first
   scan or with an unreadable cursor), newest first, at most 50 per call:
   `HEAD <base>/<g>/<a>/<v>/<a>-<v>.pom`, `Last-Modified` as `PublishedAt`. A 404, a 5xx or another failed
   lookup leaves the version undated with a WARN; a 429 or a cancelled context fails the call, so the cursor
   does not advance. Versions past the 50-lookup cap of a call are recorded in the cursor and stay undated:
   they are not looked up on a later call.
4. **What is reported (D98).** Every version, `-SNAPSHOT` included (the scheme makes it a pre-release, so the
   default `prerelease: none` drops it). `<latest>`, `<release>`, `<lastUpdated>` ignored. `Yanked` false,
   `Prerelease` nil. `URL` is `https://central.sonatype.com/artifact/<g>/<a>/<v>` for Maven Central (both
   `https://repo.maven.apache.org/maven2` and `https://repo1.maven.org/maven2`), empty otherwise. No
   relocation, packaging or timestamped snapshot resolution.
5. **Repositories and credentials (D99).** Default `base_url` `https://repo.maven.apache.org/maven2`; both
   Central URLs leave the purl without `repository_url`. Any other `base_url`, with a path, is a private
   repository (Nexus, Artifactory, GitLab or GitHub Packages Maven endpoints). `maven` joins
   `credentialKinds`: `auth` (`basic` or `bearer`) and `token_env` (bearer) through `registryauth.Wrap`, as for
   `npm` and `pypi`. A redirect to a host the secret is not bound to fails with `secrets.ErrHostMismatch`.

## Consequences

### Positive

- The common check is one 304. A new version costs one small `HEAD` and gets its real upload time.
- No change to `source.Source`, the plugin ABI, the store or `go.mod`.
- `encoding/xml` expands no DTD entity and fetches no external one, so XXE and entity bombs do not apply;
  the netguard body cap bounds the document.

### Negative

- Versions known before the first scan, and new versions beyond 50 in one call, stay undated.
- A repository that stores artifacts on another host (a redirect) needs that host bound to the secret too.
- `settings.xml`, mirror rules, custom auth headers (GitLab `Private-Token`, `Deploy-Token`, `Job-Token`) and
  client certificates are not supported.

### Open questions

- Whether a GitLab Maven endpoint accepts Basic with a token (documented upstream; manual check pending).
- Trusting `published_at` for cooldown (`trustedPublishedAt`) is a separate decision (ADR 0012 §6).

## Alternatives considered

- **Two keys `group` and `artifact`.** Needs special cases in `event.Package` and the admin UI.
- **`name` as the key.** Means the POM's `<name>` to Maven users.
- **No `HEAD` lookups.** Every version undated.
- **Central's search API for dates.** Central only, lags the repository, a second host.
- **Dropping `-SNAPSHOT` in the source.** Hides data a snapshot repository may want; the policy filters it.
- **A `header` auth type, or reading `~/.m2/settings.xml`.** A new security decision, or files outside the
  config model.

## References

- Issue #96 (decisions D95 to D99), epic #21.
- ADR 0006 (Target and FetchState), ADR 0007 (version schemes, maven addendum), ADR 0010 §5 (baseline),
  ADR 0024 (registry credentials).
- Maven repository layout: https://maven.apache.org/repository/layout.html
