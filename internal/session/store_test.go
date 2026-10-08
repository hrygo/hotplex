package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/pkg/events"
)

func helperDB(t *testing.T) (*SQLiteStore, *config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.DB.Path = filepath.Join(t.TempDir(), "test.db")
	cfg.DB.SQLite.Path = cfg.DB.Path
	cfg.DB.WALMode = true

	store, err := NewSQLiteStore(context.Background(), cfg, sqlutil.NewWriteMu(sqlutil.DialectSQLite))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store, cfg
}

func helperUpsert(t *testing.T, store *SQLiteStore, id, userID string, state events.SessionState) {
	t.Helper()
	now := time.Now()
	err := store.Upsert(context.Background(), &SessionInfo{
		ID:         id,
		UserID:     userID,
		WorkerType: "claude_code",
		State:      state,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	require.NoError(t, err)
}

// ─── SQLiteStore: DeletePhysical ─────────────────────────────────────────────

func TestSQLiteStore_DeletePhysical(t *testing.T) {
	store, _ := helperDB(t)
	ctx := context.Background()

	helperUpsert(t, store, "sess_del_phys", "user1", events.StateTerminated)

	err := store.DeletePhysical(ctx, "sess_del_phys")
	require.NoError(t, err)

	_, err = store.Get(ctx, "sess_del_phys")
	require.Error(t, err)
}

func TestSQLiteStore_DeletePhysical_NotFound(t *testing.T) {
	store, _ := helperDB(t)

	err := store.DeletePhysical(context.Background(), "nonexistent")
	require.NoError(t, err)
}

func TestSQLiteStore_SetPermissionCeilingIfEmpty_IsImmutableAcrossStores(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg := config.Default()
	cfg.DB.Path = filepath.Join(t.TempDir(), "permission-ceiling.db")
	cfg.DB.SQLite.Path = cfg.DB.Path

	store1, err := NewSQLiteStore(ctx, cfg, sqlutil.NewWriteMu(sqlutil.DialectSQLite))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store1.Close() })
	store2, err := NewSQLiteStore(ctx, cfg, sqlutil.NewWriteMu(sqlutil.DialectSQLite))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store2.Close() })

	helperUpsert(t, store1, "sess_ceiling", "user1", events.StateCreated)
	stored, err := store1.SetPermissionCeilingIfEmpty(ctx, "sess_ceiling", "workspace")
	require.NoError(t, err)
	require.Equal(t, "workspace", stored)

	stored, err = store2.SetPermissionCeilingIfEmpty(ctx, "sess_ceiling", "bypass")
	require.NoError(t, err)
	require.Equal(t, "workspace", stored, "a second store instance must not widen the first ceiling")

	got, err := store2.Get(ctx, "sess_ceiling")
	require.NoError(t, err)
	require.Equal(t, "workspace", got.PermissionCeiling)
}

// ─── SQLiteStore: Compact ────────────────────────────────────────────────────

func TestSQLiteStore_Compact_BelowThreshold(t *testing.T) {
	store, _ := helperDB(t)
	ctx := context.Background()

	err := store.Compact(ctx, 0.99)
	require.NoError(t, err)
}

// ─── SQLiteStore: Upsert with Context and PlatformKey ────────────────────────

func TestSQLiteStore_Upsert_WithContext(t *testing.T) {
	store, _ := helperDB(t)
	ctx := context.Background()

	info := &SessionInfo{
		ID:         "sess_ctx",
		UserID:     "user1",
		WorkerType: "claude_code",
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
		Context:    map[string]any{"thread_id": "1234.56", "channel": "C123"},
		PlatformKey: map[string]string{
			"team_id":    "T123",
			"channel_id": "C123",
			"thread_ts":  "1234.56",
			"user_id":    "U123",
		},
	}
	err := store.Upsert(ctx, info)
	require.NoError(t, err)

	got, err := store.Get(ctx, "sess_ctx")
	require.NoError(t, err)
	require.Equal(t, "user1", got.UserID)

	ctxJSON, _ := json.Marshal(got.Context)
	require.Contains(t, string(ctxJSON), "thread_id")

	require.NotNil(t, got.PlatformKey)
	require.Equal(t, "T123", got.PlatformKey["team_id"])
}

