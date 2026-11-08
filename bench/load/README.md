# Benchmark 1: scale

Phase 0 benchmark 1 ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions),
story angelia-dev/angelia#62): 10,000 watches over 5 ecosystems on 1 vCPU / 256 MB, with
P95 **due-to-check delay** < 2 min (see below), memory < 100 MB (95.4 MiB) and CPU < 20%.

```
make bench-scale BENCH_N=10000                                   # Docker Engine on Linux, --cpus 1 --memory 256m
BENCH_ALLOW_NETWORK=192.168.65.254/32 make bench-scale BENCH_N=10000 # Docker Desktop (see below)
BENCH_CONTAINER=0 BENCH_DURATION=8m make bench-scale BENCH_N=200 # smoke (a short window): plain process, Linux only
```

`make bench-scale N=10000` works too.

## What a run does

1. Starts the [fake registry](../fakereg/README.md) in the harness process with
   `BENCH_FAKEREG` (default [fakereg.yaml](../fakereg/fakereg.yaml)). Each ecosystem gets
   exactly one package per watch, so every release belongs to a watched package; an ecosystem
   without watches (only when `BENCH_N` < 5) gets no releases.
2. Generates `BENCH_N` watches round robin over the ecosystems angelia has a built-in source
   for: pypi, npm, crates and github-release. oci has none yet, so it gets no watches and the
   report says it was **not measured**.
3. Writes them as a [watch file](../../docs/watch-file.md) (`watches.yaml`), every watch with the
   check interval `BENCH_CHECK_INTERVAL` and the default policy, so the first check of a package
   records its history as the baseline without events. There are no sinks or subscriptions.
4. Writes an instance configuration (`angelia.yaml`, the existing format of
   [docs/configuration.md](../../docs/configuration.md)) whose `sources.<kind>.base_url` point
   at the fake registry and whose `http.allow_networks` holds only the registry's address (see
   below), and starts `angelia serve --config angelia.yaml --watches watches.yaml` with all roles:
   - container mode (default): the Linux binary is mounted into
     `gcr.io/distroless/static-debian12:nonroot` and run with `--cpus`, `--memory` and
     `--memory-swap` (no swap) and the label `angelia-bench`. It reaches the registry as
     `host.docker.internal`;
   - process mode (`BENCH_CONTAINER=0`): a child process without limits, sampled from `/proc`.
     It needs Linux and fails at once elsewhere.
5. Waits for `/readyz`, then samples memory and CPU every 5 s for `BENCH_DURATION` (default
   30m).
6. Scrapes angelia's `/metrics` at the start and the end of the delay window (below) and reads
   the registry's statistics for the release lag window, then writes `report.json` and
   `report.md`, with the logs and config, into `BENCH_OUT` (default `bin/bench/scale-N`).

| Variable | Default | |
|---|---|---|
| `BENCH_N` | `10000` | watches (`N` is accepted by `make`) |
| `BENCH_CONTAINER` | `1` | `0`: plain process |
| `BENCH_DURATION` | `30m` | measuring time after `/readyz`; must exceed the warm-up plus 2 minutes (7.5m with the default interval, so at least 8m). For a meaningful release lag, keep at least 3 check intervals in its window: 22.5m with the default interval (see below) |
| `BENCH_CHECK_INTERVAL` | `5m` | per-watch check interval; sets the warm-up. The watch file accepts 5m to 720h |
| `BENCH_ALLOW_NETWORK` | picked per mode | the `http.allow_networks` entry for the registry (below) |
| `BENCH_MEMORY`, `BENCH_CPUS` | `256m`, `1` | container limits |
| `BENCH_OUT` | `bin/bench/scale-$BENCH_N` | output directory |
| `BENCH_FAKEREG` | `bench/fakereg/fakereg.yaml` | registry behaviour |

`go run ./bench/load -h` lists the underlying flags.

### What is measured: due-to-check delay

Since 2026-11-08 (maintainer decision on #62, [architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions))
the lag target is the **due-to-check delay**: how late each check starts compared with its due
time. angelia exposes it as the histogram `angelia_check_lag_seconds` (lease time minus the time
the target was due, [metrics](../../docs/operations/metrics.md)); the harness reads
`/metrics` and computes P50, P95 and P99 like PromQL's `histogram_quantile` (linear
interpolation inside the bucket). P95 below 2 minutes passes. If P95 falls above the highest
finite bucket (1 h) the report says so and the check fails.

