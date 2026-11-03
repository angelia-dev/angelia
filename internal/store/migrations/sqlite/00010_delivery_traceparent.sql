-- Trace context of the check that created a delivery (story #118, ADR 0020).
-- Additive only: existing rows get NULL and no row is rewritten.
--
-- delivery.traceparent is the W3C traceparent of the span active when the
-- delivery row was written. The dispatcher, possibly in another process and
-- long after the check, starts its delivery spans with a link to it, so one
-- check trace is connected to every delivery it caused. It holds ids only,
-- never a secret. NULL when tracing was off or the delivery predates this.
--
-- TODO(postgres, #85): the Postgres engine needs the same column (TEXT).

-- +goose Up
ALTER TABLE delivery ADD COLUMN traceparent TEXT;

-- +goose Down
ALTER TABLE delivery DROP COLUMN traceparent;
