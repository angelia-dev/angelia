-- removed_at records when a known version stopped being listed upstream
-- (story #56). NULL means it was listed at the last check. The diff engine
-- keeps the row (the known set never shrinks), so a removal is reported once
-- and a version that comes back is not seen as new.

-- +goose Up
ALTER TABLE version ADD COLUMN removed_at INTEGER;

-- +goose Down
ALTER TABLE version DROP COLUMN removed_at;
