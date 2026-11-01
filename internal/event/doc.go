// Package event turns what a watch policy emits into immutable release
// events and their deliveries (architecture.md §2 decision 4, §4).
//
// The contract is the spec repository's release event v1
// (angelia-dev/spec, docs/release-event-v1.md): CloudEvents 1.0 in JSON
// structured mode, types dev.angelia.release.published.v1 and
// dev.angelia.release.prerelease.v1, data schema schemas/release/v1.json.
// [Build] maps a [policy.Release] to an [Envelope]: every documented field is
// present, null when it has no value; the id is a ULID generated once, so
// every retry and replay carries it; source is
// angelia://<instance.name>/watches/<watch id>; the purl follows the spec's
// table per source kind ([PURL]).
//
// [Emit] is the outbox step: inside the check's transaction it writes each
// event with one delivery per matching subscription, held when the
// subscription's cooldown says so (subscription.Policy.Evaluate). [Commit]
// runs diff, policy and Emit in one transaction, so a check is written
// entirely or not at all. Events are never updated after insert (the store
// has no update and its schema rejects one).
//
// Not built here: CloudEvents through cloudevents/sdk-go. The envelope is a
// plain struct whose encoding/json form is the structured-mode event, which
// is all the producer side needs; the SDK can come with a sink that needs
// its transports. release.yanked events come with yank notices.
package event
