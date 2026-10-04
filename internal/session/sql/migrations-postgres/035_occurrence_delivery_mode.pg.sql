-- +goose Up
-- Record which owner delivered (or will deliver) a firing's final answer.
--
-- This is written when the occurrence is claimed, not when a mode changes.
-- A mode switch therefore only affects the NEXT unstarted occurrence: an
-- in-flight run keeps the owner it was started under, so a job edited
-- mid-run can never end up delivered twice or by two owners.
ALTER TABLE cron_occurrences ADD COLUMN delivery_mode TEXT NOT NULL DEFAULT 'legacy_cli'
    CHECK(delivery_mode IN ('legacy_cli', 'gateway'));

-- +goose Down
ALTER TABLE cron_occurrences DROP COLUMN delivery_mode;
