#!/usr/bin/env bash
# Builds the WASM plugins of benchmark 4 (angelia-dev/angelia#59) and copies
# them next to their sources, where bench/plugins/plugins.go embeds them.
#
# Reproducible: the toolchain is pinned by rust-toolchain.toml, dependencies by
# Cargo.lock (--locked), and the build paths are remapped so that the output
# does not depend on where the repository or the cargo home live.
#
# Usage: bench/plugins/build.sh          build and update the .wasm files
#        bench/plugins/build.sh --check  build and fail if they differ from
#                                        the committed ones
#
# Needs rustup (it installs the pinned toolchain and the
# wasm32-unknown-unknown target on first use) and network access to
# crates.io for the first build.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

target="wasm32-unknown-unknown"
cargo_home="${CARGO_HOME:-$HOME/.cargo}"
build_dir="$(mktemp -d)"
trap 'rm -rf "$build_dir"' EXIT

export CARGO_TARGET_DIR="$build_dir"
export SOURCE_DATE_EPOCH=0
export RUSTFLAGS="--remap-path-prefix=$here=/angelia/bench/plugins --remap-path-prefix=$cargo_home=/cargo --remap-path-prefix=$build_dir=/target"
unset CARGO_BUILD_RUSTFLAGS CARGO_ENCODED_RUSTFLAGS

cargo build --locked --release --target "$target" -p angelia-plugin-crates -p angelia-plugin-hostile

status=0
for p in crates hostile; do
	built="$build_dir/$target/release/angelia_plugin_${p}.wasm"
	dest="$here/$p/$p.wasm"
	if [[ "${1:-}" == "--check" ]]; then
		if cmp -s "$built" "$dest"; then
			echo "$dest: up to date ($(sha256sum "$dest" | cut -d' ' -f1))"
		else
			echo "$dest: differs from a fresh build" >&2
			status=1
		fi
	else
		cp "$built" "$dest"
		chmod 0644 "$dest"
		sha256sum "$dest" | sed "s|$here/||" > "$dest.sha256"
		echo "$dest: $(cut -d' ' -f1 "$dest.sha256")"
	fi
done
exit "$status"
