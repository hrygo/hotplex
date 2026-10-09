package gateway

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/pkg/events"
)

// newQueueHandler builds a handler backed by a REAL execution store with the
// durable queue enabled, so these tests exercise the actual accept and claim
// transactions rather than a fake that agrees with whatever the code does.
func newQueueHandler(t *testing.T, sessionID string) (*Handler, *mockWorkerForHandler, *mockPlatformConn, *execution.SQLStore, *sql.DB) {
	t.Helper()
	ctx := context.Background()

	cfg := config.Default()
	cfg.DB.Path = filepath.Join(t.TempDir(), "queue.db")
	cfg.DB.SQLite.Path = cfg.DB.Path
	writeMu := sqlutil.NewWriteMu(sqlutil.DialectSQLite)
	sessionStore, err := session.NewSQLiteStore(ctx, cfg, writeMu)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sessionStore.Close() })

	now := time.Now()
	require.NoError(t, sessionStore.Upsert(ctx, &session.SessionInfo{
		ID:         sessionID,
		UserID:     "user1",
		WorkerType: "claude_code",
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}))
	store, err := execution.NewSQLStore(ctx, sessionStore.DB(), dbutil.DialectSQLite, writeMu, testLogger(t))
	require.NoError(t, err)

	sm := new(mockInputSM)
	w := new(mockWorkerForHandler)
	sm.On("Get", sessionID).Return(&session.SessionInfo{
		State: events.StateIdle, Platform: "webchat", WorkDir: t.TempDir(),
	}, nil).Maybe()
	sm.On("GetWorker", sessionID).Return(w).Maybe()
	sm.On("TransitionWithInput", mock.Anything, sessionID, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()

	hub := newTestHub(t)
	conn := &mockPlatformConn{}
	hub.JoinPlatformSession(sessionID, conn)

	queueCfg := config.Default()
	queueCfg.Execution.Queue.Enabled = true
	h := &Handler{
		log:             testLogger(t),
		hub:             hub,
		sm:              sm,
		executionStore:  store,
		ownerInstanceID: "gw-test",
		configProvider:  func() *config.Config { return queueCfg },
	}
	return h, w, conn, store, sessionStore.DB()
}

func queuedInputEnv(sessionID, clientMessageID, content string) *events.Envelope {
	return events.NewEnvelope("evt-"+clientMessageID, sessionID, 0, events.Input, map[string]any{
		"content":           content,
		"client_message_id": clientMessageID,
	})
}

func inputAcks(t *testing.T, conn *mockPlatformConn) []events.InputAckData {
	t.Helper()
	var acks []events.InputAckData
	for _, env := range conn.envelopes() {
		if env.Event.Type != events.InputAck {
			continue
		}
		data, ok := env.Event.Data.(events.InputAckData)
		require.True(t, ok, "input ack must decode as InputAckData")
		acks = append(acks, data)
	}
	return acks
}

// ageQueue pushes a session's queued items past their TTL without sleeping.
// Async tests in this project forbid time.Sleep.
func ageQueue(t *testing.T, db *sql.DB, sessionID string, expiresAt time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`UPDATE execution_queue SET expires_at = ? WHERE session_id = ?`,
		expiresAt.UnixMilli(), sessionID)
	require.NoError(t, err)
}

// TestQueue_DisabledByDefault proves the safe state: with no queue config the
// busy path must not create a durable record, because nothing would dispatch
// it.
func TestQueue_DisabledByDefault(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, _, _, store, _ := newQueueHandler(t, "s-exec")
	h.configProvider = func() *config.Config { return config.Default() }
	require.False(t, h.queueEnabled())

	env := queuedInputEnv("s-exec", "m1", "later please")
	record, queued, err := h.EnqueueBusyInput(ctx, env, "later please", nil)
	require.NoError(t, err)
	require.False(t, queued)
	require.Nil(t, record)

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth, "a disabled queue must not accumulate anything")
}

