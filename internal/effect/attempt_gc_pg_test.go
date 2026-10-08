package effect

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
)

func TestDeleteSettledAttemptsPostgresRebindsAndUsesTransaction(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cutoff := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	mock.ExpectExec(regexp.QuoteMeta(dbutil.DialectPostgres.Rebind(deleteSettledAttemptsSQL))).
		WithArgs(cutoff.UnixMilli(), 25).
		WillReturnResult(sqlmock.NewResult(0, 3))

	deleted, err := deleteSettledAttempts(context.Background(), tx, dbutil.DialectPostgres, cutoff, 25)
	require.NoError(t, err)
	require.EqualValues(t, 3, deleted)

	mock.ExpectCommit()
	require.NoError(t, tx.Commit())
	require.NoError(t, mock.ExpectationsWereMet())
}
