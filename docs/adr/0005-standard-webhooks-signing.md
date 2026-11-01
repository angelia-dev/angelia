# ADR 0005: Sign webhook deliveries with Standard Webhooks

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#43

## Context

The original notifier proposal lacked signing, idempotency, templating and pull-based consumers
([architecture.md §2](../architecture.md#2-review-of-the-proposed-split), change 5). Webhook consumers must
be able to verify that a delivery comes from their Angelia instance. Signed webhooks are part of goal G4 and
requirement F7 ([prd.md §5](../prd.md#5-goals-and-non-goals), [§6](../prd.md#6-functional-requirements)).
Delivery is at-least-once with an idempotency key ([ADR 0004](0004-cloudevents-purl-event-contract.md)).

## Decision

The `webhook` sink signs every delivery following the **Standard Webhooks** specification
([architecture.md §3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver)):

- Headers: `webhook-id`, `webhook-timestamp`, `webhook-signature`.
- Signature: HMAC-SHA256, as defined by the spec.
- The default body is the CloudEvents JSON event (ADR 0004).
- Webhook secrets are encrypted at rest and never returned by the API ([architecture.md §8](../architecture.md#8-security)).

## Consequences

### Positive

- Consumers can use existing Standard Webhooks verification libraries instead of custom code.
- The timestamp header lets receivers reject stale messages.
- The id header gives receivers a stable value for de-duplication under at-least-once delivery.

### Negative

- Secrets must be generated, stored and shared with each receiver.
- Changing the signing scheme later is a breaking change for consumers.

### Open questions

These are security decisions the docs do not settle; AGENTS.md requires the maintainer to decide them.

- What `webhook-id` carries: the idempotency key (`event.id + subscription.id`), the event id, or a per-attempt id. architecture.md §3.5 names the idempotency key but does not map it to a header.
- Whether `webhook-timestamp` is refreshed on each retry attempt.
- Secret format, generation, rotation and multiple active secrets.
- Whether asymmetric signatures (also in the spec) are supported; the docs mention HMAC-SHA256 only.
- Whether other sinks (`github-dispatch`, `gitlab-pipeline-trigger`, `exec`, `nats`, `mqtt`) have any signing or rely on the target platform's auth; the docs cover webhooks only.
- Recommended timestamp tolerance for receivers.

## Alternatives considered

- **A custom HMAC scheme** (own header names and signed-string format). Every consumer would need bespoke
  verification code and could not reuse existing libraries, and there is no published spec to review.
- **No signing, only a bearer token or shared-secret header.** Authenticates the sender but does not bind the
  body or timestamp, so it neither detects tampering nor limits replay. Signed webhooks are a stated goal
  (prd.md G4, F7).

## References

- [architecture.md §2](../architecture.md#2-review-of-the-proposed-split), [§3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver), [§8](../architecture.md#8-security)
- [prd.md §5](../prd.md#5-goals-and-non-goals) (G4), [§6](../prd.md#6-functional-requirements) (F7)
- Standard Webhooks specification
