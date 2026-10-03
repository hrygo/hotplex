package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
	noopworker "github.com/hrygo/hotplex/internal/worker/noop"
	"github.com/hrygo/hotplex/pkg/events"
)

type teardownPanicWorker struct {
	mockWorkerForHandler
	killCalls int
}

func TestBridge_StopAndDisposeCurrentRun_NaturalTerminalSkipsProviderStop(t *testing.T) {
	t.Parallel()

	_, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_natural_terminal_before_stop"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(mockWorkerForHandler)
	w.conn = noopworker.NewConn(sid, "user1")
	w.On("StopCurrentTurn", mock.Anything).Return(nil).Maybe()
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	bridge := NewBridge(BridgeDeps{Log: slog.Default(), Hub: hub, SM: mgr})
	binding := bridge.bindWorkerRun(sid, w, "run-natural-terminal", launchPlan{})
	binding.lifecycle.terminalCommitted.Store(true)
	close(binding.lifecycle.done)

	stopErr := bridge.StopAndDisposeCurrentRun(context.Background(), sid, binding.id)
	require.ErrorIs(t, stopErr, errWorkerRunTerminal)
	w.AssertNotCalled(t, "StopCurrentTurn", mock.Anything)
	w.AssertNotCalled(t, "Terminate", mock.Anything)
	require.False(t, binding.lifecycle.stopping.Load())
	_, _, stillBound := bridge.CurrentWorkerBinding(sid)
	require.True(t, stillBound, "natural completion must retain the reusable worker run")
}

