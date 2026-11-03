// Package goplugin is the hashicorp/go-plugin prototype of the third-party
// plugin spike (angelia-dev/angelia#60, ADR 0013): the core side (Source, a
// source.Source backed by a plugin subprocess, and the Host service through
// which the plugin sends HTTP) and the plugin side (Serve). The crates.io
// plugin is in ./crates, its binary in ./cmd/angelia-plugin-crates.
//
// The plugin talks gRPC to the core and dials back through go-plugin's
// GRPCBroker for HTTP, so allowed_hosts, netguard and the request budget
// apply as in the WASM host. Nothing under internal/ or cmd/ imports this
// package: go-plugin is a spike-only dependency (architecture.md §6).
package goplugin
