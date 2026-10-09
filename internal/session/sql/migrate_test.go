package session_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

// openMigrationTestDB opens a fresh on-disk SQLite file (avoids modernc.org/sqlite
// quirks with shared :memory:) and returns the *sql.DB after PRAGMA init. The caller
// owns Close.
func openMigrationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "migrate_test.db")
	db, err := sql.Open(sqlutil.DriverName, dbPath)
	require.NoError(t, err, "open sqlite")
	t.Cleanup(func() { _ = db.Close() })
	// Reuse the project PRAGMA stack so tests run against the same tuning as production.
	cfg := config.Default()
	require.NoError(t, sqlutil.InitSQLiteDB(db, &cfg.DB, sqlutil.DialectSQLite, "migrate_test"))
	return db
}

// TestMigrations_AuditChainsApplyAndStayImmutable covers the user-facing contract
// of audit migrations 023 and 044:
//   - user_activity table exists with the expected columns
//   - audit_chain_checkpoints table exists
//   - the BEFORE UPDATE trigger blocks mutations with the spec-mandated message
//   - all 3 expected indexes are present
//   - the 3 dead v_turns* views (spec §11.2) are dropped
func TestMigrations_AuditChainsApplyAndStayImmutable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openMigrationTestDB(t)

	// Boot goose against the full migration set. This exercises the real
	// go:embed glob (internal/session/migrate.go:17) end-to-end.
	require.NoError(t, session.RunMigrations(ctx, db, dbutil.DialectSQLite))

	// 1) Both Phase 1 tables exist.
	for _, table := range []string{"user_activity", "audit_chain_checkpoints"} {
		var n int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&n)
		require.NoError(t, err, "lookup table %s", table)
		require.Equal(t, 1, n, "expected table %s to exist after migration 023", table)
	}

	// 2) user_activity has every spec column.
	wantCols := []string{
		"id", "ts", "user_id", "user_id_type", "platform", "session_id",
		"action", "resource_type", "resource_id", "outcome", "detail_json",
		"event_ref", "ip", "user_agent", "prev_hash", "self_hash",
	}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(user_activity)`)
	require.NoError(t, err)
	defer rows.Close()
	gotCols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		gotCols[name] = true
	}
	require.NoError(t, rows.Err())
	for _, c := range wantCols {
		require.True(t, gotCols[c], "user_activity missing column %s", c)
	}

	// 3) INSERT works (smoke) and UPDATE is rejected by the trigger with the
	//    spec-exact message. SQLite returns SQLITE_CONSTRAINT (19) on ABORT.
	_, err = db.ExecContext(ctx, `INSERT INTO user_activity
		(ts, user_id, user_id_type, platform, action, outcome, detail_json, prev_hash, self_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		1700000000000, "u_test", "registered", "webchat",
		"auth.login", "success", `{}`, "", "h0")
	require.NoError(t, err, "INSERT into user_activity")

	_, err = db.ExecContext(ctx, `UPDATE user_activity SET user_id='x' WHERE 1=1`)
	require.Error(t, err, "UPDATE must be blocked by trg_ua_no_update")
	require.Contains(t, err.Error(), "audit: rows are immutable",
		"trigger must surface the spec-exact message")

	// 4) All 3 indexes present (idx_ua_user_ts, idx_ua_ts, idx_ua_action_ts).
	for _, idx := range []string{"idx_ua_user_ts", "idx_ua_ts", "idx_ua_action_ts"} {
		var n int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=? AND tbl_name='user_activity'`, idx,
		).Scan(&n)
		require.NoError(t, err)
		require.Equal(t, 1, n, "expected index %s on user_activity", idx)
	}

	// 5) Spec §11.2: dead v_turns* views must be gone after migration 023.
	//    Even though migration 009 already replaced them with the materialized turns
	//    table, fresh DBs and pre-009 leftovers must all be cleaned.
	for _, view := range []string{"v_turns", "v_turns_user", "v_turns_assistant"} {
		var n int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='view' AND name=?`, view,
		).Scan(&n)
		require.NoError(t, err)
		require.Equal(t, 0, n, "dead view %s should be dropped by migration 023", view)
	}

	// 6) The audit_chain_checkpoints schema is the contract for the rebase logic
	//    in spec §5.5 — guard against accidental column drift.
	cpRows, err := db.QueryContext(ctx, `PRAGMA table_info(audit_chain_checkpoints)`)
	require.NoError(t, err)
	defer cpRows.Close()
	wantCP := map[string]bool{"id": false, "pruned_at": false, "last_self_hash": false, "next_id": false}
	for cpRows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		require.NoError(t, cpRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		if _, expected := wantCP[name]; expected {
			wantCP[name] = true
		}
	}
	require.NoError(t, cpRows.Err())
	for col, found := range wantCP {
		require.True(t, found, "audit_chain_checkpoints missing column %s", col)
	}

	// 7) Migration 044 gives lifecycle-v2 its own chain and frozen,
	// hash-covered expiry deadline without changing the legacy table.
	for _, table := range []string{"user_activity_v2", "audit_chain_checkpoints_v2"} {
		var n int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&n)
		require.NoError(t, err)
		require.Equal(t, 1, n, "expected table %s to exist after migration 044", table)
	}
	v2Columns := map[string]bool{}
	v2Rows, err := db.QueryContext(ctx, `PRAGMA table_info(user_activity_v2)`)
	require.NoError(t, err)
	for v2Rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		require.NoError(t, v2Rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		v2Columns[name] = true
	}
	require.NoError(t, v2Rows.Err())
	require.NoError(t, v2Rows.Close())
	require.True(t, v2Columns["expires_at"], "lifecycle-v2 audit rows must persist their expiry")
	var expiryIndex int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_ua_v2_expires_at' AND tbl_name='user_activity_v2'`,
	).Scan(&expiryIndex))
	require.Equal(t, 1, expiryIndex)

	result, err := db.ExecContext(ctx, `INSERT INTO user_activity_v2
		(ts, user_id, user_id_type, platform, action, outcome, detail_json, prev_hash, self_hash, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		1700000000000, "u_v2", "system", "api", "auth.login", "success", "{}", "", "hash-v2", 1700000000000+180*24*60*60*1000)
	require.NoError(t, err, "insert into lifecycle-v2 audit chain")
	v2ID, err := result.LastInsertId()
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE user_activity_v2 SET expires_at=0 WHERE id=?`, v2ID)
	require.Error(t, err, "lifecycle-v2 audit updates must be rejected")
	_, err = db.ExecContext(ctx, `DELETE FROM user_activity_v2 WHERE id=?`, v2ID)
	require.Error(t, err, "unanchored lifecycle-v2 audit deletes must be rejected")
	_, err = db.ExecContext(ctx, `INSERT INTO audit_chain_checkpoints_v2 (pruned_at, last_self_hash, next_id) VALUES (?, ?, ?)`,
		time.Now().UnixMilli(), "hash-v2", v2ID+1)
	require.NoError(t, err, "checkpoint must anchor the lifecycle-v2 prefix")
	_, err = db.ExecContext(ctx, `DELETE FROM user_activity_v2 WHERE id=?`, v2ID)
	require.NoError(t, err, "checkpoint-anchored lifecycle-v2 cleanup must be allowed")
}