func TestBridge_BeginWorkerRunTurn_DrainsPriorEventBeforeReopen(t *testing.T) {
	t.Parallel()

	_, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_begin_turn_event_barrier"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(mockWorkerForHandler)
	w.conn = noopworker.NewConn(sid, "user1")
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	bridge := NewBridge(BridgeDeps{Log: slog.Default(), Hub: hub, SM: mgr})
	binding := bridge.bindWorkerRun(sid, w, "run-begin-turn-barrier", launchPlan{})
	releaseEvent, admitted := binding.lifecycle.beginEvent()
	require.True(t, admitted)
	binding.lifecycle.terminalCommitted.Store(true)

	done := make(chan error, 1)
	go func() {
		done <- bridge.beginWorkerRunTurn(context.Background(), sid, binding.id)
	}()
	select {
	case err := <-done:
		require.FailNow(t, "new turn reopened before the preceding event drained", "error: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	releaseEvent()
	require.NoError(t, <-done)
	require.False(t, binding.lifecycle.terminalCommitted.Load())
}

func TestForwardContext_TerminalStateMirrorsLifecycle(t *testing.T) {
	t.Parallel()

	lifecycle := newWorkerRunLifecycle(nil)
	fc := &forwardContext{lifecycle: lifecycle}

	require.True(t, fc.claimTerminal())
	require.True(t, fc.terminalSent.Load())
	require.True(t, lifecycle.terminalCommitted.Load())
	require.False(t, fc.claimTerminal(), "one turn may claim only one terminal")

	fc.reopenTerminal()
	require.False(t, fc.terminalSent.Load())
	require.False(t, lifecycle.terminalCommitted.Load())
	require.True(t, fc.claimTerminal(), "the next turn must be able to claim a terminal")
}

func (w *teardownPanicWorker) Kill() error {
	w.killCalls++
	return nil
}

func TestBridge_StopAndDisposeCurrentRun_EventBarrierHonorsTeardownTimeout(t *testing.T) {
	_, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_stop_event_barrier_timeout"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(mockWorkerForHandler)
	w.conn = noopworker.NewConn(sid, "user1")
	w.On("StopCurrentTurn", mock.Anything).Return(errors.New("stop must not be called")).Maybe()
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	bridge := NewBridge(BridgeDeps{
		Log:                  slog.Default(),
		Hub:                  hub,
		SM:                   mgr,
		StopTeardownTimeout:  50 * time.Millisecond,
		StopForwarderTimeout: 50 * time.Millisecond,
	})
	binding := bridge.bindWorkerRun(sid, w, "run-event-barrier-timeout", launchPlan{})
	releaseEvent, admitted := binding.lifecycle.beginEvent()
	require.True(t, admitted)

	done := make(chan error, 1)
	go func() {
		done <- bridge.StopAndDisposeCurrentRun(context.Background(), sid, binding.id)
	}()

	var stopErr error
	select {
	case stopErr = <-done:
	case <-time.After(250 * time.Millisecond):
		releaseEvent()
		stopErr = <-done
		require.FailNow(t, "event barrier acquisition exceeded the teardown budget")
	}
	releaseEvent()

	require.Error(t, stopErr)
	require.Contains(t, stopErr.Error(), "event barrier")
	w.AssertNotCalled(t, "StopCurrentTurn", mock.Anything)
	_, _, stillBound := bridge.CurrentWorkerBinding(sid)
	require.True(t, stillBound, "a pre-cancel barrier timeout must retain the live run")
}

func TestBridge_StopAndDisposeCurrentRun_StopPanicReleasesBarrier(t *testing.T) {
	t.Parallel()

	_, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_stop_panic"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(mockWorkerForHandler)
	w.conn = noopworker.NewConn(sid, "user1")
	w.On("StopCurrentTurn", mock.Anything).Run(func(mock.Arguments) {
		panic("injected stop panic")
	}).Return(nil).Once()
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	bridge := NewBridge(BridgeDeps{
		Log:                  slog.Default(),
		Hub:                  hub,
		SM:                   mgr,
		StopTeardownTimeout:  50 * time.Millisecond,
		StopForwarderTimeout: 50 * time.Millisecond,
	})
	binding := bridge.bindWorkerRun(sid, w, "run-stop-panic", launchPlan{})

	stopErr := bridge.StopAndDisposeCurrentRun(context.Background(), sid, binding.id)
	require.ErrorIs(t, stopErr, errWorkerStopNotApplied)
	require.True(t, stopFailureAllowsRollback(stopErr))
	require.False(t, binding.lifecycle.stopping.Load())

	eventAdmitted := make(chan bool, 1)
	go func() {
		releaseEvent, admitted := binding.lifecycle.beginEvent()
		if admitted {
			releaseEvent()
		}
		eventAdmitted <- admitted
	}()

	select {
	case admitted := <-eventAdmitted:
		require.True(t, admitted, "a recovered stop panic must reopen event admission")
	case <-time.After(250 * time.Millisecond):
		require.FailNow(t, "event barrier remained locked after a recovered stop panic")
	}

	_, _, stillBound := bridge.CurrentWorkerBinding(sid)
	require.True(t, stillBound, "a pre-commit stop panic must retain the live run")
}

func TestBridge_StopAndDisposeCurrentRun_TerminatePanicFallsBackAndFailsClosed(t *testing.T) {
	t.Parallel()

	_, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_terminate_panic"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(teardownPanicWorker)
	w.conn = noopworker.NewConn(sid, "user1")
	w.On("StopCurrentTurn", mock.Anything).Return(nil).Once()
	var panicOnce sync.Once
	w.On("Terminate", mock.Anything).Run(func(mock.Arguments) {
		panicOnce.Do(func() { panic("injected terminate panic") })
	}).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	bridge := NewBridge(BridgeDeps{
		Log:                  slog.Default(),
		Hub:                  hub,
		SM:                   mgr,
		StopTeardownTimeout:  50 * time.Millisecond,
		StopForwarderTimeout: 50 * time.Millisecond,
	})
	binding := bridge.bindWorkerRun(sid, w, "run-terminate-panic", launchPlan{})
	close(binding.lifecycle.done)

	stopErr := bridge.StopAndDisposeCurrentRun(context.Background(), sid, binding.id)
	require.ErrorIs(t, stopErr, errWorkerRunQuiescence)
	require.Equal(t, 1, w.killCalls, "a recovered terminate panic must use the kill fallback")
	_, _, stillBound := bridge.CurrentWorkerBinding(sid)
	require.False(t, stillBound, "post-commit teardown failure must fail closed")
}

func TestWorkerRunLifecycle_WithEventReleasesAfterPanic(t *testing.T) {
	t.Parallel()

	lifecycle := newWorkerRunLifecycle(nil)
	require.Panics(t, func() {
		lifecycle.withEvent(func() {
			panic("injected event panic")
		})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.True(t, lifecycle.lockEventBarrier(ctx), "event panic must not leak the admission or read lock")
	lifecycle.unlockEventBarrier()
}
