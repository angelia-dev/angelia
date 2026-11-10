# Benchmarks

The Phase 0 benchmarks of [architecture.md §6](../docs/architecture.md#6-language-and-framework-phase-0-assumptions):

| # | What | Run with | Where |
|---|---|---|---|
| 1 | Scale: 10,000 watches on 1 vCPU / 256 MB | `make bench-scale` | [load](load/README.md), the benchmark machine |
| 2 | Idle footprint: 100 watches; memory, binary, image | `make bench-idle` | [load](load/README.md), the benchmark machine |
| 3 | GitHub: 2,000 repos on one token, 24 h | `make bench-github` | [below](#benchmark-3-github-on-one-token), the benchmark machine |
| 4 | WASM plugin overhead | `go test ./bench/wasm` | anywhere |
| 5 | Kill -9 during dispatch: 100 SIGKILLs, zero lost deliveries | `make bench-crash` | [crash](crash/README.md), anywhere on Linux; [result](results/benchmark-5-kill9-2026-11-09.md) |
| 6 | Version ordering | `make bench-ordering` | [ordering](ordering/README.md), anywhere |

Benchmarks 1 to 3 measure time and resources, so their reported results come from one known
machine, H-bench-env. The others are deterministic and run anywhere, CI included.

## The benchmark machine (H-bench-env)

Decided by the maintainer on 2026-11-09 (angelia-dev/angelia#38):

- **Host**: the maintainer's (Marco's) Mac with OrbStack as the Docker engine. The report
  records the CPU model, memory, macOS version, Docker version and engine (`docker info`'s
  OperatingSystem, "OrbStack") and the CPUs and memory of the engine's VM.
- **Container mode**: angelia runs in a Linux container limited with `--cpus 1 --memory 256m
  --memory-swap 256m` (1 vCPU, 256 MB, no swap), the harness's default. The harness and the
  fake registry run on the Mac, outside the limits. Process mode (`BENCH_CONTAINER=0`) needs
  Linux and is for smoke runs only.
- **Network**: nothing to set. On a Mac the harness asks a throwaway container which addresses
  `host.docker.internal` has (OrbStack: usually `0.250.250.254`; Docker Desktop:
  `192.168.65.254`) and allows exactly those; `BENCH_ALLOW_NETWORK` overrides it. See
  [load/README.md](load/README.md#reaching-the-registry-past-the-ssrf-guard).
- **GitHub token** (benchmark 3): the runner's environment holds `ANGELIA_BENCH_GITHUB_TOKEN`, a
  token with public read-only access. It is never committed, written to a report or pasted
  into an issue.
- Tools: Go (the version in `go.mod`), `make`, `git`, OrbStack. Close other heavy
  applications during a run and run one benchmark at a time.

## Running benchmarks 1 and 2

From a clean checkout of the commit to measure:

```
make bench-scale    # benchmark 1: 10,000 watches, 30 minutes after ready, with profiles
make bench-idle     # benchmark 2: 100 watches, 20 minutes after ready
```

Each command builds what it measures, runs once and prints the verdict. Exit status: 0 pass,
1 fail, 2 error, 3 incomplete (a target not measured). Variables and what a run does:
[load/README.md](load/README.md).

**Benchmark 2** checks, in MB as in the architecture (1 MB = 10⁶ bytes):

- idle memory < 40 MB: the highest memory sample after the warm-up (1.1 × the check interval:
  every watch has been checked once), container memory from `docker stats`. The report also
  gives the memory at the start and the end of that window, to show a trend;
- binary < 40 MB: a build with the release flags of [.goreleaser.yaml](../.goreleaser.yaml)
  (`-trimpath -ldflags "-s -w"`, `bin/angelia-release-<os>-<arch>`) for the target, Linux in
  container mode. The `make build` binary that ran (not stripped) is reported as information;
  the benchprof binary is never measured;
- image < 50 MB, uncompressed (`docker image inspect`, the stricter reading); the compressed
  size (`docker save | gzip`) is information. The release image is the
  [Dockerfile](../Dockerfile) (distroless) over the release-flag build. With Docker running, the
  harness builds the same image as `angelia-bench:idle`, measures it and removes it;
  `BENCH_IMAGE=<ref>` measures an existing image instead, such as a snapshot. Without Docker the
  check says *not measured* and does not fail the run.

## Benchmark 3: GitHub on one token

Target (story angelia-dev/angelia#64, [architecture.md §6](../docs/architecture.md#6-language-and-framework-phase-0-assumptions)):
2,000 real public repositories at the default interval for 24 hours on one token, with **zero 403
and zero 429** responses from `api.github.com`, and charged requests under the token's hourly limit.
The ETag hit rate after the first cycle is reported as information against the original 90% goal:
the first live run (2026-11-09, 6.5 h) measured 60% with the budget well inside the limit, and the
maintainer chose to judge the benchmark by the budget and to raise the hit rate in a follow-up story.

```
export ANGELIA_BENCH_GITHUB_TOKEN=…        # public read-only; see "The benchmark machine"
caffeinate -i make bench-github            # 2,000 repos, 24 h, container (1 vCPU, 256 MB)
BENCH_DURATION=3h make bench-github N=200  # a smoke run: a short window, fewer repos
BENCH_GITHUB_FAKE=1 BENCH_CONTAINER=0 BENCH_CHECK_INTERVAL=5m BENCH_DURATION=15m BENCH_HOST_RPS=10 \
  make bench-github                        # the dry run against the fake registry (no token, no network)
```

Keep the Mac awake for the whole day: `caffeinate -i` holds off idle sleep while the command runs (a
sleeping Mac pauses OrbStack and the run). `BENCH_DURATION` must be above the warm-up plus one check
interval, 2.1 × the interval (more than 2h6m at 1 hour, 10m30s at 5 minutes), so that one full round
of checks follows the first cycle. A transient failure to sample or to scrape is logged and the run
goes on; 10 in a row, or Ctrl-C (SIGINT, SIGTERM), end it with a partial report whose verdict is
**incomplete** (exit status 3), holding the counters up to then.

What a run does (the [load harness](load/README.md) with `-benchmark github`):

1. Reads the token from `ANGELIA_BENCH_GITHUB_TOKEN` and keeps it in memory. angelia gets it in its
   environment as `BENCH_GITHUB_TOKEN` (a `token_env` may not start with `ANGELIA_`), passed to the
   container with `docker run -e BENCH_GITHUB_TOKEN` from the `docker` command's environment, so it is
   on no command line. `angelia.yaml` names the variable (`sources.github-release.token_env`), never
   the token; nothing in the output directory and no log line holds it. While the container exists,
   `docker inspect` shows its environment, the token included, to anyone who can use the Docker
   socket; the run removes the container at its end, and a run that was killed leaves it for the next
   run, which removes every `angelia-bench` container first (or `docker rm -f` it by hand).
2. Calls `GET /rate_limit` (free, see below). If more than 1% of the token's hourly budget is already
   used, it waits for the window to reset (`-github-wait-budget=false` skips that), so that what
   another tool spent is not blamed on angelia.
3. Writes one `github-release` watch per repository of the watch list
   (`bench/github/repos.txt`, [below](#the-watch-list), the first `BENCH_N`) **without an interval**, so the watch
   file default (1 hour) applies, and an instance configuration with only that source: default base
   URL, no `allow_networks` and no `host_limits` (api.github.com is public and its built-in budget
   applies: 4,990/h plus a burst of 10, so at most 5,000 in any hour). There is no fake registry.
4. Starts angelia (container mode with the limits of benchmark 1, or `BENCH_CONTAINER=0`), samples
   memory and CPU every 5 s, and every minute scrapes `/metrics` and reads `GET /rate_limit`.
5. Writes `report.md` and `report.json` to `bin/bench/github-<N>/` (dry run: `github-fake-<N>/`).

`BENCH_DURATION` (default `24h`), `BENCH_N` (default 2000), `BENCH_CHECK_INTERVAL` (default `1h`, the
watch file default; anything else is not the benchmark) and `BENCH_GITHUB_REPOS` (default
`bench/github/repos.txt`) change the run. Exit status as for the others; a run shorter than 24 hours
says so in the report and is a smoke run, not a result for #64.

**What is measured**, from angelia's own metrics ([metrics](../docs/operations/metrics.md)):

| Check | Source | Target |
|---|---|---|
| 403 responses | `angelia_netguard_responses_total{host="api.github.com",class="403"}`, whole run | 0 |
| 429 responses | same, `class="429"` | 0 |
| ETag hit rate after the first cycle | `angelia_source_fetches_total{source="github-release"}`: `not_modified / (ok + not_modified)`, from the end of the first cycle to the end of the run | > 90% (information) |
| Requests charged to the token, busiest 60 minutes | every response but 304 (2xx, 3xx, 403, 4xx, 429, 5xx), from the per-minute scrapes | < the token's hourly limit: `limit` of `GET /rate_limit` (core), 5,000 if it cannot be read |
| Rate limit remaining, low-water mark (optional) | GitHub's `GET /rate_limit`, core, sampled every minute | > 0 |

**The first cycle** ends at the first scrape after the warm-up (1.1 × interval) at which the
successful checks (`ok` + `not_modified`) reach the number of watches. The counters cannot tell
targets apart, so this is a count: a target checked twice could stand in for one not checked yet
(failed checks are not counted, so a repository that answers 404 delays it until others make up for it).
With every target due at once at the start and one interval between checks, that is rare, and the
window is long (about 22 hours) next to it.

Each attempt is counted (retries and redirect hops too); the report also gives the counts per class
over the whole run and after the first cycle, the checks by result, the share of 304 among 2xx + 304
responses, server pauses and deadline refusals. The `403` class of `angelia_netguard_responses_total`
was added for this benchmark; before it, a 403 was part of `4xx`. Other 4xx (a deleted or renamed-away
repository answers 404) are reported, not failed.

### Budget math

GitHub's primary limit for a token is 5,000 requests per hour, shared by everything the token's user
runs ([rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api):
"All of these requests count towards your personal rate limit of 5,000 requests per hour"). Both
limits are signalled with 403 or 429 ("If you exceed your primary rate limit, you will receive a `403`
or `429` response", and the same for the secondary limits).

**Conditional requests that answer 304 do not count**, when authenticated: "Making a conditional
request does not count against your primary rate limit if a `304` response is returned and the request
was made while correctly authorized with an `Authorization` header"
([best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api#use-conditional-requests-if-appropriate)).
The `github-release` source sends the first page's ETag as `If-None-Match` with an `Authorization:
Bearer` header, so an unchanged repository costs nothing. `GET /rate_limit` is free too: "Calling this
endpoint does not count against your primary rate limit, but it can count against your secondary rate
limit" (rate limits page); the harness calls it once a minute.

Steady state, with *R* repositories at interval *I* (seconds), *p* requests per check and an ETag hit
rate *h*:

- requests sent per hour = *R* × 3600 / *I* × *p* = 2,000 × 3600 / 3600 × 1 = **2,000/h**;
- requests charged per hour = *R* × 3600 / *I* × (1 − *h*) × *p′*, where *p′* is the requests of a
  check that finds a change: 1, or 2 when the repository has 100 releases or more (a page holding a
  new release makes the source read the next page): at *h* = 0.9, 2,000 × 0.1 × 2 ≤ **400/h, 8% of
  5,000**. Even with no 304 at all, about 2,000 to 4,000 charged per hour stays under 5,000.
- angelia's own limiter for api.github.com (4,990/h, 1.386 rps, burst 10: at most 5,000 in any hour) takes a token for every request, 304
  included, so it caps *sent* requests at 5,000/h whatever the hit rate: 2,000 watches at 1 hour use
  40% of it, and checks never queue for it in steady state (`angelia_host_demand_rps` shows 0.56).
  The ETags matter for GitHub's budget, not angelia's: they are what lets *R* grow, or *I* shrink,
  up to the limiter's 5,000 sends per hour (for example 2,000 repositories every 25 minutes).

Two phases cost more:

- **The first cycle**: every target is due at once, and its first check is a full scan with no ETag:
  ceil(releases / 100) pages, at most 10. The watch list's header gives that total (*P* pages, from
  the release counts the generator saw). All *P* are charged, paced at 1.386 rps, so the first cycle
  takes about *P* / 1.386 seconds (for *P* = 3,000, 36 minutes). If *P* came near 5,000, the first hour
  could meet the limit: angelia's bucket allows exactly 5,000 in any hour (burst 10 plus 4,990 an hour),
  all of GitHub's budget, which is why the harness starts on a fresh window.
- **The daily rescan**: once every 24 hours, each repository's check ignores its ETag and walks all
  pages ([githubrelease](../internal/source/githubrelease/doc.go)), *P* pages a day, spread like the first
  cycle. In a 24-hour run they start at its end, so a run sees at most the first of them; they cost
  about *P* / 24 ≈ 125/h on average for *P* = 3,000.

What can miss the target: a release listing whose ETag changes without a new release. The listing
carries counters (each asset's `download_count`, reactions); if GitHub's ETag follows them, a popular
repository with downloads answers 200 on most checks and the hit rate drops while the budget still
holds (2,000/h at worst). The report's checks by result and the 2xx/304 counts show it; if it happens,
stop and report before changing the source (ADR needed for the `Source` contract). This is what
the first live run showed (60%); the follow-up story tracks it.

### The watch list

`bench/github/repos.txt` holds the repositories, one `owner/name` per line, with a header that says
when and how it was made. It is generated with a token, so the first run on the benchmark machine
starts with `make bench-github-repos` if the file is not committed yet. `make bench-github-repos` rebuilds it
([genrepos](github/genrepos/main.go), needs `ANGELIA_BENCH_GITHUB_TOKEN`): GitHub search, sorted by
stars, forks excluded, in four buckets, written round robin (one of each bucket in turn, so the first
`BENCH_N` lines are a mix too) (35% very popular, ≥ 20,000 stars; 30% active, 2,000 to
19,999 stars, pushed in the last 90 days; 20% quiet, same stars, not pushed for a year, archived
included; 15% smaller, 300 to 1,999 stars, pushed in the last 180 days), keeping a candidate only if
`GET /repos/{repo}/releases?per_page=1` lists a release; a bucket that runs short passes the rest to the
next. On a rate limit (429, or 403 with `X-RateLimit-Remaining: 0`) it waits for `Retry-After` or
`X-RateLimit-Reset` and goes on, logging the remaining budget every 200 requests; on any other error
it stops, exits non-zero and writes what it found to `bench/github/repos.txt.partial` (marked
`INCOMPLETE`), leaving the list itself as it was. It sends about 40 searches and one request per candidate (about 3,000), so it uses most of an
hour's budget: run it an hour before the benchmark, or let the harness wait. Commit the result: the
list is public data.

## Reports and profiles

| Run | Directory |
|---|---|
| `make bench-scale` | `bin/bench/scale-<N>/` (`bin/bench/scale-10000/`) |
| `make bench-idle` | `bin/bench/idle-<N>/` (`bin/bench/idle-100/`) |
| `make bench-github` | `bin/bench/github-<N>/`; dry run `bin/bench/github-fake-<N>/` |
| `make bench-crash` | `bin/bench/crash/`; the committed result is in [results/](results/) |

Each holds `report.md` (verdict, checks, environment), `report.json` (the same with every
sample), `angelia.log`, and the generated `angelia.yaml` and `watches.yaml`. `bin/` is not
committed: attach `report.md` and the profiles to the story's issue or PR.

**Profiles**: `make bench-scale` runs a binary built with `-tags benchprof`
(`bin/angelia-benchprof-linux-<arch>`), and writes `cpu.pprof` (the last `BENCH_PROFILE_CPU`,
default 30s, of the run) and `heap.pprof` (after a GC at the end) next to `report.md`, which
links them. Open one with `go tool pprof -http=: bin/bench/scale-10000/cpu.pprof`.
`BENCH_PROFILE=0` turns this off; `BENCH_PROFILE=1 make bench-idle` turns it on for benchmark 2
(the binary check still measures the release-flag build).

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
