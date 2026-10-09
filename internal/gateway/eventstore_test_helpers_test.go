package gateway

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func createEventStoreSessionBarrierSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		state TEXT NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS session_purge_jobs (
		session_id TEXT PRIMARY KEY
	)`)
	require.NoError(t, err)
}
