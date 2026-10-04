-- +goose Up
-- EffectLedger reconciled terminal states (#947).
--
-- 'reconciled_succeeded' / 'reconciled_failed' distinguish "the provider
-- receipt arrived late and converged the unknown effect" from "the system
-- saw the receipt on the attempt". 'fenced' marks an effect the operator
-- chose to quarantine: it will never be claimed or retried again.
-- Existing rows are untouched; only the CHECK set grows.
-- SQLite cannot alter a CHECK constraint in place, so rebuild the table
-- preserving every row and index (036 added next_attempt_at; keep it).

CREATE TABLE effects_new (
    effect_id         TEXT    NOT NULL PRIMARY KEY,
    occurrence_id     TEXT    NOT NULL,
    delivery_ordinal  INTEGER NOT NULL,
    target_revision   TEXT    NOT NULL,

    attempt           INTEGER NOT NULL DEFAULT 0,

    session_id        TEXT    NOT NULL DEFAULT '',
    execution_id      TEXT    NOT NULL DEFAULT '',
    worker_run_id     TEXT    NOT NULL DEFAULT '',

    payload_id        TEXT    NOT NULL DEFAULT '',
    payload_sha256    TEXT    NOT NULL DEFAULT '',

    target_kind       TEXT    NOT NULL,
    target_ref        TEXT    NOT NULL DEFAULT '',

    status            TEXT    NOT NULL
                      CHECK(status IN ('planned', 'started', 'delivered', 'failed', 'unknown',
                                       'reconciled_succeeded', 'reconciled_failed', 'fenced')),
    error_code        TEXT    NOT NULL DEFAULT '',
    reason            TEXT    NOT NULL DEFAULT '',

    owner_instance_id TEXT    NOT NULL DEFAULT '',
    lease_until       INTEGER,
    lease_version     INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   INTEGER,

    provider_ref      TEXT    NOT NULL DEFAULT '',
    evidence_ref      TEXT    NOT NULL DEFAULT '',

    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    UNIQUE(occurrence_id, delivery_ordinal, target_revision)
);
INSERT INTO effects_new (effect_id, occurrence_id, delivery_ordinal, target_revision, attempt, session_id, execution_id, worker_run_id, payload_id, payload_sha256, target_kind, target_ref, status, error_code, reason, owner_instance_id, lease_until, lease_version, provider_ref, evidence_ref, created_at, updated_at, next_attempt_at) SELECT effect_id, occurrence_id, delivery_ordinal, target_revision, attempt, session_id, execution_id, worker_run_id, payload_id, payload_sha256, target_kind, target_ref, status, error_code, reason, owner_instance_id, lease_until, lease_version, provider_ref, evidence_ref, created_at, updated_at, next_attempt_at FROM effects;
DROP TABLE effects;
ALTER TABLE effects_new RENAME TO effects;
CREATE INDEX idx_effects_occurrence ON effects(occurrence_id, delivery_ordinal);
CREATE INDEX idx_effects_status_updated ON effects(status, updated_at);

-- +goose Down
DROP INDEX IF EXISTS idx_effects_status_updated;
DROP INDEX IF EXISTS idx_effects_occurrence;
DROP TABLE IF EXISTS effects;