// TestEnqueueBusyInput_ACKsQueuedDurably pins what the client is told. The ACK
// must name the REAL durable execution (an operator can look it up), and must
// say accepted + queued + durable — never delivered, which would claim a worker
// already has it.
func TestEnqueueBusyInput_ACKsQueuedDurably(t *testing.T) {
	t.Parallel()

	h, _, conn, store, _ := newQueueHandler(t, "s-exec")
	env := queuedInputEnv("s-exec", "m1", "later please")

	record, queued, err := h.EnqueueBusyInput(context.Background(), env, "later please", nil)
	require.NoError(t, err)
	require.True(t, queued)
	require.True(t, record.IsQueued())
	require.NotContains(t, record.ExecutionID, "supplement-",
		"a queued input is a real durable execution, not a synthetic correlation id")

	h.ackQueuedInput(context.Background(), env, record, false)
	require.Eventually(t, func() bool { return len(inputAcks(t, conn)) >= 1 },
		2*time.Second, 5*time.Millisecond, "the queued ACK must reach the client")
	acks := inputAcks(t, conn)
	require.Len(t, acks, 1)
	require.Equal(t, events.ExecutionStatusAccepted, acks[0].Status)
	require.Equal(t, events.InputModeQueued, acks[0].InputMode)
	require.Equal(t, events.InputDurabilityDurable, acks[0].Durability)
	require.Equal(t, record.ExecutionID, acks[0].ExecutionID)

	payload, err := store.QueuePayload(context.Background(), record.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, "later please", payload.Content)
}

func TestEnqueueBusyInput_UsesInteractiveTTL(t *testing.T) {
	t.Parallel()

	h, _, _, store, _ := newQueueHandler(t, "s-exec")
	cfg := config.Default()
	cfg.Execution.Queue.Enabled = true
	cfg.Execution.Queue.InteractiveTTL = 15 * time.Minute
	cfg.Execution.Queue.TTL = 24 * time.Hour
	h.configProvider = func() *config.Config { return cfg }

	record, queued, err := h.EnqueueBusyInput(context.Background(),
		queuedInputEnv("s-exec", "m1", "later please"), "later please", nil)
	require.NoError(t, err)
	require.True(t, queued)

	entry, err := store.QueueByExecution(context.Background(), record.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, int64((15*time.Minute)/time.Millisecond), entry.ExpiresAt-entry.EnqueuedAt)
}

// TestEnqueueBusyInput_DuplicateIsOneQueueEntry proves a retried submission
// does not double the queue, and that a different payload under the same
// client message id is refused rather than quietly queued as a second input.
func TestEnqueueBusyInput_DuplicateIsOneQueueEntry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, _, _, store, _ := newQueueHandler(t, "s-exec")
	env := queuedInputEnv("s-exec", "m1", "later please")

	first, _, err := h.EnqueueBusyInput(ctx, env, "later please", nil)
	require.NoError(t, err)
	second, _, err := h.EnqueueBusyInput(ctx, env, "later please", nil)
	require.NoError(t, err)
	require.Equal(t, first.ExecutionID, second.ExecutionID)

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), depth)

	conflicting := queuedInputEnv("s-exec", "m1", "something else entirely")
	_, _, err = h.EnqueueBusyInput(ctx, conflicting, "something else entirely", nil)
	require.ErrorIs(t, err, execution.ErrPayloadConflict)
}

// TestDispatchQueued_DeliversTheHeadToTheWorker is the end-to-end proof that
// the queue actually dispatches. A queue that accepted inputs and never sent
// them would be worse than not having one.
func TestDispatchQueued_DeliversTheHeadToTheWorker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, w, _, store, _ := newQueueHandler(t, "s-exec")
	w.On("Input", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	first, _, err := h.EnqueueBusyInput(ctx, queuedInputEnv("s-exec", "m1", "first"), "first", nil)
	require.NoError(t, err)
	second, _, err := h.EnqueueBusyInput(ctx, queuedInputEnv("s-exec", "m2", "second"), "second", nil)
	require.NoError(t, err)

	h.DispatchQueued(ctx, "s-exec")

	require.Eventually(t, func() bool { return len(w.Calls) >= 1 },
		2*time.Second, 5*time.Millisecond)
	require.Equal(t, "first", w.Calls[0].Arguments.Get(1), "FIFO: the head goes first")
	require.Len(t, w.Calls, 1, "only the head may be dispatched while the gate is held")

	_, err = store.QueueByExecution(ctx, first.ExecutionID)
	require.ErrorIs(t, err, execution.ErrNotFound, "a dispatched input leaves the queue")
	_, err = store.QueueByExecution(ctx, second.ExecutionID)
	require.NoError(t, err, "the next input stays queued for the next gate release")
}

