package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/sqlutil"
)

// testCtx returns a context with a 10s timeout for use with BeginTx.
// This prevents withDefaultTimeout from creating a derived ctx whose cancel
// fires immediately upon BeginTx return, which would roll back the transaction.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func init() {
	_ = sqlutil.DriverName
}

// newTestStoreWithTurnsTable creates a store with events + turns tables.
func newTestStoreWithTurnsTable(t *testing.T) *SQLiteStore {
	t.Helper()
	store := newTestStore(t)

	_, err := store.db.Exec(`CREATE TABLE IF NOT EXISTS turns (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id          TEXT    NOT NULL,
		client_message_id   TEXT,
		generation          INTEGER NOT NULL DEFAULT 1,
		turn_num            INTEGER NOT NULL,
		seq                 INTEGER NOT NULL DEFAULT 0,
		role                TEXT    NOT NULL,
		content             TEXT    NOT NULL DEFAULT '',
		platform            TEXT    NOT NULL DEFAULT '',
		user_id             TEXT    NOT NULL DEFAULT '',
		model               TEXT    NOT NULL DEFAULT '',
		success             INTEGER,
		source              TEXT    NOT NULL DEFAULT 'normal',
		tools_json          TEXT,
		tool_count          INTEGER NOT NULL DEFAULT 0,
		tokens_input        INTEGER NOT NULL DEFAULT 0,
		tokens_cache_write  INTEGER NOT NULL DEFAULT 0,
		tokens_cache_read   INTEGER NOT NULL DEFAULT 0,
		tokens_out          INTEGER NOT NULL DEFAULT 0,
		duration_ms         INTEGER NOT NULL DEFAULT 0,
		cost_usd            REAL    NOT NULL DEFAULT 0.0,
		created_at          INTEGER NOT NULL,
		expires_at          INTEGER NOT NULL DEFAULT 0
	)`)
	require.NoError(t, err)

	_, err = store.db.Exec(`CREATE INDEX IF NOT EXISTS idx_turns_session_gen_id ON turns(session_id, generation, id)`)
	require.NoError(t, err)
	_, err = store.db.Exec(`CREATE INDEX IF NOT EXISTS idx_turns_created ON turns(created_at)`)
	require.NoError(t, err)

	return store
}

