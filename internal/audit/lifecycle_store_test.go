package audit

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
)

func TestLifecycleStoreHasIndependentChainAndRetention(t *testing.T) {
	t.Parallel()

	legacy, lifecycle := newTestSQLiteLifecycleStores(t)
	ctx := context.Background()
	oldTs := time.Now().Add(-48 * time.Hour).UnixMilli()
	appendTestActivity(t, legacy, oldTs, "legacy")
	appendTestActivity(t, lifecycle, oldTs, "current")

	gc := NewGC(lifecycle, GCConfig{Retention: 24 * time.Hour, Interval: time.Hour}, slog.Default())
	deleted, err := gc.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	legacyRows, err := legacy.Query(ctx, Query{})
	require.NoError(t, err)
	require.Len(t, legacyRows, 1)
	require.Equal(t, "legacy", legacyRows[0].UserID)
	require.Equal(t, "legacy", legacyRows[0].ChainEpoch)

	currentRows, err := lifecycle.Query(ctx, Query{})
	require.NoError(t, err)
	require.Empty(t, currentRows)

	legacyCheckpoint, err := legacy.LatestCheckpoint(ctx)
	require.NoError(t, err)
	require.Nil(t, legacyCheckpoint, "v2 cleanup must not checkpoint or rewrite the legacy chain")

	result, err := NewVerifier(legacy, VerifierConfig{}, slog.Default()).VerifyOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), result.BrokenID)
}

func TestLifecycleGCStopsAtFirstUnexpiredRow(t *testing.T) {
	t.Parallel()

	_, lifecycle := newTestSQLiteLifecycleStores(t)
	now := time.Now().UnixMilli()
	baseTs := time.Now().Add(-48 * time.Hour).UnixMilli()
	appendTestActivityWithExpiry(t, lifecycle, baseTs, now-2*time.Minute.Milliseconds(), "expired-prefix")
	appendTestActivityWithExpiry(t, lifecycle, baseTs+1, now+time.Hour.Milliseconds(), "not-yet-expired")
	appendTestActivityWithExpiry(t, lifecycle, baseTs+2, now-time.Minute.Milliseconds(), "expired-after-gap")

	gc := NewGC(lifecycle, GCConfig{Retention: 24 * time.Hour, Interval: time.Hour}, slog.Default())
	deleted, err := gc.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted, "only the contiguous expired prefix may be pruned")

	rows, err := lifecycle.QueryAsc(context.Background(), 1, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "not-yet-expired", rows[0].UserID)
	require.Equal(t, "expired-after-gap", rows[1].UserID)

	checkpoint, err := lifecycle.LatestCheckpoint(context.Background())
	require.NoError(t, err)
	require.NotNil(t, checkpoint)
	require.Equal(t, int64(2), checkpoint.NextID)
	result, err := NewVerifier(lifecycle, VerifierConfig{}, slog.Default()).VerifyOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(0), result.BrokenID, "prefix cleanup must preserve the surviving chain")
}

func TestCombinedStoreQueriesBothAuditEpochs(t *testing.T) {
	t.Parallel()

	legacy, lifecycle := newTestSQLiteLifecycleStores(t)
	appendTestActivity(t, legacy, time.Now().UnixMilli(), "legacy")
	appendTestActivity(t, lifecycle, time.Now().Add(time.Millisecond).UnixMilli(), "current")

	combined, err := NewCombinedStore(legacy.db, dbutil.DialectSQLite, legacy, lifecycle)
	require.NoError(t, err)

	rows, err := combined.Query(context.Background(), Query{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "current", rows[0].UserID)
	require.Equal(t, "lifecycle-v2", rows[0].ChainEpoch)
	require.Equal(t, "legacy", rows[1].UserID)
	require.Equal(t, "legacy", rows[1].ChainEpoch)

	stats, err := combined.Stats(context.Background(), Query{})
	require.NoError(t, err)
	require.Equal(t, int64(2), stats.Total)
	require.Equal(t, int64(2), stats.ByOutcome[OutcomeSuccess])
}

func TestLifecycleCollectorFreezesRetentionDeadlineAtEnqueue(t *testing.T) {
	t.Parallel()

	_, lifecycle := newTestSQLiteLifecycleStores(t)
	retention := 48 * time.Hour
	collector := NewCollector(lifecycle, nil, nil, slog.Default(), CollectorConfig{
		FactsRetention: retention,
	})

	ts := time.Now().UnixMilli()
	ua := &UserActivity{Ts: ts, UserID: "user-v2", Action: ActionAuthLogin, Outcome: OutcomeSuccess}
	require.NoError(t, collector.Enqueue(context.Background(), ua))
	require.Equal(t, lifecycleChainProfile.epoch, ua.ChainEpoch)
	require.Equal(t, time.UnixMilli(ts).Add(retention).UnixMilli(), ua.ExpiresAt)
}

func newTestSQLiteLifecycleStores(t *testing.T) (*sqliteStore, Store) {
	t.Helper()

	legacy := newTestSQLiteStore(t).(*sqliteStore)
	_, err := legacy.db.Exec(`
CREATE TABLE user_activity_v2 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    user_id TEXT NOT NULL,
    user_id_type TEXT NOT NULL,
    platform TEXT NOT NULL,
    session_id TEXT,
    action TEXT NOT NULL,
    resource_type TEXT,
    resource_id TEXT,
    outcome TEXT NOT NULL,
    detail_json TEXT NOT NULL,
    event_ref TEXT,
    ip TEXT,
    user_agent TEXT,
    prev_hash TEXT NOT NULL,
    self_hash TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE TABLE audit_chain_checkpoints_v2 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    pruned_at INTEGER NOT NULL,
    last_self_hash TEXT NOT NULL,
    next_id INTEGER NOT NULL
);`)
	require.NoError(t, err)

	lifecycle, err := NewLifecycleStore(legacy.db, dbutil.DialectSQLite, legacy.writeMu, slog.Default())
	require.NoError(t, err)
	return legacy, lifecycle
}

func appendTestActivity(t *testing.T, store Store, ts int64, userID string) {
	t.Helper()

	expiry := int64(0)
	if provider, ok := store.(interface{ UsesPersistedExpiry() bool }); ok && provider.UsesPersistedExpiry() {
		expiry = time.UnixMilli(ts).Add(24 * time.Hour).UnixMilli()
	}
	appendTestActivityWithExpiry(t, store, ts, expiry, userID)
}

func appendTestActivityWithExpiry(t *testing.T, store Store, ts, expiry int64, userID string) {
	t.Helper()

	ctx := context.Background()
	epoch := legacyChainProfile.epoch
	if provider, ok := store.(interface{ ChainEpoch() string }); ok {
		epoch = provider.ChainEpoch()
	}
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	tail, err := tx.TailHash(ctx)
	require.NoError(t, err)
	ua := &UserActivity{
		Ts:         ts,
		UserID:     userID,
		UserIDType: UserIDTypeSystem,
		Platform:   PlatformAPI,
		Action:     ActionAuthLogin,
		Outcome:    OutcomeSuccess,
		DetailJSON: `{}`,
		ChainEpoch: epoch,
		ExpiresAt:  expiry,
	}
	ua.PrevHash = tail
	ua.SelfHash, err = ComputeSelfHash(tail, ua)
	require.NoError(t, err)
	require.NoError(t, tx.Append(ctx, ua))
	require.NoError(t, tx.Commit())
}
