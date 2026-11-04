# Benchmark 1: scale

Phase 0 benchmark 1 ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions),
story angelia-dev/angelia#62): 10,000 watches over 5 ecosystems on 1 vCPU / 256 MB, with
P95 check lag < 2 min, memory < 100 MB (95.4 MiB) and CPU < 20%.

```
make bench-scale BENCH_N=10000                                   # Docker, --cpus 1 --memory 256m
BENCH_CONTAINER=0 BENCH_DURATION=4m make bench-scale BENCH_N=100 # plain process, Linux only
```

`make bench-scale N=10000` works too.

## What a run does

1. Starts the [fake registry](../fakereg/README.md) in the harness process with
   `BENCH_FAKEREG` (default [fakereg.yaml](../fakereg/fakereg.yaml)). Each ecosystem gets
   exactly one package per watch, so every release belongs to a watched package; an ecosystem
   without watches (only when `BENCH_N` < 5) gets no releases.
2. Generates `BENCH_N` watches round robin over pypi, npm, crates, github-release and oci.
3. Hands them to angelia through the **watch loader** (see below), which asks for a check
   interval of `BENCH_CHECK_INTERVAL`.
4. Writes an instance configuration (`angelia.yaml`, the existing format of
   [docs/configuration.md](../../docs/configuration.md)) whose `sources.<kind>.base_url` point
   at the fake registry, and starts `angelia serve`:
   - container mode (default): the Linux binary is mounted into
     `gcr.io/distroless/static-debian12:nonroot` and run with `--cpus`, `--memory` and
     `--memory-swap` (no swap) and the label `angelia-bench`. It reaches the registry as
     `host.docker.internal`;
   - process mode (`BENCH_CONTAINER=0`): a child process without limits, sampled from `/proc`.
     It needs Linux and fails at once elsewhere.
5. Waits for `/readyz`, then samples memory and CPU every 5 s for `BENCH_DURATION` (default
   15m).
6. Reads the registry's statistics for the lag window (below) and writes `report.json` and
   `report.md`, with the logs and config, into `BENCH_OUT` (default `bin/bench/scale-N`).

| Variable | Default | |
|---|---|---|
| `BENCH_N` | `10000` | watches (`N` is accepted by `make`) |
| `BENCH_CONTAINER` | `1` | `0`: plain process |
| `BENCH_DURATION` | `15m` | measuring time after `/readyz`; must exceed the warm-up plus 2 minutes |
| `BENCH_CHECK_INTERVAL` | `1m` | per-watch check interval; sets the warm-up |
| `BENCH_MEMORY`, `BENCH_CPUS` | `256m`, `1` | container limits |
| `BENCH_OUT` | `bin/bench/scale-$BENCH_N` | output directory |
| `BENCH_FAKEREG` | `bench/fakereg/fakereg.yaml` | registry behaviour |

`go run ./bench/load -h` lists the underlying flags.

### Lag window

Check lag counts only releases published between **ready + warm-up** and **end − 2 min**:

- the warm-up is 1.1 × the check interval (one interval plus the scheduler's 10% jitter), so
  the first round of checks, which finds the history, does not count;
- the last 2 minutes are cut off because the run's end censors them: a release published 10 s
  before the end and not yet seen would count with a lag of 10 s. With the cut-off, every
  unseen release in the window counts with a lag of at least 2 minutes, so it fails the
  target instead of flattering it.

### Where the registry listens (container mode)

`host.docker.internal` is the host's loopback on Docker Desktop (macOS, Windows, Desktop for
Linux), so the registry listens on `127.0.0.1` there. On Linux Docker Engine it is the bridge
gateway (`--add-host host.docker.internal:host-gateway`), so the registry listens on that
address (`docker network inspect bridge`, usually `172.17.0.1`). Only when the gateway cannot
be found does it listen on all interfaces, with a warning. With `ufw` or another firewall that
drops input from `docker0`, the container cannot reach the registry: allow it for the run
(`sudo ufw allow in on docker0`) and remove the rule afterwards.

Run one benchmark per Docker host at a time: a run removes every container labelled
`angelia-bench` when it starts and stops.

## Report

- **Environment**: mode, CPU model, host CPUs and memory, CPU and memory limits, OS, kernel,
  Docker version and the CPUs and memory of the Docker VM (`docker info`), Go version of the
  angelia binary (from its build info) and of the harness, commit (`vcs.revision` of the
  binary, else `git rev-parse HEAD`) and whether the tree was modified.
- **Checks** against the targets, all in MiB and percent of one CPU:
  - check lag P95, from the registry (see
    [fakereg](../fakereg/README.md#statistics-and-check-lag)), over the lag window;
  - memory max: **RSS (VmRSS)** in process mode, **container memory (docker stats)** in
    container mode. The latter is the cgroup's usage minus inactive page cache, which is close
    to, but not the same as, the process RSS;
  - CPU mean (in container mode as reported by `docker stats`).
- Resource summary, per-ecosystem registry counts and lag, and every sample.

Exit status: 0 pass, 1 fail, 2 error, 3 **incomplete** (a target was not measured).
Incomplete is deliberately non-zero, so `make bench-scale` fails: a run that measured nothing
must not look green.

## Status: incomplete until angelia can be given watches

On `main`, `angelia serve` runs stub roles and has no way to receive watches, so every run is
reported **incomplete**: memory and CPU are those of an idle instance and check lag is not
measured. The harness keeps the missing part behind one seam, `WatchLoader` in
[plan.go](plan.go), whose only implementation (`none`) says so. What is missing:

1. **Watch loading** (#48, watch file): a way to give `angelia serve` N watches with a check
   interval. A loader then writes the watch file into the output directory and returns the
   `serve` arguments that load it. The harness deliberately does not define that format.
2. **Real roles**: the scheduler and worker roles are stubs (`supervisor.NewStub`), and
   `serve` does not build the sources from `sources.<kind>.base_url` yet.
3. **Private addresses**: the netguard SSRF guard rejects loopback and private addresses. The
   fake registry is on `127.0.0.1` or the Docker bridge gateway, so the instance needs a way
   to allow it (`netguard.Config.Allow` is not in the instance configuration yet).
4. **Sources**: crates and oci have no built-in source yet (crates exists as the WASM plugin
   prototype in `bench/plugins/crates`). The `oci` watch coordinates (`repository`) are
   provisional: ADR 0006 leaves them open.
5. **Image**: there is no Dockerfile yet, hence the mounted binary.
