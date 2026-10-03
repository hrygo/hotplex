-- +goose Up
-- Per-attempt facts for the delivery ledger.
--
-- The effects row holds the CURRENT state; this table holds what each send
-- attempt actually did. They are separate on purpose: an effect is a delivery,
-- an attempt is one try at it, and "the third try was rate limited" must stay
-- readable after the effect settles.
--
-- The lease token is per attempt and unforgeable. An executor that lost its
-- lease cannot present the current one, so a late writer cannot complete an
-- attempt it no longer owns even if it guesses the version.
ALTER TABLE effects ADD COLUMN next_attempt_at BIGINT;

CREATE TABLE effect_attempts (
    attempt_id        TEXT    PRIMARY KEY,
    effect_id         TEXT    NOT NULL,
    attempt           BIGINT  NOT NULL,
    owner_instance_id TEXT    NOT NULL,
    lease_version     BIGINT  NOT NULL,
    -- Random per attempt. Completion must present this exact value.
    lease_token       TEXT    NOT NULL,
    started_at        BIGINT  NOT NULL,
    finished_at       BIGINT,
    -- Empty until the attempt finishes. '' is deliberately distinct from
    -- 'unknown': an unfinished attempt is not an outcome.
    outcome           TEXT    NOT NULL DEFAULT ''
                      CHECK(outcome IN ('', 'accepted', 'rejected', 'unknown', 'not_sent')),
    rejection_class   TEXT    NOT NULL DEFAULT ''
                      CHECK(rejection_class IN ('', 'safe_retry', 'permanent', 'unspecified')),
    provider_ref      TEXT    NOT NULL DEFAULT '',
    evidence_ref      TEXT    NOT NULL DEFAULT '',
    error_code        TEXT    NOT NULL DEFAULT '',
    -- Bounded, human-readable. Never a raw provider or worker error.
    reason            TEXT    NOT NULL DEFAULT '',
    UNIQUE(effect_id, attempt)
);
CREATE INDEX IF NOT EXISTS idx_effect_attempts_effect ON effect_attempts(effect_id, attempt);

-- +goose Down
DROP INDEX IF EXISTS idx_effect_attempts_effect;
DROP TABLE IF EXISTS effect_attempts;
ALTER TABLE effects DROP COLUMN next_attempt_at;