func TestSQLiteStore_DeleteConversationBySession(t *testing.T) {
	t.Parallel()
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)
	require.NoError(t, store.Append(ctx, &StoredEvent{
		SessionID: "sess-purge", Seq: 1, Type: "message",
		Data: json.RawMessage(`{"content":"private"}`), Direction: "outbound",
		Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
	}))
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO turns (session_id, turn_num, role, content, created_at) VALUES (?, ?, ?, ?, ?)`,
		"sess-purge", 1, "user", "private", time.Now().UnixMilli())
	require.NoError(t, err)

	require.NoError(t, store.DeleteConversationBySession(ctx, "sess-purge"))

	var eventCount, turnCount int
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE session_id = ?`, "sess-purge").Scan(&eventCount))
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE session_id = ?`, "sess-purge").Scan(&turnCount))
	require.Zero(t, eventCount)
	require.Zero(t, turnCount)
}

func TestSQLiteStore_DeleteConversationBySessionRollsBackTogether(t *testing.T) {
	t.Parallel()
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)
	require.NoError(t, store.Append(ctx, &StoredEvent{
		SessionID: "sess-purge-rollback", Seq: 1, Type: "message",
		Data: json.RawMessage(`{"content":"private"}`), Direction: "outbound",
		Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
	}))
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO turns (session_id, turn_num, role, content, created_at) VALUES (?, ?, ?, ?, ?)`,
		"sess-purge-rollback", 1, "user", "private", time.Now().UnixMilli())
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `CREATE TRIGGER reject_turn_purge BEFORE DELETE ON turns
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
	require.NoError(t, err)

	require.Error(t, store.DeleteConversationBySession(ctx, "sess-purge-rollback"))

	var eventCount, turnCount int
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE session_id = ?`, "sess-purge-rollback").Scan(&eventCount))
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE session_id = ?`, "sess-purge-rollback").Scan(&turnCount))
	require.Equal(t, 1, eventCount)
	require.Equal(t, 1, turnCount)
}

func TestSQLiteStore_RejectsTurnAppendAfterSessionDeletion(t *testing.T) {
	t.Parallel()
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	_, err := store.db.ExecContext(ctx, `INSERT INTO sessions (id, state) VALUES (?, 'deleted')`, "sess-deleted-turn")
	require.NoError(t, err)

	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	err = tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "sess-deleted-turn", Generation: 1, TurnNum: 1, Role: "user",
		Content: "late", CreatedAt: time.Now().UnixMilli(),
	})
	require.ErrorIs(t, err, ErrSessionDeleted)
	require.NoError(t, tx.Rollback())
}

// ─── TURN-001: turns table creation ─────────────────────────────────────────

func TestTurnsTable_Schema(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)

	// Verify turns table exists and has expected columns.
	rows, err := store.db.Query("PRAGMA table_info(turns)")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dfltVal any
		var pk int
		require.NoError(t, rows.Scan(&cid, &name, &typ, &notnull, &dfltVal, &pk))
		columns[name] = true
	}
	require.NoError(t, rows.Err())

	expected := []string{
		"id", "session_id", "client_message_id", "generation", "turn_num", "seq", "role", "content",
		"platform", "user_id", "model", "success", "source", "tools_json",
		"tool_count", "tokens_input", "tokens_cache_write", "tokens_cache_read",
		"tokens_out", "duration_ms", "cost_usd", "created_at",
	}
	for _, col := range expected {
		require.True(t, columns[col], "missing column: %s", col)
	}
}

// ─── TURN-002: turns table INSERT correctness ───────────────────────────────

func TestTurnsTable_InsertCorrectness(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	t.Run("AppendTurn basic insert", func(t *testing.T) {
		tx, err := store.BeginTx(ctx)
		require.NoError(t, err)
		success := true
		tools := `{"Read":2,"Bash":1}`
		err = tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s1", Generation: 1, TurnNum: 1, Seq: 10,
			Role: "assistant", Content: "hello world",
			Platform: "feishu", UserID: "u1", Model: "claude-3",
			Success: &success, Source: SourceNormal,
			ToolsJSON: tools, ToolCount: 3,
			TokensInput: 100, TokensCacheWrite: 50, TokensCacheRead: 30,
			TokensOut: 200, DurationMs: 5000, CostUSD: 0.042,
			CreatedAt: time.Now().UnixMilli(),
		})
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		records, err := store.QueryTurns(ctx, "s1", 10, 0)
		require.NoError(t, err)
		require.Len(t, records, 1)
		r := records[0]
		require.Equal(t, "assistant", r.Role)
		require.Equal(t, "hello world", r.Content)
		require.Equal(t, "feishu", r.Platform)
		require.Equal(t, "u1", r.UserID)
		require.Equal(t, "claude-3", r.Model)
		require.NotNil(t, r.Success)
		require.True(t, *r.Success)
		require.Equal(t, 2, r.Tools["Read"])
		require.Equal(t, 3, r.ToolCount)
		require.Equal(t, int64(100), r.TokensInput)
		require.Equal(t, int64(50), r.TokensCacheWrite)
		require.Equal(t, int64(30), r.TokensCacheRead)
		require.Equal(t, int64(180), r.TokensIn) // 100+50+30
		require.Equal(t, int64(200), r.TokensOut)
		require.Equal(t, int64(5000), r.DurationMs)
		require.InDelta(t, 0.042, r.CostUSD, 0.001)
	})

	t.Run("success NULL for user turns", func(t *testing.T) {
		tx, err := store.BeginTx(ctx)
		require.NoError(t, err)
		err = tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s2", Generation: 1, TurnNum: 1, Seq: 1,
			Role: "user", Content: "hello",
			Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
		})
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		records, err := store.QueryTurns(ctx, "s2", 10, 0)
		require.NoError(t, err)
		require.Len(t, records, 1)
		require.Nil(t, records[0].Success)
	})

	t.Run("tools_json round-trip", func(t *testing.T) {
		tx, err := store.BeginTx(ctx)
		require.NoError(t, err)
		success := true
		err = tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s3", Generation: 1, TurnNum: 1, Seq: 1,
			Role: "assistant", Content: "result",
			Success: &success, Source: SourceNormal,
			ToolsJSON: `{"Read":2,"Bash":1}`, ToolCount: 3,
			CreatedAt: time.Now().UnixMilli(),
		})
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		records, err := store.QueryTurns(ctx, "s3", 10, 0)
		require.NoError(t, err)
		require.Len(t, records, 1)
		require.Equal(t, map[string]int{"Read": 2, "Bash": 1}, records[0].Tools)
	})

	t.Run("id strictly increasing", func(t *testing.T) {
		tx, err := store.BeginTx(ctx)
		require.NoError(t, err)
		for i := 0; i < 3; i++ {
			err = tx.AppendTurn(ctx, &TurnWriteRequest{
				SessionID: "s4", Generation: 1, TurnNum: i + 1, Seq: int64(i + 1),
				Role: "user", Content: "msg",
				Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
			})
			require.NoError(t, err)
		}
		require.NoError(t, tx.Commit())

		records, err := store.QueryTurns(ctx, "s4", 10, 0)
		require.NoError(t, err)
		require.Len(t, records, 3)
		require.Less(t, records[0].ID, records[1].ID)
		require.Less(t, records[1].ID, records[2].ID)
	})
}

// ─── TURN-003: Collector dual-channel ──────────────────────────────────────

func TestTurnsTable_CollectorDualChannel(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)
	c := NewCollector(store, slog.Default())

	// Capture event and turn in same batch.
	c.Capture("s1", 1, "input", json.RawMessage(`{"content":"hello"}`), "inbound", SourceNormal)
	success := true
	c.CaptureTurn(&TurnWriteRequest{
		SessionID: "s1", Generation: 1, TurnNum: 1, Seq: 2,
		Role: "assistant", Content: "hi there",
		Success: &success, Source: SourceNormal,
		TokensInput: 100, TokensCacheWrite: 50, TokensCacheRead: 30,
		TokensOut: 200, DurationMs: 5000, CostUSD: 0.01,
		CreatedAt: time.Now().UnixMilli(),
	})
	c.Capture("s1", 3, "done", json.RawMessage(`{"success":true}`), "outbound", SourceNormal)
	require.NoError(t, c.Close())

	// Verify events stored.
	page, err := store.QueryBySession(ctx, "s1", 0, CursorLatest, 100)
	require.NoError(t, err)
	require.Len(t, page.Events, 2) // input + done

	// Verify turns stored.
	records, err := store.QueryTurns(ctx, "s1", 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "assistant", records[0].Role)
	require.Equal(t, "hi there", records[0].Content)
}

// ─── TURN-007: User/Assistant turn strict ordering ──────────────────────────

func TestTurnsTable_UserAssistantOrdering(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)
	c := NewCollector(store, slog.Default())

	now := time.Now().UnixMilli()
	// 3 complete turns: user → assistant pairs.
	for i := 0; i < 3; i++ {
		c.CaptureTurn(&TurnWriteRequest{
			SessionID: "s1", Generation: 1, TurnNum: i + 1, Seq: int64(i*2 + 1),
			Role: "user", Content: "question",
			Source: SourceNormal, CreatedAt: now + int64(i*1000),
		})
		success := true
		c.CaptureTurn(&TurnWriteRequest{
			SessionID: "s1", Generation: 1, TurnNum: i + 1, Seq: int64(i*2 + 2),
			Role: "assistant", Content: "answer",
			Success: &success, Source: SourceNormal,
			CreatedAt: now + int64(i*1000) + 100,
		})
	}
	require.NoError(t, c.Close())

	records, err := store.QueryTurns(ctx, "s1", 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 6)

	// Verify strict ordering: user/assistant interleaved.
	roles := make([]string, len(records))
	for i, r := range records {
		roles[i] = r.Role
	}
	require.Equal(t, []string{"user", "assistant", "user", "assistant", "user", "assistant"}, roles)

	// Verify IDs strictly increasing.
	for i := 1; i < len(records); i++ {
		require.Less(t, records[i-1].ID, records[i].ID, "turn IDs must be strictly increasing")
	}
}

// ─── TURN-008/027: Generation initialization ───────────────────────────────

func TestTurnsTable_LatestGeneration(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	t.Run("no turns returns 0", func(t *testing.T) {
		gen, err := store.LatestGeneration(ctx, "s-new")
		require.NoError(t, err)
		require.Equal(t, int64(0), gen)
	})

	t.Run("single generation returns 1", func(t *testing.T) {
		tx, err := store.BeginTx(ctx)
		require.NoError(t, err)
		require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s1", Generation: 1, TurnNum: 1, Role: "user",
			Content: "hi", Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
		}))
		require.NoError(t, tx.Commit())

		gen, err := store.LatestGeneration(ctx, "s1")
		require.NoError(t, err)
		require.Equal(t, int64(1), gen)
	})

	t.Run("multiple generations returns max", func(t *testing.T) {
		tx, err := store.BeginTx(ctx)
		require.NoError(t, err)
		for _, gen := range []int64{1, 2, 3} {
			require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
				SessionID: "s2", Generation: gen, TurnNum: 1, Role: "user",
				Content: "hi", Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
			}))
		}
		require.NoError(t, tx.Commit())

		gen, err := store.LatestGeneration(ctx, "s2")
		require.NoError(t, err)
		require.Equal(t, int64(3), gen)
	})
}

// ─── TURN-010/012: Generation reset + turn_num ─────────────────────────────

func TestTurnsTable_GenerationReset(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	// Generation 1: 3 turns.
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s1", Generation: 1, TurnNum: i + 1, Seq: int64(i + 1),
			Role: "user", Content: "q",
			Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
		}))
	}
	require.NoError(t, tx.Commit())

	// Generation 2: 2 turns (after reset).
	tx, err = store.BeginTx(ctx)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s1", Generation: 2, TurnNum: i + 1, Seq: int64(i + 10),
			Role: "user", Content: "q2",
			Source: SourceNormal, CreatedAt: time.Now().UnixMilli() + 100000,
		}))
	}
	require.NoError(t, tx.Commit())

	t.Run("QueryTurns returns latest generation only", func(t *testing.T) {
		records, err := store.QueryTurns(ctx, "s1", 10, 0)
		require.NoError(t, err)
		require.Len(t, records, 2)
		for _, r := range records {
			require.Equal(t, int64(2), r.Generation)
			require.Equal(t, "s1", r.SessionID)
		}
	})

	t.Run("turn_num resets within generation", func(t *testing.T) {
		records, err := store.QueryTurns(ctx, "s1", 10, 0)
		require.NoError(t, err)
		require.Equal(t, 1, records[0].TurnNum)
		require.Equal(t, 2, records[1].TurnNum)
	})
}

// ─── TURN-013: QueryTurns default latest generation ────────────────────────

func TestTurnsTable_QueryTurnsDefaultGeneration(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	// Only generation=1 data.
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 1, TurnNum: 1, Role: "user",
		Content: "q", Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
	}))
	require.NoError(t, tx.Commit())

	records, err := store.QueryTurns(ctx, "s1", 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, int64(1), records[0].Generation)

	// No turns for unknown session.
	_, err = store.QueryTurns(ctx, "s-unknown", 10, 0)
	require.ErrorIs(t, err, ErrNotFound)
}

// ─── TURN-014: QueryTurnsBefore cross-generation ───────────────────────────

func TestTurnsTable_QueryTurnsBeforeCrossGeneration(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	// Generation 1: 2 turns.
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 1, TurnNum: 1, Role: "user",
		Content: "g1-1", Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
	}))
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 1, TurnNum: 2, Role: "user",
		Content: "g1-2", Source: SourceNormal, CreatedAt: time.Now().UnixMilli() + 1,
	}))
	require.NoError(t, tx.Commit())

	// Generation 2: 2 turns.
	tx, err = store.BeginTx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 2, TurnNum: 1, Role: "user",
		Content: "g2-1", Source: SourceNormal, CreatedAt: time.Now().UnixMilli() + 2,
	}))
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 2, TurnNum: 2, Role: "user",
		Content: "g2-2", Source: SourceNormal, CreatedAt: time.Now().UnixMilli() + 3,
	}))
	require.NoError(t, tx.Commit())

	// Get all turns to find IDs.
	all, err := store.QueryTurns(ctx, "s1", 10, 0)
	require.NoError(t, err)

	// all contains only gen=2 (2 records). Get gen=1 via cursor.
	// Use the ID of the first gen=2 turn as cursor.
	gen2FirstID := all[0].ID

	// QueryTurnsBefore with cursor = gen2FirstID should return gen=1 records.
	before, err := store.QueryTurnsBefore(ctx, "s1", gen2FirstID, 10)
	require.NoError(t, err)
	require.Len(t, before, 2)
	for _, r := range before {
		require.Equal(t, int64(1), r.Generation)
	}

	// QueryTurnsBefore beyond all data returns ErrNotFound.
	_, err = store.QueryTurnsBefore(ctx, "s1", 1, 10)
	require.ErrorIs(t, err, ErrNotFound)
}

// ─── TURN-015: QueryTurnStats aggregation ──────────────────────────────────

func TestTurnsTable_QueryTurnStats(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	// Generation 2: 3 assistant turns.
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)

	// Generation 1: 1 assistant turn (should NOT appear in stats).
	s1 := true
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 1, TurnNum: 1, Seq: 1,
		Role: "assistant", Content: "old",
		Success: &s1, Source: SourceNormal,
		TokensInput: 999, TokensOut: 999, DurationMs: 999, CostUSD: 9.99,
		CreatedAt: time.Now().UnixMilli(),
	}))

	// User turns (should NOT appear in stats).
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 2, TurnNum: 1, Seq: 10,
		Role: "user", Content: "q",
		Source: SourceNormal, CreatedAt: time.Now().UnixMilli() + 100,
	}))

	// Generation 2 assistant turns.
	success := true
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 2, TurnNum: 1, Seq: 11,
		Role: "assistant", Content: "a1", Model: "claude-3",
		Success: &success, Source: SourceNormal,
		TokensInput: 100, TokensCacheWrite: 50, TokensCacheRead: 30,
		TokensOut: 200, DurationMs: 500, CostUSD: 0.01,
		CreatedAt: time.Now().UnixMilli() + 101,
	}))

	failed := false
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 2, TurnNum: 2, Seq: 12,
		Role: "assistant", Content: "a2", Model: "claude-3",
		Success: &failed, Source: SourceCrash,
		TokensInput: 200, TokensCacheWrite: 30, TokensCacheRead: 0,
		TokensOut: 400, DurationMs: 600, CostUSD: 0.02,
		CreatedAt: time.Now().UnixMilli() + 102,
	}))

	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 2, TurnNum: 3, Seq: 13,
		Role: "assistant", Content: "a3", Model: "sonnet",
		Success: &success, Source: SourceNormal,
		TokensInput: 50, TokensCacheWrite: 0, TokensCacheRead: 0,
		TokensOut: 100, DurationMs: 300, CostUSD: 0.005,
		CreatedAt: time.Now().UnixMilli() + 103,
	}))

	require.NoError(t, tx.Commit())

	stats, err := store.QueryTurnStats(ctx, "s1")
	require.NoError(t, err)

	require.Equal(t, int64(2), stats.Generation)
	require.Equal(t, 3, stats.TotalTurns)
	require.Equal(t, 2, stats.SuccessTurns)
	require.Equal(t, 1, stats.FailedTurns)
	require.Equal(t, int64(1400), stats.TotalDurMs)
	require.InDelta(t, 0.035, stats.TotalCostUSD, 0.001)

	// Token aggregation.
	require.Equal(t, int64(350), stats.TotalTokInput)     // 100+200+50
	require.Equal(t, int64(80), stats.TotalTokCacheWrite) // 50+30+0
	require.Equal(t, int64(30), stats.TotalTokCacheRead)  // 30+0+0
	require.Equal(t, int64(460), stats.TotalTokIn)        // (100+50+30)+(200+30+0)+(50+0+0) = 460
	require.Equal(t, int64(700), stats.TotalTokOut)       // 200+400+100

	// Per-turn items.
	require.Len(t, stats.Turns, 3)
	require.Equal(t, SourceCrash, stats.Turns[1].Source)
	require.Equal(t, "sonnet", stats.Turns[2].Model)
}

// ─── TURN-016: LatestGeneration ────────────────────────────────────────────

func TestTurnsTable_LatestGenerationQuery(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	// Insert turns across 3 generations.
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	for _, gen := range []int64{1, 2, 3} {
		require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s1", Generation: gen, TurnNum: 1, Role: "user",
			Content: "q", Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
		}))
	}
	require.NoError(t, tx.Commit())

	gen, err := store.LatestGeneration(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, int64(3), gen)

	gen, err = store.LatestGeneration(ctx, "no-such")
	require.NoError(t, err)
	require.Equal(t, int64(0), gen)
}

// ─── TURN-018: DeleteExpiredTurns ──────────────────────────────────────────

func TestTurnsTable_DeleteExpiredTurns(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	now := time.Now()
	oldMs := now.Add(-8 * 24 * time.Hour).UnixMilli()
	recentMs := now.Add(-1 * 24 * time.Hour).UnixMilli()

	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	// 2 old turns (should be deleted).
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 1, TurnNum: 1, Role: "user",
		Content: "old1", Source: SourceNormal, CreatedAt: oldMs,
	}))
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 1, TurnNum: 2, Role: "user",
		Content: "old2", Source: SourceNormal, CreatedAt: oldMs + 1,
	}))
	// 1 recent turn (should be kept).
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 1, TurnNum: 3, Role: "user",
		Content: "recent", Source: SourceNormal, CreatedAt: recentMs,
	}))
	require.NoError(t, tx.Commit())

	cutoff := now.Add(-7 * 24 * time.Hour)
	deleted, err := store.DeleteExpiredTurns(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, int64(2), deleted)

	// Only recent turn remains.
	records, err := store.QueryTurns(ctx, "s1", 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "recent", records[0].Content)
}

func TestTurnsTable_PerRecordContentExpiry(t *testing.T) {
	t.Parallel()
	store := newTestStoreWithTurnsTable(t)
	store.retention = RetentionPolicy{
		Content: 10 * 24 * time.Hour,
		Legacy:  30 * 24 * time.Hour,
	}
	ctx := testCtx(t)
	now := time.Now().Truncate(time.Millisecond)

	_, err := store.db.ExecContext(ctx,
		`INSERT INTO sessions (id, lifecycle_policy) VALUES (?, 'v2')`, "s-exp")
	require.NoError(t, err)

	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s-exp", Generation: 1, TurnNum: 1, Role: RoleUser,
		Content: "expired", Source: SourceNormal, CreatedAt: now.Add(-11 * 24 * time.Hour).UnixMilli(),
	}))
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s-exp", Generation: 1, TurnNum: 2, Role: RoleUser,
		Content: "live", Source: SourceNormal, CreatedAt: now.Add(-9 * 24 * time.Hour).UnixMilli(),
	}))
	require.NoError(t, tx.Commit())

	_, err = store.db.ExecContext(ctx, `INSERT INTO turns
		(session_id, generation, turn_num, role, content, created_at, expires_at)
		VALUES (?, 1, 3, 'user', 'legacy-expired', ?, 0)`,
		"s-exp", now.Add(-31*24*time.Hour).UnixMilli())
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `INSERT INTO turns
		(session_id, generation, turn_num, role, content, created_at, expires_at)
		VALUES (?, 1, 4, 'user', 'legacy-live', ?, 0)`,
		"s-exp", now.Add(-29*24*time.Hour).UnixMilli())
	require.NoError(t, err)

	var expiresAt int64
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT expires_at FROM turns WHERE session_id = ? AND turn_num = 2`, "s-exp").Scan(&expiresAt))
	require.Equal(t, now.Add(24*time.Hour).UnixMilli(), expiresAt)

	records, err := store.QueryTurns(ctx, "s-exp", 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, []string{"live", "legacy-live"}, []string{records[0].Content, records[1].Content})

	deleted, err := store.DeleteExpiredTurns(ctx, now.Add(-30*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(2), deleted)

	records, err = store.QueryTurns(ctx, "s-exp", 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, []string{"live", "legacy-live"}, []string{records[0].Content, records[1].Content})
}

