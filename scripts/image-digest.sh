#!/usr/bin/env bash
# Prints the digest of the image pushed by GoReleaser (dockers_v2), read from
# dist/artifacts.json. Fails unless there is exactly one unique sha256 digest.
# GoReleaser v2 serialises dockers_v2 artifacts with the type "Docker Image".
# Usage: scripts/image-digest.sh [dist/artifacts.json]
set -euo pipefail

file="${1:-dist/artifacts.json}"
digest="$(jq -r '[.[] | select(.type == "Docker Image") | .extra.Digest // empty] | unique | if length == 1 then .[0] else empty end' "$file")"
if [[ "$digest" != sha256:* ]]; then
	echo "no unique image digest in $file" >&2
	exit 1
fi
echo "$digest"
