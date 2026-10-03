package execution

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fillQueue enqueues n inputs for a session and returns their queue entries.
func fillQueue(t *testing.T, store *SQLStore, sessionID string, n int) []*QueueEntry {
	t.Helper()
	entries := make([]*QueueEntry, 0, n)
	for i := range n {
		_, entry, _, err := store.AcceptQueued(
			context.Background(), queuedReq(sessionID, string(rune('a'+i))), QueueLimits{})
		require.NoError(t, err)
		entries = append(entries, entry)
	}
	return entries
}

// TestClaimQueued_PromotesHeadAcrossTheDispatchBoundary pins the transition
// that everything else depends on. Claiming is where a queued input stops being
// safely resumable and starts being an ordinary lease-protected dispatch, so
// the test asserts all three facts at once: the head (not a later item) moved,
// it now holds the caller's lease, and it no longer occupies a queue row.
func TestClaimQueued_PromotesHeadAcrossTheDispatchBoundary(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	entries := fillQueue(t, store, "session-1", 3)

	claimed, entry, err := store.ClaimQueued(ctx, ClaimQueuedRequest{
		SessionID:       "session-1",
		OwnerInstanceID: testOwner,
		WorkerRunID:     testRun,
	})
	require.NoError(t, err)
	require.Equal(t, entries[0].ExecutionID, entry.ExecutionID, "FIFO: the head must be claimed first")

	require.Equal(t, RuntimePending, claimed.RuntimeStatus)
	require.Equal(t, StatusAccepted, claimed.Status)
	require.Equal(t, testOwner, claimed.OwnerInstanceID)
	require.Equal(t, testRun, claimed.WorkerRunID)
	require.Greater(t, claimed.LeaseUntil, claimed.UpdatedAt, "a claim must take an owner lease")
	require.NotEmpty(t, claimed.ClientMessageID, "the stored record carries the input key")

	_, err = store.QueueByExecution(ctx, claimed.ExecutionID)
	require.ErrorIs(t, err, ErrNotFound, "a claimed input is no longer queued")

	depth, err := store.QueueDepthBySession(ctx, "session-1")
	require.NoError(t, err)
	require.Equal(t, int64(2), depth)

	// It now occupies the single active slot, so the next claim must wait.
	active, err := store.ActiveBySession(ctx, "session-1")
	require.NoError(t, err)
	require.Equal(t, claimed.ExecutionID, active.ExecutionID)

	_, _, err = store.ClaimQueued(ctx, ClaimQueuedRequest{
		SessionID:       "session-1",
		OwnerInstanceID: testOwner,
		WorkerRunID:     testRun,
	})
	require.ErrorIs(t, err, ErrSessionBusy, "a busy session leaves its queue intact")

	remaining, err := store.QueueBySession(ctx, "session-1", 0)
	require.NoError(t, err)
	require.Len(t, remaining, 2, "a refused claim must not drop anything")
}

// TestClaimQueued_RefusesHeadTheCallerDidNotValidate is the reason
// ExpectedExecutionID exists. A dispatcher validates an item before dispatching
// it (session ownership, permissions, skill materialization); if another
// process claimed or settled that item in between, the claim must fail rather
// than promote a DIFFERENT item the caller never inspected.
func TestClaimQueued_RefusesHeadTheCallerDidNotValidate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	entries := fillQueue(t, store, "session-1", 2)

	_, _, err := store.ClaimQueued(ctx, ClaimQueuedRequest{
		SessionID:           "session-1",
		OwnerInstanceID:     testOwner,
		WorkerRunID:         testRun,
		ExpectedExecutionID: entries[1].ExecutionID,
	})
	require.ErrorIs(t, err, ErrQueueHeadMoved)

	_, _, _, err = store.AcceptQueued(ctx, QueuedRequest{
		SessionID:         "session-1",
		ClientMessageID:   "rev7",
		PayloadHash:       "hash_rev7",
		LifecycleRevision: 7,
	}, QueueLimits{})
	require.NoError(t, err)

	// The stale-revision check is independent of the head check.
	head, err := store.QueueBySession(ctx, "session-1", 0)
	require.NoError(t, err)
	require.Len(t, head, 3)
	_, _, err = store.ClaimQueued(ctx, ClaimQueuedRequest{
		SessionID:         "session-1",
		OwnerInstanceID:   testOwner,
		WorkerRunID:       testRun,
		LifecycleRevision: 6,
	})
	require.ErrorIs(t, err, ErrQueueLifecycleStale,
		"an item from a superseded lifecycle must not be dispatched")
}

