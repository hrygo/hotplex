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

func insertPreModeOccurrence(t *testing.T, ctx context.Context, db *sql.DB, id, triggerKey string) {
	t.Helper()
	_, err := db.ExecContext(ctx, `INSERT INTO cron_occurrences
		(occurrence_id, trigger_key, generation, job_id, trigger_kind,
		 status, created_at, updated_at)
		VALUES (?, ?, 0, 'job-1', 'scheduled', 'completed', 1, 1)`, id, triggerKey)
	require.NoError(t, err)
}

// TestMigration035AddsDeliveryModeAdditively pins the property the scheduler
// depends on: an occurrence written before this migration keeps its identity
// and lands on the legacy owner, so upgrading cannot hand an existing run to
// the gateway.
func TestMigration035AddsDeliveryModeAdditively(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(sqlutil.DriverName, filepath.Join(t.TempDir(), "migration-035.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Default()
	require.NoError(t, sqlutil.InitSQLiteDB(db, &cfg.DB, sqlutil.DialectSQLite, "migration_035_test"))

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

	insertPreModeOccurrence(t, ctx, db, "occ-legacy-1", "sched|job-1|rev1|1700000000000")
	insertPreModeOccurrence(t, ctx, db, "occ-legacy-2", "sched|job-1|rev1|1700000060000")

	_, err = provider.UpTo(ctx, 35)
	require.NoError(t, err)

	rows, err := db.QueryContext(ctx,
		`SELECT occurrence_id, delivery_mode FROM cron_occurrences ORDER BY occurrence_id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var id, mode string
		require.NoError(t, rows.Scan(&id, &mode))
		require.Equal(t, "legacy_cli", mode,
			"a pre-migration occurrence must keep its original owner")
		seen++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, 2, seen, "the migration must not drop occurrences")

	// The gateway owner must be selectable, and nothing outside the two
	// declared modes may be stored.
	_, err = db.ExecContext(ctx, `INSERT INTO cron_occurrences
		(occurrence_id, trigger_key, generation, job_id, trigger_kind,
		 delivery_mode, status, created_at, updated_at)
		VALUES ('occ-gateway', 'sched|job-1|rev1|1700000120000', 0, 'job-1', 'scheduled',
		 'gateway', 'completed', 1, 1)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO cron_occurrences
		(occurrence_id, trigger_key, generation, job_id, trigger_kind,
		 delivery_mode, status, created_at, updated_at)
		VALUES ('occ-bogus', 'sched|job-1|rev1|1700000180000', 0, 'job-1', 'scheduled',
		 'both', 'completed', 1, 1)`)
	require.Error(t, err, "only the two declared owners may be stored")
}
