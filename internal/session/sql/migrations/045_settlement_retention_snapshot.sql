-- +goose Up
-- Capture the policy when an obligation is created. The expiry is its
-- settlement clock plus this immutable window, independent of current config.
-- NULL protects old rows until an explicitly reviewed data migration.
ALTER TABLE effects ADD COLUMN payload_retention_ms INTEGER CHECK (payload_retention_ms > 0);
ALTER TABLE effects ADD COLUMN facts_retention_ms INTEGER CHECK (facts_retention_ms > 0);
ALTER TABLE effects ADD COLUMN retention_policy_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE execution_inputs ADD COLUMN facts_retention_ms INTEGER CHECK (facts_retention_ms > 0);
ALTER TABLE execution_inputs ADD COLUMN retention_policy_revision TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE execution_inputs DROP COLUMN retention_policy_revision;
ALTER TABLE execution_inputs DROP COLUMN facts_retention_ms;
ALTER TABLE effects DROP COLUMN retention_policy_revision;
ALTER TABLE effects DROP COLUMN facts_retention_ms;
ALTER TABLE effects DROP COLUMN payload_retention_ms;
