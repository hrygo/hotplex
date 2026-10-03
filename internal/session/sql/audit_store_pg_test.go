//go:build pg

package session_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

// openTestPGAuditStore opens a PG connection (HOTPLEX_TEST_PG_DSN), runs the
// full migration set so user_activity + audit_chain_checkpoints exist, and
// returns an audit.Store backed by it. Skips when the DSN env var is unset.
// The caller owns Close on both the store and the underlying db.
//
// Lives in package session_test (not audit) to avoid a test-only import cycle
// (internal/session imports internal/audit, so audit tests cannot import
// session to run migrations).
func openTestPGAuditStore(t *testing.T) (audit.Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("HOTPLEX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HOTPLEX_TEST_PG_DSN not set; skipping PG audit store test")
	}
	db, err := sql.Open(sqlutil.DriverNamePG, dsn)
	require.NoError(t, err, "open pg")

	// Clean slate for determinism (matches migrate_pg_test.go).
	_, err = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS public CASCADE")
	require.NoError(t, err, "drop schema")
	_, err = db.ExecContext(context.Background(), "CREATE SCHEMA public")
	require.NoError(t, err, "recreate schema")

	require.NoError(t, session.RunMigrations(context.Background(), db, dbutil.DialectPostgres),
		"PG migrations must apply")

	store, err := audit.NewStore(db, dbutil.DialectPostgres, nil, nil)
	require.NoError(t, err, "new pg audit store")
	return store, db
}

// TestPGAuditStore_RoundTripAndGC verifies the PostgreSQL audit store path
// end-to-end against a real PG instance: BeginTx (with the advisory lock) →
// Append + chain linking → Commit → GC Tick (which now runs the whole prune
// under one advisory-locked transaction, review C2) → VerifyOnce (chain
// intact). This is the integration guard the PR was missing (review I4).
func TestPGAuditStore_RoundTripAndGC(t *testing.T) {
	ctx := context.Background()
	store, db := openTestPGAuditStore(t)
	defer func() { _ = db.Close() }()

	// Append a small chain of old rows (2h ago — old enough for GC).
	const n = 5
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	tail, err := tx.TailHash(ctx)
	require.NoError(t, err)
	base := time.Now().Add(-2 * time.Hour).UnixMilli()
	for i := 0; i < n; i++ {
		ua := &audit.UserActivity{
			Ts:         base + int64(i),
			UserID:     "u1",
			UserIDType: audit.UserIDTypePlatform,
			Platform:   "test",
			Action:     audit.ActionAuthLogin,
			Outcome:    audit.OutcomeSuccess,
			DetailJSON: "{}",
			PrevHash:   tail,
		}
		h, err := audit.ComputeSelfHash(tail, ua)
		require.NoError(t, err)
		ua.SelfHash = h
		require.NoError(t, tx.Append(ctx, ua))
		tail = h
	}
	require.NoError(t, tx.Commit())

	// GC prunes everything older than 1ms. On PG this now runs the entire
	// prune (LastRowBefore → DeleteByIDLEQ → SaveCheckpoint → Commit) inside
	// one advisory-locked transaction (review C2 fix).
	gc := audit.NewGC(store, audit.GCConfig{Retention: 1 * time.Millisecond}, nil)
	deleted, err := gc.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(n), deleted, "GC should prune all %d rows", n)

	// Verifier must accept the post-prune state (empty table → genesis
	// checkpoint). A chain break here would indicate the advisory-lock fix is
	// wrong.
	v := audit.NewVerifier(store, audit.VerifierConfig{}, nil)
	result, err := v.VerifyOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), result.BrokenID, "chain must verify after PG GC prune")

	// Append a new row after the full prune → must be treated as genesis.
	tx2, err := store.BeginTx(ctx)
	require.NoError(t, err)
	tail2, err := tx2.TailHash(ctx)
	require.NoError(t, err)
	require.Empty(t, tail2, "tail must be empty after full prune (genesis)")
	ua := &audit.UserActivity{
		Ts:         time.Now().UnixMilli(),
		UserID:     "u2",
		UserIDType: audit.UserIDTypeRegistered,
		Platform:   audit.PlatformWebChat,
		Action:     audit.ActionAuthLogin,
		Outcome:    audit.OutcomeSuccess,
		DetailJSON: "{}",
		PrevHash:   "",
	}
	h, err := audit.ComputeSelfHash("", ua)
	require.NoError(t, err)
	ua.SelfHash = h
	require.NoError(t, tx2.Append(ctx, ua))
	require.NoError(t, tx2.Commit())

	// Verify the new genesis row links correctly.
	result, err = v.VerifyOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), result.BrokenID, "genesis row after full prune must verify")
	require.Equal(t, 1, result.RowsChecked)
}

// TestPGAuditStore_AdvisoryLockSerializesTail asserts that a second
// transaction blocks on pg_advisory_xact_lock while the first holds it open.
// Without the lock (review C2), two overlapping BeginTx→TailHash→Append would
// both read the same tail and produce two rows with the same prev_hash,
// breaking the chain. This test observes the serialization point directly.
func TestPGAuditStore_AdvisoryLockSerializesTail(t *testing.T) {
	ctx := context.Background()
	store, db := openTestPGAuditStore(t)
	defer func() { _ = db.Close() }()

	// tx1 begins and reads the (empty) tail while holding the advisory lock.
	tx1, err := store.BeginTx(ctx)
	require.NoError(t, err)

	// tx2 begins concurrently. It must BLOCK on pg_advisory_xact_lock until
	// tx1 commits/rolls back. Run it in a goroutine and assert it hasn't
	// completed within a short window while tx1 is still open.
	type beginResult struct {
		tx  audit.Tx
		err error
	}
	tx2Done := make(chan beginResult, 1)
	go func() {
		tx, err := store.BeginTx(ctx)
		tx2Done <- beginResult{tx: tx, err: err}
	}()

	select {
	case res := <-tx2Done:
		// BeginTx can only return once it holds the lock, and tx1 holds it. A
		// return here means the lock is not serializing, which is the defect
		// this test exists to catch — not a timing artefact to be logged.
		require.NoError(t, res.err)
		t.Fatal("concurrent BeginTx returned while tx1 still held the advisory lock; " +
			"the serialization point is not being enforced")
	case <-time.After(300 * time.Millisecond):
		// Expected: tx2 is still blocked on the advisory lock held by tx1.
	}

	// Releasing tx1 lets tx2 proceed.
	require.NoError(t, tx1.Commit())

	// tx2 should now complete within a reasonable window. Its transaction MUST
	// be rolled back here: the advisory lock is transaction-scoped, so an
	// abandoned tx2 goes back into the pool idle-in-transaction still holding
	// it, and the next BeginTx in this suite then blocks forever. That is not
	// hypothetical — it hung the package until the test binary's timeout, and
	// only when -shuffle=on happened to order another BeginTx test after this
	// one.
	select {
	case res := <-tx2Done:
		require.NoError(t, res.err, "concurrent BeginTx must succeed after tx1 commits")
		require.NoError(t, res.tx.Rollback(), "release tx2's advisory lock")
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent BeginTx did not complete after tx1 committed")
	}

	// Guard against the wrong-dialect path silently making this a no-op.
	require.NotEqual(t, dbutil.DialectSQLite, store.Dialect())
}
