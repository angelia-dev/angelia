# ADR 0013: Keep WASM for third-party plugins; go-plugin measured as a subprocess alternative (prototype)

- **Status:** Accepted (2026-11-02)
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#60

## Context

[architecture.md §3.6](../architecture.md#36-third-party-plugins) lists three ways to run third-party sources and
sinks: WASM (Extism on `wazero`), a gRPC subprocess (`hashicorp/go-plugin`) and exec/JSON-over-stdio. ADR 0011
built and measured the WASM option. This story builds the same crates.io source as a go-plugin subprocess, runs it
against the same contract suite and the same benchmark 4, and compares overhead, isolation and packaging.

The maintainer approved go-plugin and gRPC for this spike only. The direct dependencies are `hashicorp/go-plugin`,
`hashicorp/go-hclog` (its logger interface) and `google.golang.org/grpc`; `google.golang.org/protobuf` was already
an indirect dependency ([§6](../architecture.md#6-language-and-framework-phase-0-assumptions)): they are used under `bench/`
and nothing under `internal/` or `cmd/` imports them, so the shipped binary does not change
(`go list -deps ./cmd/... ./internal/... | grep -E 'hashicorp|grpc'` is empty).

## What was built

- `bench/plugins/goplugin`: the core side. `Source` is a `source.Source` backed by one plugin process. It starts the
  process with go-plugin (gRPC protocol, AutoMTLS, `SkipHostEnv`, optional SHA-256 `Checksum`), restarts it on the
  next call after it dies, and serves a **Host** service that the plugin dials back through go-plugin's
  `GRPCBroker` to send HTTP. The plugin side is `Serve`.
- `bench/plugins/goplugin/crates`: `bench/plugins/crates/src/lib.rs` ported to Go line for line (same name check,
  same pagination, same validators, same token header, same output). Its binary is
  `bench/plugins/goplugin/cmd/angelia-plugin-crates`. The Rust source itself is not reused: a Rust go-plugin needs a
  gRPC server (tonic), go-plugin's stdout handshake, AutoMTLS and the broker protocol, none of which has a Rust
  library, so a Go port is what a go-plugin author would actually write.
- Tests: `bench/plugins/goplugin/host_test.go` (contract suite and deny paths); `bench/wasm` now builds all three
  implementations and checks that they return identical results (`TestImplementationsAgree`).

**Wire format.** `protoc` could not be run in the sandbox that built this prototype (the binary download was
refused by the session's policy), so there is no generated protobuf code. The two services are hand-written
`grpc.ServiceDesc` values (`wire.go`, the `.proto` they stand for is in a comment) and the messages are the JSON
shapes of the WASM ABI v0 (ADR 0011), carried by a gRPC codec registered under the content-subtype `angelia-json`.
go-plugin's own services keep protobuf. This keeps serialisation identical to the WASM host, so the comparison
isolates the runtime and the IPC. A production go-plugin would use generated protobuf, which would remove the base64
of bodies and some JSON cost: the go-plugin numbers below are, if anything, slightly pessimistic.

**HTTP routing.** The plugin does not dial the network itself: each `List` call carries a random call id, and the
plugin sends every request as `Host.HTTP(call_id, request)`. The core applies the same rules as the WASM
`http_request` host function (ADR 0011): `allowed_hosts` on every hop before netguard, the netguard client (SSRF
guard, per-host limits), 100 requests per call, GET/HEAD/POST, reserved headers, https downgrade refusal, 10 MiB
bodies, and a refused host or SSRF block fails the call even if the plugin swallows the error. A request with an
unknown call id (outside a `List`, during `Validate`, or forged) is refused with `not_allowed_here`, and a
request ends when its `List` call or its RPC ends, whichever comes first. This is
**cooperative**: see Isolation.

## Measurements

### Contract suite

The go-plugin crates source passes `sourcetest.Run` with the fixtures of the WASM crates source
(`TestCratesContract`), plus pagination, ETag and auth (`TestCratesPaginationETagAuth`), `allowed_hosts` on the first
hop and on redirects, the request budget, the call-id check, restart after `SIGKILL` and a checksum mismatch.
`rules_test.go` checks the HTTP rules over the gRPC path with a second test plugin (`goplugintest`, probe): https
downgrade refused before it is sent, netguard's SSRF block, redirect hops counted in the budget, the body cap,
reserved and malformed headers, a refusal that the plugin swallows still failing the call, and a request that
arrives after its call has returned being refused.

### Benchmark 4

`go test -tags bench -run TestBenchmark4 -v ./bench/wasm`, measured exactly as in ADR 0011: 2,000 calls per
workload, rotating native, WASM and go-plugin, same netguard client, in-memory fake upstream (no sockets), overhead
= plugin percentile − native percentile. Same 4-vCPU cloud sandbox (Xeon 2.1 GHz, linux/amd64), not reference
hardware. First run shown; a second run was within 20% on every cell, with the same conclusions. Run-to-run
variance is real: an independent re-run by the reviewer measured the go-plugin 304 overhead p95 at 1.65 ms against
1.07 ms here, so read small differences between cells as noise:

| Workload | native p50 / p95 | go-plugin p50 / p95 | overhead p50 / p95 | < 5 ms? |
|---|---|---|---|---|
| 1 page, 10 versions | 118 µs / 191 µs | 1.13 ms / 1.72 ms | 1.02 ms / 1.53 ms | yes |
| 1 page, 100 versions | 0.57 ms / 0.94 ms | 2.83 ms / 4.15 ms | 2.27 ms / 3.22 ms | yes |
| 3 pages, 300 versions | 1.54 ms / 2.73 ms | 7.42 ms / 9.87 ms | 5.88 ms / 7.14 ms | **no** |
| 304 Not Modified | 34 µs / 72 µs | 0.75 ms / 1.14 ms | 0.72 ms / 1.07 ms | yes |

WASM in the same runs (same table as ADR 0011, re-measured next to go-plugin):

| Workload | wasm p50 / p95 | overhead p50 / p95 |
|---|---|---|
| 1 page, 10 versions | 1.03 ms / 2.00 ms | 0.91 ms / 1.81 ms |
| 1 page, 100 versions | 6.15 ms / 7.36 ms | 5.58 ms / 6.42 ms |
| 3 pages, 300 versions | 17.5 ms / 21.9 ms | 15.9 ms / 19.1 ms |
| 304 Not Modified | 0.35 ms / 0.60 ms | 0.32 ms / 0.53 ms |

Reading:

- go-plugin's overhead also grows with payload, but 2.5× more slowly than WASM's: it pays JSON and gRPC framing on
  two sides of a Unix socket, while WASM pays Extism's 8-byte-at-a-time copy (ADR 0011).
- The fixed cost is higher: at least two RPCs per call (`List`, then one `Host.HTTP` per page), each a socket round
  trip and two context switches. The 304 path, the most common one with ETags, costs 0.7 ms against 0.3 ms.
- Neither passes at 300 versions. go-plugin passes at 100 versions, WASM does not.
- Measured with 4 vCPUs. Benchmark 1 targets 1 vCPU, where every RPC is a context switch between two processes on
  one core; that case is not measured.

Other costs (`go test -tags bench -run TestStartup -v ./bench/plugins/goplugin`):

| | go-plugin | WASM |
|---|---|---|
| Artifact | one static binary per OS/arch: 13.5 MB linux/amd64, 12.6 MB linux/arm64 (`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`) | `crates.wasm`, 233 KiB, every arch |
| Start | 30 ms p50 (exec, handshake, mTLS certificate, `Configure`) | compile in `New`, once per process with a compilation cache |
| Resident memory | ~24 MB RSS per plugin process after 50 calls of 300 versions (measured on the test binary) | instance memory in the core, capped at 64 MiB per pooled instance |

A plugin process costs about as much memory as the whole core at idle (§6: 15–30 MB). Benchmark 2 (idle RSS < 40 MB)
is for the core alone; with go-plugin, each installed plugin adds a process of that size.

## Isolation

| | WASM (ADR 0011) | go-plugin subprocess |
|---|---|---|
| Network | none: the only way out is the `http_request` host function | full OS access. The Host service is used only by plugins that choose to |
| `allowed_hosts`, netguard | enforced on every request | enforced only on requests sent through `Host.HTTP`; a plugin can `net.Dial` anything, including private and metadata addresses |
| File system | none (no WASI) | everything the core's user can read: the SQLite database, the secret-encryption key file, tokens |
| Environment | none | `SkipHostEnv` stops the core's environment (and its secrets) being inherited; set in the prototype |
| Memory, CPU | memory capped, calls interrupted on timeout | unbounded unless set with rlimits or cgroups; a timeout cancels the RPC, not the process's work |
| Crash | a trap discards the instance | the process dies, the core restarts it on the next call (`TestRestartAfterCrash`); the core is unaffected. A start is bounded by `StartTimeout` (default 10 s, capped by the caller's deadline; `TestStartTimeout`), and after a failed restart calls fail fast for `RestartBackoff` (default 5 s; `TestRestartBackoff`). A restart holds the Source's lock, so concurrent calls of that kind wait for it |
| Language | any language with an Extism PDK | gRPC is language-neutral, but the handshake, AutoMTLS and broker exist only in Go |

netguard cannot be enforced inside another process. To make `Host.HTTP` the only route out, the core would have to
start each plugin in an OS sandbox: a network namespace with no interfaces (only the Unix socket to the core,
which is a file and crosses namespaces), a read-only view of the file system without the data directory, rlimits
or a cgroup for memory and CPU, and a seccomp filter. On Linux that is `unshare`/`bwrap`/landlock, each with its own
privilege needs (user namespaces are often disabled in containers, and Docker's default seccomp profile blocks
`unshare`); on macOS and Windows it is a separate design. An egress proxy (`HTTPS_PROXY` to a core-run forward
proxy) is no substitute: it is bypassable without the namespace, and a CONNECT tunnel hides the headers that the
per-host limits and conditional requests need.

Without that sandbox, a go-plugin plugin is as trusted as the core. That is how Terraform and Vault use go-plugin:
their plugins are trusted code chosen by the operator.

## Packaging notes for self-hosters

- **A binary per plugin, per OS and architecture.** The official image is linux/amd64 and linux/arm64, so a plugin
  author publishes at least those two (`CGO_ENABLED=0`, so a plain `GOOS`/`GOARCH` cross-build works; armv7 for
  older Raspberry Pis). Homelab users on other boards need a build for their arch. A `.wasm` file runs everywhere.
- **Installing into the image.** The image is distroless and read-only: plugins come in through a derived image
  (`COPY angelia-plugin-crates /usr/lib/angelia/plugins/`) or a volume. The volume must not be mounted `noexec`. On
  Kubernetes, an init container copies the binaries into an `emptyDir`. A `.wasm` file can sit next to the config,
  or come from the database.
- **Config.** Each plugin entry would name the binary path and its SHA-256. go-plugin refuses a binary whose
  checksum differs (`SecureConfig`, `Options.Checksum` here, `TestChecksumMismatchRefused`); the check runs at every
  start, so replacing a binary in place fails closed (`TestChecksumMatchAccepted`). go-plugin hashes the file and
  then executes the path: a binary swapped between the two is not caught, so the plugin directory must be writable
  only by the operator (read-only image layer or root-owned directory).
- **Signing.** One signature per binary and arch: `cosign sign-blob` (keyless, GitHub OIDC) in the plugin's release
  workflow, and an SBOM per binary. The installer verifies the signature and identity once, then pins the
  checksum. Angelia's own release pipeline (goreleaser, cosign) can serve as the template for plugin authors.
- **Distribution.** GitHub releases with one asset per OS/arch, or an OCI artifact per arch under a multi-arch index
  (`oras push`), which lets the installer pick the arch the way container runtimes do. The future
  `plugins-contrib` index (§9) would list, per plugin version: per-arch URL, SHA-256, cosign identity, protocol
  version.
- **Versioning.** The handshake carries a protocol version (`ProtocolVersion: 1` here); go-plugin can negotiate
  among several (`VersionedPlugins`). Core upgrades that change the protocol need every plugin rebuilt.
- **Process count.** One process per installed plugin (per kind), shared by every target of that kind; gRPC
  multiplexes concurrent calls. Process supervision, restarts and logs (the plugin's stderr goes to the core's
  logger) are the core's job.

## Decision (proposed)

**Keep WASM (ADR 0011) as the mechanism for third-party sources and sinks. Do not ship go-plugin.**

- **Isolation decides.** WASM gives the sandbox that §8 requires ("plugins only reach hosts declared in their
  manifest") by construction, in-process, on every platform. go-plugin gives none: it would need a per-OS sandbox
  that many container setups cannot provide, and until then plugins would have to be fully trusted.
- **Packaging.** One portable 233 KiB file against two or more 13 MB binaries per plugin, each signed, plus a
  process and ~24 MB per plugin. For a self-hosted homelab tool, that is a real cost.
- **Overhead does not decide.** go-plugin is 2.5× faster on large payloads, WASM 2× faster on the common 304 path,
  and neither meets 5 ms at 300 versions. The WASM gap comes from the Extism PDK's copy loop, which the zero-copy ABI
  measurement already planned in ADR 0011 targets. If that does not bring WASM's overhead at 300 versions below
  go-plugin's 7 ms p95, this ADR should be revisited.
- go-plugin remains a reasonable choice for a different case: trusted, out-of-tree plugins written in Go by the
  operator or the project. That is not a Phase 0 need.

## Consequences

### Positive

- The shipped binary keeps one plugin runtime and no gRPC stack.
- The comparison is on record with the same harness, so the zero-copy WASM ABI can be measured against go-plugin.
- `Host.HTTP` shows that the brokered-HTTP design works over IPC too: the same wire shapes and rules served both
  runtimes, which argues for extracting the WASM host's HTTP rules into a runtime-neutral package later.

### Negative

- `go.mod` carries go-plugin, gRPC and their transitive deps while `bench/plugins/goplugin` exists, even though
  nothing ships them. Dependency updates (Renovate) and govulncheck will cover them.
- The go-plugin numbers use a hand-written JSON codec, not generated protobuf.
- The 1-vCPU case is not measured for either runtime.

### Maintainer decisions (2026-11-02)

1. The recommendation is accepted: third-party plugins use WASM, and go-plugin is not shipped. The 1-vCPU and
   protobuf measurements are not required before deciding.
2. `bench/plugins/goplugin` and its dependencies stay in the repository as a reference benchmark for future
   comparisons, rather than being deleted.

## Alternatives considered

- **go-plugin with an OS sandbox.** Closes the isolation gap on Linux with enough privileges; not portable, and the
  sandbox becomes security-critical code we would own. Not pursued in the spike.
- **go-plugin with direct HTTP in the plugin.** Simpler plugins and no Host round trips, but no netguard, rate
  limits or `allowed_hosts` at all, and the per-host budgets of ADR 0011 would be split between processes. Rejected.
- **go-plugin net/rpc mode.** Go-only and gob-encoded; gRPC is go-plugin's recommended mode and the one that keeps
  other languages possible.
- **exec / JSON-over-stdio** (§3.6): not measured; it shares go-plugin's isolation problem, with a process per call.

## References

- angelia-dev/angelia#60, epic #7; ADR 0011 (#59); `bench/plugins/goplugin`, `bench/wasm`
- go-plugin: https://github.com/hashicorp/go-plugin (gRPC protocol, `GRPCBroker`, `SecureConfig`, AutoMTLS)