func TestSQLiteStore_Upsert_SessionLifecycleFields(t *testing.T) {
	t.Parallel()

	store, _ := helperDB(t)
	ctx := t.Context()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	info := &SessionInfo{
		ID:                      "sess_lifecycle",
		UserID:                  "user1",
		WorkerType:              "claude_code",
		State:                   events.StateCreated,
		CreatedAt:               now,
		UpdatedAt:               now,
		LifecyclePolicy:         "v2",
		LifecyclePolicyRevision: "rev-1",
		RuntimeFinishedAt:       ptr(now.Add(-time.Hour)),
		ArchiveAt:               ptr(now.Add(7 * 24 * time.Hour)),
		ConversationExpiresAt:   ptr(now.Add(180 * 24 * time.Hour)),
		LastContentExpiresAt:    ptr(now.Add(181 * 24 * time.Hour)),
		HistoryExpiresAt:        ptr(now.Add(181 * 24 * time.Hour)),
		LastInputAt:             ptr(now),
	}
	require.NoError(t, store.Upsert(ctx, info))

	got, err := store.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, info.LifecyclePolicy, got.LifecyclePolicy)
	require.Equal(t, info.LifecyclePolicyRevision, got.LifecyclePolicyRevision)
	require.Equal(t, info.LastInputAt, got.LastInputAt)
	require.Equal(t, info.RuntimeFinishedAt, got.RuntimeFinishedAt)
	require.Equal(t, info.ArchiveAt, got.ArchiveAt)
	require.Equal(t, info.ConversationExpiresAt, got.ConversationExpiresAt)
	require.Equal(t, info.LastContentExpiresAt, got.LastContentExpiresAt)
	require.Equal(t, info.HistoryExpiresAt, got.HistoryExpiresAt)

	// General stale Upsert snapshots must not overwrite lifecycle deadlines
	// written by the dedicated lifecycle update path.
	future := now.Add(365 * 24 * time.Hour)
	_, err = store.db.ExecContext(ctx,
		`UPDATE sessions SET conversation_expires_at = ?, history_expires_at = ? WHERE id = ?`,
		future, future, info.ID)
	require.NoError(t, err)
	require.NoError(t, store.Upsert(ctx, info))

	got, err = store.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, future, *got.ConversationExpiresAt)
	require.Equal(t, future, *got.HistoryExpiresAt)
}

// ─── SQLiteStore: List with pagination ───────────────────────────────────────

func TestSQLiteStore_List_DefaultLimit(t *testing.T) {
	store, _ := helperDB(t)
	ctx := context.Background()

	helperUpsert(t, store, "sess_list1", "user1", events.StateRunning)
	helperUpsert(t, store, "sess_list2", "user1", events.StateIdle)

	// limit=0 should default to 100
	sessions, err := store.List(ctx, "", "", "", 0, 0)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(sessions), 2)
}

// Regression: an empty result must serialize to JSON `[]`, never `null`.
// The webchat frontend calls .filter() directly on the `sessions` field of
// the list response; a Go nil slice marshals to `null` and crashes the UI
// ("Cannot read properties of null (reading 'filter')"), which prevented
// auto-creating a default session for an empty workspace.
func TestSQLiteStore_List_EmptyMarshalsToArrayNotNull(t *testing.T) {
	store, _ := helperDB(t)
	ctx := context.Background()

	sessions, err := store.List(ctx, "user_with_no_sessions", "", "", 20, 0)
	require.NoError(t, err)
	require.NotNil(t, sessions, "List must return non-nil slice so JSON is [] not null (frontend does resp.sessions.filter)")
	b, err := json.Marshal(sessions)
	require.NoError(t, err)
	require.Equal(t, "[]", string(b))
}

// ─── SQLiteStore: GetExpiredMaxLifetime / GetExpiredIdle ──────────────────────

func TestSQLiteStore_GetExpiredMaxLifetime(t *testing.T) {
	store, _ := helperDB(t)
	ctx := context.Background()

	now := time.Now()
	info := &SessionInfo{
		ID:         "sess_expired",
		UserID:     "user1",
		WorkerType: "claude_code",
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  &now,
	}
	err := store.Upsert(ctx, info)
	require.NoError(t, err)

	ids, err := store.GetExpiredMaxLifetime(ctx, now.Add(time.Second))
	require.NoError(t, err)
	require.Contains(t, ids, "sess_expired")
}

func TestSQLiteStore_GetExpiredIdle(t *testing.T) {
	store, _ := helperDB(t)
	ctx := context.Background()

	past := time.Now().Add(-2 * time.Hour)
	info := &SessionInfo{
		ID:            "sess_idle_exp",
		UserID:        "user1",
		WorkerType:    "claude_code",
		State:         events.StateIdle,
		CreatedAt:     past,
		UpdatedAt:     past,
		IdleExpiresAt: &past,
	}
	err := store.Upsert(ctx, info)
	require.NoError(t, err)

	ids, err := store.GetExpiredIdle(ctx, time.Now())
	require.NoError(t, err)
	require.Contains(t, ids, "sess_idle_exp")
}

