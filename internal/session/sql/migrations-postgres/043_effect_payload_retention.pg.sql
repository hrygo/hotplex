-- +goose Up
-- Track when an external-delivery obligation reaches a terminal state. The
-- nullable value intentionally leaves legacy and unresolved effects protected.
ALTER TABLE "effects" ADD COLUMN "settled_at" BIGINT;
CREATE INDEX "idx_effects_payload_settled" ON "effects"("payload_id", "settled_at");

-- +goose Down
DROP INDEX IF EXISTS "idx_effects_payload_settled";
ALTER TABLE "effects" DROP COLUMN "settled_at";
