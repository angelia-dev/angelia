// Package wasm hosts source plugins compiled to WebAssembly, on Extism and
// wazero (pure Go, no CGO). It is the Phase 0 prototype of
// angelia-dev/angelia#59; the manifest, the exports and the host functions
// are a draft (ADR 0011).
//
// A plugin reaches the network only through the http_request host function,
// which sends every hop through the injected netguard client after checking
// it against allowed_hosts: the manifest's list plus the operator's
// (Options.AllowedHosts). A request to a host that is not allowed, or one
// netguard's SSRF guard blocks, fails the whole call even if the plugin
// ignores the error.
//
// Credentials (Options.Credentials) are added by the host to the hops to the
// host each one is bound to, over https only; the plugin never sees them
// (ADR 0011, amendment of #145).
package wasm
