-- +goose Up
-- Assign immutable per-record deadlines to newly written conversation content.
-- Existing rows receive zero and continue to use the legacy events.retention window.
ALTER TABLE "events" ADD COLUMN "expires_at" BIGINT NOT NULL DEFAULT 0;
ALTER TABLE "turns" ADD COLUMN "expires_at" BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS "idx_events_expires_at" ON "events"("expires_at");
CREATE INDEX IF NOT EXISTS "idx_turns_expires_at" ON "turns"("expires_at");

-- +goose Down
DROP INDEX IF EXISTS "idx_turns_expires_at";
DROP INDEX IF EXISTS "idx_events_expires_at";
ALTER TABLE "turns" DROP COLUMN "expires_at";
ALTER TABLE "events" DROP COLUMN "expires_at";
