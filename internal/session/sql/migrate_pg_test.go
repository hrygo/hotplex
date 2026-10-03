//go:build pg

package session_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

// openTestPGDB opens a PostgreSQL connection using HOTPLEX_TEST_PG_DSN, runs
// the full migration set, and returns the *sql.DB. The caller owns Close.
// Tests call t.Skip when HOTPLEX_TEST_PG_DSN is unset so the default suite
// (which has no PG instance) is unaffected; a developer with PG available
// runs: HOTPLEX_TEST_PG_DSN=... go test -tags pg ./internal/session/sql/...
func openTestPGDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("HOTPLEX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HOTPLEX_TEST_PG_DSN not set; skipping PG migration test")
	}
	db, err := sql.Open(sqlutil.DriverNamePG, dsn)
	require.NoError(t, err, "open pg")

	// Start from a clean slate so the test is deterministic regardless of
	// what previous runs (or other tests) left behind. DROP SCHEMA cascade
	// removes every table/sequence/function/trigger goose created.
	_, err = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS public CASCADE")
	require.NoError(t, err, "drop schema")
	_, err = db.ExecContext(context.Background(), "CREATE SCHEMA public")
	require.NoError(t, err, "recreate schema")

	require.NoError(t, session.RunMigrations(context.Background(), db, dbutil.DialectPostgres),
		"PG migrations must apply cleanly")
	return db
}

// TestMigrations_PG_023UserActivity_TriggerBlocksUpdate is the PostgreSQL
// counterpart to TestMigrations_023UserActivity_AppliesAndIsImmutable. It
// guards the audit system's core tamper-evidence invariant on PG: migration
// 023 creates a BEFORE UPDATE trigger that must reject every mutation of
// user_activity (review I4 — the PR shipped with zero PG migration tests).
//
// A semantic bug here (e.g. the trigger firing AFTER instead of BEFORE, or
// the function existing but never bound to the table) would let UPDATEs
// succeed silently and defeat the entire audit chain's tamper-evidence.
func TestMigrations_PG_023UserActivity_TriggerBlocksUpdate(t *testing.T) {
	ctx := context.Background()
	db := openTestPGDB(t)
	defer func() { _ = db.Close() }()

	// Seed a row. The append path goes through audit.Store, but for a focused
	// migration test we insert directly — the trigger must fire regardless of
	// how the row got there.
	_, err := db.ExecContext(ctx,
		`INSERT INTO user_activity (ts, user_id, user_id_type, platform, action, outcome, detail_json, prev_hash, self_hash)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		1700000000000, "u1", "platform", "api", "auth.login", "success", "{}", "", "hash1",
	)
	require.NoError(t, err, "seed insert")

	// An UPDATE of ANY column must be rejected by the trigger with the
	// spec-mandated message. We use the spec phrase so a future copy-edit of
	// the message is caught here too.
	_, err = db.ExecContext(ctx, `UPDATE user_activity SET outcome = 'failure' WHERE user_id = 'u1'`)
	require.Error(t, err, "UPDATE must be blocked by the immutability trigger")
	require.True(t,
		strings.Contains(err.Error(), "audit: rows are immutable") || strings.Contains(err.Error(), "immutable"),
		"trigger error should mention immutability, got: %v", err)

	// A no-op self-assign (SET ts = ts) must ALSO be blocked — the trigger is
	// BEFORE UPDATE with no WHEN clause, so it fires unconditionally.
	_, err = db.ExecContext(ctx, `UPDATE user_activity SET ts = ts`)
	require.Error(t, err, "self-assign UPDATE must also be blocked")
}

// TestMigrations_PG_030AuditNoDelete_BlocksUnauthorizedRowDeletes is the
// PostgreSQL counterpart to TestMigrations_030AuditNoDelete_BlocksUnauthorizedRowDeletes:
// migration 030's fn_ua_no_delete trigger must reject every DELETE that is
// not covered by a checkpoint anchor, while the GC prune path (checkpoint
// written in the same transaction before the delete) must still pass.
func TestMigrations_PG_030AuditNoDelete_BlocksUnauthorizedRowDeletes(t *testing.T) {
	ctx := context.Background()
	db := openTestPGDB(t)
	defer func() { _ = db.Close() }()

	insertRow := func(id int, selfHash string) {
		t.Helper()
		_, err := db.ExecContext(ctx,
			`INSERT INTO user_activity (ts, user_id, user_id_type, platform, action, outcome, detail_json, prev_hash, self_hash)
			 VALUES ($1, 'u', 'registered', 'webchat', 'x', 'success', '{}', 'prev', $2)`,
			int64(id), selfHash)
		require.NoError(t, err, "INSERT row %d", id)
	}
	insertRow(1, "h1")
	insertRow(2, "h2")

	// Unauthorized DELETE (no checkpoint anchor) must abort with the spec message.
	_, err := db.ExecContext(ctx, `DELETE FROM user_activity WHERE id = 1`)
	require.Error(t, err, "unauthorized DELETE must be rejected by trg_ua_no_delete")
	require.True(t,
		strings.Contains(err.Error(), "audit: rows are immutable") || strings.Contains(err.Error(), "immutable"),
		"trigger error should mention immutability, got: %v", err)

	// The GC prune path (checkpoint written in the SAME transaction before
	// the DELETE) must still pass.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO audit_chain_checkpoints (pruned_at, last_self_hash, next_id) VALUES ($1, $2, $3)`,
		time.Now().UnixMilli(), "h1", int64(2))
	require.NoError(t, err, "checkpoint insert inside GC tx")
	_, err = tx.ExecContext(ctx, `DELETE FROM user_activity WHERE id <= 1`)
	require.NoError(t, err, "checkpoint-anchored DELETE (GC prune) must pass")
	require.NoError(t, tx.Commit())

	// A row beyond the checkpoint's next_id (id=2 survives, next_id=2)
	// must NOT be deletable.
	_, err = db.ExecContext(ctx, `DELETE FROM user_activity WHERE id = 2`)
	require.Error(t, err, "row past the checkpoint anchor must stay immutable")
}

