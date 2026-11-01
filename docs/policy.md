# Watch policy (draft)

Story: angelia-dev/angelia#57. Decisions: [ADR 0012](adr/0012-policy-evaluation-order-and-cooldown.md).
Status: **draft**. The key names are the ones of the watch file's `policy` block
([watch-file.md §2.3](watch-file.md), story #48, being reviewed); this page defines what each key
does. They become a public contract in Phase 1.

A watch's policy decides which of the new versions found by the diff engine
([ADR 0010](adr/0010-diff-engine-known-set-and-baseline.md)) become `release.published` events, and
when. It is stored as JSON in `watch.policy_json` with the same keys, plus the watch's `scheme`
override. Code: `internal/policy`.

```yaml
policy:
  prerelease: none          # none | only | include
  include: ['^v?1\.']       # RE2, unanchored, matched against the raw version
  exclude: ['-canary']
  constraint: ">=1.2 <2"    # compared with the watch's version scheme
  bump: [minor, patch]      # major | minor | patch
  cooldown: 72h             # Go duration
  baseline: true
```

## Keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `prerelease` | string | `none` | `none` drops pre-releases, `only` drops final releases, `include` keeps both. A version is a pre-release when the source says so (GitHub, npm dist-tags), otherwise when the watch's scheme says so (`1.0.0-rc.1`, `1.0rc1`, `24.04-beta`). |
| `include` | list of regex | `[]` (all) | Keep a version only if its **raw** upstream string (`v2.3.0-rc.1`, not the normalised form) matches at least one pattern. Go RE2 syntax, unanchored: use `^` and `$`. At most 16. |
| `exclude` | list of regex | `[]` | Drop a version whose raw string matches any pattern. Exclude wins over include. At most 16. |
| `constraint` | string | empty (any) | A version range, below. |
| `bump` | list | `[]` (all) | Keep a version only if its bump is listed: `major`, `minor` or `patch`. The bump compares the version with the greatest earlier version (ADR 0007): the backport `1.9.4` after `2.0.0` is a `patch`. A version with nothing to compare with (the first release, or a tag the scheme cannot parse) and a pre-release step (`1.0.0-rc.1` to `1.0.0-rc.2`) have no listed bump, so a non-empty `bump` drops them. |
| `cooldown` | duration | `0` | Hold a version until it is older than this, then emit it if it was not yanked meanwhile. Go syntax (`90m`, `72h`; no `d`), whole seconds only (`1.5s` and `500ms` are errors, as in the watch file), at most `8760h`. Below. |
| `baseline` | bool | `true` | Record the versions known at a watch's first check without events. Applied by the diff engine (ADR 0010). **`false` is accepted but not honoured yet**: the first check is always a baseline (follow-up). |

The watch's `scheme` (top-level key of a watch, stored with the policy) chooses how `constraint`
operands are parsed and compared, and how pre-releases, bumps and aliases are classified.

Unknown keys are errors. An invalid policy should be rejected when it is stored; if one reaches a
check anyway, that watch is skipped for the check with an `ERROR` log and emits nothing from it.

### `constraint`

- Comparators `>=`, `>`, `<=`, `<`, `==` (or `=`), `!=`, followed by a version, optionally with a space
  (`>= 2`). A bare version means `==`.
- Comparators separated by spaces or commas must **all** hold: `>=2 <3`, `>=2,<3`.
- `||` separates alternatives, **one** of which must hold: `<1 || >=2 <3`.
- Operands are parsed with the watch's scheme. A short operand that the scheme rejects is completed
  with `.0` (at most twice): `>=2` means `>=2.0.0` in strict SemVer, `>=2026` means `>=2026.0` in
  calver. An operand that still does not parse is an error.
- Comparison is the scheme's precedence and nothing else. Pre-releases are not special (that is the
  `prerelease` key's job), so with `prerelease: include`, `<3` matches `3.0.0-rc.1` in SemVer and
  `3.0rc1` in PEP 440. Write `<3.0.0-0` (SemVer) or `<3.0.dev0` (PEP 440) to exclude them.
- Equal precedence counts as equal: `==1.0` matches `1.0.0` in PEP 440, `==1.2.3` matches `v1.2.3`
  and `1.2.3+build` in SemVer.
- A version the scheme cannot parse (a lexical fallback such as `latest`) never matches a non-empty
  constraint.
- At most 256 bytes and 16 comparators, on one line.
- No caret, tilde or wildcard ranges (`^1.2`, `~1.2`, `1.x`) in Phase 0. The error names the
  unsupported operator (`unsupported operator "~"`) or the missing separator (`>=1<2`: `missing
  separator before "<2"`).

### `cooldown`

- **Pending maintainer confirmation** (ADR 0012 §6). The age of a version counts from the time
  Angelia first saw it (`detected_at`), except for source kinds whose publish date is assigned by the
  registry at upload, which the publisher cannot back-date: `pypi`, `npm`, `crates` and
  `github-release`. For those, it counts from the upstream **publish date** when the source gives
  one and it is not later than `detected_at` (an upstream date in the future cannot hold a version
  longer). Every other kind counts from detection: `github-tag` (the commit date, which whoever
  pushes the tag chooses), `oci` (the image's own `created` field), plugins and any kind not listed.
  So a tag dated 2020 that appears today is held for the full cooldown. Code:
  `policy.TrustsPublishedAt`.
- `published_at` is still carried in the event as the source reported it.
- A version older than the cooldown when first seen is emitted at once (an old backport).
- Otherwise it is **held** (table `pending_release`), and every check of the target re-evaluates what
  the watch holds, including checks answered `304 Not Modified`:
  - **yanked** (at any check while held): dropped; it never emits, even if un-yanked later;
  - no longer listed upstream: kept and not emitted; if it is listed again it continues;
  - no longer allowed by the **current** policy (filters changed): dropped;
  - older than the **current** cooldown: emitted, with `cooldown_satisfied_at` = start of the age +
    cooldown. Shortening or removing the cooldown releases held versions at the next check.
- Emission happens at the first check after the cooldown ends, so up to one check interval late;
  `cooldown_satisfied_at` gives the exact time. A version is never emitted while the target's checks
  fail: its yank state is unknown.
- Held versions are rows in the database, so they survive restarts.
- Held versions keep the classification of the check that found them (`previous_version`, `bump`,
  `detected_at`, URLs).

## Evaluation order

For every check of a target, after the diff engine and in the same transaction:

1. **Held versions** of the watch: dropped if yanked; kept if removed upstream; dropped if the current
   filters (steps 5 to 9 below) reject them; emitted if their cooldown is over.
2. Only **new** versions are candidates. Yank, un-yank, removal and return of known versions are not
   `release.published` events (yank events: #112).
3. A new version already **yanked** when first seen is dropped.
4. **Aliases** are dropped: a new version with the same precedence as an existing one (`v1.2.0` after
   `1.2.0`, `1.0.0` after `1.0` in PEP 440, `1.2.0+b` after `1.2.0+a`), so one release is announced
   once. In a single fetch the first listed entry wins. This applies only when the earlier version
   passes this watch's `include` and `exclude`: with `include: ['^v']`, `v1.2.0` after `1.2.0` is
   not an alias, because the watch never announced `1.2.0`.
5. `include`, then 6. `exclude` (on the raw string), 7. `prerelease`, 8. `constraint`, 9. `bump`.
10. `cooldown`: emit now, or hold.

The first rule that drops a version decides the logged reason (`yanked`, `alias`, `include`,
`exclude`, `prerelease`, `constraint`, `bump`; `corrupt` for a held row that cannot be read). Drops of
new versions are logged at `DEBUG`, drops of held versions at `INFO`.

Emitted releases are ordered: formerly held versions first (oldest detection first, then version
order), then new versions in version order.

## Not in Phase 0

- Policy on subscriptions (architecture.md §3.4 says "per watch or subscription"); subscriptions have
  a `filter` instead.
- `baseline: false`.
