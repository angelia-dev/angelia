#!/bin/sh
# Phase 0 benchmarks 1 (scale), 2 (idle) and 3 (github). Called by
# `make bench-scale`, `make bench-idle` and `make bench-github`; see
# bench/README.md and bench/load/README.md.
# Environment:
#   BENCH_KIND        scale (default), idle or github
#   BENCH_N           watches (default 10000; github: 2000)
#   BENCH_CONTAINER   1 (default): angelia in Docker with --memory/--cpus;
#                     0: angelia as a plain process (Linux, no limits)
#   BENCH_DURATION    measuring time after angelia is ready (default 30m, idle 20m)
#   BENCH_CHECK_INTERVAL  per-watch check interval (default 5m, the watch file minimum)
#   BENCH_ALLOW_NETWORK   CIDRs, comma-separated, angelia may dial for the fake registry
#                     (default: picked per mode, or asked of a container)
#   BENCH_MEMORY      container memory limit (default 256m)
#   BENCH_CPUS        container CPU limit (default 1)
#   BENCH_OUT         output directory (default bin/bench/$BENCH_KIND-$BENCH_N)
#   BENCH_PROFILE     1 (default for scale): run a -tags benchprof build and write
#                     cpu.pprof and heap.pprof next to the report; 0 (default for idle): off
#   BENCH_PROFILE_CPU length of the CPU profile, ending with the run (default 30s)
#   BENCH_IMAGE       idle: image whose size is checked (default: built from the
#                     Dockerfile like the release image when Docker runs; else not measured).
#                     Benchmark 2 checks the size of a binary built with the release flags
#                     (-trimpath -ldflags "-s -w", .goreleaser.yaml), never the benchprof one.
#   BENCH_FAKEREG     fake registry behaviour (default bench/fakereg/fakereg.yaml)
#   BENCH_HOST_RPS    rate limit of the fake registry host, http.host_limits (default 50;
#                     5 is angelia's default limit)
#   BENCH_HOST_BURST  its burst (default ceil(BENCH_HOST_RPS))
# Benchmark 3 (github; defaults: 24h, interval 1h, the watch file default):
#   ANGELIA_BENCH_GITHUB_TOKEN  the token (live run); read by the harness only,
#                     never written or printed
#   BENCH_GITHUB_REPOS  watch list (default bench/github/repos.txt)
#   BENCH_GITHUB_FAKE   1: dry run against the fake registry's github endpoint
#                     (BENCH_FAKEREG default bench/fakereg/github.yaml, host limit
#                     GitHub's 5,000/h: BENCH_HOST_RPS 1.3888889, burst 10)
set -eu

BENCH_KIND=${BENCH_KIND:-scale}
n=10000 interval=5m fakereg=bench/fakereg/fakereg.yaml rps=50 burst= out=$BENCH_KIND
case $BENCH_KIND in
scale) profile=1 duration=30m ;;
idle) profile=0 duration=20m ;;
github)
	profile=0 duration=24h n=2000 interval=1h
	if [ "${BENCH_GITHUB_FAKE:-0}" = 1 ]; then
		fakereg=bench/fakereg/github.yaml rps=1.3888889 burst=10 out=github-fake
	fi
	;;
*) echo "bench: BENCH_KIND must be scale, idle or github" >&2; exit 2 ;;
esac
BENCH_N=${BENCH_N:-$n}
BENCH_CONTAINER=${BENCH_CONTAINER:-1}
BENCH_DURATION=${BENCH_DURATION:-$duration}
BENCH_CHECK_INTERVAL=${BENCH_CHECK_INTERVAL:-$interval}
BENCH_OUT=${BENCH_OUT:-bin/bench/$out-$BENCH_N}
BENCH_FAKEREG=${BENCH_FAKEREG:-$fakereg}
BENCH_HOST_RPS=${BENCH_HOST_RPS:-$rps}
BENCH_HOST_BURST=${BENCH_HOST_BURST:-$burst}
BENCH_PROFILE=${BENCH_PROFILE:-$profile}
MAKE=${MAKE:-make}