// ─── TURN-020: TurnRecord ID type ──────────────────────────────────────────

func TestTurnsTable_TurnRecordIDInt64(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 1, TurnNum: 1, Role: "user",
		Content: "q", Source: SourceNormal, CreatedAt: time.Now().UnixMilli(),
	}))
	require.NoError(t, tx.Commit())

	records, err := store.QueryTurns(ctx, "s1", 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Greater(t, records[0].ID, int64(0))
}

// ─── TURN-021: GetHistory JSON serialization ───────────────────────────────

func TestTurnsTable_JSONSerialization(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	success := true
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 2, TurnNum: 3, Seq: 5,
		Role: "assistant", Content: "result",
		Model: "sonnet", Success: &success, Source: SourceNormal,
		TokensInput: 100, TokensCacheWrite: 50, TokensCacheRead: 30,
		TokensOut: 200, DurationMs: 1000, CostUSD: 0.05,
		CreatedAt: time.Now().UnixMilli(),
	}))
	require.NoError(t, tx.Commit())

	records, err := store.QueryTurns(ctx, "s1", 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)

	b, err := json.Marshal(records[0])
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))

	require.Contains(t, m, "id")
	require.Contains(t, m, "generation")
	require.Contains(t, m, "turn_num")
	require.Contains(t, m, "tokens_input")
	require.Contains(t, m, "tokens_cache_write")
	require.Contains(t, m, "tokens_cache_read")
	require.Contains(t, m, "tokens_in")
	require.Contains(t, m, "role")
	require.Contains(t, m, "content")
	require.Contains(t, m, "model")
	require.Contains(t, m, "success")
	require.Contains(t, m, "source")
	require.Contains(t, m, "created_at")
	require.Equal(t, "assistant", m["role"])
	require.Equal(t, "result", m["content"])
}

