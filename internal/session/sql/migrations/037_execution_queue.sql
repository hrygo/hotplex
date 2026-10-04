-- +goose Up
-- Bounded persistent input queue (#851).
--
-- 'queued' is one more runtime fact on the canonical execution row: accepted,
-- durable, and deliberately NOT yet handed to a worker. It is deliberately
-- absent from every existing index that gates activity --
-- idx_execution_one_active_per_session still covers only pending/running and
-- fenced rows -- so a session holding twenty queued inputs still presents
-- exactly one active slot to the dispatcher.
--
-- SQLite cannot widen a CHECK constraint in place, so execution_inputs is
-- rebuilt with the same columns, data, indexes and foreign key. Only the
-- runtime_status CHECK changes.
CREATE TABLE execution_inputs_rebuild (
    execution_id        TEXT PRIMARY KEY,
    session_id          TEXT    NOT NULL,
    client_message_id   TEXT    NOT NULL,
    payload_hash        TEXT    NOT NULL,
    status              TEXT    NOT NULL CHECK(status IN ('accepted', 'delivered', 'unknown', 'failed')),
    error_code          TEXT    NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    delivered_at        INTEGER,
    owner_instance_id   TEXT    NOT NULL DEFAULT '',
    worker_run_id       TEXT    NOT NULL DEFAULT '',
    lease_until         INTEGER NOT NULL DEFAULT 0,
    runtime_status      TEXT    NOT NULL DEFAULT 'unknown'
                        CHECK(runtime_status IN ('pending', 'queued', 'running', 'completed', 'failed', 'unknown')),
    runtime_error_code  TEXT    NOT NULL DEFAULT '',
    started_at          INTEGER,
    finished_at         INTEGER,
    fence_reason        TEXT    NOT NULL DEFAULT '',
    fence_version       INTEGER NOT NULL DEFAULT 0,
    fence_created_at    INTEGER,
    UNIQUE(session_id, client_message_id),
    FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

INSERT INTO execution_inputs_rebuild (
    execution_id, session_id, client_message_id, payload_hash, status,
    error_code, created_at, updated_at, delivered_at,
    owner_instance_id, worker_run_id, lease_until,
    runtime_status, runtime_error_code, started_at, finished_at, fence_reason,
    fence_version, fence_created_at
)
SELECT
    execution_id, session_id, client_message_id, payload_hash, status,
    error_code, created_at, updated_at, delivered_at,
    owner_instance_id, worker_run_id, lease_until,
    runtime_status, runtime_error_code, started_at, finished_at, fence_reason,
    fence_version, fence_created_at
FROM execution_inputs;

DROP TABLE execution_inputs;
ALTER TABLE execution_inputs_rebuild RENAME TO execution_inputs;

CREATE INDEX idx_execution_inputs_session_created ON execution_inputs(session_id, created_at);
CREATE INDEX idx_execution_inputs_status_updated ON execution_inputs(status, updated_at);
CREATE INDEX idx_execution_owner_runtime
    ON execution_inputs(owner_instance_id, runtime_status, lease_until);

CREATE UNIQUE INDEX idx_execution_one_active_per_session
    ON execution_inputs(session_id)
    WHERE runtime_status IN ('pending', 'running') OR fence_reason <> '';

CREATE INDEX idx_execution_fenced
    ON execution_inputs(fence_created_at)
    WHERE fence_reason <> '';

-- Scheduling state only. Delivery status, runtime status and input
-- idempotency stay on execution_inputs; duplicating them here would let two
-- copies of the truth drift apart.
CREATE TABLE execution_queue (
    execution_id       TEXT    PRIMARY KEY,
    session_id         TEXT    NOT NULL,
    -- Assigned by the database, never by a client clock: FIFO order is a
    -- database fact so two gateways cannot disagree about who is next.
    queue_seq          INTEGER NOT NULL,
    enqueued_at        INTEGER NOT NULL,
    expires_at         INTEGER NOT NULL,
    -- Bumped by /reset and session delete so a stale dispatcher holding an old
    -- revision refuses the item instead of resurrecting a previous turn.
    lifecycle_revision INTEGER NOT NULL DEFAULT 0,
    -- Opaque reference into the content domain. Never prompt text, metadata or
    -- a credential; the payload body lives behind the content store's ACL.
    payload_ref        TEXT    NOT NULL DEFAULT '',
    payload_bytes      INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY(execution_id) REFERENCES execution_inputs(execution_id) ON DELETE CASCADE,
    FOREIGN KEY(session_id)   REFERENCES sessions(id) ON DELETE CASCADE,
    UNIQUE(session_id, queue_seq)
);
CREATE INDEX idx_execution_queue_head ON execution_queue(session_id, queue_seq);
CREATE INDEX idx_execution_queue_expiry ON execution_queue(expires_at);

-- Per-session ordinal allocator. Taking the row lock here is what serialises
-- concurrent enqueues for one session across gateway instances.
CREATE TABLE execution_queue_counters (
    session_id TEXT    PRIMARY KEY,
    next_seq   INTEGER NOT NULL DEFAULT 1
);

-- Global queue budget row. It exists to be WRITTEN, not read: the enqueue path
-- touches it first, which is what serialises the global capacity decision
-- across gateway instances. A lock-free COUNT would let two transactions both
-- observe "room available" and both insert.
--
-- used mirrors the current queue depth; it is refreshed by every enqueue
-- rather than maintained by each delete path. The plan proposed a conditional
-- increment paired with per-path decrements, and the decrement is where that
-- design breaks: SQLite does not fire row triggers for every row an
-- ON DELETE CASCADE removes, and a decrement living in application code would
-- be skipped by whichever future path forgets it -- a leaked slot is a queue
-- that silently refuses everything forever. Recounting here makes dispatch,
-- cancel, TTL sweep and session delete cascade all correct for free, and a
-- stale value can never refuse an enqueue.
CREATE TABLE execution_queue_budget (
    budget_id INTEGER PRIMARY KEY CHECK(budget_id = 1),
    used      INTEGER NOT NULL DEFAULT 0
);
INSERT INTO execution_queue_budget (budget_id, used) VALUES (1, 0);

-- +goose Down
DROP TABLE IF EXISTS execution_queue_budget;
DROP TABLE IF EXISTS execution_queue_counters;
DROP INDEX IF EXISTS idx_execution_queue_expiry;
DROP INDEX IF EXISTS idx_execution_queue_head;
DROP TABLE IF EXISTS execution_queue;

-- Rebuild execution_inputs to restore the pre-037 CHECK. Rows still sitting in
-- 'queued' have no representation in the old schema, so they are settled as
-- failed rather than silently dropped: a downgrade must not erase the fact
-- that these inputs were accepted.
CREATE TABLE execution_inputs_rollback (
    execution_id        TEXT PRIMARY KEY,
    session_id          TEXT    NOT NULL,
    client_message_id   TEXT    NOT NULL,
    payload_hash        TEXT    NOT NULL,
    status              TEXT    NOT NULL CHECK(status IN ('accepted', 'delivered', 'unknown', 'failed')),
    error_code          TEXT    NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    delivered_at        INTEGER,
    owner_instance_id   TEXT    NOT NULL DEFAULT '',
    worker_run_id       TEXT    NOT NULL DEFAULT '',
    lease_until         INTEGER NOT NULL DEFAULT 0,
    runtime_status      TEXT    NOT NULL DEFAULT 'unknown'
                        CHECK(runtime_status IN ('pending', 'running', 'completed', 'failed', 'unknown')),
    runtime_error_code  TEXT    NOT NULL DEFAULT '',
    started_at          INTEGER,
    finished_at         INTEGER,
    fence_reason        TEXT    NOT NULL DEFAULT '',
    fence_version       INTEGER NOT NULL DEFAULT 0,
    fence_created_at    INTEGER,
    UNIQUE(session_id, client_message_id),
    FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

INSERT INTO execution_inputs_rollback (
    execution_id, session_id, client_message_id, payload_hash, status,
    error_code, created_at, updated_at, delivered_at,
    owner_instance_id, worker_run_id, lease_until,
    runtime_status, runtime_error_code, started_at, finished_at, fence_reason,
    fence_version, fence_created_at
)
SELECT
    execution_id, session_id, client_message_id, payload_hash, status,
    error_code, created_at, updated_at, delivered_at,
    owner_instance_id, worker_run_id, lease_until,
    CASE WHEN runtime_status = 'queued' THEN 'failed' ELSE runtime_status END,
    CASE WHEN runtime_status = 'queued' THEN 'QUEUE_UNSUPPORTED' ELSE runtime_error_code END,
    started_at,
    CASE WHEN runtime_status = 'queued'
         THEN CAST(strftime('%s','now') AS INTEGER) * 1000
         ELSE finished_at END,
    fence_reason, fence_version, fence_created_at
FROM execution_inputs
WHERE runtime_status <> 'queued';

UPDATE execution_inputs_rollback
SET status = 'failed'
WHERE status = 'accepted';

DROP TABLE execution_inputs;
ALTER TABLE execution_inputs_rollback RENAME TO execution_inputs;

CREATE INDEX idx_execution_inputs_session_created ON execution_inputs(session_id, created_at);
CREATE INDEX idx_execution_inputs_status_updated ON execution_inputs(status, updated_at);
CREATE INDEX idx_execution_owner_runtime
    ON execution_inputs(owner_instance_id, runtime_status, lease_until);

CREATE UNIQUE INDEX idx_execution_one_active_per_session
    ON execution_inputs(session_id)
    WHERE runtime_status IN ('pending', 'running') OR fence_reason <> '';

CREATE INDEX idx_execution_fenced
    ON execution_inputs(fence_created_at)
    WHERE fence_reason <> '';
