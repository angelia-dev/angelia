#!/usr/bin/env bash
# Offline test of image-digest.sh against artifacts.json files shaped like
# GoReleaser v2.19 output.
# Usage: scripts/image-digest_test.sh
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

archive='{"name":"angelia_0.1.0_linux_amd64.tar.gz","path":"dist/angelia_0.1.0_linux_amd64.tar.gz","goos":"linux","goarch":"amd64","internal_type":2,"type":"Archive","extra":{"Format":"tar.gz"}}'
image() { # name digest
	printf '{"name":"%s","path":"%s","internal_type":29,"type":"Docker Image","extra":{"Digest":"%s","ID":"angelia"}}' "$1" "$1" "$2"
}
d1='sha256:7cbfedc3847c886a66c6278e29f780c8c4e2a95f8e12d6eae01ca40ccc72f1dd'
d2='sha256:0000000000000000000000000000000000000000000000000000000000000001'

fail=0
expect() { # name want-exit want-output json
	printf '%s' "$4" >"$tmp/a.json"
	set +e
	out="$("$here/image-digest.sh" "$tmp/a.json" 2>/dev/null)"
	rc=$?
	set -e
	if [[ "$rc" != "$2" || "$out" != "$3" ]]; then
		echo "FAIL $1: exit $rc (want $2), output '$out' (want '$3')"
		fail=1
	else
		echo "ok   $1"
	fi
}

expect "one image" 0 "$d1" "[$archive,$(image ghcr.io/angelia-dev/angelia:0.1.0 "$d1")]"
expect "two tags, same digest" 0 "$d1" "[$(image ghcr.io/angelia-dev/angelia:0.1.0 "$d1"),$(image ghcr.io/angelia-dev/angelia:latest "$d1")]"
expect "two digests" 1 "" "[$(image ghcr.io/angelia-dev/angelia:0.1.0 "$d1"),$(image ghcr.io/angelia-dev/angelia:0.1.0-arm64 "$d2")]"
expect "no image" 1 "" "[$archive]"
expect "image without digest (snapshot)" 1 "" "[{\"name\":\"x\",\"type\":\"Docker Image\",\"extra\":{}}]"
expect "digest not sha256" 1 "" "[$(image ghcr.io/angelia-dev/angelia:0.1.0 "abc")]"
exit "$fail"
