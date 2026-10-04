package execution

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/pkg/events"
)

// seedExtraSession adds a second session so capacity tests can distinguish a
// per-session limit from a global one.
func seedExtraSession(t *testing.T, store *session.SQLiteStore, id string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, store.Upsert(context.Background(), &session.SessionInfo{
		ID:         id,
		UserID:     "user-1",
		WorkerType: "claude_code",
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}))
}

func queuedReq(sessionID, msgID string) QueuedRequest {
	return QueuedRequest{
		SessionID:       sessionID,
		ClientMessageID: msgID,
		PayloadHash:     "hash_" + msgID,
		Payload:         QueuedPayload{Content: "please do " + msgID},
		OwnerInstanceID: testOwner,
	}
}

// TestAcceptQueued_WritesQueuedExecutionAndDurableEntry pins what a caller is
// promised when the call returns: an accepted record that is queued, a queue
// entry with a database-assigned ordinal, and no lease. The last part matters
// most — a queued input has not crossed the dispatch boundary, so holding a
// lease would let lease recovery fence something nobody ever sent.
func TestAcceptQueued_WritesQueuedExecutionAndDurableEntry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)

	record, entry, duplicate, err := store.AcceptQueued(ctx, queuedReq("session-1", "m1"), QueueLimits{})
	require.NoError(t, err)
	require.False(t, duplicate)
	require.Equal(t, StatusAccepted, record.Status)
	require.Equal(t, RuntimeQueued, record.RuntimeStatus)
	require.True(t, record.IsQueued())
	require.Empty(t, record.WorkerRunID, "a queued input has no worker run yet")

	require.NotNil(t, entry)
	require.Equal(t, record.ExecutionID, entry.ExecutionID)
	require.Equal(t, "session-1", entry.SessionID)
	require.NotEmpty(t, entry.PayloadRef, "the store assigns the content reference")
	require.Greater(t, entry.ExpiresAt, entry.EnqueuedAt, "an undispatched input must expire later than it arrived")

	// The read-back path agrees with what was written.
	stored, err := store.QueueByExecution(ctx, record.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, entry, stored)
}

// TestAcceptQueued_DuplicateReturnsOriginalWithoutConsumingCapacity proves a
// retried message costs nothing: no second queue row, no second ordinal, and
// the capacity budget it would have consumed stays available.
func TestAcceptQueued_DuplicateReturnsOriginalWithoutConsumingCapacity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	limits := QueueLimits{PerSession: 2, Global: 10}

	first, firstEntry, _, err := store.AcceptQueued(ctx, queuedReq("session-1", "m1"), limits)
	require.NoError(t, err)

	second, secondEntry, duplicate, err := store.AcceptQueued(ctx, queuedReq("session-1", "m1"), limits)
	require.NoError(t, err)
	require.True(t, duplicate)
	require.Equal(t, first.ExecutionID, second.ExecutionID)
	require.Equal(t, firstEntry, secondEntry)

	entries, err := store.QueueBySession(ctx, "session-1", 0)
	require.NoError(t, err)
	require.Len(t, entries, 1, "a duplicate must not enqueue a second copy")

	// The per-session budget of 2 was consumed exactly once, so one more
	// distinct message still fits.
	_, _, duplicate, err = store.AcceptQueued(ctx, queuedReq("session-1", "m2"), limits)
	require.NoError(t, err)
	require.False(t, duplicate)
	_, _, _, err = store.AcceptQueued(ctx, queuedReq("session-1", "m3"), limits)
	require.ErrorIs(t, err, ErrQueueFull, "the duplicate must not have consumed a slot")
}

func TestAcceptQueued_ConflictingPayloadIsRejected(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)

	_, _, _, err := store.AcceptQueued(ctx, queuedReq("session-1", "m1"), QueueLimits{})
	require.NoError(t, err)

	conflicting := queuedReq("session-1", "m1")
	conflicting.PayloadHash = "hash_of_something_else"
	_, _, duplicate, err := store.AcceptQueued(ctx, conflicting, QueueLimits{})
	require.ErrorIs(t, err, ErrPayloadConflict)
	require.True(t, duplicate, "the key was taken, so the caller is told it was a duplicate")
}