// TestDispatchQueued_EmptyQueueIsANoOp keeps the gate-release hook cheap: it
// runs on every turn completion, including the overwhelming majority where
// nothing is queued.
func TestDispatchQueued_EmptyQueueIsANoOp(t *testing.T) {
	t.Parallel()

	h, w, _, _, _ := newQueueHandler(t, "s-exec")
	h.DispatchQueued(context.Background(), "s-exec")
	h.DispatchQueued(context.Background(), "")
	require.Empty(t, w.Calls)
}

// TestClearSessionQueue_SettlesEverythingUndispatched is the reset/delete
// contract: after a clear, nothing from the abandoned turn can dispatch, and
// the cancellation fact remains readable in the ledger.
func TestClearSessionQueue_SettlesEverythingUndispatched(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, w, _, store, _ := newQueueHandler(t, "s-exec")
	_, _, err := h.EnqueueBusyInput(ctx, queuedInputEnv("s-exec", "m1", "abandoned"), "abandoned", nil)
	require.NoError(t, err)

	cleared, err := h.ClearSessionQueue(ctx, "s-exec")
	require.NoError(t, err)
	require.Equal(t, int64(1), cleared)

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth)

	w.On("Input", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	h.DispatchQueued(ctx, "s-exec")
	require.Empty(t, w.Calls, "a cleared queue must not resurrect the abandoned input")
}

// TestSweepExpiredQueue_SettlesPastTTL proves the sweeper settles what it
// should and leaves unexpired inputs alone.
func TestSweepExpiredQueue_SettlesPastTTL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, _, _, store, db := newQueueHandler(t, "s-exec")

	_, _, err := h.EnqueueBusyInput(ctx, queuedInputEnv("s-exec", "old", "old"), "old", nil)
	require.NoError(t, err)
	require.Zero(t, h.SweepExpiredQueue(ctx), "nothing has expired yet")

	ageQueue(t, db, "s-exec", time.Now().Add(-time.Hour))
	require.Equal(t, 1, h.SweepExpiredQueue(ctx))

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth)
}

// TestCancelQueuedInput_SettlesOneUndispatchedInput covers the operator
// surface. A dispatched input is a different story and is covered by the
// store's ErrQueueNotQueued contract.
func TestCancelQueuedInput_SettlesOneUndispatchedInput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, _, _, store, _ := newQueueHandler(t, "s-exec")
	record, _, err := h.EnqueueBusyInput(ctx, queuedInputEnv("s-exec", "m1", "never mind"), "never mind", nil)
	require.NoError(t, err)

	cancelled, err := h.CancelQueuedInput(ctx, record.ExecutionID)
	require.NoError(t, err)
	require.True(t, cancelled)

	depth, err := store.QueueDepth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth)

	cancelled, err = h.CancelQueuedInput(ctx, record.ExecutionID)
	require.NoError(t, err)
	require.False(t, cancelled, "an input that is already settled is not cancelled again")
}

// TestQueueSettle_EmitsTerminalRuntimeEvent proves #851: a queue settlement
// reaches the client as a terminal runtime event correlated by execution_id,
// so a queued input that never ran is observable, not silent.
func TestQueueSettle_EmitsTerminalRuntimeEvent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, _, conn, _, db := newQueueHandler(t, "s-exec")

	_, _, err := h.EnqueueBusyInput(ctx, queuedInputEnv("s-exec", "old", "old"), "old", nil)
	require.NoError(t, err)
	ageQueue(t, db, "s-exec", time.Now().Add(-time.Hour))
	require.Equal(t, 1, h.SweepExpiredQueue(ctx))

	var found *events.Envelope
	require.Eventually(t, func() bool {
		for _, env := range conn.envelopes() {
			if env.Event.Type == events.RuntimeExecutionFailed {
				found = env
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "expired queue settlement must emit a terminal runtime event")
	data, ok := found.Event.Data.(events.RuntimeExecutionData)
	require.True(t, ok, "settlement event carries RuntimeExecutionData, got %T", found.Event.Data)
	require.NotEmpty(t, data.ExecutionID)
	require.Equal(t, string(execution.RuntimeFailed), data.Status)
}
