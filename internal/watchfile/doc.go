// Package watchfile loads the declarative watch file (docs/watch-file.md): the
// sinks, watches and subscriptions an instance manages until the REST API
// exists. Parse validates a file strictly and reports every problem with its
// path and line; Plan and Apply reconcile a valid file into the store.
//
// The file is a public contract from Phase 1 (angelia apply). Key names and
// semantics are fixed by the document, not by this package.
package watchfile