// TestMigrations_027_ExecutionOwnerLease_SchemaAndConstraints covers the durable
// ingress reliability closure migration (spec 2026-07-14, lines 143-188):
//   - all 8 new columns exist on execution_inputs
//   - both indexes (owner_runtime composite + one_active_per_session partial unique) exist
//   - the partial unique index rejects a second pending/running execution per session
//   - fence_reason also activates the partial unique index for terminal-but-fenced rows
//   - the runtime_status CHECK constraint rejects invalid values
func TestMigrations_027_ExecutionOwnerLease_SchemaAndConstraints(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openMigrationTestDB(t)
	require.NoError(t, session.RunMigrations(ctx, db, dbutil.DialectSQLite))

	wantCols := map[string]bool{
		"owner_instance_id":  false,
		"worker_run_id":      false,
		"lease_until":        false,
		"runtime_status":     false,
		"runtime_error_code": false,
		"started_at":         false,
		"finished_at":        false,
		"fence_reason":       false,
	}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(execution_inputs)`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		if _, ok := wantCols[name]; ok {
			wantCols[name] = true
		}
	}
	require.NoError(t, rows.Err())
	for col, found := range wantCols {
		require.True(t, found, "execution_inputs missing column %s after migration 027", col)
	}

	for _, idx := range []string{"idx_execution_owner_runtime", "idx_execution_one_active_per_session"} {
		var n int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=? AND tbl_name='execution_inputs'`, idx,
		).Scan(&n)
		require.NoError(t, err)
		require.Equal(t, 1, n, "expected index %s on execution_inputs", idx)
	}

	_, err = db.ExecContext(ctx, `PRAGMA foreign_keys=OFF`)
	require.NoError(t, err)

	const ts = 1700000000000
	insertExec := func(execID, sessionID, runtime, fence string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO execution_inputs
			(execution_id, session_id, client_message_id, payload_hash, status, error_code,
			 created_at, updated_at, owner_instance_id, worker_run_id, lease_until,
			 runtime_status, runtime_error_code, fence_reason)
			VALUES (?, ?, ?, ?, 'accepted', '', ?, ?, '', '', 0, ?, '', ?)`,
			execID, sessionID, "msg_"+execID, "hash_"+execID, ts, ts, runtime, fence)
		return err
	}

	require.NoError(t, insertExec("exec_a", "s1", "pending", ""),
		"first pending execution for s1 should succeed")

	err = insertExec("exec_b", "s1", "pending", "")
	require.Error(t, err, "partial unique index must reject second pending execution per session")

	require.NoError(t, insertExec("exec_c", "s1", "completed", ""),
		"completed execution must not conflict with pending via partial index")

	require.NoError(t, insertExec("exec_d", "s2", "unknown", "LEASE_EXPIRED"),
		"fenced execution for s2 should succeed")

	require.NoError(t, insertExec("exec_e", "s2", "unknown", ""),
		"non-fenced unknown execution must not conflict with fenced one")

	_, err = db.ExecContext(ctx, `INSERT INTO execution_inputs
		(execution_id, session_id, client_message_id, payload_hash, status, error_code,
		 created_at, updated_at, owner_instance_id, worker_run_id, lease_until,
		 runtime_status, runtime_error_code, fence_reason)
		VALUES (?, ?, ?, ?, 'unknown', '', ?, ?, '', '', 0, 'unknown', '', 'ANOTHER_FENCE')`,
		"exec_f", "s2", "msg_f", "hash_f", ts, ts)
	require.Error(t, err, "partial unique index must reject second fenced execution per session")

	err = insertExec("exec_g", "s3", "bogus", "")
	require.Error(t, err, "CHECK constraint must reject invalid runtime_status")
}

// TestMigrations_023UserActivity_TriggerBlocksEveryColumnShapeOfUpdate guards against
// the trigger being accidentally weakened to allow some UPDATE shapes. We try a
// no-op-equivalent UPDATE (SET ts=ts) and confirm it still aborts.
func TestMigrations_023UserActivity_TriggerBlocksEveryColumnShapeOfUpdate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openMigrationTestDB(t)
	require.NoError(t, session.RunMigrations(ctx, db, dbutil.DialectSQLite))

	_, err := db.ExecContext(ctx, `INSERT INTO user_activity
		(ts, user_id, user_id_type, platform, action, outcome, detail_json, prev_hash, self_hash)
		VALUES (1, 'u', 'registered', 'webchat', 'x', 'success', '{}', '', 'h')`)
	require.NoError(t, err)

	for _, stmt := range []string{
		`UPDATE user_activity SET user_id='y'`,
		`UPDATE user_activity SET ts=ts`, // "self-assign" — should still abort
		`UPDATE user_activity SET detail_json='{"k":1}'`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.Error(t, err, "expected trigger to abort: %s", stmt)
		require.True(t,
			strings.Contains(err.Error(), "audit: rows are immutable"),
			"unexpected error for %s: %v", stmt, err)
	}
}

// TestMigrations_030AuditNoDelete_BlocksUnauthorizedRowDeletes covers the
// root-cause fix for the audit-chain breakage (broken_id=1253 era): rows
// were historically DELETEable with no trace, orphaning the hash chain.
// Migration 030 must reject every DELETE that is not anchored by a
// checkpoint written in the same transaction (the GC prune path), while
// still letting the GC transaction through.
func TestMigrations_030AuditNoDelete_BlocksUnauthorizedRowDeletes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openMigrationTestDB(t)
	require.NoError(t, session.RunMigrations(ctx, db, dbutil.DialectSQLite))

	insertRow := func(id int, selfHash string) {
		t.Helper()
		_, err := db.ExecContext(ctx, `INSERT INTO user_activity
			(ts, user_id, user_id_type, platform, action, outcome, detail_json, prev_hash, self_hash)
			VALUES (?, 'u', 'registered', 'webchat', 'x', 'success', '{}', 'prev', ?)`,
			int64(id), selfHash)
		require.NoError(t, err, "INSERT row %d", id)
	}
	insertRow(1, "h1")
	insertRow(2, "h2")

	// 1) Unauthorized DELETE (no checkpoint anchor) must abort with the
	//    spec message — this is the historical breakage vector.
	_, err := db.ExecContext(ctx, `DELETE FROM user_activity WHERE id = 1`)
	require.Error(t, err, "unauthorized DELETE must be rejected by trg_ua_no_delete")
	require.Contains(t, err.Error(), "audit: rows are immutable",
		"trigger must surface the immutability message")

	// 2) The GC prune path (checkpoint written in the SAME transaction
	//    before the DELETE) must still pass.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO audit_chain_checkpoints (pruned_at, last_self_hash, next_id) VALUES (?, ?, ?)`,
		time.Now().UnixMilli(), "h1", int64(2))
	require.NoError(t, err, "checkpoint insert inside GC tx")
	_, err = tx.ExecContext(ctx, `DELETE FROM user_activity WHERE id <= 1`)
	require.NoError(t, err, "checkpoint-anchored DELETE (GC prune) must pass")
	require.NoError(t, tx.Commit())

	// 3) A row beyond the checkpoint's next_id (id=2 survives, next_id=2)
	//    must NOT be deletable — the anchor only covers the pruned prefix.
	_, err = db.ExecContext(ctx, `DELETE FROM user_activity WHERE id = 2`)
	require.Error(t, err, "row past the checkpoint anchor must stay immutable")
	require.Contains(t, err.Error(), "audit: rows are immutable")

	// 4) A fresh checkpoint extending the prefix to id=2 lets GC prune it.
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO audit_chain_checkpoints (pruned_at, last_self_hash, next_id) VALUES (?, ?, ?)`,
		time.Now().UnixMilli(), "h2", int64(3))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM user_activity WHERE id <= 2`)
	require.NoError(t, err, "extended checkpoint must cover row 2")
	require.NoError(t, tx.Commit())

	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_activity`).Scan(&n))
	require.Equal(t, 0, n, "all rows pruned via GC path")
}

// insertMigrationSession creates a real sessions row so the execution_queue
// foreign keys are exercised rather than switched off.
func insertMigrationSession(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO sessions
		(id, user_id, worker_type, state, created_at, updated_at)
		VALUES (?, 'u1', 'claude_code', 'idle', ?, ?)`,
		id, time.Now().UnixMilli(), time.Now().UnixMilli())
	require.NoError(t, err, "seed session %s", id)
}