// ─── TURN-025: Tokens input splitting ──────────────────────────────────────

func TestTurnsTable_TokensInputSplitting(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	success := true
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID: "s1", Generation: 1, TurnNum: 1, Seq: 1,
		Role: "assistant", Content: "result",
		Success: &success, Source: SourceNormal,
		TokensInput: 100, TokensCacheWrite: 50, TokensCacheRead: 30,
		TokensOut: 200, DurationMs: 1000, CostUSD: 0.05,
		CreatedAt: time.Now().UnixMilli(),
	}))
	require.NoError(t, tx.Commit())

	records, err := store.QueryTurns(ctx, "s1", 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)

	r := records[0]
	require.Equal(t, int64(100), r.TokensInput)
	require.Equal(t, int64(50), r.TokensCacheWrite)
	require.Equal(t, int64(30), r.TokensCacheRead)
	require.Equal(t, int64(180), r.TokensIn) // computed: 100+50+30
}

// ─── Synthetic turn sources (TURN-006) ─────────────────────────────────────

func TestTurnsTable_SyntheticSources(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	failed := false
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)

	for _, source := range []string{SourceCrash, SourceTimeout, SourceFreshStart} {
		require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s1", Generation: 1, TurnNum: 1, Seq: 1,
			Role: "assistant", Content: "synthetic",
			Success: &failed, Source: source,
			CreatedAt: time.Now().UnixMilli(),
		}))
	}
	require.NoError(t, tx.Commit())

	records, err := store.QueryTurns(ctx, "s1", 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 3)

	require.Equal(t, SourceCrash, records[0].Source)
	require.Equal(t, SourceTimeout, records[1].Source)
	require.Equal(t, SourceFreshStart, records[2].Source)
	for _, r := range records {
		require.NotNil(t, r.Success)
		require.False(t, *r.Success)
	}
}

