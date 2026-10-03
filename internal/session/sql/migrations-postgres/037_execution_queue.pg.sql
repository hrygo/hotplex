-- +goose Up
-- Bounded persistent input queue (#851).
--
-- 'queued' is one more runtime fact on the canonical execution row: accepted,
-- durable, and deliberately NOT yet handed to a worker. It stays absent from
-- idx_execution_one_active_per_session, which still covers only
-- pending/running and fenced rows, so a session holding twenty queued inputs
-- presents exactly one active slot to the dispatcher.
--
-- PostgreSQL can widen a CHECK in place, so unlike SQLite this is two
-- statements and no table rebuild. The existing constraint is located by what
-- it checks rather than by the name the server happened to generate.
DO $$
DECLARE
    existing_constraint TEXT;
BEGIN
    SELECT con.conname INTO existing_constraint
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    WHERE rel.relname = 'execution_inputs'
      AND con.contype = 'c'
      AND pg_get_constraintdef(con.oid) LIKE '%runtime_status%'
    LIMIT 1;

    IF existing_constraint IS NOT NULL THEN
        EXECUTE format('ALTER TABLE execution_inputs DROP CONSTRAINT %I', existing_constraint);
    END IF;
END
$$;

ALTER TABLE "execution_inputs"
    ADD CONSTRAINT "execution_inputs_runtime_status_check"
    CHECK ("runtime_status" IN ('pending', 'queued', 'running', 'completed', 'failed', 'unknown'));

-- Scheduling state only. Delivery status, runtime status and input
-- idempotency stay on execution_inputs; duplicating them here would let two
-- copies of the truth drift apart.
CREATE TABLE execution_queue (
    execution_id       TEXT   PRIMARY KEY,
    session_id         TEXT   NOT NULL,
    -- Assigned by the database, never by a client clock: FIFO order is a
    -- database fact so two gateways cannot disagree about who is next.
    queue_seq          BIGINT NOT NULL,
    enqueued_at        BIGINT NOT NULL,
    expires_at         BIGINT NOT NULL,
    -- Bumped by /reset and session delete so a stale dispatcher holding an old
    -- revision refuses the item instead of resurrecting a previous turn.
    lifecycle_revision BIGINT NOT NULL DEFAULT 0,
    -- Opaque reference into the content domain. Never prompt text, metadata or
    -- a credential; the payload body lives behind the content store's ACL.
    payload_ref        TEXT   NOT NULL DEFAULT '',
    payload_bytes      BIGINT NOT NULL DEFAULT 0,
    FOREIGN KEY ("execution_id") REFERENCES "execution_inputs"("execution_id") ON DELETE CASCADE,
    FOREIGN KEY ("session_id")   REFERENCES "sessions"("id") ON DELETE CASCADE,
    UNIQUE ("session_id", "queue_seq")
);
CREATE INDEX IF NOT EXISTS idx_execution_queue_head ON execution_queue(session_id, queue_seq);
CREATE INDEX IF NOT EXISTS idx_execution_queue_expiry ON execution_queue(expires_at);

-- Per-session ordinal allocator. Taking the row lock here is what serialises
-- concurrent enqueues for one session across gateway instances.
CREATE TABLE execution_queue_counters (
    session_id TEXT   PRIMARY KEY,
    next_seq   BIGINT NOT NULL DEFAULT 1
);

-- Global queue budget row. It exists to be WRITTEN, not read: the enqueue path
-- touches it first, taking a row lock, which is what serialises the global
-- capacity decision across gateway instances. A lock-free COUNT would let two
-- transactions both observe "room available" and both insert.
--
-- used mirrors the current queue depth; it is refreshed by every enqueue
-- rather than maintained by each delete path, so dispatch, cancel, TTL sweep
-- and session delete cascade are all correct without any of them remembering
-- to report. This mirrors SQLite 037 exactly on purpose: one mechanism, one
-- set of semantics, no per-dialect accounting that could drift apart.
CREATE TABLE execution_queue_budget (
    budget_id BIGINT PRIMARY KEY CHECK (budget_id = 1),
    used      BIGINT NOT NULL DEFAULT 0
);
INSERT INTO execution_queue_budget (budget_id, used) VALUES (1, 0);

-- +goose Down
DROP TABLE IF EXISTS execution_queue_budget;
DROP TABLE IF EXISTS execution_queue_counters;
DROP INDEX IF EXISTS idx_execution_queue_expiry;
DROP INDEX IF EXISTS idx_execution_queue_head;
DROP TABLE IF EXISTS execution_queue;

-- Rows still sitting in 'queued' have no representation in the old CHECK, so
-- they are settled as failed rather than silently dropped: a downgrade must
-- not erase the fact that these inputs were accepted.
UPDATE "execution_inputs"
SET "status"           = 'failed',
    "error_code"       = 'QUEUE_UNSUPPORTED',
    "runtime_status"   = 'failed',
    "runtime_error_code" = 'QUEUE_UNSUPPORTED',
    "finished_at"      = CAST(EXTRACT(EPOCH FROM clock_timestamp()) * 1000 AS BIGINT),
    "updated_at"       = CAST(EXTRACT(EPOCH FROM clock_timestamp()) * 1000 AS BIGINT)
WHERE "runtime_status" = 'queued';

ALTER TABLE "execution_inputs" DROP CONSTRAINT IF EXISTS "execution_inputs_runtime_status_check";
ALTER TABLE "execution_inputs"
    ADD CONSTRAINT "execution_inputs_runtime_status_check"
    CHECK ("runtime_status" IN ('pending', 'running', 'completed', 'failed', 'unknown'));
