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

func TestMigration041ContentExpiryPreservesExistingRows(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(sqlutil.DriverName, filepath.Join(t.TempDir(), "migration-041.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Default()
	require.NoError(t, sqlutil.InitSQLiteDB(db, &cfg.DB, sqlutil.DialectSQLite, "migration_041_test"))
	migrations, err := fs.Sub(migrationFS, "sql/migrations")
	require.NoError(t, err)
	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		migrations,
		goose.WithDisableGlobalRegistry(true),
	)
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, 40)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO events
		(session_id, seq, type, data, direction, source, created_at)
		VALUES ('s-legacy', 1, 'message', '{}', 'outbound', 'normal', 1000)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO turns
		(session_id, generation, turn_num, role, content, created_at)
		VALUES ('s-legacy', 1, 1, 'user', 'legacy content', 1000)`)
	require.NoError(t, err)

	_, err = provider.UpTo(ctx, 41)
	require.NoError(t, err)

	var eventExpiry, turnExpiry int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT expires_at FROM events WHERE session_id = 's-legacy'`).Scan(&eventExpiry))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT expires_at FROM turns WHERE session_id = 's-legacy'`).Scan(&turnExpiry))
	require.Zero(t, eventExpiry, "existing event rows must keep legacy expiry handling")
	require.Zero(t, turnExpiry, "existing turns must keep legacy expiry handling")
}
