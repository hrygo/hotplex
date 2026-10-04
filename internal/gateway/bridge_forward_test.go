package gateway

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/pkg/events"
)

// fakeReplayer is a test double for PendingReplayer. calls counts
// DeliverReplay invocations; lastContent captures the content of the most
// recent replay envelope. When callCh is non-nil, it is closed on the first
// call so the negative test can detect a spurious replay from a t.Cleanup
// select — without any time.Sleep.
type fakeReplayer struct {
	calls       atomic.Int32
	lastContent atomic.Value // string
	callCh      chan struct{}
	err         error
	releaseCh   chan struct{}
	finishedCh  chan struct{}
}

func (r *fakeReplayer) DeliverReplay(ctx context.Context, env *events.Envelope) error {
	if r.finishedCh != nil {
		defer close(r.finishedCh)
	}
	r.calls.Add(1)
	if data, ok := env.Event.Data.(map[string]any); ok {
		if c, _ := data["content"].(string); c != "" {
			r.lastContent.Store(c)
		}
	}
	if r.callCh != nil {
		select {
		case <-r.callCh:
		default:
			close(r.callCh)
		}
	}
	if r.releaseCh != nil {
		select {
		case <-r.releaseCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.err
}

// newDoneBridgeWithOpenExec builds a Bridge + forwardContext + Done envelope
// backed by a fake execution store returning an open Delivered record that
// FinishRuntime accepts. Mirrors the harness in
// TestFinishRuntimeOnDone_UsesEmittingForwarderRunID; the pending buffer is
// initialized (the production Bridge literal leaves it nil).
func newDoneBridgeWithOpenExec(t *testing.T, sessionID string) (*Bridge, *forwardContext, *events.Envelope, *fakeExecutionStore) {
	t.Helper()
	rec := testExecutionRecord(execution.StatusDelivered)
	rec.SessionID = sessionID
	rec.WorkerRunID = "run-current"
	store := &fakeExecutionStore{openRecord: rec}
	b := &Bridge{
		executionStore: store,
		hub:            newTestHub(t),
		log:            testLogger(t),
		pending:        NewPendingBuffer(),
	}
	fc := &forwardContext{sessionID: sessionID, workerRunID: "run-current"}
	env := events.NewEnvelope("evt-done", sessionID, 1, events.Done, events.DoneData{Success: true})
	return b, fc, env, store
}

// TestFinishRuntimeOnDone_ReplaysPending verifies that when Done arrives and
// the active gate is released, a buffered supplement is drained, merged, and
// replayed exactly once via the PendingReplayer. The replay goroutine runs
// async (seq-lease safety), so we poll via require.Eventually — no sleep.
func TestFinishRuntimeOnDone_ReplaysPending(t *testing.T) {
	t.Parallel()
	b, fc, env, _ := newDoneBridgeWithOpenExec(t, "s-replay")
	rp := &fakeReplayer{}
	b.SetPendingReplayer(rp)
	// Buffer a supplement that the Done path must drain + replay.
	b.pending.Append("s-replay", "追问", newInputEnvelope(t, "s-replay", "x"))

	b.finishRuntimeOnDone("s-replay", fc, env)

	// Poll the actual asserted condition (call count AND replayed content) in a
	// single predicate, rather than checking calls then content separately:
	// calls.Add(1) happens before lastContent.Store inside DeliverReplay, so a
	// standalone calls==1 gate can observe the call before the Store is visible,
	// racing the subsequent content assertion under -race load.
	require.Eventually(t, func() bool {
		c, _ := rp.lastContent.Load().(string)
		return rp.calls.Load() == 1 && c == "追问"
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, "追问", rp.lastContent.Load().(string))
}

// TestFinishRuntimeOnDone_NoPendingNoReplay verifies that with an empty
// pending buffer, finishRuntimeOnDone does NOT spawn a replay goroutine.
// Detection uses NO time.Sleep: the fakeReplayer closes callCh if ever
// invoked, and t.Cleanup's select catches the spurious call. A runtime.Gosched
// yields the test goroutine so any spawned replay has a chance to fire before
// cleanup runs — this is a scheduler hint, not a sleep.
func TestFinishRuntimeOnDone_NoPendingNoReplay(t *testing.T) {
	t.Parallel()
	b, fc, env, _ := newDoneBridgeWithOpenExec(t, "s-nopending")
	rp := &fakeReplayer{callCh: make(chan struct{})}
	b.SetPendingReplayer(rp)

	b.finishRuntimeOnDone("s-nopending", fc, env)
	// Yield so a (buggy) spawned goroutine can fire and close callCh before
	// cleanup's select. Not a sleep — just a scheduling hint.
	runtime.Gosched()

	t.Cleanup(func() {
		select {
		case <-rp.callCh:
			t.Errorf("unexpected replay call")
		default:
		}
	})
}

func TestRuntimeRepairSuccess_ReplaysPending(t *testing.T) {
	t.Parallel()
	b, fc, env, store := newDoneBridgeWithOpenExec(t, "s-repair-replay")
	store.finishErr = errors.New("db temporarily unavailable")
	rp := &fakeReplayer{}
	b.SetPendingReplayer(rp)
	b.pending.Append("s-repair-replay", "追问", newInputEnvelope(t, "s-repair-replay", "x"))

	b.finishRuntimeOnDone("s-repair-replay", fc, env)
	require.Equal(t, int32(0), rp.calls.Load())

	b.HandleRepairSuccess(execution.RepairIntent{
		SessionID: "s-repair-replay",
		Kind:      execution.RepairRuntime,
	})
	require.Eventually(t, func() bool {
		return rp.calls.Load() == 1
	}, time.Second, 10*time.Millisecond)
}

func TestReplayFailure_DoesNotRequeueAfterSessionClear(t *testing.T) {
	t.Parallel()
	b, fc, env, _ := newDoneBridgeWithOpenExec(t, "s-replay-clear")
	rp := &fakeReplayer{
		err:        errors.New("session terminated"),
		callCh:     make(chan struct{}),
		releaseCh:  make(chan struct{}),
		finishedCh: make(chan struct{}),
	}
	b.SetPendingReplayer(rp)
	b.pending.Append("s-replay-clear", "追问", newInputEnvelope(t, "s-replay-clear", "x"))

	b.finishRuntimeOnDone("s-replay-clear", fc, env)
	select {
	case <-rp.callCh:
	case <-time.After(time.Second):
		t.Fatal("replay did not start")
	}
	clearDone := make(chan struct{})
	go func() {
		b.ClearPending("s-replay-clear")
		close(clearDone)
	}()
	select {
	case <-clearDone:
		t.Fatal("session clear crossed an in-flight replay fence")
	default:
	}
	close(rp.releaseCh)
	select {
	case <-rp.finishedCh:
	case <-time.After(time.Second):
		t.Fatal("replay did not finish")
	}
	<-clearDone

	_, _, ok := b.pending.DrainAndMerge("s-replay-clear")
	require.False(t, ok, "a failed stale replay must not resurrect cleared content")
}

func TestReplayClearedBeforeDispatchDoesNotReachNewContext(t *testing.T) {
	t.Parallel()
	b, _, _, _ := newDoneBridgeWithOpenExec(t, "s-replay-reset")
	rp := &fakeReplayer{}
	b.SetPendingReplayer(rp)
	b.pending.Append("s-replay-reset", "旧上下文追问", newInputEnvelope(t, "s-replay-reset", "x"))

	fence := b.replayFence("s-replay-reset")
	fence.Lock()
	b.replayPending("s-replay-reset")
	// ResetSession performs this clear while holding the same write fence.
	b.pending.Clear("s-replay-reset")
	fence.Unlock()
	b.WaitPendingReplays(t.Context())

	require.Zero(t, rp.calls.Load(), "stale replay must be fenced before delivery into reset context")
}

func TestStopPendingReplaysBlocksRepairCallback(t *testing.T) {
	t.Parallel()
	b, _, _, _ := newDoneBridgeWithOpenExec(t, "s-replay-shutdown")
	rp := &fakeReplayer{}
	b.SetPendingReplayer(rp)
	b.pending.Append("s-replay-shutdown", "追问", newInputEnvelope(t, "s-replay-shutdown", "x"))

	b.StopPendingReplays()
	b.HandleRepairSuccess(execution.RepairIntent{SessionID: "s-replay-shutdown", Kind: execution.RepairRuntime})
	b.WaitPendingReplays(t.Context())

	require.Zero(t, rp.calls.Load(), "shutdown must reject replay work from a late repair callback")
}

// Releasing the active gate is what lets a session's durable queue promote its
// head. Both trigger points are asserted here, because the queue dispatcher is
// late-injected and neither path had any test: a regression here would leave a
// queued input sitting undispatched forever with everything reporting success.
//
// dispatchQueued runs async (it is called while the forwarding path holds a seq
// lease), so both cases poll the condition instead of sleeping.
func TestTerminalTurnWakesTheDurableQueue(t *testing.T) {
	t.Parallel()

	t.Run("done releases the gate and promotes the queue head", func(t *testing.T) {
		t.Parallel()
		b, fc, env, _ := newDoneBridgeWithOpenExec(t, "s-queue-done")
		dispatcher := &recordingQueueDispatcher{}
		b.SetQueueDispatcher(dispatcher)

		b.finishRuntimeOnDone("s-queue-done", fc, env)

		require.Eventually(t, func() bool {
			return len(dispatcher.dispatchedSessions()) == 1
		}, time.Second, 10*time.Millisecond)
		require.Equal(t, []string{"s-queue-done"}, dispatcher.dispatchedSessions())
	})

	// A runtime terminal write that failed is replayed by the repairer. When it
	// finally succeeds the gate is released too, and a queue parked behind that
	// failure must be woken as well -- otherwise the repair releases the gate
	// and the queue never notices.
	t.Run("a repaired terminal write also wakes the queue", func(t *testing.T) {
		t.Parallel()
		b, fc, env, store := newDoneBridgeWithOpenExec(t, "s-queue-repair")
		store.finishErr = errors.New("db temporarily unavailable")
		dispatcher := &recordingQueueDispatcher{}
		b.SetQueueDispatcher(dispatcher)

		b.finishRuntimeOnDone("s-queue-repair", fc, env)
		// The durable write failed, so nothing may have been promoted.
		require.Never(t, func() bool {
			return len(dispatcher.dispatchedSessions()) > 0
		}, 50*time.Millisecond, 10*time.Millisecond)

		b.HandleRepairSuccess(execution.RepairIntent{
			SessionID: "s-queue-repair",
			Kind:      execution.RepairRuntime,
		})

		require.Eventually(t, func() bool {
			return len(dispatcher.dispatchedSessions()) == 1
		}, time.Second, 10*time.Millisecond)
		require.Equal(t, []string{"s-queue-repair"}, dispatcher.dispatchedSessions())
	})
}