// TestMigrations_037ExecutionQueue_SchemaAndInvariants covers migration 037 on
// SQLite, where widening the runtime_status CHECK required a full table
// rebuild. A rebuild that dropped an index, the input-idempotency unique key
// or the sessions foreign key would silently weaken guarantees that migrations
// 026/027/031 established, so those are asserted directly rather than assumed.
func TestMigrations_037ExecutionQueue_SchemaAndInvariants(t *testing.T) {
	t.Parallel()

	const ts = 1700000000000
	ctx := context.Background()
	db := openMigrationTestDB(t)
	require.NoError(t, session.RunMigrations(ctx, db, dbutil.DialectSQLite))

	insertMigrationSession(t, db, "s-queue")

	// 1) The rebuild kept every column the older migrations added.
	wantCols := []string{
		"execution_id", "session_id", "client_message_id", "payload_hash", "status",
		"error_code", "created_at", "updated_at", "delivered_at",
		"owner_instance_id", "worker_run_id", "lease_until",
		"runtime_status", "runtime_error_code", "started_at", "finished_at",
		"fence_reason", "fence_version", "fence_created_at",
	}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(execution_inputs)`)
	require.NoError(t, err)
	gotCols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		gotCols[name] = true
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for _, col := range wantCols {
		require.True(t, gotCols[col], "execution_inputs lost column %s in the 037 rebuild", col)
	}

	// 2) Every pre-037 index survived the rebuild.
	for _, idx := range []string{
		"idx_execution_inputs_session_created",
		"idx_execution_inputs_status_updated",
		"idx_execution_owner_runtime",
		"idx_execution_one_active_per_session",
		"idx_execution_fenced",
	} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=? AND tbl_name='execution_inputs'`, idx,
		).Scan(&n))
		require.Equal(t, 1, n, "expected index %s to survive the 037 rebuild", idx)
	}

	insertExec := func(execID, sessionID, msgID, runtime string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO execution_inputs
			(execution_id, session_id, client_message_id, payload_hash, status, error_code,
			 created_at, updated_at, owner_instance_id, worker_run_id, lease_until,
			 runtime_status, runtime_error_code, fence_reason)
			VALUES (?, ?, ?, ?, 'accepted', '', ?, ?, '', '', 0, ?, '', '')`,
			execID, sessionID, msgID, "hash_"+msgID, ts, ts, runtime)
		return err
	}

	// 3) 'queued' is now a legal runtime fact and still does not occupy the
	//    single active slot: twenty queued rows may coexist with one pending.
	require.NoError(t, insertExec("exec_q1", "s-queue", "msg-q1", "queued"))
	require.NoError(t, insertExec("exec_q2", "s-queue", "msg-q2", "queued"))
	require.NoError(t, insertExec("exec_pending", "s-queue", "msg-pending", "pending"))
	err = insertExec("exec_second_pending", "s-queue", "msg-second-pending", "pending")
	require.Error(t, err, "a queued backlog must not weaken the single-active gate")

	// 4) An unknown value is still rejected: the CHECK widened, it did not open.
	require.Error(t, insertExec("exec_bogus", "s-queue", "msg-bogus", "bogus"),
		"invalid runtime_status must still be rejected")

	// 5) Input idempotency survived: the (session_id, client_message_id) key.
	require.Error(t, insertExec("exec_dup", "s-queue", "msg-q1", "completed"),
		"unique (session_id, client_message_id) must survive the 037 rebuild")

	// 6) The queue, allocator and budget tables exist with their indexes.
	for _, table := range []string{"execution_queue", "execution_queue_counters", "execution_queue_budget"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n))
		require.Equal(t, 1, n, "expected table %s after migration 037", table)
	}
	for _, idx := range []string{"idx_execution_queue_head", "idx_execution_queue_expiry"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=? AND tbl_name='execution_queue'`, idx,
		).Scan(&n))
		require.Equal(t, 1, n, "expected index %s on execution_queue", idx)
	}

	// 7) FIFO order is a database fact: the per-session ordinal is unique.
	_, err = db.ExecContext(ctx, `INSERT INTO execution_queue
		(execution_id, session_id, queue_seq, enqueued_at, expires_at)
		VALUES ('exec_q1', 's-queue', 1, ?, ?)`, ts, ts+1000)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO execution_queue
		(execution_id, session_id, queue_seq, enqueued_at, expires_at)
		VALUES ('exec_q2', 's-queue', 1, ?, ?)`, ts, ts+1000)
	require.Error(t, err, "two queue rows must not share a per-session ordinal")

	// 8) The budget row is a single well-known row, so the enqueue path can
	//    always find it to serialise the global capacity decision.
	var used int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT used FROM execution_queue_budget WHERE budget_id = 1`).Scan(&used))
	require.Equal(t, 0, used, "budget starts empty")

	_, err = db.ExecContext(ctx, `DELETE FROM sessions WHERE id = 's-queue'`)
	require.NoError(t, err, "session delete must cascade to execution_queue")

	var remaining int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_queue`).Scan(&remaining))
	require.Equal(t, 0, remaining, "queued rows must not outlive their session")

	// 9) Depth is derived from execution_queue, never tallied by the delete
	//    paths. Session delete cascade removed both rows without running any
	//    application code, which is exactly why the enqueue path recounts
	//    instead of trusting a maintained counter.
	var depth int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_queue`).Scan(&depth))
	require.Equal(t, 0, depth)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT used FROM execution_queue_budget WHERE budget_id = 1`).Scan(&used))
	require.Equal(t, 0, used)
}

// TestMigrations_038ExecutionQueuePayloads_ContentFollowsControlFacts guards
// the retention rule the queue depends on. The payload foreign key must be a
// real ON DELETE CASCADE to execution_queue: without it, cancelling, expiring
// or deleting a session would leave queued prompts behind with nothing that
// says they are no longer dispatchable.
func TestMigrations_038ExecutionQueuePayloads_ContentFollowsControlFacts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openMigrationTestDB(t)
	require.NoError(t, session.RunMigrations(ctx, db, dbutil.DialectSQLite))

	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='execution_queue_payloads'`).Scan(&n))
	require.Equal(t, 1, n, "expected the payload table after migration 038")

	insertMigrationSession(t, db, "s-payload")
	_, err := db.ExecContext(ctx, `INSERT INTO execution_inputs
		(execution_id, session_id, client_message_id, payload_hash, status, error_code,
		 created_at, updated_at, owner_instance_id, worker_run_id, lease_until,
		 runtime_status, runtime_error_code, fence_reason)
		VALUES ('exec_p', 's-payload', 'msg-p', 'hash_p', 'accepted', '', 1, 1, '', '', 0, 'queued', '', '')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO execution_queue
		(execution_id, session_id, queue_seq, enqueued_at, expires_at, payload_ref, payload_bytes)
		VALUES ('exec_p', 's-payload', 1, 1, 9999999999999, 'qpayload_p', 12)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO execution_queue_payloads
		(payload_id, execution_id, session_id, content, invocation_json,
		 content_bytes, content_sha256, created_at)
		VALUES ('qpayload_p', 'exec_p', 's-payload', 'do the thing', '', 12, 'hash_p', 1)`)
	require.NoError(t, err)

	// A payload with no queue row cannot exist: the foreign key is the retention
	// rule, not a convenience constraint.
	_, err = db.ExecContext(ctx, `INSERT INTO execution_queue_payloads
		(payload_id, execution_id, session_id, content, invocation_json,
		 content_bytes, content_sha256, created_at)
		VALUES ('qpayload_orphan', 'exec_missing', 's-payload', 'x', '', 1, 'h', 1)`)
	require.Error(t, err, "payload content must not exist without a queued input to dispatch it for")

	// Removing the queue row removes the content with it.
	_, err = db.ExecContext(ctx, `DELETE FROM execution_queue WHERE execution_id = 'exec_p'`)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM execution_queue_payloads`).Scan(&n))
	require.Zero(t, n, "content must not outlive the promise to dispatch it")
}

// TestMigrations_039EffectReconciledTerminal guards the #947 CHECK growth:
// the reconciled/fenced states must be storable and old rows must survive
// the SQLite table rebuild with identical content.
func TestMigrations_039EffectReconciledTerminal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openMigrationTestDB(t)
	require.NoError(t, session.RunMigrations(ctx, db, dbutil.DialectSQLite))

	insertMigrationSession(t, db, "s-effect")
	_, err := db.ExecContext(ctx, `INSERT INTO effects
		(effect_id, occurrence_id, delivery_ordinal, target_revision, attempt,
		 session_id, execution_id, worker_run_id, payload_id, payload_sha256,
		 target_kind, target_ref, status, error_code, reason,
		 owner_instance_id, lease_version, provider_ref, evidence_ref,
		 created_at, updated_at)
		VALUES ('eff-039', 'occ-039', 0, 'rev-a', 1,
		 's-effect', 'exec-039', 'run-039', 'pay-039', 'sha',
		 'slack', 'C1', 'unknown', '', '',
		 '', 0, '', '', 1, 1)`)
	require.NoError(t, err)

	for _, status := range []string{"reconciled_succeeded", "reconciled_failed", "fenced"} {
		_, err := db.ExecContext(ctx, `UPDATE effects SET status = ? WHERE effect_id = 'eff-039'`, status)
		require.NoError(t, err, "039 must admit status %s", status)
		var got string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT status FROM effects WHERE effect_id = 'eff-039'`).Scan(&got))
		require.Equal(t, status, got)
	}

	var attempt int
	var kind, ref string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT attempt, target_kind, target_ref FROM effects WHERE effect_id = 'eff-039'`).Scan(&attempt, &kind, &ref))
	require.Equal(t, 1, attempt)
	require.Equal(t, "slack", kind)
	require.Equal(t, "C1", ref)
}
