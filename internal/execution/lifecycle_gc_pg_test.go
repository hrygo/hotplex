package execution

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
)

func TestCompactSettledFactsPostgresRebindsAndUsesTransaction(t *testing.T) {
	t.Parallel()

	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = mockDB.Close() })

	store, err := NewSQLStore(context.Background(), mockDB, dbutil.DialectPostgres, nil, nil)
	require.NoError(t, err)

	cutoff := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(dbutil.DialectPostgres.Rebind(compactSettledFactsSQL))).
		WithArgs(cutoff.UnixMilli(), 25).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()

	compacted, err := store.CompactSettledFacts(context.Background(), cutoff, 25)
	require.NoError(t, err)
	require.EqualValues(t, 3, compacted)
	require.NoError(t, mock.ExpectationsWereMet())
}
