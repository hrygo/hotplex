package audit

import (
	"context"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
)

func TestLifecyclePGStoreUsesSeparateChainTablesAndLock(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewLifecycleStore(db, dbutil.DialectPostgres, nil, slog.Default())
	require.NoError(t, err)

	ctx := context.Background()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock($1)")).
		WithArgs(lifecycleAuditAdvisoryLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT self_hash FROM user_activity_v2 ORDER BY id DESC LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"self_hash"}))
	tail, err := tx.TailHash(ctx)
	require.NoError(t, err)
	require.Empty(t, tail)

	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT id, self_hash FROM user_activity_v2 WHERE id < COALESCE((SELECT MIN(id) FROM user_activity_v2 WHERE expires_at >= $1), (SELECT MAX(id)+1 FROM user_activity_v2)) ORDER BY id DESC LIMIT 1",
	)).WithArgs(int64(2000)).WillReturnRows(sqlmock.NewRows([]string{"id", "self_hash"}))
	id, hash, err := tx.LastRowBefore(ctx, time.UnixMilli(2000))
	require.NoError(t, err)
	require.Zero(t, id)
	require.Empty(t, hash)

	ua := &UserActivity{
		Ts:         1000,
		UserID:     "user-v2",
		UserIDType: UserIDTypeSystem,
		Platform:   PlatformAPI,
		Action:     ActionAuthLogin,
		Outcome:    OutcomeSuccess,
		DetailJSON: `{}`,
		ChainEpoch: lifecycleChainProfile.epoch,
		ExpiresAt:  2000,
	}
	ua.SelfHash, err = ComputeSelfHash("", ua)
	require.NoError(t, err)

	mock.ExpectExec(regexp.QuoteMeta(
		"INSERT INTO user_activity_v2 (ts, user_id, user_id_type, platform, session_id, action, resource_type, resource_id, outcome, detail_json, event_ref, ip, user_agent, prev_hash, self_hash, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)",
	)).WithArgs(
		ua.Ts, ua.UserID, ua.UserIDType, ua.Platform, ua.SessionID,
		ua.Action, ua.ResourceType, ua.ResourceID, ua.Outcome, ua.DetailJSON, ua.EventRef,
		ua.IP, ua.UserAgent, ua.PrevHash, ua.SelfHash, ua.ExpiresAt,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	require.NoError(t, tx.Append(ctx, ua))
	mock.ExpectCommit()
	require.NoError(t, tx.Commit())

	require.NoError(t, mock.ExpectationsWereMet())
}
