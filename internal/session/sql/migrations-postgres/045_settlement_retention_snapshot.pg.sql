-- +goose Up
-- NULL windows protect existing records; configuration never backfills them.
ALTER TABLE effects ADD COLUMN payload_retention_ms BIGINT CHECK (payload_retention_ms > 0);
ALTER TABLE effects ADD COLUMN facts_retention_ms BIGINT CHECK (facts_retention_ms > 0);
ALTER TABLE effects ADD COLUMN retention_policy_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE execution_inputs ADD COLUMN facts_retention_ms BIGINT CHECK (facts_retention_ms > 0);
ALTER TABLE execution_inputs ADD COLUMN retention_policy_revision TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE execution_inputs DROP COLUMN retention_policy_revision;
ALTER TABLE execution_inputs DROP COLUMN facts_retention_ms;
ALTER TABLE effects DROP COLUMN retention_policy_revision;
ALTER TABLE effects DROP COLUMN facts_retention_ms;
ALTER TABLE effects DROP COLUMN payload_retention_ms;
