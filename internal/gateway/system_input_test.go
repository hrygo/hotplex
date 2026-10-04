package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/cron"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/pkg/events"
)

func newSystemInputHandler(
	t *testing.T, store execution.Store, state events.SessionState, inputErr error,
) (*Handler, *mockInputSM, *mockWorkerForHandler) {
	t.Helper()
	sm := new(mockInputSM)
	w := new(mockWorkerForHandler)
	sm.On("Get", "s-sys").Return(&session.SessionInfo{State: state, Platform: "cron"}, nil).Maybe()
	sm.On("GetWorker", "s-sys").Return(w).Maybe()
	w.On("Input", mock.Anything, mock.Anything, mock.Anything).Return(inputErr).Maybe()
	return &Handler{
		log:             testLogger(t),
		sm:              sm,
		executionStore:  store,
		ownerInstanceID: "gw-test",
	}, sm, w
}

func sysRecord() *execution.Record {
	return &execution.Record{
		ExecutionID:     "exec-sys-1",
		SessionID:       "s-sys",
		ClientMessageID: "sys-occ-1",
		PayloadHash:     "hash",
		Status:          execution.StatusAccepted,
		WorkerRunID:     "run-sys-1",
	}
}

func sysRequest() cron.SystemInputRequest {
	return cron.SystemInputRequest{
		SessionID:    "s-sys",
		OccurrenceID: "occ-1",
		Content:      "do the thing",
	}
}

func TestDispatchSystemInput_AcceptsDispatchesAndMarksDelivered(t *testing.T) {
	t.Parallel()

	store := &fakeExecutionStore{record: sysRecord()}
	h, _, w := newSystemInputHandler(t, store, events.StateIdle, nil)

	res, err := h.DispatchSystemInput(context.Background(), sysRequest())
	require.NoError(t, err)
	require.False(t, res.Duplicate)
	require.Equal(t, "exec-sys-1", res.ExecutionID)

	// The prompt reached the Worker...
	w.AssertCalled(t, "Input", mock.Anything, "do the thing", mock.Anything)
	// ...through the durable ledger, not around it.
	require.Equal(t, "sys-occ-1", store.lastAccept.ClientMessageID,
		"the system input must enter the execution ledger under a stable identity")
	require.Equal(t, "run-sys-1", store.markRunID, "dispatch must bind the worker run")
	status, code, _ := store.snapshot()
	require.Equal(t, execution.StatusDelivered, status)
	require.Empty(t, code)
}

func TestDispatchSystemInput_DuplicateResolvesWithoutDispatching(t *testing.T) {
	t.Parallel()

	store := &fakeExecutionStore{record: sysRecord(), duplicate: true}
	h, _, w := newSystemInputHandler(t, store, events.StateIdle, nil)

	res, err := h.DispatchSystemInput(context.Background(), sysRequest())
	require.NoError(t, err)
	require.True(t, res.Duplicate, "an already-accepted identity must resolve as duplicate")
	require.Equal(t, "exec-sys-1", res.ExecutionID)
	w.AssertNotCalled(t, "Input", mock.Anything, mock.Anything, mock.Anything)
}

func TestDispatchSystemInput_RejectsWithoutExecutionStore(t *testing.T) {
	t.Parallel()

	h, _, _ := newSystemInputHandler(t, nil, events.StateIdle, nil)
	h.executionStore = nil

	_, err := h.DispatchSystemInput(context.Background(), sysRequest())
	require.ErrorIs(t, err, ErrSystemInputRejected,
		"without the ledger there is no accept, lease or terminal correlation")
}

func TestDispatchSystemInput_RejectsIncompleteRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  cron.SystemInputRequest
	}{
		{"missing session", cron.SystemInputRequest{OccurrenceID: "occ-1", Content: "x"}},
		{"missing occurrence", cron.SystemInputRequest{SessionID: "s-sys", Content: "x"}},
		{"empty content", cron.SystemInputRequest{SessionID: "s-sys", OccurrenceID: "occ-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeExecutionStore{record: sysRecord()}
			h, _, _ := newSystemInputHandler(t, store, events.StateIdle, nil)
			_, err := h.DispatchSystemInput(context.Background(), tt.req)
			require.ErrorIs(t, err, ErrSystemInputRejected)
		})
	}
}

func TestDispatchSystemInput_SessionBusyIsRejectedNotSupplemented(t *testing.T) {
	t.Parallel()

	store := &fakeExecutionStore{acceptErr: execution.ErrSessionBusy}
	h, _, _ := newSystemInputHandler(t, store, events.StateIdle, nil)

	_, err := h.DispatchSystemInput(context.Background(), sysRequest())
	require.ErrorIs(t, err, ErrSystemInputRejected)
	require.Contains(t, err.Error(), "active execution",
		"a system input owns its session and must not be buffered as a supplement")
}

func TestDispatchSystemInput_WorkerInputFailureMarksExecutionFailed(t *testing.T) {
	t.Parallel()

	store := &fakeExecutionStore{record: sysRecord()}
	h, _, _ := newSystemInputHandler(t, store, events.StateIdle, errors.New("worker rejected"))

	_, err := h.DispatchSystemInput(context.Background(), sysRequest())
	require.ErrorIs(t, err, ErrSystemInputRejected)
	status, _, _ := store.snapshot()
	require.Equal(t, execution.StatusFailed, status,
		"a rejected dispatch must be recorded as failed, not left pending")
}

func TestWaitForExecution_ReturnsWhenExecutionReachesTerminalState(t *testing.T) {
	t.Parallel()

	store := &fakeExecutionStore{openRecord: &execution.Record{
		ExecutionID:   "exec-sys-1",
		RuntimeStatus: execution.RuntimeCompleted,
	}}
	h, _, _ := newSystemInputHandler(t, store, events.StateRunning, nil)

	require.NoError(t, h.WaitForExecution(context.Background(), "s-sys", "exec-sys-1", time.Second),
		"a terminal runtime status on our own execution is authoritative")
}

func TestWaitForExecution_KeepsWaitingWhileExecutionRuns(t *testing.T) {
	t.Parallel()

	// Session reads idle, but our execution is still running. Waiting on the
	// session's global IDLE would wrongly declare the run complete.
	store := &fakeExecutionStore{openRecord: &execution.Record{
		ExecutionID:   "exec-sys-1",
		RuntimeStatus: execution.RuntimeRunning,
	}}
	h, _, _ := newSystemInputHandler(t, store, events.StateIdle, nil)

	err := h.WaitForExecution(context.Background(), "s-sys", "exec-sys-1", 150*time.Millisecond)
	require.Error(t, err, "an idle session must not stand in for a still-running execution")
	require.Contains(t, err.Error(), "timeout")
}

func TestWaitForExecution_FallsBackToSessionWhenExecutionSuperseded(t *testing.T) {
	t.Parallel()

	// Our execution is no longer the session's latest, so the store cannot say
	// more; the session state decides.
	store := &fakeExecutionStore{openRecord: &execution.Record{
		ExecutionID:   "exec-other",
		RuntimeStatus: execution.RuntimeRunning,
	}}
	h, _, _ := newSystemInputHandler(t, store, events.StateTerminated, nil)

	require.NoError(t, h.WaitForExecution(context.Background(), "s-sys", "exec-sys-1", time.Second))
}
