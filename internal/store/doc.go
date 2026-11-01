// Package store defines the persistence contract for watches, versions, events
// and deliveries (docs/architecture.md §5). Engines implement [Store]: SQLite in
// package store/sqlite (Phase 0), Postgres later (Phase 2).
//
// Conventions shared by every engine:
//   - IDs are ULID strings chosen by the caller. The pull-API cursor is not
//     the event ID (ULIDs from concurrent writers can commit out of order) but
//     [Event.Seq], an integer the store assigns in commit order (ADR 0008).
//   - Foreign keys link config rows (target, watch, sink, subscription) to
//     each other and a delivery to its event. History rows (event, delivery)
//     keep the watch and subscription IDs without a foreign key, so deleting
//     a watch, subscription or sink never touches or blocks retained history.
//   - Timestamps are stored as INTEGER unix milliseconds in UTC (BIGINT on
//     Postgres) and returned as UTC [time.Time] values truncated to the
//     millisecond.
//   - JSON columns are TEXT. Fields whose shape belongs to a later story
//     (policy, filter, fetch state, event payload) are [encoding/json.RawMessage]
//     and must be valid JSON; coordinates and labels are string maps.
//     coordinates_json is canonical (keys sorted, as encoding/json writes
//     maps), so (kind, coordinates_json) is unique and identifies a target.
//   - Required timestamps and the event payload must not be zero, empty or
//     null ([ErrInvalid]).
//   - The schema is versioned with goose migrations in store/migrations/<engine>; the
//     applied versions are recorded in the schema_migrations table.
package store
