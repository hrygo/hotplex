package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/pkg/events"
)

func init() {
	// Ensure SQLite driver is registered.
	_ = sqlutil.DriverName
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewIndependentStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// Create the events table (normally done by goose migration 002).
	_, err = store.db.Exec(`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL,
		direction TEXT NOT NULL DEFAULT 'outbound',
		source TEXT NOT NULL DEFAULT 'normal'
			CHECK(source IN ('normal', 'crash', 'timeout', 'fresh_start')),
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL DEFAULT 0
	)`)
	require.NoError(t, err)
	_, err = store.db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		state TEXT NOT NULL DEFAULT '',
		lifecycle_policy TEXT,
		last_content_expires_at DATETIME,
		history_expires_at DATETIME,
		deleted_at DATETIME
	)`)
	require.NoError(t, err)
	_, err = store.db.Exec(`CREATE TABLE IF NOT EXISTS session_purge_jobs (
		session_id TEXT PRIMARY KEY
	)`)
	require.NoError(t, err)
	return store
}

func TestSQLiteStore_AppendAndQuery(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Append events
	for i := int64(1); i <= 5; i++ {
		err := store.Append(ctx, &StoredEvent{
			SessionID: "sess1",
			Seq:       i,
			Type:      "message",
			Data:      json.RawMessage(`{"content":"hello"}`),
			Direction: "outbound",
			Source:    SourceNormal,
			CreatedAt: time.Now().UnixMilli(),
		})
		require.NoError(t, err)
	}

	t.Run("cursor latest", func(t *testing.T) {
		page, err := store.QueryBySession(ctx, "sess1", 0, CursorLatest, 3)
		require.NoError(t, err)
		require.Len(t, page.Events, 3)
		require.Equal(t, int64(3), page.OldestID)
		require.Equal(t, int64(5), page.NewestID)
		require.Equal(t, int64(3), page.OldestSeq)
		require.Equal(t, int64(5), page.NewestSeq)
		require.True(t, page.HasOlder)
	})

	t.Run("cursor after", func(t *testing.T) {
		page, err := store.QueryBySession(ctx, "sess1", 3, CursorAfter, 10)
		require.NoError(t, err)
		require.Len(t, page.Events, 2) // seq 4, 5
		require.Equal(t, int64(4), page.OldestSeq)
		// HasOlder checks if events older than oldest exist — they do (seq 1-3)
		require.True(t, page.HasOlder)
	})

	t.Run("cursor before", func(t *testing.T) {
		page, err := store.QueryBySession(ctx, "sess1", 3, CursorBefore, 10)
		require.NoError(t, err)
		require.Len(t, page.Events, 2) // seq 1, 2
		require.Equal(t, int64(1), page.OldestSeq)
		require.False(t, page.HasOlder)
	})

	t.Run("nonexistent session", func(t *testing.T) {
		_, err := store.QueryBySession(ctx, "no-such-session", 0, CursorLatest, 10)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestSQLiteStore_QueryBySessionUsesIDCursor(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()

	// Simulate legacy reconnect history: insertion order is authoritative even
	// though the in-memory seq counter previously reset from 3 back to 1.
	for _, seq := range []int64{3, 1, 2} {
		require.NoError(t, store.Append(ctx, &StoredEvent{
			SessionID: "legacy", Seq: seq, Type: "message",
			Data: raw(`{}`), Direction: "outbound", Source: SourceNormal,
			CreatedAt: time.Now().UnixMilli(),
		}))
	}

	latest, err := store.QueryBySession(ctx, "legacy", 0, CursorLatest, 2)
	require.NoError(t, err)
	require.Equal(t, []int64{2, 3}, []int64{latest.Events[0].ID, latest.Events[1].ID})
	require.Equal(t, []int64{1, 2}, []int64{latest.Events[0].Seq, latest.Events[1].Seq})
	require.True(t, latest.HasOlder)

	before, err := store.QueryBySession(ctx, "legacy", latest.OldestID, CursorBefore, 2)
	require.NoError(t, err)
	require.Len(t, before.Events, 1)
	require.Equal(t, int64(1), before.Events[0].ID)
	require.Equal(t, int64(3), before.Events[0].Seq)

	after, err := store.QueryBySession(ctx, "legacy", latest.OldestID, CursorAfter, 2)
	require.NoError(t, err)
	require.Len(t, after.Events, 1)
	require.Equal(t, int64(3), after.Events[0].ID)
}

func TestSQLiteStore_DeleteBySession(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.Append(ctx, &StoredEvent{
		SessionID: "sess-del", Seq: 1, Type: "message",
		Data: json.RawMessage(`{}`), Direction: "outbound", Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
	})
	require.NoError(t, err)

	err = store.DeleteBySession(ctx, "sess-del")
	require.NoError(t, err)

	_, err = store.QueryBySession(ctx, "sess-del", 0, CursorLatest, 10)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSQLiteStore_RejectsAppendsAfterSessionDeletion(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.db.ExecContext(ctx, `INSERT INTO sessions (id, state) VALUES (?, 'deleted')`, "sess-deleted")
	require.NoError(t, err)
	err = store.Append(ctx, &StoredEvent{
		SessionID: "sess-deleted", Seq: 1, Type: "message",
		Data: raw(`{"content":"late"}`), Direction: "outbound",
		Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
	})
	require.ErrorIs(t, err, ErrSessionDeleted)

	// The purge job remains as a durable tombstone after session metadata is
	// physically removed, so a delayed writer cannot recreate conversation data.
	_, err = store.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, "sess-deleted")
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `INSERT INTO session_purge_jobs (session_id) VALUES (?)`, "sess-deleted")
	require.NoError(t, err)
	err = store.Append(ctx, &StoredEvent{
		SessionID: "sess-deleted", Seq: 2, Type: "message",
		Data: raw(`{"content":"late"}`), Direction: "outbound",
		Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
	})
	require.ErrorIs(t, err, ErrSessionDeleted)

	_, err = store.QueryBySession(ctx, "sess-deleted", 0, CursorLatest, 10)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSQLiteStore_LatestSeq(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()

	// Empty session → 0.
	seq, err := store.LatestSeq(ctx, "empty")
	require.NoError(t, err)
	require.Equal(t, int64(0), seq)

	// Append events with seq 1,2,3 → MAX=3.
	for i := int64(1); i <= 3; i++ {
		require.NoError(t, store.Append(ctx, &StoredEvent{
			SessionID: "sess1", Seq: i, Type: "message",
			Data: json.RawMessage(`{}`), Direction: "outbound",
			Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
		}))
	}
	seq, err = store.LatestSeq(ctx, "sess1")
	require.NoError(t, err)
	require.Equal(t, int64(3), seq)

	// Different session is isolated; high seq does not leak across sessions.
	require.NoError(t, store.Append(ctx, &StoredEvent{
		SessionID: "sess2", Seq: 99, Type: "message",
		Data: json.RawMessage(`{}`), Direction: "outbound",
		Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
	}))
	seq, err = store.LatestSeq(ctx, "sess2")
	require.NoError(t, err)
	require.Equal(t, int64(99), seq)

	// sess1 still 3 (unaffected by sess2).
	seq, err = store.LatestSeq(ctx, "sess1")
	require.NoError(t, err)
	require.Equal(t, int64(3), seq)
}

func TestSQLiteStore_DeleteExpired(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UnixMilli()
	err := store.Append(ctx, &StoredEvent{
		SessionID: "sess-exp", Seq: 1, Type: "message",
		Data: json.RawMessage(`{}`), Direction: "outbound", Source: SourceNormal, CreatedAt: now - 86400000, // 1 day ago
	})
	require.NoError(t, err)

	err = store.Append(ctx, &StoredEvent{
		SessionID: "sess-exp", Seq: 2, Type: "message",
		Data: json.RawMessage(`{}`), Direction: "outbound", Source: SourceNormal, CreatedAt: now,
	})
	require.NoError(t, err)

	deleted, err := store.DeleteExpired(ctx, time.Now().Add(-12*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	// Recent event should remain
	page, err := store.QueryBySession(ctx, "sess-exp", 0, CursorLatest, 10)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	require.Equal(t, int64(2), page.Events[0].Seq)
}

func TestSQLiteStore_PerRecordContentExpiry(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	store.retention = RetentionPolicy{
		Content: 10 * 24 * time.Hour,
		Legacy:  30 * 24 * time.Hour,
	}
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)

	_, err := store.db.ExecContext(ctx,
		`INSERT INTO sessions (id, lifecycle_policy) VALUES (?, 'v2')`, "s1")
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO sessions (id, lifecycle_policy) VALUES (?, 'legacy')`, "s-legacy")
	require.NoError(t, err)

	// New rows receive an immutable deadline from their creation time.
	require.NoError(t, store.Append(ctx, &StoredEvent{
		SessionID: "s1", Seq: 1, Type: "message", Data: json.RawMessage(`{}`),
		Direction: "outbound", Source: SourceNormal, CreatedAt: now.Add(-11 * 24 * time.Hour).UnixMilli(),
	}))
	require.NoError(t, store.Append(ctx, &StoredEvent{
		SessionID: "s1", Seq: 2, Type: "message", Data: json.RawMessage(`{}`),
		Direction: "outbound", Source: SourceNormal, CreatedAt: now.Add(-9 * 24 * time.Hour).UnixMilli(),
	}))

	// Rows that predate the per-record policy keep the legacy zero deadline.
	_, err = store.db.ExecContext(ctx, `INSERT INTO events
		(session_id, seq, type, data, direction, source, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		"s1", 3, "message", []byte(`{}`), "outbound", SourceNormal, now.Add(-31*24*time.Hour).UnixMilli())
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `INSERT INTO events
		(session_id, seq, type, data, direction, source, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		"s1", 4, "message", []byte(`{}`), "outbound", SourceNormal, now.Add(-29*24*time.Hour).UnixMilli())
	require.NoError(t, err)

	var expiresAt int64
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT expires_at FROM events WHERE session_id = ? AND seq = 2`, "s1").Scan(&expiresAt))
	require.Equal(t, now.Add(time.Hour*24).UnixMilli(), expiresAt)
	contentExpiresAt := expiresAt

	require.NoError(t, store.Append(ctx, &StoredEvent{
		SessionID: "s-legacy", Seq: 1, Type: "message", Data: json.RawMessage(`{}`),
		Direction: "outbound", Source: SourceNormal, CreatedAt: now.UnixMilli(),
	}))
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT expires_at FROM events WHERE session_id = ? AND seq = 1`, "s-legacy").Scan(&expiresAt))
	require.Zero(t, expiresAt, "legacy sessions must keep the legacy retention window")

	var lastContentExpiry, historyExpiry sql.NullTime
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT last_content_expires_at, history_expires_at FROM sessions WHERE id = ?`, "s1").
		Scan(&lastContentExpiry, &historyExpiry))
	require.True(t, lastContentExpiry.Valid)
	require.True(t, historyExpiry.Valid)
	require.WithinDuration(t, time.UnixMilli(contentExpiresAt), lastContentExpiry.Time, time.Millisecond)
	require.WithinDuration(t, time.UnixMilli(contentExpiresAt), historyExpiry.Time, time.Millisecond)

	// Reconfiguring retention does not rewrite deadlines already assigned.
	store.retention.Content = 24 * time.Hour
	page, err := store.QueryBySession(ctx, "s1", 0, CursorLatest, 10)
	require.NoError(t, err)
	require.Len(t, page.Events, 2)
	require.Equal(t, []int64{2, 4}, []int64{page.Events[0].Seq, page.Events[1].Seq})

	deleted, err := store.DeleteExpired(ctx, now.Add(-30*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(2), deleted)

	page, err = store.QueryBySession(ctx, "s1", 0, CursorLatest, 10)
	require.NoError(t, err)
	require.Len(t, page.Events, 2)
	require.Equal(t, []int64{2, 4}, []int64{page.Events[0].Seq, page.Events[1].Seq})
}

func TestSQLiteStore_Transaction(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)

	for i := int64(1); i <= 3; i++ {
		err := tx.Append(ctx, &StoredEvent{
			SessionID: "sess-tx", Seq: i, Type: "message",
			Data: json.RawMessage(`{}`), Direction: "outbound", Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
		})
		require.NoError(t, err, "append event seq=%d", i)
	}

	require.NoError(t, tx.Commit())

	page, err := store.QueryBySession(ctx, "sess-tx", 0, CursorLatest, 10)
	require.NoError(t, err)
	require.Len(t, page.Events, 3)
}

func TestSQLiteStore_QueryLimitBounds(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert 5 events
	for i := int64(1); i <= 5; i++ {
		err := store.Append(ctx, &StoredEvent{
			SessionID: "sess-lim", Seq: i, Type: "message",
			Data: json.RawMessage(`{}`), Direction: "outbound", Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
		})
		require.NoError(t, err)
	}

	t.Run("limit 0 uses default", func(t *testing.T) {
		page, err := store.QueryBySession(ctx, "sess-lim", 0, CursorLatest, 0)
		require.NoError(t, err)
		require.Len(t, page.Events, 5) // default 200, we only have 5
	})

	t.Run("limit over 1000 clamped", func(t *testing.T) {
		page, err := store.QueryBySession(ctx, "sess-lim", 0, CursorLatest, 5000)
		require.NoError(t, err)
		require.Len(t, page.Events, 5)
	})
}

func TestIsStorable(t *testing.T) {
	t.Parallel()
	require.True(t, IsStorable(events.Message))
	require.True(t, IsStorable(events.Done))
	require.True(t, IsStorable(events.ToolCall))
	require.False(t, IsStorable(events.MessageDelta))
	require.False(t, IsStorable(events.Kind("unknown")))
}

func TestEventsTable_SourceCheck(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := []struct {
		source string
		ok     bool
	}{
		{SourceNormal, true},
		{SourceCrash, true},
		{SourceTimeout, true},
		{SourceFreshStart, true},
		{"invalid_source", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			err := store.Append(ctx, &StoredEvent{
				SessionID: "sess-check", Seq: 1, Type: "done",
				Data: raw(`{}`), Direction: "outbound", Source: tt.source, CreatedAt: time.Now().UnixMilli(),
			})
			if tt.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestCollector_CaptureAndFlush(t *testing.T) {
	store := newTestStore(t)
	collector := NewCollector(store, slog.Default())

	// Capture storable events
	collector.Capture("sess1", 1, events.Message, json.RawMessage(`{"content":"hello"}`), "outbound", SourceNormal)
	collector.Capture("sess1", 2, events.Done, json.RawMessage(`{}`), "outbound", SourceNormal)

	// Close drains and flushes remaining events synchronously
	require.NoError(t, collector.Close())

	ctx := context.Background()
	page, err := store.QueryBySession(ctx, "sess1", 0, CursorLatest, 100)
	require.NoError(t, err)
	require.Len(t, page.Events, 2)
	require.Equal(t, int64(1), page.Events[0].Seq)
	require.Equal(t, int64(2), page.Events[1].Seq)
}

func TestCollector_DropNonStorable(t *testing.T) {
	store := newTestStore(t)
	collector := NewCollector(store, slog.Default())
	defer func() { _ = collector.Close() }()

	// Non-storable event should be silently dropped
	collector.Capture("sess1", 1, events.Kind("non_storable_type"), json.RawMessage(`{}`), "outbound", SourceNormal)

	require.Eventually(t, func() bool {
		_, err := store.QueryBySession(context.Background(), "sess1", 0, CursorLatest, 10)
		return errors.Is(err, ErrNotFound)
	}, 2*time.Second, 10*time.Millisecond)
}

func TestSqliteTx_DoubleRelease(t *testing.T) {
	store := newTestStoreWithWriteMu(t)
	ctx := context.Background()

	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)

	_ = tx.Commit()
	_ = tx.Rollback()

	// Core assertion: writeMu is NOT deadlocked after Commit+Rollback.
	require.NoError(t, store.writeMu.WithLock(func() error { return nil }))
}

func TestSqliteTx_RollbackThenCommit(t *testing.T) {
	store := newTestStoreWithWriteMu(t)
	ctx := context.Background()

	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)

	_ = tx.Rollback()
	_ = tx.Commit()

	require.NoError(t, store.writeMu.WithLock(func() error { return nil }))
}

func newTestStoreWithWriteMu(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewIndependentStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	store.writeMu = sqlutil.NewWriteMu(sqlutil.DialectSQLite)

	_, err = store.db.Exec(`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL,
		direction TEXT NOT NULL DEFAULT 'outbound',
		source TEXT NOT NULL DEFAULT 'normal'
			CHECK(source IN ('normal', 'crash', 'timeout', 'fresh_start')),
		created_at INTEGER NOT NULL DEFAULT 0,
		expires_at INTEGER NOT NULL DEFAULT 0
	)`)
	require.NoError(t, err)
	_, err = store.db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		state TEXT NOT NULL DEFAULT '',
		lifecycle_policy TEXT,
		last_content_expires_at DATETIME,
		history_expires_at DATETIME,
		deleted_at DATETIME
	)`)
	require.NoError(t, err)
	_, err = store.db.Exec(`CREATE TABLE IF NOT EXISTS session_purge_jobs (session_id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	return store
}
