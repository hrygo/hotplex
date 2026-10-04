-- +goose Up
-- External-delivery effect ledger.
--
-- Two tables, deliberately separate:
--   * effect_payloads holds the bounded final assistant content snapshot.
--   * effects holds only identity, lifecycle and REFERENCES to that payload.
--
-- The snapshot and the planned effect row are written in ONE transaction, so
-- an effect can never be observable as planned while its content is missing.
-- Nothing here stores credentials, raw provider requests or tool arguments:
-- the target is a projection of already-authorized job config, and the
-- adapter resolves credentials at send time.

CREATE TABLE effect_payloads (
    payload_id      TEXT PRIMARY KEY,
    occurrence_id   TEXT    NOT NULL,
    execution_id    TEXT    NOT NULL DEFAULT '',
    worker_run_id   TEXT    NOT NULL DEFAULT '',
    -- Bounded publishable content only (final assistant text).
    content         TEXT    NOT NULL,
    content_bytes   BIGINT  NOT NULL,
    content_sha256  TEXT    NOT NULL,
    created_at      BIGINT  NOT NULL,
    UNIQUE(occurrence_id, execution_id)
);
CREATE INDEX IF NOT EXISTS idx_effect_payloads_occurrence ON effect_payloads(occurrence_id);

CREATE TABLE effects (
    effect_id         TEXT    PRIMARY KEY,
    occurrence_id     TEXT    NOT NULL,
    -- Logical delivery ordinal within the occurrence (0-based). Together with
    -- occurrence_id and target_revision this is the business key.
    delivery_ordinal  BIGINT  NOT NULL,
    -- Revision of the delivery target's configuration. Editing the target
    -- produces a NEW effect rather than mutating an in-flight one.
    target_revision   TEXT    NOT NULL,
    -- Attempt is deliberately NOT part of the business key: a retry is a new
    -- attempt on the same effect, never a second effect.
    attempt           BIGINT  NOT NULL DEFAULT 0,

    session_id        TEXT    NOT NULL DEFAULT '',
    execution_id      TEXT    NOT NULL DEFAULT '',
    worker_run_id     TEXT    NOT NULL DEFAULT '',

    payload_id        TEXT    NOT NULL DEFAULT '',
    payload_sha256    TEXT    NOT NULL DEFAULT '',

    target_kind       TEXT    NOT NULL,
    target_ref        TEXT    NOT NULL DEFAULT '',

    status            TEXT    NOT NULL
                      CHECK(status IN ('planned', 'started', 'delivered', 'failed', 'unknown')),
    error_code        TEXT    NOT NULL DEFAULT '',
    -- Bounded, human-readable reason. Never raw provider or worker errors.
    reason            TEXT    NOT NULL DEFAULT '',

    owner_instance_id TEXT    NOT NULL DEFAULT '',
    lease_until       BIGINT,
    lease_version     BIGINT  NOT NULL DEFAULT 0,

    provider_ref      TEXT    NOT NULL DEFAULT '',
    evidence_ref      TEXT    NOT NULL DEFAULT '',

    created_at        BIGINT  NOT NULL,
    updated_at        BIGINT  NOT NULL,
    UNIQUE(occurrence_id, delivery_ordinal, target_revision)
);
CREATE INDEX IF NOT EXISTS idx_effects_occurrence ON effects(occurrence_id, delivery_ordinal);
CREATE INDEX IF NOT EXISTS idx_effects_status_updated ON effects(status, updated_at);

-- +goose Down
DROP INDEX IF EXISTS idx_effects_status_updated;
DROP INDEX IF EXISTS idx_effects_occurrence;
DROP TABLE IF EXISTS effects;
DROP INDEX IF EXISTS idx_effect_payloads_occurrence;
DROP TABLE IF EXISTS effect_payloads;