// ─── Pagination (TURN-017) ─────────────────────────────────────────────────

func TestTurnsTable_Pagination(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s1", Generation: 1, TurnNum: i + 1, Seq: int64(i + 1),
			Role: "user", Content: "q",
			Source: SourceNormal, CreatedAt: time.Now().UnixMilli() + int64(i),
		}))
	}
	require.NoError(t, tx.Commit())

	t.Run("page 1", func(t *testing.T) {
		records, err := store.QueryTurns(ctx, "s1", 2, 0)
		require.NoError(t, err)
		require.Len(t, records, 2)
	})

	t.Run("page 2", func(t *testing.T) {
		records, err := store.QueryTurns(ctx, "s1", 2, 2)
		require.NoError(t, err)
		require.Len(t, records, 2)
	})

	t.Run("page 3 partial", func(t *testing.T) {
		records, err := store.QueryTurns(ctx, "s1", 2, 4)
		require.NoError(t, err)
		require.Len(t, records, 1)
	})

	t.Run("offset beyond data", func(t *testing.T) {
		_, err := store.QueryTurns(ctx, "s1", 10, 100)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestTurnsTable_ClientMessageIDRoundTrip(t *testing.T) {
	t.Parallel()
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
		SessionID:       "s-client-id",
		Generation:      1,
		TurnNum:         1,
		Seq:             7,
		Role:            RoleUser,
		Content:         "hello",
		ClientMessageID: "cm-roundtrip",
		CreatedAt:       time.Now().UnixMilli(),
	}))
	require.NoError(t, tx.Commit())

	records, err := store.QueryLatestTurns(ctx, "s-client-id", 10)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "cm-roundtrip", records[0].ClientMessageID)
}

