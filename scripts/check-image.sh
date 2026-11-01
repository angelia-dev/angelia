#!/usr/bin/env bash
# Checks a locally available container image against the release criteria:
#   - runs as a non-root user
#   - has no shell
#   - is smaller than MAX_BYTES (default 50 MB = 50,000,000 bytes)
# and writes an SPDX JSON SBOM of the image with syft.
#
# Prints a Markdown table to stdout and, when set, to $GITHUB_STEP_SUMMARY.
#
# Usage: scripts/check-image.sh <image-ref> [sbom-output-file]
#   MAX_BYTES  size limit (default 50000000)
set -euo pipefail

image="${1:?usage: check-image.sh <image-ref> [sbom-output-file]}"
sbom="${2:-}"
max_bytes="${MAX_BYTES:-50000000}"

size="$(docker image inspect --format '{{.Size}}' "$image")"
user="$(docker image inspect --format '{{.Config.User}}' "$image")"
arch="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")"

failed=0
rows=""
row() { rows+="| $1 | $2 | $3 |"$'\n'; }

# Size.
size_mb="$(awk -v s="$size" 'BEGIN { printf "%.2f", s / 1000000 }')"
if ((size < max_bytes)); then
	row "Size ($arch)" "$size_mb MB" "ok (limit $((max_bytes / 1000000)) MB)"
else
	row "Size ($arch)" "$size_mb MB" "FAIL (limit $((max_bytes / 1000000)) MB)"
	failed=1
fi

# Non-root: a numeric or named user that is not root / uid 0.
case "${user%%:*}" in
'' | 0 | root)
	row "User" "\`${user:-<unset>}\`" "FAIL (runs as root)"
	failed=1
	;;
*) row "User" "\`$user\`" "ok (non-root)" ;;
esac

# No shell: starting one must fail.
shell_found=""
for sh in /bin/sh /busybox/sh /bin/bash; do
	if docker run --rm --entrypoint "$sh" "$image" -c true >/dev/null 2>&1; then
		shell_found="$sh"
	fi
done
if [[ -z "$shell_found" ]]; then
	row "Shell" "none" "ok"
else
	row "Shell" "\`$shell_found\`" "FAIL (image has a shell)"
	failed=1
fi

# SBOM.
if [[ -n "$sbom" ]]; then
	syft "$image" --output "spdx-json=$sbom" >/dev/null
	if grep -q '"spdxVersion"' "$sbom"; then
		row "SBOM" "\`$(basename "$sbom")\`" "ok (SPDX JSON)"
	else
		row "SBOM" "\`$(basename "$sbom")\`" "FAIL (not SPDX)"
		failed=1
	fi
fi

report="### Image check: \`$image\`"$'\n\n'"| Check | Value | Result |"$'\n'"|---|---|---|"$'\n'"$rows"
printf '%s\n' "$report"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
	printf '%s\n' "$report" >>"$GITHUB_STEP_SUMMARY"
fi
exit "$failed"
