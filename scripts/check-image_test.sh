#!/usr/bin/env bash
# Offline test of check-image.sh with fake `docker` and `syft` binaries.
# Usage: scripts/check-image_test.sh
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"

# Fake docker: behaviour is driven by FAKE_SIZE, FAKE_USER and FAKE_SHELL.
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
case "$1" in
image)
	case "$*" in
	*'.Size'*) echo "${FAKE_SIZE}" ;;
	*'.Config.User'*) echo "${FAKE_USER}" ;;
	*) echo "linux/amd64" ;;
	esac
	;;
run)
	if [[ "$*" == *--entrypoint* ]]; then
		[[ "${FAKE_SHELL:-}" == yes ]] && exit 0
		echo "${FAKE_PROBE_MSG:-exec: not found: no such file or directory}" >&2
		exit "${FAKE_PROBE_RC:-127}"
	fi
	exit "${FAKE_VERSION_RC:-0}"
	;;
esac
EOF
cat >"$tmp/bin/syft" <<'EOF'
#!/usr/bin/env bash
out="${3#spdx-json=}"
echo '{"spdxVersion":"SPDX-2.3"}' >"$out"
EOF
chmod +x "$tmp/bin/docker" "$tmp/bin/syft"
export PATH="$tmp/bin:$PATH"

fail=0
expect() { # name want-exit
	local name="$1" want="$2" got=0
	"$here/check-image.sh" img "$tmp/sbom.json" >"$tmp/out" 2>&1 || got=$?
	if [[ "$got" != "$want" ]]; then
		echo "FAIL $name: exit $got, want $want"
		cat "$tmp/out"
		fail=1
	else
		echo "ok   $name"
	fi
}

FAKE_SIZE=6000000 FAKE_USER=65532:65532 FAKE_SHELL=no expect "good image" 0
FAKE_SIZE=60000000 FAKE_USER=65532:65532 FAKE_SHELL=no expect "too big" 1
FAKE_SIZE=6000000 FAKE_USER= FAKE_SHELL=no expect "no user (root)" 1
FAKE_SIZE=6000000 FAKE_USER=root FAKE_SHELL=no expect "root user" 1
FAKE_SIZE=6000000 FAKE_USER=65532 FAKE_SHELL=yes expect "has shell" 1
FAKE_SIZE=6000000 FAKE_USER=0:0 FAKE_SHELL=no expect "user 0:0" 1
FAKE_SIZE=6000000 FAKE_USER=65532 FAKE_VERSION_RC=1 expect "image does not start" 1
FAKE_SIZE=6000000 FAKE_USER=65532 FAKE_PROBE_RC=125 FAKE_PROBE_MSG="docker: daemon error" expect "probe fails for another reason" 1
FAKE_SIZE=6000000 FAKE_USER=65532 FAKE_PROBE_RC=1 FAKE_PROBE_MSG="OCI runtime: executable file not found" expect "not-found message with odd rc" 0
grep -q SPDX "$tmp/sbom.json" || { echo "FAIL: sbom not written"; fail=1; }
exit "$fail"
