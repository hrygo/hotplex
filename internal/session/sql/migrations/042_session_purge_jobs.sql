-- +goose Up
CREATE TABLE session_purge_jobs (
    id                TEXT PRIMARY KEY,
    session_id        TEXT NOT NULL UNIQUE,
    user_id           TEXT NOT NULL,
    owner_id          TEXT NOT NULL DEFAULT '',
    workspace_id      TEXT NOT NULL DEFAULT '',
    policy_revision   TEXT NOT NULL DEFAULT 'legacy',
    status            TEXT NOT NULL CHECK(status IN ('pending', 'in_progress', 'blocked', 'complete')),
    requested_at      TIMESTAMP NOT NULL,
    completed_at      TIMESTAMP NULL,
    created_at        TIMESTAMP NOT NULL,
    updated_at        TIMESTAMP NOT NULL
);
CREATE INDEX idx_session_purge_jobs_user
    ON session_purge_jobs(user_id, requested_at);

CREATE TABLE session_purge_items (
    id                TEXT PRIMARY KEY,
    job_id            TEXT NOT NULL,
    session_id        TEXT NOT NULL,
    kind              TEXT NOT NULL,
    status            TEXT NOT NULL CHECK(status IN ('pending', 'running', 'retrying', 'blocked', 'unsupported', 'complete')),
    attempts          INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   TIMESTAMP NOT NULL,
    lease_until       TIMESTAMP NULL,
    lease_token       TEXT NULL,
    completed_at      TIMESTAMP NULL,
    error_code        TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMP NOT NULL,
    updated_at        TIMESTAMP NOT NULL,
    UNIQUE(job_id, kind)
);
CREATE INDEX idx_session_purge_items_due
    ON session_purge_items(kind, status, next_attempt_at, lease_until);
CREATE INDEX idx_session_purge_items_session
    ON session_purge_items(session_id, kind);

-- +goose Down
DROP INDEX IF EXISTS idx_session_purge_items_session;
DROP INDEX IF EXISTS idx_session_purge_items_due;
DROP TABLE IF EXISTS session_purge_items;
DROP INDEX IF EXISTS idx_session_purge_jobs_user;
DROP TABLE IF EXISTS session_purge_jobs;