func TestClaimQueued_EmptyQueueIsNotAnError(t *testing.T) {
	t.Parallel()

	store, _ := newTestSQLStore(t)
	_, _, err := store.ClaimQueued(context.Background(), ClaimQueuedRequest{
		SessionID:       "session-1",
		OwnerInstanceID: testOwner,
		WorkerRunID:     testRun,
	})
	require.ErrorIs(t, err, ErrNotFound)
}

// TestCancelQueued_SettlesWithoutEverRunning covers the distinction the plan
// insists on: a cancelled queued input never reached a worker, so its code says
// so. A console that reads QUEUE_CANCELLED as "the worker failed" is describing
// a turn that never happened.
func TestCancelQueued_SettlesWithoutEverRunning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	entries := fillQueue(t, store, "session-1", 2)

	cancelled, err := store.CancelQueued(ctx, entries[0].ExecutionID, QueueReasonCancelled)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, cancelled.Status)
	require.Equal(t, RuntimeFailed, cancelled.RuntimeStatus)
	require.Equal(t, QueueReasonCancelled, cancelled.ErrorCode)
	require.Equal(t, QueueReasonCancelled, cancelled.RuntimeErrorCode)
	require.Empty(t, cancelled.WorkerRunID, "a cancelled queued input never entered a worker run")

	_, err = store.QueueByExecution(ctx, entries[0].ExecutionID)
	require.ErrorIs(t, err, ErrNotFound)

	// The next item is untouched and becomes the head.
	head, err := store.QueueBySession(ctx, "session-1", 0)
	require.NoError(t, err)
	require.Len(t, head, 1)
	require.Equal(t, entries[1].ExecutionID, head[0].ExecutionID)
}

// TestCancelQueued_RefusesDispatchedInput is a safety property, not a
// convenience: cancelling an input that already entered a worker run would
// record "never ran" for a turn that ran.
func TestCancelQueued_RefusesDispatchedInput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	fillQueue(t, store, "session-1", 1)

	claimed, _, err := store.ClaimQueued(ctx, ClaimQueuedRequest{
		SessionID:       "session-1",
		OwnerInstanceID: testOwner,
		WorkerRunID:     testRun,
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkRunning(ctx, claimed.ExecutionID, testOwner, testRun))

	_, err = store.CancelQueued(ctx, claimed.ExecutionID, QueueReasonCancelled)
	require.ErrorIs(t, err, ErrQueueNotQueued,
		"a dispatched input must go through the ordinary stop path, not a queued cancel")

	running, err := store.getByID(ctx, claimed.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, RuntimeRunning, running.RuntimeStatus)
}

// TestClearQueue_SettlesEverythingUndispatched is what /reset and session delete
// rely on: a queue belonging to an abandoned turn must never dispatch after the
// turn is gone. The dispatch fact of already-running inputs is preserved.
func TestClearQueue_SettlesEverythingUndispatched(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	entries := fillQueue(t, store, "session-1", 4)

	cleared, err := store.ClearQueue(ctx, "session-1", QueueReasonCancelled)
	require.NoError(t, err)
	require.Equal(t, int64(4), cleared)

	depth, err := store.QueueDepthBySession(ctx, "session-1")
	require.NoError(t, err)
	require.Zero(t, depth, "a cleared queue holds nothing that could resurrect a turn")

	for _, entry := range entries {
		record, err := store.getByID(ctx, entry.ExecutionID)
		require.NoError(t, err)
		require.Equal(t, QueueReasonCancelled, record.RuntimeErrorCode,
			"the cancellation fact is kept, not erased")
		require.Equal(t, RuntimeFailed, record.RuntimeStatus)
	}
}

