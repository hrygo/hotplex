//go:build pg

package session

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

// TestPGStore_DeleteTerminatedEnqueuesCleanupTask is the regression test for
// D04: session GC failed on PostgreSQL with
// "session cleanup: enqueue: driver: bad connection" while SQLite stayed
// green. The DELETE ... RETURNING rows were scanned and left open while the
// cleanup INSERT ran on the same transaction. pgx (database/sql) multiplexes
// one server connection per *sql.Tx and an open result set holds it, so the
// INSERT failed with bad connection instead of waiting. modernc/sqlite
// buffers rows client-side, which is why the same code passed on SQLite.
// The fix closes the scanned rows before the INSERT. This test runs the real
// path against PostgreSQL, so removing the close turns it red with exactly
// the live symptom.
func TestPGStore_DeleteTerminatedEnqueuesCleanupTask(t *testing.T) {
	dsn := os.Getenv("HOTPLEX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HOTPLEX_TEST_PG_DSN not set; skipping PG delete-terminated test")
	}
	ctx := context.Background()
	db, err := sql.Open(sqlutil.DriverNamePG, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE SCHEMA public`)
	require.NoError(t, err)

	store, err := NewPGStore(ctx, &dbutil.DB{DB: db})
	require.NoError(t, err)

	now := time.Now()
	old := now.Add(-time.Hour)
	info := ocsTerminatedInfo("sess-gc-enqueue", "ocs-gc-old", old)
	require.NoError(t, store.Upsert(ctx, info))

	deleted, err := store.DeleteTerminated(ctx, now, now)
	require.NoError(t, err, "GC delete must not fail with driver: bad connection")
	require.Len(t, deleted, 1)
	require.Equal(t, "ocs-gc-old", deleted[0].WorkerSessionID)

	var pending int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_cleanup_tasks WHERE session_id = 'sess-gc-enqueue'`).Scan(&pending))
	require.Equal(t, 1, pending, "the deleted session must leave one cleanup task behind")
}
