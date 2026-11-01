// Package store defines the persistence contract for watches, versions, events
// and deliveries (docs/architecture.md §5). Engines implement [Store]: SQLite in
// package store/sqlite (Phase 0), Postgres later (Phase 2).
//
// Conventions shared by every engine:
//   - IDs are ULID strings chosen by the caller; event IDs double as the
//     monotonic cursor of the pull API.
//   - Timestamps are stored as INTEGER unix milliseconds in UTC and returned
//     as UTC [time.Time] values truncated to the millisecond.
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
