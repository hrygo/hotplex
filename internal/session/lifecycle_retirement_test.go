package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/pkg/events"
)

func expiredLifecycleSession(id string, now time.Time) *SessionInfo {
	conversationExpiresAt := now.Add(-2 * time.Hour)
	historyExpiresAt := now.Add(-time.Hour)
	lastContentExpiresAt := now.Add(-time.Hour)
	return &SessionInfo{
		ID:                      id,
		UserID:                  "user-1",
		WorkerType:              "claude_code",
		WorkerSessionID:         "worker-session-" + id,
		State:                   events.StateTerminated,
		CreatedAt:               now.Add(-200 * 24 * time.Hour),
		UpdatedAt:               now.Add(-24 * time.Hour),
		LifecyclePolicy:         config.LifecyclePolicyV2,
		LifecyclePolicyRevision: "v2-test",
		ConversationExpiresAt:   &conversationExpiresAt,
		LastContentExpiresAt:    &lastContentExpiresAt,
		HistoryExpiresAt:        &historyExpiresAt,
		Context:                 map[string]any{"private": "remove-on-retirement"},
		Title:                   "retained title",
		WorkDir:                 "/private/workdir",
	}
}

func TestSQLiteStore_ListExpiredLifecycleSessionsRequiresExpiredHistoryAndNoRecoveryObligations(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()

	expired := expiredLifecycleSession("sess-lifecycle-expired", now)
	require.NoError(t, store.Upsert(ctx, expired))

	liveHistory := expiredLifecycleSession("sess-lifecycle-live-history", now)
	liveHistory.HistoryExpiresAt = ptr(now.Add(time.Hour))
	require.NoError(t, store.Upsert(ctx, liveHistory))

	liveLastContent := expiredLifecycleSession("sess-lifecycle-live-content", now)
	liveLastContent.LastContentExpiresAt = ptr(now.Add(time.Hour))
	require.NoError(t, store.Upsert(ctx, liveLastContent))

	unknown := expiredLifecycleSession("sess-lifecycle-unknown", now)
	require.NoError(t, store.Upsert(ctx, unknown))
	_, err := store.db.ExecContext(ctx, `INSERT INTO execution_inputs
		(execution_id, session_id, client_message_id, payload_hash, status, created_at, updated_at,
		 runtime_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"exec-unknown", unknown.ID, "msg-unknown", "hash", "unknown",
		now.UnixMilli(), now.UnixMilli(), "unknown")
	require.NoError(t, err)

	ids, err := store.ListExpiredLifecycleSessions(ctx, now, 10)
	require.NoError(t, err)
	require.Equal(t, []string{expired.ID}, ids)
}

func TestSQLiteStore_GetLifecycleGCStatusCountsBacklogAndBlockers(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	eligible := expiredLifecycleSession("sess-lifecycle-status-eligible", now)
	require.NoError(t, store.Upsert(ctx, eligible))

	blocked := expiredLifecycleSession("sess-lifecycle-status-blocked", now)
	blocked.ConversationExpiresAt = ptr(now.Add(-8 * time.Hour))
	blocked.LastContentExpiresAt = ptr(now.Add(-6 * time.Hour))
	blocked.HistoryExpiresAt = ptr(now.Add(-5 * time.Hour))
	require.NoError(t, store.Upsert(ctx, blocked))
	_, err := store.db.ExecContext(ctx, `INSERT INTO execution_inputs
		(execution_id, session_id, client_message_id, payload_hash, status, created_at, updated_at,
		 runtime_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"exec-status-blocked", blocked.ID, "msg-status-blocked", "hash", "accepted",
		now.UnixMilli(), now.UnixMilli(), "pending")
	require.NoError(t, err)

	unknown := expiredLifecycleSession("sess-lifecycle-status-unknown", now)
	unknown.ConversationExpiresAt = ptr(now.Add(-6 * time.Hour))
	unknown.LastContentExpiresAt = ptr(now.Add(-4 * time.Hour))
	unknown.HistoryExpiresAt = ptr(now.Add(-3 * time.Hour))
	require.NoError(t, store.Upsert(ctx, unknown))
	_, err = store.db.ExecContext(ctx, `INSERT INTO execution_inputs
		(execution_id, session_id, client_message_id, payload_hash, status, created_at, updated_at,
		 runtime_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"exec-status-unknown", unknown.ID, "msg-status-unknown", "hash", "unknown",
		now.UnixMilli(), now.UnixMilli(), "unknown")
	require.NoError(t, err)

	notExpired := expiredLifecycleSession("sess-lifecycle-status-live", now)
	notExpired.HistoryExpiresAt = ptr(now.Add(time.Hour))
	require.NoError(t, store.Upsert(ctx, notExpired))

	status, err := store.GetLifecycleGCStatus(ctx, now)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.eligible)
	require.EqualValues(t, 2, status.blocked)
	require.EqualValues(t, 1, status.unknownExecutionHold)
	require.Equal(t, time.Hour, status.eligibleLag)
	require.Equal(t, 5*time.Hour, status.blockedLag)
}

func TestSQLiteStore_RetireExpiredLifecycleSessionRedactsAndQueuesCleanupAtomically(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := expiredLifecycleSession("sess-lifecycle-retire", now)
	require.NoError(t, store.Upsert(ctx, info))

	retired, err := store.RetireExpiredLifecycleSession(ctx, info.ID, now)
	require.NoError(t, err)
	require.NotNil(t, retired)
	require.Equal(t, info.WorkerSessionID, retired.WorkerSessionID)
	require.Equal(t, events.StateDeleted, retired.State)
	require.NotNil(t, retired.DeletedAt)

	stored, err := store.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, events.StateDeleted, stored.State)
	require.Equal(t, "", stored.Title)
	require.Equal(t, "", stored.WorkDir)
	require.Empty(t, stored.Context)
	require.Equal(t, "", stored.WorkerSessionID)

	cleanup, err := store.ClaimCleanupTasks(ctx, now, now.Add(time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, cleanup, 1)
	require.Equal(t, info.WorkerSessionID, cleanup[0].WorkerSessionID)

	status, err := store.GetPurgeStatus(ctx, info.ID, info.UserID)
	require.NoError(t, err)
	require.Equal(t, PurgeJobInProgress, status.Status)
	require.Len(t, status.Items, 3)
}

func TestSQLiteStore_RetireExpiredLifecycleSessionRechecksDeadlineAndExecutionState(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := expiredLifecycleSession("sess-lifecycle-retire-recheck", now)
	require.NoError(t, store.Upsert(ctx, info))

	extended := now.Add(time.Hour)
	_, err := store.db.ExecContext(ctx,
		`UPDATE sessions SET conversation_expires_at = ?, history_expires_at = ? WHERE id = ?`,
		extended, extended, info.ID)
	require.NoError(t, err)
	retired, err := store.RetireExpiredLifecycleSession(ctx, info.ID, now)
	require.NoError(t, err)
	require.Nil(t, retired, "a stale expiry candidate must not retire a renewed session")

	_, err = store.db.ExecContext(ctx,
		`UPDATE sessions SET conversation_expires_at = ?, history_expires_at = ? WHERE id = ?`,
		now.Add(-time.Hour), now.Add(-time.Hour), info.ID)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `INSERT INTO execution_inputs
		(execution_id, session_id, client_message_id, payload_hash, status, created_at, updated_at,
		 runtime_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"exec-active", info.ID, "msg-active", "hash", "accepted",
		now.UnixMilli(), now.UnixMilli(), "pending")
	require.NoError(t, err)

	retired, err = store.RetireExpiredLifecycleSession(ctx, info.ID, now)
	require.NoError(t, err)
	require.Nil(t, retired, "an active execution must protect its session from retirement")
}

func TestSQLiteStore_DeleteTerminatedPreservesUnknownExecutionEvidence(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := &SessionInfo{
		ID:         "sess-legacy-unknown",
		UserID:     "user-1",
		WorkerType: "claude_code",
		State:      events.StateTerminated,
		CreatedAt:  now.Add(-30 * 24 * time.Hour),
		UpdatedAt:  now.Add(-8 * 24 * time.Hour),
	}
	require.NoError(t, store.Upsert(ctx, info))
	_, err := store.db.ExecContext(ctx, `INSERT INTO execution_inputs
		(execution_id, session_id, client_message_id, payload_hash, status, created_at, updated_at,
		 runtime_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"exec-legacy-unknown", info.ID, "msg-legacy-unknown", "hash", "unknown",
		now.UnixMilli(), now.UnixMilli(), "unknown")
	require.NoError(t, err)

	deleted, err := store.DeleteTerminated(ctx, now.Add(-24*time.Hour), now.Add(-7*24*time.Hour))
	require.NoError(t, err)
	require.Empty(t, deleted)
	stored, err := store.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, events.StateTerminated, stored.State)

	var count int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM execution_inputs WHERE session_id = ?`, info.ID).Scan(&count))
	require.Equal(t, 1, count, "unknown execution evidence must not cascade away")
}

func TestManager_GCRetiresExpiredV2SessionThroughCleanupOutbox(t *testing.T) {
	t.Parallel()
	store, cfg := helperDB(t)
	cfg.Session.GCScanInterval = time.Hour
	ctx := context.Background()
	manager, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer func() { require.NoError(t, manager.Close()) }()

	now := time.Now()
	info := expiredLifecycleSession("sess-manager-lifecycle-expired", now)
	require.NoError(t, store.Upsert(ctx, info))
	loaded, err := manager.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, config.LifecyclePolicyV2, loaded.LifecyclePolicy)

	manager.gc(ctx)

	stored, err := store.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, events.StateDeleted, stored.State)
	require.NotNil(t, stored.DeletedAt)
	require.Empty(t, stored.Title)
}
