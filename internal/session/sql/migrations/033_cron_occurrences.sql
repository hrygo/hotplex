-- +goose Up
-- Durable identity for one logical cron firing. The occurrence is recorded
-- before any Worker starts, so a crash between accept and dispatch never loses
-- the fact that the trigger was already taken.
--
-- Content-free by design: this table carries identity and lifecycle only. The
-- prompt body, worker output and provider payloads never land here.
CREATE TABLE cron_occurrences (
    occurrence_id     TEXT PRIMARY KEY,
    -- Stable business key. A repeated trigger with the same key and generation
    -- resolves to the existing occurrence instead of starting a second run.
    trigger_key       TEXT NOT NULL,
    generation        INTEGER NOT NULL DEFAULT 0,
    job_id            TEXT NOT NULL,
    trigger_kind      TEXT NOT NULL CHECK(trigger_kind IN ('scheduled', 'manual', 'webhook')),
    -- Revision fingerprint of the schedule at the time of the firing, so an
    -- edited schedule cannot collide with a pre-edit occurrence.
    schedule_rev      TEXT NOT NULL DEFAULT '',
    -- Scheduled UTC instant in Unix ms; 0 for non-scheduled triggers.
    scheduled_at_ms   INTEGER NOT NULL DEFAULT 0,
    -- Manual request nonce; empty for other kinds.
    nonce             TEXT NOT NULL DEFAULT '',
    -- Verified webhook source/event ID; empty for other kinds.
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
CREATE INDEX idx_cron_occurrences_job_created ON cron_occurrences(job_id, created_at);
CREATE INDEX idx_cron_occurrences_status_updated ON cron_occurrences(status, updated_at);

-- +goose Down
DROP INDEX IF EXISTS idx_cron_occurrences_status_updated;
DROP INDEX IF EXISTS idx_cron_occurrences_job_created;
DROP TABLE IF EXISTS cron_occurrences;
