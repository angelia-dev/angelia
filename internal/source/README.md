# internal/source

The `Source` contract of [architecture.md section 3.3](../../docs/architecture.md#33-source-plugins-detect),
plus a `Registry` and the contract test suite every source must pass.

Sources are **dumb**: they list raw upstream versions and an opaque fetch state. Diffing, version
parsing, filtering and cooldown belong to the core (architecture.md section 3.4).

## Types

| Type | Purpose |
|---|---|
| `Source` | `Kind()`, `Validate(Target) error`, `List(ctx, Target, FetchState) (Result, error)` |
| `Target` | `Kind` + source-specific `Coordinates` (`map[string]string`), stored as `coordinates_json` |
| `FetchState` | `ETag`, `LastModified`, `Cursor`; opaque to the core, stored as `fetch_state_json` |
| `Result` | `Versions` (newest first if known), the next `State`, `NotModified` |
| `Version` | `Raw`, `PublishedAt`, `Prerelease *bool`, `Yanked`, `URL`, `NotesURL`, `Metadata` |
| `Registry` | explicit value (no package global): `NewRegistry`, `Register`, `Get`, `Kinds` |

Errors: `ErrInvalidTarget` (every `Validate` failure wraps it), `ErrUnknownKind`, `ErrDuplicateKind`.

## Writing a source

1. Create `internal/source/<kind>/` and implement `source.Source`.
2. `Validate` does no I/O and wraps `source.ErrInvalidTarget`.
3. `List` takes `ctx` first, wraps errors with `%w` (a cancelled context must surface `context.Canceled`),
   sends conditional headers from `FetchState`, and on a 304 returns `NotModified: true`, no versions
   and the (still non-zero) state.
4. Fill `PublishedAt` whenever upstream has it; leave `Prerelease` nil when upstream has no flag.
5. Return versions in a deterministic order.
6. Register it on the `Registry` in the composition root (`registry.Register(pypi.New(...))`).

## The contract suite: `sourcetest.Run`

```go
func TestContract(t *testing.T) {
	srv := httptest.NewServer(fakeUpstream()) // recorded fixtures in testdata/
	t.Cleanup(srv.Close)
	sourcetest.Run(t, pypi.New(srv.URL, srv.Client()), sourcetest.Fixtures{
		Valid:          source.Target{Kind: "pypi", Coordinates: map[string]string{"name": "requests"}},
		Invalid:        []source.Target{{Kind: "pypi"}, {Kind: "pypi", Coordinates: map[string]string{"name": "a/b"}}},
		WantRaw:        []string{"2.0.0", "1.0.0"},
		Conditional:    true,
		HasPublishedAt: true,
	})
}
```

| Subtest | Checks |
|---|---|
| `Kind` | `Kind()` is non-empty and equals `Fixtures.Valid.Kind` |
| `ValidateAccepts` / `ValidateRejects` | the valid target passes; every `Invalid` target fails with an error wrapping `ErrInvalidTarget` |
| `ListDeterministic` | two `List` calls with empty state return identical results, at least one version, no empty `Raw`, not `NotModified` |
| `ListMatchesFixture` | if `WantRaw` is set, `Raw` values match it in order |
| `ListHonoursContext` | a cancelled context makes `List` fail with `context.Canceled` |
| `NotModified` | (if `Conditional`) the state from call 1 makes call 2 return `NotModified`, no versions, non-zero state |
| `PublishedAt` | (if `HasPublishedAt`) every version has a non-zero `PublishedAt` |

`Run` also blocks the network: it replaces `http.DefaultTransport` for the test so any request to a
non-loopback host fails. Your source must therefore talk only to the `httptest` server. Because of that
swap, do not call `t.Parallel()` in a test that calls `Run`. A source that uses its own `http.Client`
must be given one built on `srv.Client()` (loopback only) in the test.

`sourcetest.Fake` is an in-memory source that passes the suite; use it for tests of the scheduler and
the core. Live-registry tests go behind the `live` build tag and never run in `go test`.