// ─── SQLiteStore: DeleteTerminated ───────────────────────────────────────────

func TestSQLiteStore_DeleteTerminated(t *testing.T) {
	store, _ := helperDB(t)
	ctx := context.Background()

	now := time.Now()
	// Cron session: terminated 25h ago → should be deleted (cutoff 24h)
	require.NoError(t, store.Upsert(ctx, &SessionInfo{
		ID: "cron_old", UserID: "u1", WorkerType: "claude_code",
		State: events.StateTerminated, Source: SourceCron,
		CreatedAt: now.Add(-25 * time.Hour), UpdatedAt: now.Add(-25 * time.Hour),
	}))
	// Normal session: terminated 8d ago → should be deleted (cutoff 7d)
	require.NoError(t, store.Upsert(ctx, &SessionInfo{
		ID: "normal_old", UserID: "u1", WorkerType: "claude_code",
		State:     events.StateTerminated,
		CreatedAt: now.Add(-8 * 24 * time.Hour), UpdatedAt: now.Add(-8 * 24 * time.Hour),
	}))
	// Cron session: terminated 12h ago → should survive (cutoff 24h)
	require.NoError(t, store.Upsert(ctx, &SessionInfo{
		ID: "cron_recent", UserID: "u1", WorkerType: "claude_code",
		State: events.StateTerminated, Source: SourceCron,
		CreatedAt: now.Add(-12 * time.Hour), UpdatedAt: now.Add(-12 * time.Hour),
	}))
	// Normal session: terminated 3d ago → should survive (cutoff 7d)
	require.NoError(t, store.Upsert(ctx, &SessionInfo{
		ID: "normal_recent", UserID: "u1", WorkerType: "claude_code",
		State:     events.StateTerminated,
		CreatedAt: now.Add(-3 * 24 * time.Hour), UpdatedAt: now.Add(-3 * 24 * time.Hour),
	}))

	cronCutoff := now.Add(-24 * time.Hour)
	defaultCutoff := now.Add(-7 * 24 * time.Hour)
	deleted, err := store.DeleteTerminated(ctx, cronCutoff, defaultCutoff)
	require.NoError(t, err)
	require.Len(t, deleted, 2)

	_, err = store.Get(ctx, "cron_old")
	require.ErrorIs(t, err, ErrSessionNotFound, "old cron session should be deleted")
	_, err = store.Get(ctx, "normal_old")
	require.ErrorIs(t, err, ErrSessionNotFound, "old normal session should be deleted")

	_, err = store.Get(ctx, "cron_recent")
	require.NoError(t, err, "recent cron session should survive")
	_, err = store.Get(ctx, "normal_recent")
	require.NoError(t, err, "recent normal session should survive")
}

func TestSQLiteStore_DeleteTerminated_PreservesLifecycleV2(t *testing.T) {
	store, _ := helperDB(t)
	ctx := t.Context()
	now := time.Now()
	require.NoError(t, store.Upsert(ctx, &SessionInfo{
		ID: "v2_old_terminated", UserID: "u1", WorkerType: "claude_code",
		State: events.StateTerminated, LifecyclePolicy: config.LifecyclePolicyV2,
		CreatedAt: now.Add(-20 * 24 * time.Hour), UpdatedAt: now.Add(-8 * 24 * time.Hour),
		ArchiveAt:             ptr(now.Add(-13 * 24 * time.Hour)),
		ConversationExpiresAt: ptr(now.Add(160 * 24 * time.Hour)),
		HistoryExpiresAt:      ptr(now.Add(160 * 24 * time.Hour)),
	}))

	_, err := store.DeleteTerminated(ctx, now.Add(-24*time.Hour), now.Add(-7*24*time.Hour))
	require.NoError(t, err)

	_, err = store.Get(ctx, "v2_old_terminated")
	require.NoError(t, err, "v2 sessions must not use the legacy terminated-row cutoff")
}

// ─── SQLiteStore: GetSessionsByState ─────────────────────────────────────────

func TestSQLiteStore_GetSessionsByState(t *testing.T) {
	store, _ := helperDB(t)
	ctx := context.Background()

	helperUpsert(t, store, "sess_state_r", "user1", events.StateRunning)
	helperUpsert(t, store, "sess_state_i", "user1", events.StateIdle)

	ids, err := store.GetSessionsByState(ctx, events.StateRunning)
	require.NoError(t, err)
	require.Contains(t, ids, "sess_state_r")
	require.NotContains(t, ids, "sess_state_i")
}
