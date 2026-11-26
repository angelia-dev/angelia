// Package oci is the source of image tags of an OCI registry (Docker Hub,
// GHCR, Quay, Harbor, any registry implementing the OCI Distribution API).
// Each tag is a version.
//
// The coordinate is "image", the repository with its registry host:
// "ghcr.io/org/app", or "node" for docker.io/library/node (canonical form,
// ADR 0037). List reads GET /v2/<name>/tags/list?n=1000 and follows the Link
// header. A 401 with a Bearer challenge makes it fetch an anonymous token
// from the challenge's realm and retry once; the token lives for one List
// call.
//
// Every request goes through the injected client (the netguard client in
// production). The source sends no credential of its own.
package oci
