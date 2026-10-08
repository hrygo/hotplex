-- +goose Up
-- Separate logical-history clocks from Worker runtime expiry and reserve
-- immutable per-turn deadline fields. Existing rows keep legacy behavior.
ALTER TABLE "sessions" ADD COLUMN "lifecycle_policy" TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE "sessions" ADD COLUMN "lifecycle_policy_revision" TEXT NOT NULL DEFAULT '';
ALTER TABLE "sessions" ADD COLUMN "last_input_at" TIMESTAMPTZ;
ALTER TABLE "sessions" ADD COLUMN "runtime_finished_at" TIMESTAMPTZ;
ALTER TABLE "sessions" ADD COLUMN "archive_at" TIMESTAMPTZ;
ALTER TABLE "sessions" ADD COLUMN "conversation_expires_at" TIMESTAMPTZ;
ALTER TABLE "sessions" ADD COLUMN "last_content_expires_at" TIMESTAMPTZ;
ALTER TABLE "sessions" ADD COLUMN "history_expires_at" TIMESTAMPTZ;
ALTER TABLE "sessions" ADD COLUMN "deleted_at" TIMESTAMPTZ;

ALTER TABLE "execution_inputs" ADD COLUMN "turn_started_at" BIGINT;
ALTER TABLE "execution_inputs" ADD COLUMN "turn_deadline_at" BIGINT;
ALTER TABLE "execution_inputs" ADD COLUMN "turn_policy_revision" TEXT NOT NULL DEFAULT '';

CREATE INDEX "idx_sessions_lifecycle_archive" ON "sessions"("lifecycle_policy", "archive_at");
CREATE INDEX "idx_sessions_lifecycle_history" ON "sessions"("lifecycle_policy", "history_expires_at");

-- +goose Down
DROP INDEX IF EXISTS "idx_sessions_lifecycle_history";
DROP INDEX IF EXISTS "idx_sessions_lifecycle_archive";
ALTER TABLE "execution_inputs" DROP COLUMN "turn_policy_revision";
ALTER TABLE "execution_inputs" DROP COLUMN "turn_deadline_at";
ALTER TABLE "execution_inputs" DROP COLUMN "turn_started_at";
ALTER TABLE "sessions" DROP COLUMN "deleted_at";
ALTER TABLE "sessions" DROP COLUMN "history_expires_at";
ALTER TABLE "sessions" DROP COLUMN "last_content_expires_at";
ALTER TABLE "sessions" DROP COLUMN "conversation_expires_at";
ALTER TABLE "sessions" DROP COLUMN "archive_at";
ALTER TABLE "sessions" DROP COLUMN "runtime_finished_at";
ALTER TABLE "sessions" DROP COLUMN "last_input_at";
ALTER TABLE "sessions" DROP COLUMN "lifecycle_policy_revision";
ALTER TABLE "sessions" DROP COLUMN "lifecycle_policy";
