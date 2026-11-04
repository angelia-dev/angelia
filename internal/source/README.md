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
| `Canonicalizer` | optional: `Canonical(Target) (Target, error)` returns the one spelling stored and used as the coalescing key ([ADR 0021](../../docs/adr/0021-source-canonical-target.md)); call it through `source.Canonical(src, t)` |
| `Registry` | explicit value (no package global): `NewRegistry`, `Register`, `Get`, `Kinds` |

Errors: `ErrInvalidTarget` (every `Validate` failure wraps it), `ErrUnknownKind`, `ErrDuplicateKind`.

## Writing a source

1. Create `internal/source/<kind>/` and implement `source.Source`.
2. **Inject the HTTP client.** The constructor takes a base URL and an `*http.Client`
   (`pypi.New(baseURL string, c *http.Client)`); never use `http.DefaultClient`. Production passes the
   netguard client (#49); tests pass the guarded client from `sourcetest`. The #49 client must therefore
   accept a base `http.RoundTripper`, so the test guard can sit underneath it.
3. `Validate` does no I/O (the suite asserts zero requests) and wraps `source.ErrInvalidTarget`.
4. `List` takes `ctx` first, wraps errors with `%w` (a cancelled context must surface `context.Canceled`),
   sends conditional headers from `FetchState`, and on a 304 returns `NotModified: true`, no versions
   and a non-zero state (keep the incoming one if the 304 carries no ETag). A target that does not exist
   upstream is an error, not an empty result. `List` must be safe for concurrent use.
5. Fill `PublishedAt` whenever upstream has it; leave `Prerelease` nil when upstream has no flag.
6. Return versions in a deterministic order.
7. If one upstream object can be spelled several ways in the coordinates (case, separators), implement
   `source.Canonicalizer`: no I/O, keep `Kind`, idempotent, result passes `Validate`. Cite the registry's
   own naming rule, and list other spellings in `Fixtures.Aliases`. If the registry is case-sensitive, don't.
8. Register it on the `Registry` in the composition root (`registry.Register(pypi.New(...))`).

## The contract suite: `sourcetest.Run`

```go
func TestContract(t *testing.T) {
	var rev atomic.Int64 // bumped by Change to simulate a new release
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: fakeUpstream(&rev), // recorded fixtures in testdata/
		New: func(baseURL string, c *http.Client) source.Source {
			return pypi.New(baseURL, c)
		},
		Valid:          source.Target{Kind: "pypi", Coordinates: map[string]string{"name": "requests"}},
		Invalid:        []source.Target{{Kind: "pypi"}, {Kind: "pypi", Coordinates: map[string]string{"name": "a/b"}}},
		Missing:        &source.Target{Kind: "pypi", Coordinates: map[string]string{"name": "does-not-exist"}},
		WantRaw:        []string{"2.0.0", "1.0.0"},
		Conditional:    true,
		Change:         func() { rev.Add(1) },
		HasPublishedAt: true,
	})
}
```

`Run` starts the `httptest` server for `Handler`, builds the source with `New(srv.URL, client)` and
checks:

| Subtest | Checks |
|---|---|
| `Kind` | `Kind()` is non-empty and equals `Fixtures.Valid.Kind` |
| `ValidateAccepts` / `ValidateRejects` | the valid target passes; every `Invalid` target fails with an error wrapping `ErrInvalidTarget` |
| `ValidateNoRequests` | `Validate` sent no HTTP request (owned server only) |
| `Canonical` | for a `Canonicalizer`: `Canonical(Valid)` keeps the kind, passes `Validate`, is idempotent, does not mutate its argument and sends no request; every `Aliases` entry has the same canonical target. A `Canonicalizer` without `Aliases` fails, and so do `Aliases` without `Canonicalizer` |
| `ListDeterministic` | two `List` calls with empty state return identical results, at least one version, no empty `Raw`, not `NotModified` |
| `ListConcurrent` | 8 concurrent `List` calls all equal the first (run with `-race`) |
| `ListMatchesFixture` | if `WantRaw` is set, `Raw` values match it in order |
| `ListMissing` | if `Missing` is set, `List` returns an error |
| `ListHonoursContext` | a cancelled context makes `List` fail with `context.Canceled` |
| `NotModified` | (if `Conditional`) the state from call 1 makes call 2 return `NotModified`, no versions, non-zero state |
| `PublishedAt` | (if `HasPublishedAt`) every version has a non-zero `PublishedAt` |
| `ChangeInvalidatesState` | (if `Conditional` and `Change`) after `Change`, `List` with the old state is not `NotModified` and returns versions. Runs last |

### Network isolation

`sourcetest.Client(t, srv)` returns an `*http.Client` whose transport fails the test, with a clear message,
and sends nothing for any request whose `URL.Host` is not the fake server's listener address. A
`net.Dialer` `Control` hook refuses any other address as defence in depth. `sourcetest.Requests(c)` returns
the number of requests sent. Nothing global is modified, so tests calling `Run` may use `t.Parallel()`.
The guard only covers clients you inject: a source that dials with its own client or `http.DefaultClient`
is not caught, which is why injection is required.

For a source that needs no HTTP, pass it as `src` and leave `Handler` and `New` unset (the HTTP-specific
checks are then skipped). `sourcetest.Fake` is an in-memory, concurrency-safe source that passes the suite;
use it for tests of the scheduler and the core. Live-registry tests go behind the `live` build tag and
never run in `go test`.

Design decisions for `Target`, `FetchState` and errors: [ADR 0006](../../docs/adr/0006-source-target-and-fetchstate.md).
