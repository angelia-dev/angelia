#!/bin/sh
# Phase 0 benchmark 1 (scale). Called by `make bench-scale`; see
# bench/load/README.md. Environment:
#   N                 watches (default 10000)
#   BENCH_CONTAINER   1 (default): angelia in Docker with --memory/--cpus;
#                     0: angelia as a plain process (Linux, no limits)
#   BENCH_DURATION    measuring time (default 15m)
#   BENCH_MEMORY      container memory limit (default 256m)
#   BENCH_CPUS        container CPU limit (default 1)
#   BENCH_OUT         output directory (default bin/bench/scale-$N)
#   BENCH_FAKEREG     fake registry behaviour (default bench/fakereg/fakereg.yaml)
set -eu

N=${N:-10000}
BENCH_CONTAINER=${BENCH_CONTAINER:-1}
BENCH_DURATION=${BENCH_DURATION:-15m}
BENCH_OUT=${BENCH_OUT:-bin/bench/scale-$N}
BENCH_FAKEREG=${BENCH_FAKEREG:-bench/fakereg/fakereg.yaml}
MAKE=${MAKE:-make}

if [ "$BENCH_CONTAINER" = 1 ]; then
	command -v docker >/dev/null || { echo "bench-scale: docker not found; use BENCH_CONTAINER=0" >&2; exit 2; }
	# The binary runs inside a Linux container of the Docker host's architecture.
	arch=$(docker version --format '{{.Server.Arch}}')
	bin=bin/angelia-linux-$arch
	GOOS=linux GOARCH=$arch $MAKE --no-print-directory build BINARY="angelia-linux-$arch"
	set -- -container=true -memory "${BENCH_MEMORY:-256m}" -cpus "${BENCH_CPUS:-1}"
else
	bin=bin/angelia
	$MAKE --no-print-directory build
	set -- -container=false
fi

exec go run ./bench/load -n "$N" -duration "$BENCH_DURATION" -angelia "$bin" \
	-out "$BENCH_OUT" -fakereg-config "$BENCH_FAKEREG" "$@"
