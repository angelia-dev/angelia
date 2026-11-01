#!/usr/bin/env bash
# Installs the pinned release tooling (goreleaser, syft, cosign) into GOBIN.
#
# They are built from source with `go install` at an exact version, so the module
# proxy and checksum database verify them and no third-party GitHub Action is
# needed to fetch binaries. Bump the versions here; Renovate does not track them.
#
# Usage: scripts/install-release-tools.sh [tool ...]   (default: all three)
set -euo pipefail

GORELEASER_VERSION="v2.19.0"
SYFT_VERSION="v1.55.0"
COSIGN_VERSION="v3.1.4"

declare -A pkg=(
	[goreleaser]="github.com/goreleaser/goreleaser/v2@${GORELEASER_VERSION}"
	[syft]="github.com/anchore/syft/cmd/syft@${SYFT_VERSION}"
	[cosign]="github.com/sigstore/cosign/v3/cmd/cosign@${COSIGN_VERSION}"
)

tools=("$@")
if ((${#tools[@]} == 0)); then
	tools=(goreleaser syft cosign)
fi

for t in "${tools[@]}"; do
	if [[ -z "${pkg[$t]:-}" ]]; then
		echo "install-release-tools: unknown tool '$t'" >&2
		exit 2
	fi
	echo "installing $t (${pkg[$t]})"
	CGO_ENABLED=0 go install "${pkg[$t]}"
done
