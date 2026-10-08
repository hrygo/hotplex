-- +goose Up
-- Lifecycle v2 starts a separate content-minimized audit chain so its
-- configurable retention cannot shorten the legacy user_activity chain.

CREATE TABLE "user_activity_v2" (
    "id"            BIGSERIAL PRIMARY KEY,
    "ts"            BIGINT  NOT NULL,
    "user_id"       TEXT    NOT NULL,
    "user_id_type"  TEXT    NOT NULL,
    "platform"      TEXT    NOT NULL,
    "session_id"    TEXT,
    "action"        TEXT    NOT NULL,
    "resource_type" TEXT,
    "resource_id"   TEXT,
    "outcome"       TEXT    NOT NULL,
    "detail_json"   TEXT    NOT NULL,
    "event_ref"     TEXT,
    "ip"            TEXT,
    "user_agent"    TEXT,
    "prev_hash"     TEXT    NOT NULL,
    "self_hash"     TEXT    NOT NULL,
    "expires_at"    BIGINT  NOT NULL
);

CREATE INDEX "idx_ua_v2_user_ts"   ON "user_activity_v2"("user_id", "ts");
CREATE INDEX "idx_ua_v2_ts"        ON "user_activity_v2"("ts");
CREATE INDEX "idx_ua_v2_action_ts" ON "user_activity_v2"("action", "ts");
CREATE INDEX "idx_ua_v2_expires_at" ON "user_activity_v2"("expires_at");

CREATE TABLE "audit_chain_checkpoints_v2" (
    "id"              BIGSERIAL PRIMARY KEY,
    "pruned_at"       BIGINT  NOT NULL,
    "last_self_hash"  TEXT    NOT NULL,
    "next_id"         BIGINT  NOT NULL
);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION "fn_ua_v2_no_update"() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit: rows are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION "fn_ua_v2_no_delete"() RETURNS TRIGGER AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM "audit_chain_checkpoints_v2" WHERE "next_id" > OLD."id"
    ) THEN
        RAISE EXCEPTION 'audit: rows are immutable except via checkpoint-anchored GC';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER "trg_ua_v2_no_update"
BEFORE UPDATE ON "user_activity_v2"
FOR EACH ROW EXECUTE FUNCTION "fn_ua_v2_no_update"();

CREATE TRIGGER "trg_ua_v2_no_delete"
BEFORE DELETE ON "user_activity_v2"
FOR EACH ROW EXECUTE FUNCTION "fn_ua_v2_no_delete"();

-- +goose Down
DROP TRIGGER IF EXISTS "trg_ua_v2_no_delete" ON "user_activity_v2";
DROP TRIGGER IF EXISTS "trg_ua_v2_no_update" ON "user_activity_v2";
DROP FUNCTION IF EXISTS "fn_ua_v2_no_delete"();
DROP FUNCTION IF EXISTS "fn_ua_v2_no_update"();
DROP TABLE IF EXISTS "user_activity_v2";
DROP TABLE IF EXISTS "audit_chain_checkpoints_v2";