// TestMigrations_PG_037ExecutionQueue_SchemaAndInvariants is the PostgreSQL
// counterpart of the SQLite 037 guard. PostgreSQL widens a CHECK in place
// rather than rebuilding the table, and the constraint it has to find is one
// the server named for us back in migration 027 — so the DO block's discovery
// step is the part most likely to silently match nothing and leave the old
// five-value CHECK in place.
func TestMigrations_PG_037ExecutionQueue_SchemaAndInvariants(t *testing.T) {
	ctx := context.Background()
	db := openTestPGDB(t)
	defer func() { _ = db.Close() }()

	const ts = 1700000000000
	_, err := db.ExecContext(ctx, `INSERT INTO sessions
		(id, user_id, worker_type, state, created_at, updated_at)
		VALUES ('s-queue', 'u1', 'claude_code', 'idle', $1, $1)`, ts)
	require.NoError(t, err, "seed session")

	// 1) Exactly one runtime_status CHECK survives, and it admits 'queued'.
	var checks int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		WHERE rel.relname = 'execution_inputs'
		  AND con.contype = 'c'
		  AND pg_get_constraintdef(con.oid) LIKE '%runtime_status%'`,
	).Scan(&checks))
	require.Equal(t, 1, checks, "expected exactly one runtime_status CHECK after 037")

	insertExec := func(execID, sessionID, msgID, runtime string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO execution_inputs
			(execution_id, session_id, client_message_id, payload_hash, status, error_code,
			 created_at, updated_at, owner_instance_id, worker_run_id, lease_until,
			 runtime_status, runtime_error_code, fence_reason)
			VALUES ($1, $2, $3, $4, 'accepted', '', $5, $5, '', '', 0, $6, '', '')`,
			execID, sessionID, msgID, "hash_"+msgID, ts, runtime)
		return err
	}

	require.NoError(t, insertExec("exec_q1", "s-queue", "msg-q1", "queued"))
	require.NoError(t, insertExec("exec_q2", "s-queue", "msg-q2", "queued"))
	require.NoError(t, insertExec("exec_pending", "s-queue", "msg-pending", "pending"))
	require.Error(t, insertExec("exec_second_pending", "s-queue", "msg-p2", "pending"),
		"a queued backlog must not weaken the single-active gate")
	require.Error(t, insertExec("exec_bogus", "s-queue", "msg-bogus", "bogus"),
		"the CHECK widened, it did not open")

	// 2) Queue, allocator and budget tables exist.
	for _, table := range []string{"execution_queue", "execution_queue_counters", "execution_queue_budget"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1`, table).Scan(&n))
		require.Equal(t, 1, n, "expected table %s after migration 037", table)
	}

	// 3) FIFO ordinal is a database fact.
	_, err = db.ExecContext(ctx, `INSERT INTO execution_queue
		(execution_id, session_id, queue_seq, enqueued_at, expires_at)
		VALUES ('exec_q1', 's-queue', 1, $1, $2)`, ts, ts+1000)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO execution_queue
		(execution_id, session_id, queue_seq, enqueued_at, expires_at)
		VALUES ('exec_q2', 's-queue', 1, $1, $2)`, ts, ts+1000)
	require.Error(t, err, "two queue rows must not share a per-session ordinal")

	// 4) The budget row the enqueue path locks exists.
	var used int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT used FROM execution_queue_budget WHERE budget_id = 1`).Scan(&used))
	require.Equal(t, int64(0), used)

	// 5) Session delete cascades into the queue; depth is derived, so nothing
	//    has to remember to release the slot.
	_, err = db.ExecContext(ctx, `DELETE FROM sessions WHERE id = 's-queue'`)
	require.NoError(t, err)
	var remaining int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_queue`).Scan(&remaining))
	require.Equal(t, 0, remaining, "queued rows must not outlive their session")
}

// TestMigrations_PG_038ExecutionQueuePayloads_ContentFollowsControlFacts is the
// PostgreSQL counterpart of the 038 payload guard. The foreign key to
// execution_queue is the retention rule: content must disappear exactly when
// the promise to dispatch it does.
func TestMigrations_PG_038ExecutionQueuePayloads_ContentFollowsControlFacts(t *testing.T) {
	ctx := context.Background()
	db := openTestPGDB(t)
	defer func() { _ = db.Close() }()

	var n int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'execution_queue_payloads'`).Scan(&n))
	require.Equal(t, 1, n, "expected the payload table after migration 038")

	_, err := db.ExecContext(ctx, `INSERT INTO sessions
		(id, user_id, worker_type, state, created_at, updated_at)
		VALUES ('s-payload', 'u1', 'claude_code', 'idle', $1, $1)`, 1700000000000)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO execution_inputs
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
		VALUES ('qpayload_orphan', 'exec_missing', 's-payload', 'x', '', 1, 'h', 1)`)
	require.Error(t, err, "payload content must not exist without a queued input")

	_, err = db.ExecContext(ctx, `INSERT INTO execution_queue_payloads
		(payload_id, execution_id, session_id, content, invocation_json,
		 content_bytes, content_sha256, created_at)
		VALUES ('qpayload_p', 'exec_p', 's-payload', 'do the thing', '', 12, 'hash_p', 1)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `DELETE FROM execution_queue WHERE execution_id = 'exec_p'`)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM execution_queue_payloads`).Scan(&n))
	require.Zero(t, n, "content must not outlive the promise to dispatch it")
}
