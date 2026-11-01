-- pending_release holds the versions a watch's policy keeps back until their
-- cooldown ends (story #57, ADR 0012). One row per (watch, target, version):
-- the target is part of the key so that rows a watch left on its previous
-- target never collide with the same version on its new one. The row is
-- deleted when the version is emitted or dropped (yanked meanwhile, or
-- filtered out by a changed policy), with its watch or version, and by
-- DeletePendingReleasesOfWatch when the watch moves to another target.
-- release_json is the policy engine's snapshot of the version as detected
-- (classification, URLs), opaque to the store.

-- +goose Up
CREATE TABLE pending_release (
    watch_id     TEXT    NOT NULL REFERENCES watch (id) ON DELETE CASCADE,
    target_id    TEXT    NOT NULL,
    raw          TEXT    NOT NULL,
    held_at      INTEGER NOT NULL,
    release_json TEXT    NOT NULL CHECK (json_valid(release_json)),
    PRIMARY KEY (watch_id, target_id, raw),
    FOREIGN KEY (target_id, raw) REFERENCES version (target_id, raw) ON DELETE CASCADE
) STRICT;

CREATE INDEX pending_release_target_id ON pending_release (target_id);

-- +goose Down
DROP INDEX pending_release_target_id;
DROP TABLE pending_release;
