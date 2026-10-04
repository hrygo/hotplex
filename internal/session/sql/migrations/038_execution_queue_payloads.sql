-- +goose Up
-- Content for durably queued inputs.
--
-- execution_queue schedules; it must not carry the prompt. This table holds
-- the bounded content a queued input needs in order to be dispatched after the
-- client that sent it is long gone, and nothing else: no metadata values, no
-- credentials, no raw worker errors.
--
-- A queued Skill stores its invocation rather than the raw slash text, so a
-- queued native command keeps its identity, arguments and materialization
-- instead of being re-read as an ordinary prompt that happens to start with a
-- slash.
--
-- The foreign key is the whole point. Content is deleted exactly when its
-- queue row is — dispatch, cancel, TTL expiry or session delete — so the
-- retention rule can never keep (or drop) content independently of whether the
-- control fact still promises a recoverable input. An input whose content is
-- gone is not silently "still queued".
CREATE TABLE execution_queue_payloads (
    payload_id      TEXT    PRIMARY KEY,
    execution_id    TEXT    NOT NULL,
    session_id      TEXT    NOT NULL,
    -- Resolved text to deliver. Empty for a native command invocation.
    content         TEXT    NOT NULL DEFAULT '',
    -- Bounded JSON of the native command invocation, or ''. Never free-form
    -- metadata: only the four fields that decide whether the item can still be
    -- dispatched as a Skill.
    invocation_json TEXT    NOT NULL DEFAULT '',
    content_bytes   INTEGER NOT NULL,
    content_sha256  TEXT    NOT NULL,
    created_at      INTEGER NOT NULL,
    FOREIGN KEY(execution_id) REFERENCES execution_queue(execution_id) ON DELETE CASCADE
);
CREATE INDEX idx_execution_queue_payloads_session
    ON execution_queue_payloads(session_id);

-- +goose Down
DROP INDEX IF EXISTS idx_execution_queue_payloads_session;
DROP TABLE IF EXISTS execution_queue_payloads;
