# ADR 0017: The dispatcher leases deliveries through the lease table, retries with full-jitter backoff, and passes sinks one request struct

- **Status:** Accepted (2026-11-02)
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#107

ADR 0016 is reserved for #114 (message templating).

## Context

[architecture.md §3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver) asks for at-least-once
delivery, an idempotency key `event.id + subscription.id`, exponential backoff with jitter (default 8
attempts over about 24 h), then a dead state. Issue #107 adds: lease due deliveries with the scheduler's
lease mechanism ([ADR 0009](0009-scheduler-target-leases-and-backoff.md)), full jitter, concurrent
dispatchers never send one delivery in parallel, a crash mid-send leads to a redelivery after the lease
expires, and permanent errors (4xx except 408/429) go dead at once. The maintainer decided on #107 that
deliveries whose subscription or sink was deleted become dead with a reason, and that sink configuration
is stored in plaintext until encryption at rest exists.

The outbox is the `delivery` rows ([ADR 0015](0015-release-event-outbox-in-house.md)); held deliveries are
pending rows due at the end of the hold ([ADR 0014](0014-subscription-policy-overrides.md)).

## Decision

1. **Leases.** One lease per delivery being sent, named `delivery:<id>` (`store.DeliveryLeaseName`) in the
   existing `lease` table. No migration. `store.Store.LeaseDueDeliveries(holder, now, until, limit)`
   selects, in one immediate transaction, the deliveries that are due and not under an unexpired lease,
   oldest due first, takes their leases, sets them `in_flight` and increments `attempts`. Due means
   `pending` with `next_attempt_at <= now` (NULL counts as due, so a row cannot be lost), **or `in_flight`
   with no unexpired lease**: its holder crashed or stalled, so it is sent again. `HasDueDeliveries` is a
   read run first, so idle polls take no write lock. On Postgres the select becomes
   `FOR UPDATE SKIP LOCKED`, as for targets.
2. **No lease renewal; the attempt is bounded instead.** Deliveries are short calls, so a dispatcher
   does not renew its leases (unlike a scheduler check). Rendering and the sink call run under
   `DeliverTimeout` (default 20 s), which must be at most `LeaseTTL − max(30 s, LeaseTTL/4)` (`LeaseTTL`
   default 2 min). The holder gives up before its lease can expire, so two dispatchers never send a
   delivery at the same time, provided the sink honours its context and host clocks differ by less than
   that margin (at least 30 s; 100 s by default).
3. **Fenced finish.** `FinishDelivery` writes the outcome (`delivered`, `pending` with a retry time, or
   `dead`) and deletes the lease in one transaction, only while the delivery is `in_flight`, **still at
   the attempt number that was leased** (`attempts = DeliveryFinish.Attempt`), and the holder still owns
   its lease row. The row may be expired: as long as no one took it over, the result is still right and
   recording it avoids a duplicate. A holder whose delivery was taken over writes nothing; the attempt
   number also fences the **same** holder's stale attempt, when its sink ignored the context and outlived
   the lease and the holder leased the delivery again. A dispatcher keeps the set of delivery ids it is
   attempting and does not start a second attempt of one of them; that new lease simply expires and the
   delivery is due again (a duplicate is possible, a loss is not).
4. **An attempt is counted when it is leased, and poison deliveries stop.** An attempt cut short by a
   crash counts. When a delivery is leased with `attempts > MaxAttempts`, its last attempt never finished
   (the holder crashed or stalled); the dispatcher sets it dead with `giving up after N attempts (previous
   attempt did not finish)` without calling the sink, so a delivery that crashes the process cannot
   crash-loop the binary. A panic in a sink or the renderer is recovered and logged with its stack, and
   the delivery is dead with `sink panicked: <value>`. Dead deliveries stay visible and replayable.
5. **Shutdown drains.** Cancelling `Run` stops polling but does not cancel the attempts in progress
   (cancelling would turn a likely success into a likely duplicate); they end within `DeliverTimeout`,
   and the final write has 4 s. The defaults (20 s + 4 s) stay below the supervisor's 25 s drain timeout,
   so an attempt in progress at shutdown is recorded. If the process is killed first, rule 1 redelivers
   after the lease TTL.
6. **Retry schedule: full jitter.** After the n-th failed attempt the wait is uniform in
   `[0, min(base·2^(n−1), max))`, base 12 min and max 12 h. The seven wait ceilings are 12 min, 24 min,
   48 min, 1 h 36, 3 h 12, 6 h 24 and 12 h. The eighth attempt is therefore at most 24 h 36 min after the
   first, about 12 h on average. A `Retry-After` carried by the error (`sink.StatusError`) is a lower
   bound on the wait (capped at max). After `MaxAttempts` (8) failures the delivery is `dead` with
   `last_error` `giving up after 8 attempts: <error>`.
