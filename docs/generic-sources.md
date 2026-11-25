# Generic HTTP sources: `http-regex` and `http-json`

Two built-in sources watch upstreams that are not a registry Angelia knows: a release page, a download
directory, a small JSON endpoint. Both GET a URL that the **watch** chooses, extract raw version strings and
leave diffing, schemes and policy to the core, like every other source. Design and decisions:
[ADR 0038](adr/0038-generic-http-sources.md) (#97).

| Kind | Reads | Extracts with |
|---|---|---|
| `http-regex` | any text body (HTML, plain text) | an RE2 regular expression |
| `http-json` | a JSON document | a JSONPath expression (a subset of RFC 9535) |

## `http-regex`

```yaml
watches:
  - name: tool
    source: http-regex
    coordinates:
      name: tool                                   # data.package of the events
      url: https://downloads.example.org/tool/     # the page to read
      regex: 'tool-([0-9]+\.[0-9]+\.[0-9]+)\.tar\.gz'
    scheme: semver                                 # see "Version scheme" below
```

| Coordinate | Required | Rule |
|---|---|---|
| `name` | yes | the package name in events, see [Package identity](#package-identity) |
| `url` | yes | see [URL rules](#url-rules) |
| `regex` | yes | Go [`regexp` syntax](https://pkg.go.dev/regexp/syntax) (RE2: no back-references or look-around, linear time), at most 1024 bytes |

- No capture group: the whole match is the version (`v[0-9.]+` yields `v1.2.3`).
- One capture group: that group is the version (`tool-([0-9.]+)\.tar` yields `1.2.3`).
- Several groups: one must be named `version` (`(?P<version>[0-9.]+)`); use `(?:...)` for groups that only
  group. A match in which the group did not take part is skipped.
- The expression runs over the whole body; `(?m)` makes `^` and `$` match at line breaks, `(?s)` lets `.`
  match a line break. The request sends `Accept: text/html, text/plain;q=0.9, */*;q=0.8`.

## Common rules

### URL rules

Every request goes through the instance's HTTP client, with its SSRF guard, per-host rate limits and retries
([Outgoing HTTP](configuration.md#outgoing-http-and-private-networks)). In addition:

- The URL must be `https://`. `http://` is accepted only for a host in `http.allow_http_hosts`.
- No user information (`user:password@`) and no fragment (`#...`); at most 2048 bytes; an IP address host must
  be written in its usual form (`a.b.c.d`, not `2130706433`). These sources send no credential: watch public
  URLs only (for GitHub or GitLab with a token, use `github-release`, `github-tag` or `gitlab-release`).
- Loopback and private addresses are refused unless the operator lists them in `http.allow_networks`. That list
  is shared by every source and sink, so allowing a private registry's network also lets watches of these kinds
  read any host on it: see [Who can create these watches](#who-can-create-these-watches).
- At most 5 redirects are followed; a redirect to plain `http://` is refused unless its host is in
  `http.allow_http_hosts`.
- The URL is stored as written except for its spelling: scheme and host in lower case, the default port
  removed and an empty path written `/`. The path and query are kept byte for byte.

### Limits

| Limit | Value | When exceeded |
|---|---|---|
| Response body | 2 MiB | the check fails ("larger than 2097152 bytes") |
| Distinct versions per fetch | 1,000 | the check fails: narrow the expression |
| Extracted values before de-duplication | 10,000 | the check fails: narrow the expression |
| One version | 256 bytes, valid UTF-8, no control character | the value is skipped (counted in a `debug` log line) |

Versions keep the order of the document, duplicates keep their first position. **No usable match is an
error**, shown on the watch's status, so that a page whose layout changed does not look like "no new
release". Surrounding white space is not trimmed by the source: the core skips such values, so write the
expression to match the version only.

### Conditional requests

The source sends `If-None-Match` and `If-Modified-Since` when the server gave validators, and also keeps a
hash of the extracted list: a page that changed (an ad, a timestamp) but lists the same versions is treated as
unchanged, without touching the database.

### Package identity

`name` is the package of every event: `data.package`, the CloudEvents `subject`
(`http-regex:tool@1.2.3`) and the subscription filter's `package` glob. It is 1 to 128 bytes, printable, with
no white space. `data.purl` and the `urls` are `null`: an arbitrary URL has no package URL type. `name` is part
of the target, so two watches of one URL with different names are two targets fetched separately.

### Version scheme

The default scheme of both kinds is `lexical`: versions are only compared as strings, so `previous_version`
and `bump` are `null` and a `constraint` never matches ([policy.md](policy.md)). Set `scheme:` on the watch
(`semver`, `calver`, `pep440`) when you know the upstream's scheme.

### Per-version details

The sources read versions only: `published_at` is `null`, and the prerelease flag comes from the scheme.
Methods other than GET, request headers and bodies are not supported.

### Who can create these watches

- The watch file (operator-controlled): no extra rule.
- The REST API and MCP: creating a watch of these kinds, or changing the `source` or `coordinates` of a watch
  when the old or the new source is of these kinds, needs a token with the **`admin` scope**; a `write` token
  gets `403` with `required_scope: admin`. Changing only `labels`, `policy`, `scheme` or `interval` keeps the
  `write` scope. `angelia apply` of a file with such watches therefore needs an admin token. The reason: a
  `write` token (a CI bot, an agent) could otherwise make the server read internal pages on allowed networks
  and expose their strings as versions.

### Instance configuration

`sources.http-regex` and `sources.http-json` take no key (`base_url`, `token_env`, `auth`, `api`), and a named
instance of either kind (`sources.<name>.kind: http-json`) is refused by `config check` and `serve`: the URL
is each watch's.