// TestAcceptQueued_RefusesAtCapacityWithoutEvicting covers both bounds. The
// refusal has to leave the queue untouched: evicting an already-accepted input
// to make room for a new one would turn a durable promise into a silent loss.
func TestAcceptQueued_RefusesAtCapacityWithoutEvicting(t *testing.T) {
	t.Parallel()

	t.Run("per session", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		store, sessions := newTestSQLStore(t)
		seedExtraSession(t, sessions, "session-2")
		limits := QueueLimits{PerSession: 2, Global: 10}

		_, _, _, err := store.AcceptQueued(ctx, queuedReq("session-1", "m1"), limits)
		require.NoError(t, err)
		_, _, _, err = store.AcceptQueued(ctx, queuedReq("session-1", "m2"), limits)
		require.NoError(t, err)

		_, _, _, err = store.AcceptQueued(ctx, queuedReq("session-1", "m3"), limits)
		require.ErrorIs(t, err, ErrQueueFull)

		// A different session is unaffected by session-1's full queue.
		_, _, _, err = store.AcceptQueued(ctx, queuedReq("session-2", "m1"), limits)
		require.NoError(t, err)

		entries, err := store.QueueBySession(ctx, "session-1", 0)
		require.NoError(t, err)
		require.Len(t, entries, 2, "the refused input must not have displaced an accepted one")
	})

	t.Run("global", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		store, sessions := newTestSQLStore(t)
		seedExtraSession(t, sessions, "session-2")
		seedExtraSession(t, sessions, "session-3")
		limits := QueueLimits{PerSession: 10, Global: 2}

		_, _, _, err := store.AcceptQueued(ctx, queuedReq("session-1", "m1"), limits)
		require.NoError(t, err)
		_, _, _, err = store.AcceptQueued(ctx, queuedReq("session-2", "m1"), limits)
		require.NoError(t, err)

		_, _, _, err = store.AcceptQueued(ctx, queuedReq("session-3", "m1"), limits)
		require.ErrorIs(t, err, ErrQueueFull)

		depth, err := store.QueueDepth(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(2), depth)
	})
}

func TestAcceptQueued_RefusesOversizedPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)

	request := queuedReq("session-1", "m1")
	request.Payload = QueuedPayload{Content: string(make([]byte, 1024))}
	_, _, _, err := store.AcceptQueued(ctx, request, QueueLimits{MaxPayloadBytes: 512})
	require.ErrorIs(t, err, ErrQueuePayloadTooLarge)

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth, "an over-sized input must not be partially recorded")
}

