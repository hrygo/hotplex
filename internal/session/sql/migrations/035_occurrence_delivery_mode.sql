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
-- SQLite cannot drop a column this way, so the table is rebuilt. Occurrence
-- identity and lifecycle are preserved; the mode falls back to its default.
CREATE TABLE cron_occurrences_without_mode (
    occurrence_id     TEXT PRIMARY KEY,
    trigger_key       TEXT NOT NULL,
    generation        INTEGER NOT NULL DEFAULT 0,
    job_id            TEXT NOT NULL,
    trigger_kind      TEXT NOT NULL CHECK(trigger_kind IN ('scheduled', 'manual', 'webhook')),
    schedule_rev      TEXT NOT NULL DEFAULT '',
    scheduled_at_ms   INTEGER NOT NULL DEFAULT 0,
    nonce             TEXT NOT NULL DEFAULT '',
    source_id         TEXT NOT NULL DEFAULT '',
    session_id        TEXT NOT NULL DEFAULT '',
    execution_id      TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL CHECK(status IN ('accepted', 'started', 'completed', 'failed', 'unknown')),
    error_code        TEXT NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    started_at        INTEGER,
    finished_at       INTEGER,
    UNIQUE(trigger_key, generation)
);
INSERT INTO cron_occurrences_without_mode
SELECT occurrence_id, trigger_key, generation, job_id, trigger_kind, schedule_rev,
       scheduled_at_ms, nonce, source_id, session_id, execution_id, status, error_code,
       created_at, updated_at, started_at, finished_at
FROM cron_occurrences;
DROP TABLE cron_occurrences;
ALTER TABLE cron_occurrences_without_mode RENAME TO cron_occurrences;
CREATE INDEX idx_cron_occurrences_job_created ON cron_occurrences(job_id, created_at);
CREATE INDEX idx_cron_occurrences_status_updated ON cron_occurrences(status, updated_at);
