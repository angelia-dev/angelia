// Package wasm hosts source plugins compiled to WebAssembly, on Extism and
// wazero (pure Go, no CGO). It is the Phase 0 prototype of
// angelia-dev/angelia#59; the manifest, the exports and the host functions
// are a draft (ADR 0011).
//
// A plugin reaches the network only through the http_request host function,
// which sends every hop through the injected netguard client after checking
// it against the manifest's allowed_hosts. A request to a host that is not
// allowed, or one netguard's SSRF guard blocks, fails the whole call even if
// the plugin ignores the error.
package wasm
