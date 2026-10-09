-- +goose Up
-- Lifecycle v2 starts a separate content-minimized audit chain so its
-- configurable retention cannot shorten the legacy user_activity chain.

CREATE TABLE user_activity_v2 (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            INTEGER NOT NULL,
    user_id       TEXT    NOT NULL,
    user_id_type  TEXT    NOT NULL,
    platform      TEXT    NOT NULL,
    session_id    TEXT,
    action        TEXT    NOT NULL,
    resource_type TEXT,
    resource_id   TEXT,
    outcome       TEXT    NOT NULL,
    detail_json   TEXT    NOT NULL,
    event_ref     TEXT,
    ip            TEXT,
    user_agent    TEXT,
    prev_hash     TEXT    NOT NULL,
    self_hash     TEXT    NOT NULL,
    expires_at    INTEGER NOT NULL
);

CREATE INDEX idx_ua_v2_user_ts   ON user_activity_v2(user_id, ts);
CREATE INDEX idx_ua_v2_ts        ON user_activity_v2(ts);
CREATE INDEX idx_ua_v2_action_ts ON user_activity_v2(action, ts);
CREATE INDEX idx_ua_v2_expires_at ON user_activity_v2(expires_at);

CREATE TABLE audit_chain_checkpoints_v2 (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    pruned_at       INTEGER NOT NULL,
    last_self_hash  TEXT    NOT NULL,
    next_id         INTEGER NOT NULL
);

-- +goose StatementBegin
CREATE TRIGGER trg_ua_v2_no_update
BEFORE UPDATE ON user_activity_v2
BEGIN
    SELECT RAISE(ABORT, 'audit: rows are immutable');
END;

CREATE TRIGGER trg_ua_v2_no_delete
BEFORE DELETE ON user_activity_v2
FOR EACH ROW
BEGIN
    SELECT RAISE(ABORT, 'audit: rows are immutable except via checkpoint-anchored GC')
    WHERE NOT EXISTS (
        SELECT 1 FROM audit_chain_checkpoints_v2 WHERE next_id > OLD.id
    );
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS trg_ua_v2_no_delete;
DROP TRIGGER IF EXISTS trg_ua_v2_no_update;
DROP TABLE IF EXISTS user_activity_v2;
DROP TABLE IF EXISTS audit_chain_checkpoints_v2;
