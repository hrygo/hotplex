package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

type lifecycleReviewSessionManager struct {
	SessionManager

	mu       sync.Mutex
	accepted []time.Time
	err      error
}

func (m *lifecycleReviewSessionManager) RecordInputAccepted(_ context.Context, _ string, acceptedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accepted = append(m.accepted, acceptedAt)
	return m.err
}

func (m *lifecycleReviewSessionManager) acceptedAt() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Time(nil), m.accepted...)
}

func newLifecycleReviewQueueHandler(
	t *testing.T,
	recorderErr error,
) (*Handler, *lifecycleReviewSessionManager, *mockPlatformConn, *execution.SQLStore) {
	t.Helper()

	h, _, conn, store, _ := newQueueHandler(t, "s-lifecycle-review")
	recorder := &lifecycleReviewSessionManager{
		SessionManager: h.sm,
		err:            recorderErr,
	}
	h.sm = recorder
	bridge := NewBridge(BridgeDeps{Log: testLogger(t), Hub: h.hub})
	t.Cleanup(bridge.shutdownCancel)
	h.bridge = bridge

	return h, recorder, conn, store
}

func TestHandleSupplementOnBusy_RecordsLifecycleOnceForDurableQueue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, recorder, conn, store := newLifecycleReviewQueueHandler(t, nil)
	env := queuedInputEnv("s-lifecycle-review", "m-lifecycle-review", "later please")

	require.NoError(t, h.handleSupplementOnBusy(ctx, env, "later please", nil))
	require.Eventually(t, func() bool {
		return len(inputAcks(t, conn)) == 1
	}, 2*time.Second, 5*time.Millisecond, "first durable queue acceptance must be acknowledged")

	record, err := store.QueuedByClientMessage(ctx, env.SessionID, clientMessageID(env))
	require.NoError(t, err)
	require.NotNil(t, record)
	require.True(t, record.IsQueued())

	accepted := recorder.acceptedAt()
	require.Len(t, accepted, 1, "the first durable acceptance must advance session lifecycle once")
	require.Equal(t, time.UnixMilli(record.CreatedAt), accepted[0],
		"the queue's durable acceptance time is the lifecycle activity time")

	// The committed supplement disposition makes this a duplicate retry. It
	// should return the existing durable queue record without renewing again.
	require.NoError(t, h.handleSupplementOnBusy(ctx, env, "later please", nil))
	require.Eventually(t, func() bool {
		return len(inputAcks(t, conn)) == 2
	}, 2*time.Second, 5*time.Millisecond, "duplicate queued input must receive the existing queue acknowledgement")
	require.Len(t, recorder.acceptedAt(), 1, "a duplicate must not move the session deadline")

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), depth, "a retry must keep exactly one durable queue item")
}

func TestHandleSupplementOnBusy_RejectsQueueWhenLifecycleRenewalFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, recorder, conn, store := newLifecycleReviewQueueHandler(t, errors.New("lifecycle store unavailable"))
	env := queuedInputEnv("s-lifecycle-review", "m-lifecycle-review-fail", "later please")

	_ = h.handleSupplementOnBusy(ctx, env, "later please", nil)

	countErrors := func() int {
		count := 0
		for _, got := range conn.envelopes() {
			if got.Event.Type == events.Error {
				count++
			}
		}
		return count
	}
	require.Eventually(t, func() bool {
		return countErrors() == 1
	}, 2*time.Second, 5*time.Millisecond, "a failed lifecycle update must be reported as an error")

	// The first acceptance was settled after lifecycle persistence failed.
	// Retrying the same id must preserve the original failure result instead of
	// presenting its settled execution as a still-queued input.
	_ = h.handleSupplementOnBusy(ctx, env, "later please", nil)
	require.Eventually(t, func() bool {
		return countErrors() == 2
	}, 2*time.Second, 5*time.Millisecond, "a retry after lifecycle failure must report the failure again")

	require.Empty(t, inputAcks(t, conn), "the gateway must not acknowledge an unrenewed queued input")
	_, _, buffered := h.bridge.pending.DrainAndMerge(env.SessionID)
	require.False(t, buffered, "a lifecycle failure must not fall back to volatile buffering")
	require.Len(t, recorder.acceptedAt(), 1, "idempotent retry must not repeat lifecycle persistence")

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth, "the queue row must be cancelled when lifecycle renewal fails")
}

func TestHandleSupplementOnBusy_CancelledQueueDuplicateReportsTerminalStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, recorder, conn, store := newLifecycleReviewQueueHandler(t, nil)
	env := queuedInputEnv("s-lifecycle-review", "m-lifecycle-review-cancelled", "later please")

	require.NoError(t, h.handleSupplementOnBusy(ctx, env, "later please", nil))
	require.Eventually(t, func() bool {
		return len(inputAcks(t, conn)) == 1
	}, 2*time.Second, 5*time.Millisecond, "first durable queue acceptance must be acknowledged")

	record, err := store.QueuedByClientMessage(ctx, env.SessionID, clientMessageID(env))
	require.NoError(t, err)
	require.NotNil(t, record)
	cancelled, err := store.CancelQueued(ctx, record.ExecutionID, execution.QueueReasonCancelled)
	require.NoError(t, err)
	require.Equal(t, execution.StatusFailed, cancelled.Status)

	// This same-Handler retry hits the cached supplementQueued disposition.
	// The queue has since settled, so the response must reflect that durable
	// terminal state rather than claiming the input is still queued.
	require.NoError(t, h.handleSupplementOnBusy(ctx, env, "later please", nil))
	require.Eventually(t, func() bool {
		return len(inputAcks(t, conn)) == 2
	}, 2*time.Second, 5*time.Millisecond, "a settled duplicate must receive its actual durable status")

	acks := inputAcks(t, conn)
	require.Len(t, acks, 2)
	require.Equal(t, events.ExecutionStatusFailed, acks[1].Status)
	require.Equal(t, events.ErrorCode(execution.QueueReasonCancelled), acks[1].ErrorCode)
	require.True(t, acks[1].Duplicate)
	require.NotEqual(t, events.InputModeQueued, acks[1].InputMode,
		"a cancelled queue record must not be acknowledged as still queued")
	require.Len(t, recorder.acceptedAt(), 1, "a terminal duplicate must not renew lifecycle again")

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth, "retrying a cancelled input must not recreate the queue item")
}