The **delay window** runs from the first scrape after the warm-up (1.1 × the check interval:
the first round of checks is all due at once) to the last scrape at the end of the run; the
report holds the difference of the two scrapes, so only checks started in the window count. No
censoring is needed: a check's delay is known when it starts. If the metrics cannot be read,
or no check started in the window, the delay is **not measured** and the run is incomplete.
Any `BENCH_DURATION` above the minimum measures it; a longer window gives more samples.

The scheduler leases at most its worker count of targets at a time (4 by default), so a slow
fetch holds up every later check. That is part of what this benchmark shows: if the delay
grows with `BENCH_N`, look at the worker count and the rate limit (limits 2 and 3 below).

### Release lag (information)

The report still shows the time from a release's publication to its first serve at the fake
registry, per ecosystem and in total, **without pass/fail**: polling at the 5-minute minimum
interval cannot get its P95 below about 4.75 minutes. It counts only releases published
between **ready + warm-up** and **end − 2 min**:

- the warm-up is 1.1 × the check interval, so the first round of checks, which finds the
  history, does not count;
- the last 2 minutes are cut off because the run's end censors them: a release published 10 s
  before the end and not yet seen would count with a lag of 10 s. With the cut-off, every
  unseen release in the window counts with a lag of at least 2 minutes.
- the window is `BENCH_DURATION` − 1.1 × interval − 2 min long. Each package is checked once
  per interval, so a shorter window holds few releases. Keep at least 3 intervals in it
  (`BENCH_DURATION` ≥ 22.5m with the default interval) for a meaningful figure. The enforced
  minimum (8m at 5m intervals) only checks that the run works.

### Reaching the registry past the SSRF guard

The fake registry has a loopback or private address, which angelia's SSRF guard refuses. The
harness lists that one address in the instance configuration's `http.allow_networks`
([Outgoing HTTP](../../docs/configuration.md#outgoing-http-and-private-networks)), the same
opt-in an operator uses for a homelab registry; netguard itself is unchanged:

- process mode: `127.0.0.1/32`;
- container mode on Linux Docker Engine: the bridge gateway, for example `172.17.0.1/32`;
- container mode on Docker Desktop: no default. The address `host.docker.internal` has inside
  the container cannot be told from the host, so the run stops and asks for
  `BENCH_ALLOW_NETWORK` (`-allow-network`). The usual value is `192.168.65.254/32`; check it with
  `docker run --rm alpine getent hosts host.docker.internal`.
- container mode when the Linux bridge gateway cannot be found: the same; set
  `BENCH_ALLOW_NETWORK` to the address `host.docker.internal` has in the container.

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
  - due-to-check delay P95, from angelia's metrics, over the delay window; P50 and P99 are
    reported next to it;
  - memory max: **RSS (VmRSS)** in process mode, **container memory (docker stats)** in
    container mode. The latter is the cgroup's usage minus inactive page cache, which is close
    to, but not the same as, the process RSS;
  - CPU mean (in container mode as reported by `docker stats`).
- Resource summary, the due-to-check delay (samples, P50, P95, P99), the release lag as
  information (from the [fakereg](../fakereg/README.md#statistics-and-check-lag) statistics),
  per-ecosystem registry counts and every sample.

Exit status: 0 pass, 1 fail, 2 error, 3 **incomplete** (a target was not measured, for example no check started in the delay window).
Incomplete is deliberately non-zero, so `make bench-scale` fails: a run that measured nothing
must not look green.

## Known limits

What a run on `main` measures, and what it cannot yet:

1. **oci is not measured**: there is no built-in oci source, so the fake registry's oci
   endpoint gets no watches and the report says so. The other four ecosystems share the
   watches.
2. **One host for every ecosystem**: all sources reach the fake registry on one address, so
   netguard's per-host rate limit (the fallback limit: 5 requests per second, burst 5) applies
   to all four ecosystems together, where the real registries are four hosts. With the default
   interval of 5 minutes, about 1,500 watches fit in that budget; above that, checks queue in
   the rate limiter and the due-to-check delay grows with `BENCH_N`: the request counts in the report's fake
   registry table then add up to about 5 per second. Real registries would each get their own
   budget.
3. **Interval and release lag**: a watch's interval is at least 5 minutes (the watch file's
   minimum), and periodic polling finds a release on average half an interval after it is
   published, so the release lag cannot meet 2 minutes with 5-minute intervals. That is why the
   target is the due-to-check delay and the release lag is information only.
4. **crates without timestamps**: with a custom index the crates source reads no API, so its
   versions have no publication time. The release lag is measured at the registry and does not need
   them.
5. **Image**: the harness mounts the binary into a distroless base image instead of building
   the repository's Dockerfile, so a run needs no image build.
