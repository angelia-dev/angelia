// Package plugins embeds the prebuilt WASM plugins of benchmark 4
// (angelia-dev/angelia#59) and their draft manifests. The Rust sources sit
// next to them; build.sh rebuilds the .wasm files reproducibly and
// `build.sh --check` verifies the committed ones.
//
// Only tests and benchmarks import this package.
package plugins

import _ "embed" // for go:embed

// The embedded files are strings, which are immutable; the accessors return
// fresh copies.
var (
	//go:embed crates/crates.wasm
	cratesWasm string
	//go:embed crates/manifest.json
	cratesManifest string
	//go:embed hostile/hostile.wasm
	hostileWasm string
	//go:embed hostile/manifest.json
	hostileManifest string
)

// CratesWasm returns the crates.io source plugin (crates/src/lib.rs).
func CratesWasm() []byte { return []byte(cratesWasm) }

// CratesManifest returns the crates plugin's draft manifest.
func CratesManifest() []byte { return []byte(cratesManifest) }

// HostileWasm returns a misbehaving plugin for the host's deny-path tests
// (hostile/src/lib.rs).
func HostileWasm() []byte { return []byte(hostileWasm) }

// HostileManifest returns the hostile plugin's draft manifest.
func HostileManifest() []byte { return []byte(hostileManifest) }