type lifecycleReviewForwardWorker struct {
	*fakeWorker

	firstMessage sync.Once
	messageReady chan struct{}
	startBoth    <-chan struct{}
	terminated   chan struct{}
	termOnce     sync.Once
}

func (w *lifecycleReviewForwardWorker) SetLastIO(time.Time) {
	w.firstMessage.Do(func() {
		close(w.messageReady)
		<-w.startBoth
	})
}

func (w *lifecycleReviewForwardWorker) Terminate(context.Context) error {
	w.termOnce.Do(func() { close(w.terminated) })
	return nil
}

func registeredTurnTimeoutHandler(lifecycle *workerRunLifecycle) func(uint64) {
	lifecycle.turnTimeoutMu.Lock()
	defer lifecycle.turnTimeoutMu.Unlock()
	return lifecycle.turnTimeoutHandler
}

func TestTurnTimeoutConcurrentForwardingKeepsOneTerminal(t *testing.T) {
	t.Parallel()

	const sessionID = "lifecycle-timeout-forwarding"
	hub := newTestHub(t)
	conn := &mockPlatformConn{}
	hub.JoinPlatformSession(sessionID, conn)

	workerConn := &fakeWorkerConn{ch: make(chan *events.Envelope, 4)}
	startBoth := make(chan struct{})
	w := &lifecycleReviewForwardWorker{
		fakeWorker:   &fakeWorker{workerType: worker.TypeACP, conn: workerConn},
		messageReady: make(chan struct{}),
		startBoth:    startBoth,
		terminated:   make(chan struct{}),
	}
	lifecycle := newWorkerRunLifecycle(workerConn)
	b := NewBridge(BridgeDeps{Log: slog.Default(), Hub: hub, TurnTimeout: time.Hour})
	t.Cleanup(b.shutdownCancel)

	forwardDone := make(chan struct{})
	go func() {
		b.forwardEvents(forwarderBinding{
			worker:    w,
			conn:      workerConn,
			lifecycle: lifecycle,
		}, sessionID, forwardOpts{ctx: t.Context(), workDir: t.TempDir()})
		close(forwardDone)
	}()

	require.Eventually(t, func() bool {
		return registeredTurnTimeoutHandler(lifecycle) != nil
	}, 2*time.Second, 5*time.Millisecond, "forwarder must register its real timeout callback")
	generation := lifecycle.armTurnTimeout(time.Now().Add(time.Hour))
	timeoutHandler := registeredTurnTimeoutHandler(lifecycle)
	require.NotNil(t, timeoutHandler)

	// Keep the message producer and the real callback poised at the same
	// boundary. Under -race this exercises the callback against the actual
	// forwardEvents content accumulator rather than a stand-alone field probe.
	workerConn.ch <- events.NewEnvelope("message", sessionID, 0, events.MessageDelta,
		map[string]any{"content": strings.Repeat("x", 64)})
	require.Eventually(t, func() bool {
		select {
		case <-w.messageReady:
			return true
		default:
			return false
		}
	}, 2*time.Second, 5*time.Millisecond, "message event must reach its forwarding boundary")

	callbackDone := make(chan struct{})
	go func() {
		<-startBoth
		timeoutHandler(generation)
		close(callbackDone)
	}()
	close(startBoth)

	require.Eventually(t, func() bool {
		select {
		case <-callbackDone:
			return true
		default:
			return false
		}
	}, 2*time.Second, 5*time.Millisecond, "the real timeout callback must finish")
	select {
	case <-w.terminated:
	default:
		require.FailNow(t, "timeout callback did not terminate the worker")
	}

	// Worker-side terminal events arriving after the timeout must be fenced.
	workerConn.ch <- events.NewEnvelope("late-message", sessionID, 0, events.MessageDelta,
		map[string]any{"content": "late"})
	workerConn.ch <- events.NewEnvelope("late-done", sessionID, 0, events.Done,
		events.DoneData{Success: true})
	close(workerConn.ch)

	require.Eventually(t, func() bool {
		select {
		case <-forwardDone:
			return true
		default:
			return false
		}
	}, 2*time.Second, 5*time.Millisecond, "forwarder must finish after the Worker stream closes")

	require.Eventually(t, func() bool {
		terminals := 0
		for _, got := range conn.envelopes() {
			if got.Event.Type == events.Done || got.Event.Type == events.Error {
				terminals++
			}
		}
		return terminals == 1
	}, 2*time.Second, 5*time.Millisecond, "one timed-out turn must expose exactly one terminal")
}
