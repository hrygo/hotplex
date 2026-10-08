package gateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWorkerRunLifecycle_TurnTimeoutUsesAbsoluteDeadline(t *testing.T) {
	t.Parallel()

	lifecycle := newWorkerRunLifecycle(nil)
	fired := make(chan uint64, 1)
	lifecycle.setTurnTimeoutHandler(func(generation uint64) {
		fired <- generation
	})
	t.Cleanup(lifecycle.clearTurnTimeoutHandler)

	firstDeadline := time.Now().Add(300 * time.Millisecond)
	firstGeneration := lifecycle.armTurnTimeout(firstDeadline)
	secondDeadline := time.Now().Add(80 * time.Millisecond)
	secondGeneration := lifecycle.armTurnTimeout(secondDeadline)
	require.Greater(t, secondGeneration, firstGeneration)

	select {
	case generation := <-fired:
		require.Equal(t, secondGeneration, generation, "a replaced turn deadline must not fire")
	case <-time.After(time.Second):
		t.Fatal("turn deadline did not fire")
	}
}

func TestWorkerRunLifecycle_StopTurnTimeoutFencesCallback(t *testing.T) {
	t.Parallel()

	lifecycle := newWorkerRunLifecycle(nil)
	fired := make(chan struct{}, 1)
	lifecycle.setTurnTimeoutHandler(func(uint64) {
		fired <- struct{}{}
	})
	t.Cleanup(lifecycle.clearTurnTimeoutHandler)

	lifecycle.armTurnTimeout(time.Now().Add(50 * time.Millisecond))
	lifecycle.stopTurnTimeout()

	select {
	case <-fired:
		t.Fatal("stopped turn deadline fired")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBridge_RestoredTurnDeadlineFiresWhenCurrentTimeoutIsDisabled(t *testing.T) {
	t.Parallel()

	b := &Bridge{}
	worker := &mockBridgeWorker{workerType: "test"}
	sm := &mockBridgeSM{}
	sm.On("GetWorker", "session-1").Return(worker).Once()
	b.sm = sm
	binding := b.bindWorkerRun("session-1", worker, "run-1", launchPlan{})
	fired := make(chan uint64, 1)
	binding.lifecycle.setTurnTimeoutHandler(func(generation uint64) {
		fired <- generation
	})
	t.Cleanup(binding.lifecycle.clearTurnTimeoutHandler)

	startedAt := time.Now().Add(-time.Minute).UnixMilli()
	deadlineAt := time.Now().Add(80 * time.Millisecond).UnixMilli()
	require.NoError(t, b.RecordTurnDeadlineForRun("session-1", "run-1", startedAt, deadlineAt))

	select {
	case generation := <-fired:
		require.True(t, binding.lifecycle.turnTimeoutCurrent(generation))
	case <-time.After(time.Second):
		t.Fatal("persisted turn deadline was not restored")
	}
	sm.AssertExpectations(t)
}