// ─── QueryLatestTurns: returns newest N turns (regression: ASC+LIMIT bug) ────
//
// Regression guard: GetHistory's default path previously used QueryTurns
// (ORDER BY id ASC LIMIT), which returned the OLDEST N turns once a session
// exceeded `limit` rows. After a refresh, the user saw stale history and lost
// the most recent exchange. QueryLatestTurns must return the NEWEST N, in ASC
// display order.
func TestTurnsTable_QueryLatestTurns(t *testing.T) {
	store := newTestStoreWithTurnsTable(t)
	ctx := testCtx(t)

	// Seed 4 turns (user+assistant = 8 rows), generation 1, auto-increment id.
	// id order: u1,a1,u2,a2,u3,a3,u4,a4
	for tn := 1; tn <= 4; tn++ {
		tx, err := store.BeginTx(ctx)
		require.NoError(t, err)
		now := time.Now().UnixMilli()
		require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s1", Generation: 1, TurnNum: tn, Role: RoleUser,
			Content: fmt.Sprintf("u%d", tn), CreatedAt: now,
		}))
		success := true
		require.NoError(t, tx.AppendTurn(ctx, &TurnWriteRequest{
			SessionID: "s1", Generation: 1, TurnNum: tn, Role: RoleAssistant,
			Content: fmt.Sprintf("a%d", tn), Success: &success, CreatedAt: now,
		}))
		require.NoError(t, tx.Commit())
	}

	t.Run("returns newest N in ASC order", func(t *testing.T) {
		// 8 rows, limit 3 → newest 3 by id DESC, reversed to ASC:
		// id6(a3), id7(u4), id8(a4)
		records, err := store.QueryLatestTurns(ctx, "s1", 3)
		require.NoError(t, err)
		require.Len(t, records, 3)
		require.Equal(t, "a3", records[0].Content)
		require.Equal(t, "u4", records[1].Content)
		require.Equal(t, "a4", records[2].Content)
		require.Equal(t, 4, records[2].TurnNum, "must include the newest turn_num")
	})

	t.Run("limit >= total returns all in ASC", func(t *testing.T) {
		records, err := store.QueryLatestTurns(ctx, "s1", 100)
		require.NoError(t, err)
		require.Len(t, records, 8)
		require.Equal(t, 1, records[0].TurnNum, "oldest first")
		require.Equal(t, 4, records[7].TurnNum, "newest last")
	})

	t.Run("empty session returns ErrNotFound", func(t *testing.T) {
		_, err := store.QueryLatestTurns(ctx, "nope", 10)
		require.ErrorIs(t, err, ErrNotFound)
	})
}
