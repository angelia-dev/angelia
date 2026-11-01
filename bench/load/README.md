# Benchmark 1: scale

Phase 0 benchmark 1 ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions),
story angelia-dev/angelia#62): 10,000 watches over 5 ecosystems on 1 vCPU / 256 MB, with
P95 check lag < 2 min, RSS < 100 MB and CPU < 20%.

```
make bench-scale N=10000                                     # Docker, --cpus 1 --memory 256m
BENCH_CONTAINER=0 BENCH_DURATION=1m make bench-scale N=100   # plain process, Linux only
```

## What a run does

1. Starts the [fake registry](../fakereg/README.md) in the harness process with
   `BENCH_FAKEREG` (default [fakereg.yaml](../fakereg/fakereg.yaml)), sized to `ceil(N/5)`
   packages per ecosystem.
2. Generates N watches round robin over pypi, npm, crates, github-release and oci.
3. Hands them to angelia through the **watch loader** (see below).
4. Writes an instance configuration (`angelia.yaml`, the existing format of
   [docs/configuration.md](../../docs/configuration.md)) whose `sources.<kind>.base_url` point
   at the fake registry, and starts `angelia serve`:
   - container mode (default): the Linux binary is mounted into
     `gcr.io/distroless/static-debian12:nonroot` and run with `--cpus`, `--memory` and
     `--memory-swap` (no swap); it reaches the registry at `host.docker.internal`, so the
     registry listens on all interfaces for the length of the run;
   - process mode (`BENCH_CONTAINER=0`): a child process without limits, sampled from `/proc`.
5. Waits for `/readyz`, then samples RSS and CPU every 5 s for `BENCH_DURATION` (default 15m;
   use at least 15 minutes for a meaningful lag P95).
6. Reads the registry's statistics and writes `report.json` and `report.md`, with the logs and
   config, into `BENCH_OUT` (default `bin/bench/scale-N`).

| Variable | Default | |
|---|---|---|
| `N` | `10000` | watches |
| `BENCH_CONTAINER` | `1` | `0`: plain process |
| `BENCH_DURATION` | `15m` | measuring time after `/readyz` |
| `BENCH_MEMORY`, `BENCH_CPUS` | `256m`, `1` | container limits |
| `BENCH_OUT` | `bin/bench/scale-$N` | output directory |
| `BENCH_FAKEREG` | `bench/fakereg/fakereg.yaml` | registry behaviour |

`go run ./bench/load -h` lists the underlying flags.

## Report

- **Environment**: mode, CPU model, host CPUs and memory, CPU and memory limits, OS, kernel,
  Docker version, Go version of the angelia binary (from its build info) and of the harness,
  commit (`vcs.revision` of the binary, else `git rev-parse HEAD`) and whether the tree was
  modified.
- **Checks** against the targets: check lag P95 (from the registry, see
  [fakereg](../fakereg/README.md#statistics-and-check-lag)), RSS max, CPU mean (percent of one
  CPU; in container mode as reported by `docker stats`).
- Resource summary, per-ecosystem registry counts and lag, and every sample.

Exit status: 0 pass, 1 fail, 2 error, 3 **incomplete** (a target was not measured).

## Status: incomplete until angelia can be given watches

On `main`, `angelia serve` runs stub roles and has no way to receive watches, so every run is
reported **incomplete**: RSS and CPU are those of an idle instance and check lag is not
measured. The harness keeps the missing part behind one seam, `WatchLoader` in
[plan.go](plan.go), whose only implementation (`none`) says so. What is missing:

1. **Watch loading** (#48, watch file): a way to give `angelia serve` N watches. A loader then
   writes the watch file into the output directory and returns the `serve` arguments that load
   it. The harness deliberately does not define that format.
2. **Real roles**: the scheduler and worker roles are stubs (`supervisor.NewStub`), and
   `serve` does not build the sources from `sources.<kind>.base_url` yet.
3. **Private addresses**: the netguard SSRF guard rejects loopback and private addresses. The
   fake registry is on `127.0.0.1` (process mode) or the Docker host gateway (container mode),
   so the instance needs a way to allow it (`netguard.Config.Allow` is not in the instance
   configuration yet).
4. **Sources**: crates and oci have no built-in source yet (crates exists as the WASM plugin
   prototype in `bench/plugins/crates`). The `oci` watch coordinates (`repository`) are
   provisional: ADR 0006 leaves them open.
5. **Image**: there is no Dockerfile yet, hence the mounted binary.