// TestClearQueue_LeavesDispatchedInputAlone proves clear is scoped to the queue
// and does not double-settle a running turn.
func TestClearQueue_LeavesDispatchedInputAlone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	entries := fillQueue(t, store, "session-1", 2)

	claimed, _, err := store.ClaimQueued(ctx, ClaimQueuedRequest{
		SessionID:       "session-1",
		OwnerInstanceID: testOwner,
		WorkerRunID:     testRun,
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkRunning(ctx, claimed.ExecutionID, testOwner, testRun))

	cleared, err := store.ClearQueue(ctx, "session-1", QueueReasonCancelled)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleared, "only the still-queued item may be settled")

	running, err := store.getByID(ctx, claimed.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, RuntimeRunning, running.RuntimeStatus)
	require.Equal(t, entries[0].ExecutionID, running.ExecutionID)
}

// TestExpireQueued_SettlesPastTTLOnly proves the sweep is bounded by time, not
// by queue position: an expired item must be settled while a fresh one behind it
// survives.
func TestExpireQueued_SettlesPastTTLOnly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)

	_, stale, _, err := store.AcceptQueued(ctx, queuedReq("session-1", "stale"), QueueLimits{TTL: time.Hour})
	require.NoError(t, err)
	_, fresh, _, err := store.AcceptQueued(ctx, queuedReq("session-1", "fresh"), QueueLimits{TTL: 24 * time.Hour})
	require.NoError(t, err)

	expired, err := store.ExpireQueued(ctx, time.UnixMilli(stale.EnqueuedAt).Add(2*time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	require.Equal(t, stale.ExecutionID, expired[0].ExecutionID)
	require.Equal(t, QueueReasonExpired, expired[0].RuntimeErrorCode)

	_, err = store.QueueByExecution(ctx, fresh.ExecutionID)
	require.NoError(t, err, "an unexpired input must survive the sweep")
}

// TestExpireQueued_HonoursTheLimit proves the sweep is bounded, because it
// runs on a timer over a shared table.
func TestExpireQueued_HonoursTheLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	for _, msgID := range []string{"e1", "e2", "e3"} {
		_, _, _, err := store.AcceptQueued(ctx, queuedReq("session-1", msgID), QueueLimits{TTL: time.Hour})
		require.NoError(t, err)
	}

	batch, err := store.ExpireQueued(ctx, time.Now().Add(2*time.Hour), 2)
	require.NoError(t, err)
	require.Len(t, batch, 2)

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), depth, "the sweep must stop at its limit")
}

func TestQueueDepthBySession_ScopesToOneSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, sessions := newTestSQLStore(t)
	seedExtraSession(t, sessions, "session-2")

	fillQueue(t, store, "session-1", 3)
	fillQueue(t, store, "session-2", 1)

	depth, err := store.QueueDepthBySession(ctx, "session-1")
	require.NoError(t, err)
	require.Equal(t, int64(3), depth)

	depth, err = store.QueueDepthBySession(ctx, "session-2")
	require.NoError(t, err)
	require.Equal(t, int64(1), depth)

	depth, err = store.QueueDepthBySession(ctx, "session-missing")
	require.NoError(t, err)
	require.Zero(t, depth)
}

// TestQueuedInputs_SurviveGatewayShutdown is the durability half of the
// restart story. Shutdown fences what it was actually running, because those
// responses may be lost; a queued input was never sent, so fencing it would
// convert a restart into data loss. It must still be claimable afterwards.
func TestQueuedInputs_SurviveGatewayShutdown(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	entries := fillQueue(t, store, "session-1", 2)

	terminated, err := store.TerminateOwnerLeases(ctx, testOwner, "GATEWAY_SHUTDOWN")
	require.NoError(t, err)
	require.Zero(t, terminated, "shutdown must not touch inputs it never dispatched")

	for _, entry := range entries {
		record, err := store.getByID(ctx, entry.ExecutionID)
		require.NoError(t, err)
		require.Equal(t, RuntimeQueued, record.RuntimeStatus)
		require.Empty(t, record.FenceReason)
	}

	claimed, queueEntry, err := store.ClaimQueued(ctx, ClaimQueuedRequest{
		SessionID:       "session-1",
		OwnerInstanceID: "gw-after-restart",
		WorkerRunID:     "run-after-restart",
	})
	require.NoError(t, err, "a queued input must still be dispatchable after a restart")
	require.Equal(t, entries[0].ExecutionID, queueEntry.ExecutionID)
	require.Equal(t, "gw-after-restart", claimed.OwnerInstanceID)
}