ctx= built_image=
cleanup() {
	[ -z "$ctx" ] || rm -rf "$ctx"
	[ -z "$built_image" ] || docker rmi "$built_image" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# add_tag GOFLAGS TAG: GOFLAGS with TAG added to its -tags=, or a new -tags=TAG.
add_tag() {
	out= found=
	for f in $1; do
		case $f in -tags=*) f="$f,$2" found=1 ;; esac
		out="${out:+$out }$f"
	done
	[ -n "$found" ] || out="${out:+$out }-tags=$2"
	printf '%s\n' "$out"
}

# The profiling hook is compiled in only with -tags benchprof
# (cmd/angelia/benchprof.go), into a binary of its own name.
base_goflags=${GOFLAGS:-} goflags=${GOFLAGS:-} suffix=
set --
if [ "$BENCH_PROFILE" = 1 ]; then
	suffix=-benchprof
	goflags=$(add_tag "$goflags" benchprof)
	set -- -profile -profile-cpu "${BENCH_PROFILE_CPU:-30s}"
fi

if [ "$BENCH_CONTAINER" = 1 ]; then
	command -v docker >/dev/null || { echo "bench: docker not found; use BENCH_CONTAINER=0" >&2; exit 2; }
	# The binary runs inside a Linux container of the Docker host's architecture.
	arch=$(docker version --format '{{.Server.Arch}}')
	goos=linux goarch=$arch
	bin=bin/angelia$suffix-linux-$arch
	GOFLAGS=$goflags GOOS=linux GOARCH=$arch $MAKE --no-print-directory build BINARY="angelia$suffix-linux-$arch"
	set -- "$@" -container=true -memory "${BENCH_MEMORY:-256m}" -cpus "${BENCH_CPUS:-1}"
else
	goos=$(go env GOOS) goarch=$(go env GOARCH)
	bin=bin/angelia$suffix
	GOFLAGS=$goflags $MAKE --no-print-directory build BINARY="angelia$suffix"
	set -- "$@" -container=false
fi

if [ -n "${BENCH_HOST_BURST:-}" ]; then
	set -- "$@" -host-burst "$BENCH_HOST_BURST"
fi

if [ "$BENCH_KIND" = github ]; then
	[ "${BENCH_GITHUB_FAKE:-0}" != 1 ] || set -- "$@" -github-fake
	set -- "$@" -github-repos "${BENCH_GITHUB_REPOS:-bench/github/repos.txt}"
fi

if [ -n "${BENCH_ALLOW_NETWORK:-}" ]; then
	set -- "$@" -allow-network "$BENCH_ALLOW_NETWORK"
fi

# Benchmark 2 checks the binary and image as released (.goreleaser.yaml):
# a -trimpath -ldflags "-s -w" build for the target, and the Dockerfile over
# it, built here unless BENCH_IMAGE names an image.
if [ "$BENCH_KIND" = idle ]; then
	release=bin/angelia-release-$goos-$goarch
	GOFLAGS=$base_goflags CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -ldflags "-s -w" -o "$release" ./cmd/angelia
	set -- "$@" -measure-binary "$release"
	image=${BENCH_IMAGE:-}
	if [ -z "$image" ] && command -v docker >/dev/null && docker info >/dev/null 2>&1; then
		darch=$(docker version --format '{{.Server.Arch}}')
		ctx=$BENCH_OUT/image-context
		rm -rf "$ctx"
		mkdir -p "$ctx/linux/$darch"
		if [ "$goos/$goarch" = "linux/$darch" ]; then
			cp "$release" "$ctx/linux/$darch/angelia"
		else
			GOFLAGS=$base_goflags CGO_ENABLED=0 GOOS=linux GOARCH=$darch go build -trimpath -ldflags "-s -w" -o "$ctx/linux/$darch/angelia" ./cmd/angelia
		fi
		image=angelia-bench:idle
		built_image=$image
		docker build -q --platform "linux/$darch" -f Dockerfile -t "$image" "$ctx" >/dev/null
		rm -rf "$ctx"
		ctx=
	fi
	if [ -n "$image" ]; then
		set -- "$@" -measure-image "$image"
	fi
fi

go run ./bench/load -benchmark "$BENCH_KIND" -n "$BENCH_N" -duration "$BENCH_DURATION" -check-interval "$BENCH_CHECK_INTERVAL" \
	-angelia "$bin" -out "$BENCH_OUT" -fakereg-config "$BENCH_FAKEREG" -host-rps "$BENCH_HOST_RPS" "$@"