// TestAcceptQueued_FailedTransactionLeavesNoReservation drives the accept path
// into a foreign-key failure after the budget row and the ordinal allocator
// were already touched inside the transaction. Both are shared, long-lived
// rows, so a partial commit would leak capacity permanently.
func TestAcceptQueued_FailedTransactionLeavesNoReservation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, sessionStore := newTestSQLStore(t)

	_, _, _, err := store.AcceptQueued(ctx, queuedReq("session-missing", "m1"), QueueLimits{})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrQueueFull, "a foreign-key failure is not a capacity refusal")

	var counters, used int
	require.NoError(t, sessionStore.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM execution_queue_counters`).Scan(&counters))
	require.Zero(t, counters, "the ordinal allocator row must not survive a rolled-back accept")
	require.NoError(t, sessionStore.DB().QueryRowContext(ctx,
		`SELECT used FROM execution_queue_budget WHERE budget_id = 1`).Scan(&used))
	require.Zero(t, used)
}

// TestQueueBySession_ReturnsFIFOOrder proves dispatch order is the
// database-assigned ordinal, not arrival time or client-supplied ordering.
func TestQueueBySession_ReturnsFIFOOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)

	var wantSeq []int64
	for _, msgID := range []string{"m1", "m2", "m3", "m4"} {
		_, entry, _, err := store.AcceptQueued(ctx, queuedReq("session-1", msgID), QueueLimits{})
		require.NoError(t, err)
		wantSeq = append(wantSeq, entry.QueueSeq)
	}

	entries, err := store.QueueBySession(ctx, "session-1", 0)
	require.NoError(t, err)
	require.Len(t, entries, len(wantSeq))
	for i, entry := range entries {
		require.Equal(t, wantSeq[i], entry.QueueSeq)
		payload, err := store.QueuePayload(ctx, entry.ExecutionID)
		require.NoError(t, err)
		require.Equal(t, []string{"please do m1", "please do m2", "please do m3", "please do m4"}[i],
			payload.Content, "dispatch order must follow the content that was queued, not a client clock")
	}

	limited, err := store.QueueBySession(ctx, "session-1", 2)
	require.NoError(t, err)
	require.Len(t, limited, 2, "the head of the queue is what a dispatcher asks for")
	require.Equal(t, entries[0].QueueSeq, limited[0].QueueSeq)
}

// TestQueuedExecution_SurvivesLeaseRecovery is a safety pin, not a behaviour
// description. Lease recovery fences every pending/running execution whose
// lease elapsed; a queued execution holds no lease, so if that filter ever
// widened to include 'queued', every restart would fence inputs that were
// never dispatched and destroy the resumable backlog.
func TestQueuedExecution_SurvivesLeaseRecovery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, sessionStore := newTestSQLStore(t)

	record, _, _, err := store.AcceptQueued(ctx, queuedReq("session-1", "m1"), QueueLimits{})
	require.NoError(t, err)

	// Age the row so any elapsed-lease filter would match it.
	_, err = sessionStore.DB().ExecContext(ctx,
		`UPDATE execution_inputs SET lease_until = 0, created_at = 1 WHERE execution_id = ?`,
		record.ExecutionID)
	require.NoError(t, err)

	result, err := store.RecoverExpiredLeases(ctx, []string{record.ExecutionID})
	require.NoError(t, err)
	require.Zero(t, result.Recovered, "a queued input has no lease to expire")
	require.Equal(t, []string{record.ExecutionID}, result.ConvergedExecutionIDs,
		"a queued input must be reported as no longer renewable, or the caller renews a lease it never took")

	stored, err := store.getByID(ctx, record.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, RuntimeQueued, stored.RuntimeStatus)
	require.Equal(t, StatusAccepted, stored.Status)
	require.Empty(t, stored.FenceReason, "recovery must not fence a never-dispatched input")

	// The gateway's renewal sweep has the same filter and must skip it too.
	renewed, err := store.RenewLeases(ctx, testOwner, LeaseTTL, nil)
	require.NoError(t, err)
	require.Zero(t, renewed)

	entries, err := store.QueueBySession(ctx, "session-1", 0)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the item must still be dispatchable after a recovery pass")
}

// TestQueuedExecution_DoesNotOccupyTheActiveSlot is the second safety pin. The
// single-active index and the lookups behind it exist to stop two turns running
// at once. A backlog must not read as "the session is busy twenty times over",
// or every queued session would be permanently undispatchable.
func TestQueuedExecution_DoesNotOccupyTheActiveSlot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)

	for i := 0; i < 5; i++ {
		_, _, _, err := store.AcceptQueued(ctx, queuedReq("session-1", string(rune('a'+i))), QueueLimits{})
		require.NoError(t, err)
	}

	_, err := store.ActiveBySession(ctx, "session-1")
	require.ErrorIs(t, err, ErrNotFound, "a queued backlog is not an active execution")

	_, err = store.OpenBySession(ctx, "session-1")
	require.ErrorIs(t, err, ErrNotFound, "a queued backlog is not an open execution either")

	_, err = store.FenceBySession(ctx, "session-1")
	require.ErrorIs(t, err, ErrNotFound)

	active, err := store.activeExecutionIDs(ctx, []string{"any-id"})
	require.NoError(t, err)
	require.Empty(t, active)

	// The session is still dispatchable: an ordinary input can be accepted and
	// take the single active slot while five inputs wait behind it.
	_, duplicate, err := store.Accept(ctx, testAcceptReq("session-1", "direct", "hash_direct"))
	require.NoError(t, err)
	require.False(t, duplicate)

	activeRecord, err := store.ActiveBySession(ctx, "session-1")
	require.NoError(t, err)
	require.Equal(t, RuntimePending, activeRecord.RuntimeStatus)
}

// TestQueuedExecution_SurvivesRestart opens a second store on the same
// database. The point of the queue is that a restart re-finds undispatched
// inputs instead of losing them, so the row has to outlive the process.
func TestQueuedExecution_SurvivesRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue_restart.db")

	openStore := func(t *testing.T) *SQLStore {
		t.Helper()
		cfg := config.Default()
		cfg.DB.Path = dbPath
		cfg.DB.SQLite.Path = dbPath
		writeMu := sqlutil.NewWriteMu(sqlutil.DialectSQLite)
		sessionStore, err := session.NewSQLiteStore(ctx, cfg, writeMu)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, sessionStore.Close()) })

		now := time.Now()
		require.NoError(t, sessionStore.Upsert(ctx, &session.SessionInfo{
			ID:         "session-1",
			UserID:     "user-1",
			WorkerType: "claude_code",
			State:      events.StateRunning,
			CreatedAt:  now,
			UpdatedAt:  now,
		}))

		store, err := NewSQLStore(ctx, sessionStore.DB(), dbutil.DialectSQLite, writeMu, nil)
		require.NoError(t, err)
		return store
	}

	before, entry, _, err := openStore(t).AcceptQueued(ctx, queuedReq("session-1", "m1"), QueueLimits{})
	require.NoError(t, err)

	after, err := openStore(t).QueueByExecution(ctx, before.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, entry, after, "the queue entry must be identical after a restart")

	entries, err := openStore(t).QueueBySession(ctx, "session-1", 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestQueueLimits_WithDefaults(t *testing.T) {
	t.Parallel()

	defaults := QueueLimits{}.withDefaults()
	require.Equal(t, DefaultQueuePerSession, defaults.PerSession)
	require.Equal(t, DefaultQueueGlobal, defaults.Global)
	require.Equal(t, DefaultQueueMaxPayloadBytes, defaults.MaxPayloadBytes)
	require.Equal(t, DefaultQueueTTL, defaults.TTL)

	// An unset or nonsensical bound must not become an unbounded queue.
	require.Equal(t, defaults, QueueLimits{PerSession: -1, Global: 0, MaxPayloadBytes: -5, TTL: -time.Second}.withDefaults())

	explicit := QueueLimits{PerSession: 3, Global: 4, MaxPayloadBytes: 5, TTL: time.Minute}
	require.Equal(t, explicit, explicit.withDefaults())
}

func TestQueueDepth_MatchesRows(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, sessions := newTestSQLStore(t)
	seedExtraSession(t, sessions, "session-2")

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth)

	for sessionID, msgIDs := range map[string][]string{
		"session-1": {"m1", "m2"},
		"session-2": {"m1"},
	} {
		for _, msgID := range msgIDs {
			_, _, _, err := store.AcceptQueued(ctx, queuedReq(sessionID, msgID), QueueLimits{})
			require.NoError(t, err)
		}
	}

	depth, err = store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), depth)
}