7. **Permanent errors.** A sink marks an error permanent with `sink.Permanent`. `sink.HTTPStatus(code,
   retryAfter)` classifies a response: 2xx success; **4xx except 408 and 429 permanent** (the request is
   wrong and resending cannot help); **3xx permanent** (`redirect not followed; update the sink URL`):
   sinks must not follow redirects (their client's `CheckRedirect` returns `http.ErrUseLastResponse`), a
   rule that also keeps a redirect from bypassing the SSRF guard (coordinator decision, pending the
   maintainer); 5xx, 408, 429, 1xx and any other unexpected code are retried. The dispatcher also sets a
   delivery dead without calling a sink when its subscription is deleted (`subscription deleted`), its
   sink is deleted (`sink deleted`), its event payload does not decode, or the renderer reports a
   permanent error. **An unregistered sink kind is retried** (coordinator decision): during a rolling
   upgrade an older binary may lease a delivery for a kind only the newer one has. A config decryption
   error is retried too, because the operator can fix a key.
   **Secrets in errors.** `last_error` and the logs go through `sink.RedactURL`, which cuts every URL in
   the message (and the URL of a `*url.Error`) down to scheme and host: chat webhook URLs (Slack, Discord)
   carry their secret in the path. Sinks must not put other configuration secrets in errors.
   `last_error` is capped at 1 KiB.
8. **Sink contract.** The §3.5 sketch `Deliver(ctx, e ReleaseEvent, r Rendered)` has no place for the
   sink's configuration or the idempotency key. The interface is:

   ```go
   type Sink interface {
       Kind() string
       Validate(cfg Config) error
       Deliver(ctx context.Context, req Request) (Result, error)
   }
   // Request: Config, Event (stored JSON byte for byte + decoded envelope), Rendered (body, content type),
   // IdempotencyKey, DeliveryID, SubscriptionID, Attempt.
   ```

   One kind-level instance serves every sink row of that kind (like `Source`), so the configuration comes
   with each request, read when the delivery is leased: a sink edit applies to the next attempt. A struct
   lets later fields (signing secret, trace context) be added without breaking every sink.
9. **Idempotency key** = `<event id>:<subscription id>` (`sink.IdempotencyKey`). The spec says it is a
   delivery header, not a CloudEvents attribute; its header name and the Standard Webhooks `webhook-id`
   belong to the webhook sink.
10. **Rendering seam.** `dispatch.Renderer.Render(ctx, template, envelope []byte) (sink.Rendered,
    error)`, which matches #114's `internal/render` (`Rendered{Body, ContentType, Templated}`). Until it
    lands, `DefaultRenderer` sends a copy of the stored CloudEvents JSON with
    `application/cloudevents+json; charset=utf-8` and `Templated` false, and rejects a non-empty
    template as permanent.

## Consequences

### Positive

- No schema change; works on SQLite today and maps onto `SKIP LOCKED` on Postgres.
- Crash recovery needs no sweeper: an in-flight row with an expired lease is simply due.
- Deterministic tests: clock and random source are injected.

### Negative

- Exclusion depends on sinks honouring their context and on bounded clock skew, like every time-based
  lease. Duplicates stay possible (at-least-once); consumers deduplicate on the event id or the key.
- A dead-because-deleted delivery still counts one attempt.
- A delivery whose last attempt was interrupted by a crash is dead without one more try.
- Full jitter can retry almost at once; on average the retry window is about 12 h, not 24 h.
- `delivery:` lease rows are deleted on finish or replaced on re-lease; a row is left only while its
  delivery stays in flight, so no periodic cleanup is needed.

### Maintainer decisions (2026-11-02, on #169)

1. The `Sink.Deliver(ctx, Request)` signature and the idempotency key (including its `:` separator) are approved.
2. A 3xx response is permanent: sinks do not follow redirects, which avoids SSRF through a redirect and the
   POST-to-GET switch.
3. A 401, 404 or 410 is dead on the first attempt; replay of dead deliveries comes later in Phase 1.
4. Full jitter is kept as is (mean retry span about 12 h, maximum about 24.6 h); the base is not doubled.

### Open questions

- Worker count, timeouts and retry settings are `dispatch.Options`; exposing them in `angelia.yaml`
  changes a public contract and is left to the maintainer. Wiring the role into `angelia serve` waits for
  a first sink.

## Alternatives considered

- **A `leased_by`/`lease_until` column on `delivery`.** Simpler queries, but needs a migration and
  duplicates the generic lease table (ADR 0003, ADR 0009).
- **Lease renewal as in the scheduler.** Unneeded for calls bounded at 30 s; adds a goroutine per send.
- **Counting an attempt only when it ends.** A delivery that crashes the process would retry forever.
- **Keeping the §3.5 signature and constructing one Sink per sink row.** Needs a factory per kind and a
  cache invalidated on sink edits.

## References

- [architecture.md §3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver), [§5](../architecture.md#5-data-model-simplified)
- [ADR 0009](0009-scheduler-target-leases-and-backoff.md), [ADR 0014](0014-subscription-policy-overrides.md), [ADR 0015](0015-release-event-outbox-in-house.md)
- Issue #107 and its maintainer comment.
