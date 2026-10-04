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

// TestMigration035PGAddsDeliveryModeAdditively mirrors the SQLite guard so
// the paired migration carries the same guarantee. It is UNVERIFIED when no
// PG test DSN is configured, which is reported rather than assumed.
func TestMigration035PGAddsDeliveryModeAdditively(t *testing.T) {
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

	insertPreModeOccurrence(t, ctx, db, "occ-legacy-1", "sched|job-1|rev1|1700000000000")

	_, err = provider.UpTo(ctx, 35)
	require.NoError(t, err)

	var mode string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT delivery_mode FROM cron_occurrences WHERE occurrence_id = 'occ-legacy-1'`).Scan(&mode))
	require.Equal(t, "legacy_cli", mode)

	_, err = db.ExecContext(ctx, `INSERT INTO cron_occurrences
		(occurrence_id, trigger_key, generation, job_id, trigger_kind,
		 delivery_mode, status, created_at, updated_at)
		VALUES ('occ-bogus', 'sched|job-1|rev1|1700000180000', 0, 'job-1', 'scheduled',
		 'both', 'completed', 1, 1)`)
	require.Error(t, err, "only the two declared owners may be stored")
}
