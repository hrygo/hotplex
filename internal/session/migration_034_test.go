package session

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

func insertTestEffect(t *testing.T, ctx context.Context, db *sql.DB, effectID, revision string) {
	t.Helper()
	_, err := db.ExecContext(ctx, `INSERT INTO effects
		(effect_id, occurrence_id, delivery_ordinal, target_revision, attempt,
		 session_id, execution_id, worker_run_id, payload_id, payload_sha256,
		 target_kind, target_ref, status, error_code, reason,
		 owner_instance_id, lease_until, lease_version, provider_ref, evidence_ref,
		 created_at, updated_at)
		VALUES (?, 'occ-1', 0, ?, 0, 's-1', 'exec-1', 'run-1', 'pay-1', 'sha',
		 'slack', 'C123', 'planned', '', '', '', NULL, 0, '', '', 1, 1)`,
		effectID, revision)
	require.NoError(t, err)
}

// TestMigration034EffectLedger_BusinessKeyAndStatusGuard pins the two
// constraints the delivery ledger depends on: one effect per business key, and
// a closed set of statuses. Both are enforced by the schema rather than by
// application code, so a buggy or rolled-back binary cannot write a state the
// rest of the pipeline has no meaning for.
func TestMigration034EffectLedger_BusinessKeyAndStatusGuard(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(sqlutil.DriverName, filepath.Join(t.TempDir(), "migration-034.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Default()
	require.NoError(t, sqlutil.InitSQLiteDB(db, &cfg.DB, sqlutil.DialectSQLite, "migration_034_test"))

	migrations, err := fs.Sub(migrationFS, "sql/migrations")
	require.NoError(t, err)
	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		migrations,
		goose.WithDisableGlobalRegistry(true),
	)
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, 34)
	require.NoError(t, err)

	insertTestEffect(t, ctx, db, "eff-1", "rev-a")

	_, err = db.ExecContext(ctx, `INSERT INTO effect_payloads
		(payload_id, occurrence_id, execution_id, worker_run_id,
		 content, content_bytes, content_sha256, created_at)
		VALUES ('pay-1', 'occ-1', 'exec-1', 'run-1', 'hello', 5, 'sha', 1)`)
	require.NoError(t, err)

	// Same business key, different surrogate ID: this is what a retry or a
	// post-crash recovery would attempt. It must be refused, otherwise one
	// delivery could be sent twice.
	_, err = db.ExecContext(ctx, `INSERT INTO effects
		(effect_id, occurrence_id, delivery_ordinal, target_revision, attempt,
		 target_kind, status, created_at, updated_at)
		VALUES ('eff-2', 'occ-1', 0, 'rev-a', 0, 'slack', 'planned', 1, 1)`)
	require.Error(t, err, "the business key must be unique across effects")

	// A different target revision is a different logical delivery.
	insertTestEffect(t, ctx, db, "eff-3", "rev-b")

	// One snapshot per occurrence+execution: re-planning must reuse it.
	_, err = db.ExecContext(ctx, `INSERT INTO effect_payloads
		(payload_id, occurrence_id, execution_id, worker_run_id,
		 content, content_bytes, content_sha256, created_at)
		VALUES ('pay-2', 'occ-1', 'exec-1', 'run-1', 'other', 5, 'sha2', 1)`)
	require.Error(t, err, "content snapshots must not be duplicated per execution")

	// An unknown status has no meaning anywhere downstream.
	_, err = db.ExecContext(ctx, `INSERT INTO effects
		(effect_id, occurrence_id, delivery_ordinal, target_revision, attempt,
		 target_kind, status, created_at, updated_at)
		VALUES ('eff-4', 'occ-2', 0, 'rev-a', 0, 'slack', 'read', 1, 1)`)
	require.Error(t, err, "status must stay inside the declared lifecycle")

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM effects`).Scan(&count))
	require.Equal(t, 2, count)
}
