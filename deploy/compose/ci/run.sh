#!/usr/bin/env bash
# Quickstart check: the SQLite compose example, a fake PyPI and a webhook receiver.
#
#   deploy/compose/ci/run.sh
#
# Needs docker with the compose plugin, Go (the version of go.mod), curl and jq.
# It builds the image from this checkout, starts the stack, waits for the first check
# (the baseline), has the fake registry publish a version, and waits for the
# release event at the receiver. It prints the logs on failure and always runs
# `docker compose down -v`. Exit 0 means the event arrived.
#
#   ANGELIA_IMAGE   image to test; when unset, it is built from this checkout as angelia:ci
#   AWAIT_TIMEOUT   how long to wait for the event (default 9m: the shortest watch
#                   interval is 5m, plus 10% jitter, plus delivery)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
project=angelia-quickstart-ci
await_timeout="${AWAIT_TIMEOUT:-9m}"

compose() {
	docker compose -p "$project" -f "$here/../sqlite/compose.yaml" -f "$here/compose.override.yaml" "$@"
}

ctx=""
cleanup() {
	status=$?
	[[ -z "$ctx" ]] || rm -rf "$ctx"
	[[ -z "${bootstrap_dir:-}" ]] || rm -rf "$bootstrap_dir"
	if ((status != 0)); then
		echo "::group::compose ps"
		compose ps -a || true
		echo "::endgroup::"
		echo "::group::compose logs"
		compose logs --no-color --timestamps || true
		echo "::endgroup::"
		echo "quickstart check FAILED (exit $status)" >&2
	fi
	compose down -v --remove-orphans || true
	exit "$status"
}
trap cleanup EXIT

step() { echo "==> $*"; }

step "tool versions"
docker compose version
docker version --format '{{.Server.Version}}'

# 1. Build: the test doubles, and the image unless one is given.
step "build ciharness"
(cd "$root" && CGO_ENABLED=0 go build -trimpath -o "$here/bin/ciharness" ./deploy/compose/ci)

if [[ -z "${ANGELIA_IMAGE:-}" ]]; then
	step "build image angelia:ci"
	ctx="$(mktemp -d)"
	arch="$(go env GOARCH)"
	mkdir -p "$ctx/linux/$arch"
	(cd "$root" && CGO_ENABLED=0 GOOS=linux go build -trimpath -o "$ctx/linux/$arch/angelia" ./cmd/angelia)
	docker build -q --build-arg "TARGETPLATFORM=linux/$arch" -f "$root/Dockerfile" -t angelia:ci "$ctx"
	export ANGELIA_IMAGE=angelia:ci
fi

# A fresh bootstrap admin token (ADR 0030 T4), the compose file's secret. The container
# runs as uid 65532, not as this user, so the file must be readable by others.
bootstrap_dir="$(mktemp -d)"
ANGELIA_BOOTSTRAP_TOKEN_FILE="$bootstrap_dir/bootstrap-token"
export ANGELIA_BOOTSTRAP_TOKEN_FILE
docker run --rm "$ANGELIA_IMAGE" token generate >"$ANGELIA_BOOTSTRAP_TOKEN_FILE"
chmod 0644 "$ANGELIA_BOOTSTRAP_TOKEN_FILE"

# A fresh signing secret for this run, shared by the sender and the receiver.
CI_WEBHOOK_SECRET="whsec_$(head -c 32 /dev/urandom | base64 -w0)"
export CI_WEBHOOK_SECRET

# 2. Start the stack.
step "compose up"
compose up -d --wait --wait-timeout 120

step "wait for the fake registry"
for _ in $(seq 1 30); do
	curl -fsS -o /dev/null http://127.0.0.1:8099/pypi/pypi/pkg-00000/json && break
	sleep 1
done
curl -fsS -o /dev/null http://127.0.0.1:8099/pypi/pypi/pkg-00000/json

# The API needs a token: none is refused, the bootstrap token works (the token is not echoed).
step "check the bootstrap token"
none="$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/v1/watches)"
bearer="$(curl -s -o /dev/null -w '%{http_code}' -K - http://127.0.0.1:8080/v1/watches <<<"header = \"Authorization: Bearer $(cat "$ANGELIA_BOOTSTRAP_TOKEN_FILE")\"")"
if [[ "$none" != 401 || "$bearer" != 200 ]]; then
	echo "GET /v1/watches: $none without a token (want 401), $bearer with the bootstrap token (want 200)" >&2
	exit 1
fi

# 3. The first check records the versions known at start (baseline) and emits nothing.
step "wait for the baseline"
deadline=$((SECONDS + 120))
while :; do
	# Captured first: `grep -q` in a pipe can exit early and fail the pipeline with SIGPIPE.
	logs="$(compose logs angelia 2>&1 || true)"
	[[ "$logs" == *'baseline recorded'* ]] && break
	if ((SECONDS > deadline)); then
		echo "no 'baseline recorded' in the angelia log within 120s" >&2
		exit 1
	fi
	sleep 1
done

# 4. A new version appears at the registry; the next check finds it.
step "publish a version"
version="$(curl -fsS -X POST http://127.0.0.1:8099/_ci/publish | jq -er .version)"
echo "published $version; waiting up to $await_timeout for the next check"

# 5. The receiver gets a verified release event for it.
step "await the event"
"$here/bin/ciharness" await -receiver http://127.0.0.1:9000 -version "$version" -timeout "$await_timeout"
echo "quickstart check passed"
