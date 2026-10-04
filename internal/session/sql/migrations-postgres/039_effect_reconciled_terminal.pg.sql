-- +goose Up
-- EffectLedger reconciled terminal states (#947): same CHECK growth as the
-- SQLite 039. Existing rows untouched. The status CHECK constraint is found
-- by discovery (server-named back in 034), mirroring 037, so a silent
-- no-match cannot leave the old five-value CHECK in place.

-- +goose StatementBegin
DO $$
DECLARE
    existing_constraint TEXT;
BEGIN
    SELECT con.conname INTO existing_constraint
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    WHERE rel.relname = 'effects'
      AND con.contype = 'c'
      AND pg_get_constraintdef(con.oid) LIKE '%status%'
    LIMIT 1;

    IF existing_constraint IS NOT NULL THEN
        EXECUTE format('ALTER TABLE effects DROP CONSTRAINT %I', existing_constraint);
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE effects
    ADD CONSTRAINT "effects_status_check"
    CHECK (status IN ('planned', 'started', 'delivered', 'failed', 'unknown',
                      'reconciled_succeeded', 'reconciled_failed', 'fenced'));

-- +goose Down
ALTER TABLE effects DROP CONSTRAINT IF EXISTS "effects_status_check";
ALTER TABLE effects
    ADD CONSTRAINT "effects_status_check"
    CHECK (status IN ('planned', 'started', 'delivered', 'failed', 'unknown'));
