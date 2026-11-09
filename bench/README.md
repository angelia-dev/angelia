# Benchmarks

The Phase 0 benchmarks of [architecture.md §6](../docs/architecture.md#6-language-and-framework-phase-0-assumptions):

| # | What | Run with | Where |
|---|---|---|---|
| 1 | Scale: 10,000 watches on 1 vCPU / 256 MB | `make bench-scale` | [load](load/README.md), the benchmark machine |
| 2 | Idle footprint: 100 watches; memory, binary, image | `make bench-idle` | [load](load/README.md), the benchmark machine |
| 3 | GitHub: 2,000 repos on one token | (story angelia-dev/angelia#64) | the benchmark machine |
| 4 | WASM plugin overhead | `go test ./bench/wasm` | anywhere |
| 5 | Kill -9 during dispatch | (its story) | anywhere |
| 6 | Version ordering | `make bench-ordering` | [ordering](ordering/README.md), anywhere |

Benchmarks 1 to 3 measure time and resources, so their reported results come from one known
machine, H-bench-env. The others are deterministic and run anywhere, CI included.

## The benchmark machine (H-bench-env)

Decided by the maintainer on 2026-11-09 (angelia-dev/angelia#38):

- **Host**: the maintainer's (Marco's) Mac with Docker Desktop. The report records the CPU
  model, memory, macOS version, Docker version and the CPUs and memory of Docker's VM.
- **Container mode**: angelia runs in a Linux container limited with `--cpus 1 --memory 256m
  --memory-swap 256m` (1 vCPU, 256 MB, no swap), the harness's default. The harness and the
  fake registry run on the Mac, outside the limits. Process mode (`BENCH_CONTAINER=0`) needs
  Linux and is for smoke runs only.
- **Network**: Docker Desktop needs `BENCH_ALLOW_NETWORK=192.168.65.254/32` (the address of
  `host.docker.internal` in the container; check it with
  `docker run --rm alpine getent hosts host.docker.internal`). See
  [load/README.md](load/README.md#reaching-the-registry-past-the-ssrf-guard).
- **GitHub token** (benchmark 3): the runner's environment holds `ANGELIA_BENCH_GITHUB_TOKEN`, a
  token with public read-only access. It is never committed, written to a report or pasted
  into an issue.
- Tools: Go (the version in `go.mod`), `make`, `git`, Docker Desktop. Close other heavy
  applications during a run and run one benchmark at a time.

## Running benchmarks 1 and 2

From a clean checkout of the commit to measure:

```
export BENCH_ALLOW_NETWORK=192.168.65.254/32
make bench-scale    # benchmark 1: 10,000 watches, 30 minutes after ready, with profiles
make bench-idle     # benchmark 2: 100 watches, 10 minutes after ready
```

Each command builds what it measures, runs once and prints the verdict. Exit status: 0 pass,
1 fail, 2 error, 3 incomplete (a target not measured). Variables and what a run does:
[load/README.md](load/README.md).

**Benchmark 2** checks, in MB as in the architecture (1 MB = 10⁶ bytes):

- idle memory < 40 MB: the highest memory sample after the warm-up (1.1 × the check interval:
  every watch has been checked once), container memory from `docker stats`;
- binary < 40 MB: the binary that ran, built by `make build` (in container mode for Linux, the
  release's target);
- image < 50 MB: the release image is the [Dockerfile](../Dockerfile) (distroless) over a
  stripped build, as in [.goreleaser.yaml](../.goreleaser.yaml). With Docker running, the
  harness builds the same image as `angelia-bench:idle`, measures it (`docker image inspect`,
  uncompressed) and removes it; `BENCH_IMAGE=<ref>` measures an existing image instead, such as
  a snapshot. Without Docker the check says *not measured* and does not fail the run.

## Reports and profiles

| Run | Directory |
|---|---|
| `make bench-scale` | `bin/bench/scale-<N>/` (`bin/bench/scale-10000/`) |
| `make bench-idle` | `bin/bench/idle-<N>/` (`bin/bench/idle-100/`) |

Each holds `report.md` (verdict, checks, environment), `report.json` (the same with every
sample), `angelia.log`, and the generated `angelia.yaml` and `watches.yaml`. `bin/` is not
committed: attach `report.md` and the profiles to the story's issue or PR.

**Profiles**: `make bench-scale` runs a binary built with `-tags benchprof`
(`bin/angelia-benchprof-linux-<arch>`), and writes `cpu.pprof` (the last `BENCH_PROFILE_CPU`,
default 30s, of the run) and `heap.pprof` (after a GC at the end) next to `report.md`, which
links them. Open one with `go tool pprof -http=: bin/bench/scale-10000/cpu.pprof`.
`BENCH_PROFILE=0` turns this off; `BENCH_PROFILE=1 make bench-idle` turns it on for benchmark 2,
which then measures the size of that profiling binary.

The hook is [cmd/angelia/benchprof.go](../cmd/angelia/benchprof.go). It is compiled only with
the `benchprof` tag, so release binaries do not contain it, and it opens no port: with
`ANGELIA_BENCH_PROFILE_DIR` set, it waits for the harness to create `profile.request` in that
directory (the run's output directory, mounted into the container) and writes the profiles
there. Profiling runs in the measured window and costs a little CPU; the report says when.

## Requesting a run (agents)

Agents do not run benchmarks 1 to 3 themselves, and a result from another machine does not
count. To get one:

1. Make the harness change and merge it, or push the branch to measure.
2. Ask for the run with the exact commit, the commands and variables (as above), and what the
   result is for:
   - in a project with a coordinator, ask the coordinator: it runs the commands on the Mac
     through a Remote Control session, which the maintainer approves, and brings back
     `report.md` and the profiles;
   - otherwise comment on the story's issue, add the `human-needed` label and move it to
     **Human needed**; the maintainer runs it and attaches the files.
3. Read the report. If a target is missed, stop and report the top causes with the profiles
   before optimizing (the story's **Human needed?** section).
