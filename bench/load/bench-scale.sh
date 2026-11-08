#!/bin/sh
# Phase 0 benchmark 1 (scale). Called by `make bench-scale`; see
# bench/load/README.md. Environment:
#   BENCH_N           watches (default 10000)
#   BENCH_CONTAINER   1 (default): angelia in Docker with --memory/--cpus;
#                     0: angelia as a plain process (Linux, no limits)
#   BENCH_DURATION    measuring time after angelia is ready (default 30m)
#   BENCH_CHECK_INTERVAL  per-watch check interval (default 5m, the watch file minimum)
#   BENCH_ALLOW_NETWORK   CIDR angelia may dial for the fake registry (default: picked per mode)
#   BENCH_MEMORY      container memory limit (default 256m)
#   BENCH_CPUS        container CPU limit (default 1)
#   BENCH_OUT         output directory (default bin/bench/scale-$BENCH_N)
#   BENCH_FAKEREG     fake registry behaviour (default bench/fakereg/fakereg.yaml)
#   BENCH_HOST_RPS    rate limit of the fake registry host, http.host_limits (default 50;
#                     5 is angelia's default limit)
#   BENCH_HOST_BURST  its burst (default ceil(BENCH_HOST_RPS))
set -eu

BENCH_N=${BENCH_N:-10000}
BENCH_CONTAINER=${BENCH_CONTAINER:-1}
BENCH_DURATION=${BENCH_DURATION:-30m}
BENCH_CHECK_INTERVAL=${BENCH_CHECK_INTERVAL:-5m}
BENCH_OUT=${BENCH_OUT:-bin/bench/scale-$BENCH_N}
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

if [ -n "${BENCH_HOST_BURST:-}" ]; then
	set -- "$@" -host-burst "$BENCH_HOST_BURST"
fi

if [ -n "${BENCH_ALLOW_NETWORK:-}" ]; then
	set -- "$@" -allow-network "$BENCH_ALLOW_NETWORK"
fi

exec go run ./bench/load -n "$BENCH_N" -duration "$BENCH_DURATION" -check-interval "$BENCH_CHECK_INTERVAL" \
	-angelia "$bin" -out "$BENCH_OUT" -fakereg-config "$BENCH_FAKEREG" -host-rps "${BENCH_HOST_RPS:-50}" "$@"
