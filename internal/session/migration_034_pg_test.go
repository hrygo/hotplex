//go:build pg

package session

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/sqlutil"
)

// TestMigration034PG_EffectLedger_BusinessKeyAndStatusGuard mirrors the SQLite
// guard test. The paired migration is only real if the PostgreSQL schema
// carries the same guarantees, so this runs whenever a PG test DSN is
// available and is reported as unverified when it is not.
func TestMigration034PG_EffectLedger_BusinessKeyAndStatusGuard(t *testing.T) {
	dsn := os.Getenv("HOTPLEX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HOTPLEX_TEST_PG_DSN not set; skipping PG migration test")
	}
	ctx := context.Background()
	db, err := sql.Open(sqlutil.DriverNamePG, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE SCHEMA public`)
	require.NoError(t, err)

	migrations, err := fs.Sub(migrationsPGFs, "sql/migrations-postgres")
	require.NoError(t, err)
	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		migrations,
		goose.WithDisableGlobalRegistry(true),
	)
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, 34)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO effect_payloads
		(payload_id, occurrence_id, execution_id, worker_run_id,
		 content, content_bytes, content_sha256, created_at)
		VALUES ('pay-1', 'occ-1', 'exec-1', 'run-1', 'hello', 5, 'sha', 1)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO effects
		(effect_id, occurrence_id, delivery_ordinal, target_revision, attempt,
		 session_id, execution_id, worker_run_id, payload_id, payload_sha256,
		 target_kind, target_ref, status, error_code, reason,
		 owner_instance_id, lease_until, lease_version, provider_ref, evidence_ref,
		 created_at, updated_at)
		VALUES ('eff-1', 'occ-1', 0, 'rev-a', 0, 's-1', 'exec-1', 'run-1', 'pay-1', 'sha',
		 'slack', 'C123', 'planned', '', '', '', NULL, 0, '', '', 1, 1)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO effects
		(effect_id, occurrence_id, delivery_ordinal, target_revision, attempt,
		 target_kind, status, created_at, updated_at)
		VALUES ('eff-2', 'occ-1', 0, 'rev-a', 0, 'slack', 'planned', 1, 1)`)
	require.Error(t, err, "the business key must be unique across effects")

	_, err = db.ExecContext(ctx, `INSERT INTO effect_payloads
		(payload_id, occurrence_id, execution_id, worker_run_id,
		 content, content_bytes, content_sha256, created_at)
		VALUES ('pay-2', 'occ-1', 'exec-1', 'run-1', 'other', 5, 'sha2', 1)`)
	require.Error(t, err, "content snapshots must not be duplicated per execution")

	_, err = db.ExecContext(ctx, `INSERT INTO effects
		(effect_id, occurrence_id, delivery_ordinal, target_revision, attempt,
		 target_kind, status, created_at, updated_at)
		VALUES ('eff-4', 'occ-2', 0, 'rev-a', 0, 'slack', 'read', 1, 1)`)
	require.Error(t, err, "status must stay inside the declared lifecycle")

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM effects`).Scan(&count))
	require.Equal(t, 1, count)
}
