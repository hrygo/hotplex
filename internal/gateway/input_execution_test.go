package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

// errFakeQueueUnsupported keeps the fake honest: it holds no queue, so it must
// not pretend one exists.
var errFakeQueueUnsupported = errors.New("fake execution store: queue unsupported")

type fakeExecutionStore struct {
	mu            sync.Mutex
	record        *execution.Record
	duplicate     bool
	acceptErr     error
	statusErr     error
	lastAccept    execution.AcceptRequest
	status        execution.Status
	errorCode     string
	statusCalls   int
	openRecord    *execution.Record
	activeRecord  *execution.Record   // when set, ActiveBySession reports the gate as held
	activeResults []*execution.Record // optional ordered ActiveBySession results; nil entry means ErrNotFound
	activeCalls   int
	markRunID     string
	markErr       error
	finishRunID   string
	finishStatus  execution.RuntimeStatus
	finishErr     error
	latestErr     error
}

func (s *fakeExecutionStore) Accept(_ context.Context, req execution.AcceptRequest) (*execution.Record, bool, error) {
	s.mu.Lock()
	s.lastAccept = req
	s.mu.Unlock()
	return s.record, s.duplicate, s.acceptErr
}

func (s *fakeExecutionStore) SetStatus(_ context.Context, _ string, status execution.Status, errorCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.errorCode = errorCode
	s.statusCalls++
	return s.statusErr
}

func (s *fakeExecutionStore) SetDelivery(_ context.Context, _ string, _ string, status execution.Status, errorCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.errorCode = errorCode
	s.statusCalls++
	return s.statusErr
}

func (s *fakeExecutionStore) MarkRunning(_ context.Context, _ string, _ string, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markRunID = runID
	return s.markErr
}
func (s *fakeExecutionStore) FinishRuntime(_ context.Context, _ string, runID string, status execution.RuntimeStatus, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishRunID = runID
	s.finishStatus = status
	return s.finishErr
}

func (s *fakeExecutionStore) ConvergeDeliveryFailed(_ context.Context, _ string, _ string) error {
	return nil
}
func (s *fakeExecutionStore) ActiveBySession(context.Context, string) (*execution.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeCalls < len(s.activeResults) {
		rec := s.activeResults[s.activeCalls]
		s.activeCalls++
		if rec == nil {
			return nil, execution.ErrNotFound
		}
		return rec, nil
	}
	if s.activeRecord != nil {
		return s.activeRecord, nil
	}
	return nil, execution.ErrNotFound
}
func (s *fakeExecutionStore) OpenBySession(context.Context, string) (*execution.Record, error) {
	if s.openRecord != nil {
		return s.openRecord, nil
	}
	return nil, execution.ErrNotFound
}
func (s *fakeExecutionStore) LatestBySession(context.Context, string) (*execution.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latestErr != nil {
		return nil, s.latestErr
	}
	if s.openRecord != nil {
		return s.openRecord, nil
	}
	if s.record != nil {
		return s.record, nil
	}
	return nil, execution.ErrNotFound
}
func (s *fakeExecutionStore) FenceBySession(context.Context, string) (*execution.Record, error) {
	return nil, execution.ErrNotFound
}
func (s *fakeExecutionStore) ClearFenceAfterFreshStart(context.Context, string, string, string) error {
	return nil
}
func (s *fakeExecutionStore) ApplyFenceDecision(_ context.Context, _ execution.FenceActionRequest) (*execution.Record, error) {
	return nil, execution.ErrNotFound
}
func (s *fakeExecutionStore) ListFences(context.Context, string, int, int) ([]*execution.Record, error) {
	return nil, nil
}
func (s *fakeExecutionStore) ByID(context.Context, string) (*execution.Record, error) {
	return nil, execution.ErrNotFound
}
func (s *fakeExecutionStore) ListRecent(context.Context, execution.ListFilter) ([]*execution.Record, error) {
	return nil, nil
}
func (s *fakeExecutionStore) RenewLeases(context.Context, string, int64, []string) (int64, error) {
	return 0, nil
}
func (s *fakeExecutionStore) RecoverExpiredLeases(context.Context, []string) (execution.LeaseRecoveryResult, error) {
	return execution.LeaseRecoveryResult{}, nil
}
func (s *fakeExecutionStore) TerminateOwnerLeases(context.Context, string, string) (int64, error) {
	return 0, nil
}

// The queue surface is not exercised by the ordinary accept paths these tests
// cover, so the fake refuses rather than inventing a plausible queue. A test
// that starts asserting about queued inputs must build a real store.
func (s *fakeExecutionStore) AcceptQueued(
	context.Context, execution.QueuedRequest, execution.QueueLimits,
) (*execution.Record, *execution.QueueEntry, bool, error) {
	return nil, nil, false, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) QueueByExecution(context.Context, string) (*execution.QueueEntry, error) {
	return nil, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) QueuePayload(context.Context, string) (*execution.QueuedPayload, error) {
	return nil, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) QueueBySession(context.Context, string, int) ([]*execution.QueueEntry, error) {
	return nil, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) QueuedByClientMessage(context.Context, string, string) (*execution.Record, error) {
	return nil, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) QueueDepth(context.Context) (int64, error) {
	return 0, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) ClaimQueued(
	context.Context, execution.ClaimQueuedRequest,
) (*execution.Record, *execution.QueueEntry, error) {
	return nil, nil, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) CancelQueued(context.Context, string, string) (*execution.Record, error) {
	return nil, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) ClearQueue(context.Context, string, string) (int64, error) {
	return 0, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) ExpireQueued(context.Context, time.Time, int) ([]*execution.Record, error) {
	return nil, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) QueueDepthBySession(context.Context, string) (int64, error) {
	return 0, errFakeQueueUnsupported
}

func (s *fakeExecutionStore) snapshot() (execution.Status, string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.errorCode, s.statusCalls
}

func newExecutionHandler(t *testing.T, store execution.Store, workerInputError error) (*Handler, *mockInputSM, *mockWorkerForHandler, *mockPlatformConn) {
	t.Helper()
	sm := new(mockInputSM)
	w := new(mockWorkerForHandler)
	sm.On("Get", "s-exec").Return(&session.SessionInfo{State: events.StateRunning, Platform: "webchat"}, nil)
	sm.On("GetWorker", "s-exec").Return(w).Maybe()
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(workerInputError).Maybe()

	hub := newTestHub(t)
	conn := &mockPlatformConn{}
	hub.JoinPlatformSession("s-exec", conn)
	return &Handler{log: testLogger(t), hub: hub, sm: sm, executionStore: store}, sm, w, conn
}

func testExecutionRecord(status execution.Status) *execution.Record {
	return &execution.Record{
		ExecutionID:     "exec_test",
		SessionID:       "s-exec",
		ClientMessageID: "evt-client-1",
		PayloadHash:     "hash",
		Status:          status,
		WorkerRunID:     "run-provisional",
	}
}

func requireInputAcks(t *testing.T, conn *mockPlatformConn, statuses ...events.ExecutionStatus) {
	t.Helper()
	require.Eventually(t, func() bool { return len(conn.envelopes()) == len(statuses) }, time.Second, 10*time.Millisecond)
	for i, ack := range conn.envelopes() {
		require.Equal(t, events.InputAck, ack.Event.Type)
		data, ok := ack.Event.Data.(events.InputAckData)
		require.True(t, ok)
		require.Equal(t, "evt-client-1", data.ClientMessageID)
		require.Equal(t, "exec_test", data.ExecutionID)
		require.Equal(t, statuses[i], data.Status)
		require.Equal(t, events.InputModePrimary, data.InputMode)
		require.Equal(t, events.InputDurabilityDurable, data.Durability)
		require.Empty(t, data.ParentExecutionID)
	}
}

func TestInputExecution_DeliveredAckAfterWorkerAccepts(t *testing.T) {
	t.Parallel()
	store := &fakeExecutionStore{record: testExecutionRecord(execution.StatusAccepted)}
	h, sm, w, conn := newExecutionHandler(t, store, nil)
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.NoError(t, h.handleInput(context.Background(), env))
	requireInputAcks(t, conn, events.ExecutionStatusAccepted, events.ExecutionStatusDelivered)
	require.False(t, conn.envelopes()[0].Event.Data.(events.InputAckData).Duplicate)
	status, code, calls := store.snapshot()
	require.Equal(t, execution.StatusDelivered, status)
	require.Empty(t, code)
	require.Equal(t, 1, calls)
	sm.AssertExpectations(t)
	w.AssertExpectations(t)
}

func TestInputExecution_HardWorkerFailureDoesNotCaptureUserTurn(t *testing.T) {
	t.Parallel()

	execStore := &fakeExecutionStore{record: testExecutionRecord(execution.StatusAccepted)}
	bridge, turnStore := newBridgeWithCollector(t)
	hub := newTestHub(t)
	bridge.hub = hub
	bridge.sm = new(mockInputSM)
	hub.JoinPlatformSession("s-exec", &mockPlatformConn{})

	sm := bridge.sm.(*mockInputSM)
	w := new(mockWorkerForHandler)
	sm.On("Get", "s-exec").Return(&session.SessionInfo{State: events.StateRunning, Platform: "webchat"}, nil).Maybe()
	sm.On("GetWorker", "s-exec").Return(w).Maybe()
	bridge.workerRuns.Store("s-exec", workerRunBinding{
		worker:    w,
		id:        "run-worker",
		lifecycle: newWorkerRunLifecycle(nil),
	})
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(errors.New("worker rejected input"))

	h := &Handler{
		log:             testLogger(t),
		hub:             hub,
		sm:              sm,
		bridge:          bridge,
		executionStore:  execStore,
		ownerInstanceID: "gw-test",
	}
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.Error(t, h.handleInput(context.Background(), env))
	require.NoError(t, bridge.collector.Close())
	turns, err := turnStore.QueryLatestTurns(context.Background(), "s-exec", 10)
	if errors.Is(err, eventstore.ErrNotFound) {
		return
	}
	require.NoError(t, err)
	require.Empty(t, turns, "a hard Worker.Input failure must not leave an unmatched user turn")
}

func TestInputExecution_SuccessCapturesUserTurnOnce(t *testing.T) {
	t.Parallel()

	execStore := &fakeExecutionStore{record: testExecutionRecord(execution.StatusAccepted)}
	bridge, turnStore := newBridgeWithCollector(t)
	hub := newTestHub(t)
	bridge.hub = hub
	bridge.sm = new(mockInputSM)
	hub.JoinPlatformSession("s-exec", &mockPlatformConn{})

	sm := bridge.sm.(*mockInputSM)
	w := new(mockWorkerForHandler)
	sm.On("Get", "s-exec").Return(&session.SessionInfo{State: events.StateRunning, Platform: "webchat"}, nil).Maybe()
	sm.On("GetWorker", "s-exec").Return(w).Maybe()
	bridge.workerRuns.Store("s-exec", workerRunBinding{
		worker:    w,
		id:        "run-worker",
		lifecycle: newWorkerRunLifecycle(nil),
	})
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(nil)

	h := &Handler{
		log:             testLogger(t),
		hub:             hub,
		sm:              sm,
		bridge:          bridge,
		executionStore:  execStore,
		ownerInstanceID: "gw-test",
	}
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.NoError(t, h.handleInput(context.Background(), env))
	require.NoError(t, bridge.collector.Close())
	turns, err := turnStore.QueryLatestTurns(context.Background(), "s-exec", 10)
	require.NoError(t, err)
	require.Len(t, turns, 1, "successful delivery must capture exactly one user turn")
	require.Equal(t, eventstore.RoleUser, turns[0].Role)
	require.Equal(t, 1, turns[0].TurnNum, "first user turn must keep its turn number")
}

func TestAcceptInputExecution_UsesAttachedWorkerRunID(t *testing.T) {
	t.Parallel()
	store := &fakeExecutionStore{record: testExecutionRecord(execution.StatusAccepted)}
	sm := new(mockBridgeSM)
	w := new(mockWorkerForHandler)
	sm.On("GetWorker", "s-exec").Return(w)
	b := &Bridge{sm: sm}
	b.workerRuns.Store("s-exec", workerRunBinding{worker: w, id: "run-attached"})
	h := &Handler{executionStore: store, bridge: b, ownerInstanceID: "gw-test"}
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	_, _, err := h.acceptInputExecution(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, "run-attached", store.lastAccept.WorkerRunID)
}

func TestInputExecution_DispatchUsesAtomicBridgeBinding(t *testing.T) {
	t.Parallel()
	store := &fakeExecutionStore{record: testExecutionRecord(execution.StatusAccepted)}
	handlerSM := new(mockInputSM)
	handlerSM.On("Get", "s-exec").Return(&session.SessionInfo{State: events.StateRunning, Platform: "webchat"}, nil)
	bridgeSM := new(mockBridgeSM)
	boundWorker := new(mockWorkerForHandler)
	bridgeSM.On("GetWorker", "s-exec").Return(boundWorker)
	boundWorker.On("Input", mock.Anything, "hello", mock.Anything).Return(nil)
	b := &Bridge{sm: bridgeSM, log: testLogger(t)}
	b.workerRuns.Store("s-exec", workerRunBinding{
		worker:    boundWorker,
		id:        "run-bound",
		lifecycle: newWorkerRunLifecycle(nil),
	})

	hub := newTestHub(t)
	conn := &mockPlatformConn{}
	hub.JoinPlatformSession("s-exec", conn)
	h := &Handler{
		log: testLogger(t), hub: hub, sm: handlerSM, bridge: b,
		executionStore: store, ownerInstanceID: "gw-test",
	}
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.NoError(t, h.handleInput(context.Background(), env))
	require.Equal(t, "run-bound", store.markRunID)
	boundWorker.AssertExpectations(t)
	handlerSM.AssertNotCalled(t, "GetWorker", mock.Anything)
}

func TestInputExecution_RefreshesSessionStateAfterAcceptance(t *testing.T) {
	t.Parallel()
	store := &fakeExecutionStore{record: testExecutionRecord(execution.StatusAccepted)}
	sm := new(mockBridgeSM)
	w := new(mockWorkerForHandler)
	sm.On("Get", "s-exec").Return(&session.SessionInfo{
		ID: "s-exec", State: events.StateTerminated, Platform: "webchat",
	}, nil).Once()
	sm.On("Get", "s-exec").Return(&session.SessionInfo{
		ID: "s-exec", State: events.StateRunning, Platform: "webchat",
	}, nil)
	sm.On("GetWorker", "s-exec").Return(w)
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(nil)
	b := &Bridge{sm: sm, log: testLogger(t)}
	b.workerRuns.Store("s-exec", workerRunBinding{
		worker:    w,
		id:        "run-fresh",
		lifecycle: newWorkerRunLifecycle(nil),
	})

	hub := newTestHub(t)
	conn := &mockPlatformConn{}
	hub.JoinPlatformSession("s-exec", conn)
	h := &Handler{
		log: testLogger(t), hub: hub, sm: sm, bridge: b,
		executionStore: store, ownerInstanceID: "gw-test",
	}
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.NoError(t, h.handleInput(context.Background(), env))
	require.Equal(t, "run-fresh", store.markRunID)
	sm.AssertNotCalled(t, "Transition", mock.Anything, mock.Anything, mock.Anything)
	w.AssertExpectations(t)
}

func TestInputExecution_MarkRunningFailureDoesNotDispatch(t *testing.T) {
	t.Parallel()
	store := &fakeExecutionStore{
		record:  testExecutionRecord(execution.StatusAccepted),
		markErr: errors.New("db unavailable"),
	}
	h, sm, w, conn := newExecutionHandler(t, store, nil)
	h.ownerInstanceID = "gw-test"
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.ErrorContains(t, h.handleInput(context.Background(), env), "execution dispatch registration failed")
	require.Eventually(t, func() bool { return len(conn.envelopes()) == 3 }, time.Second, 10*time.Millisecond)
	for i, status := range []events.ExecutionStatus{events.ExecutionStatusAccepted, events.ExecutionStatusFailed} {
		data, ok := conn.envelopes()[i].Event.Data.(events.InputAckData)
		require.True(t, ok)
		require.Equal(t, status, data.Status)
	}
	w.AssertNotCalled(t, "Input", mock.Anything, mock.Anything, mock.Anything)
	require.Equal(t, "run-provisional", store.finishRunID)
	require.Equal(t, execution.RuntimeFailed, store.finishStatus)
	sm.AssertExpectations(t)
}

func TestFinishRuntimeOnDone_UsesEmittingForwarderRunID(t *testing.T) {
	t.Parallel()
	rec := testExecutionRecord(execution.StatusDelivered)
	rec.WorkerRunID = "run-current"
	store := &fakeExecutionStore{openRecord: rec}
	b := &Bridge{executionStore: store, hub: newTestHub(t), log: testLogger(t)}
	fc := &forwardContext{sessionID: "s-exec", workerRunID: "run-stale"}
	env := events.NewEnvelope("evt-done", "s-exec", 1, events.Done, events.DoneData{Success: true})

	b.finishRuntimeOnDone("s-exec", fc, env)
	require.Equal(t, "run-stale", store.finishRunID)
}

func TestInputExecution_DuplicateReplaysAckWithoutWorkerCall(t *testing.T) {
	t.Parallel()
	store := &fakeExecutionStore{record: testExecutionRecord(execution.StatusDelivered), duplicate: true}
	h, sm, w, conn := newExecutionHandler(t, store, nil)
	retryCancel := make(chan struct{})
	h.bridge = &Bridge{retryCancel: map[string]chan struct{}{"s-exec": retryCancel}}
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.NoError(t, h.handleInput(context.Background(), env))
	requireInputAcks(t, conn, events.ExecutionStatusDelivered)
	require.True(t, conn.envelopes()[0].Event.Data.(events.InputAckData).Duplicate)
	_, _, calls := store.snapshot()
	require.Zero(t, calls)
	sm.AssertExpectations(t)
	sm.AssertNotCalled(t, "GetWorker", mock.Anything)
	w.AssertNotCalled(t, "Input", mock.Anything, mock.Anything, mock.Anything)
	select {
	case <-retryCancel:
		t.Fatal("duplicate input cancelled the active LLM retry")
	default:
	}
	_, retryStillPending := h.bridge.retryCancel["s-exec"]
	require.True(t, retryStillPending)
}

func TestInputExecution_DuplicateDoesNotResumeTerminatedSession(t *testing.T) {
	t.Parallel()
	store := &fakeExecutionStore{record: testExecutionRecord(execution.StatusDelivered), duplicate: true}
	sm := new(mockInputSM)
	sm.On("Get", "s-exec").Return(&session.SessionInfo{
		State:    events.StateTerminated,
		Platform: "webchat",
	}, nil)
	hub := newTestHub(t)
	conn := &mockPlatformConn{}
	hub.JoinPlatformSession("s-exec", conn)
	h := &Handler{
		log:            testLogger(t),
		hub:            hub,
		sm:             sm,
		bridge:         &Bridge{retryCancel: make(map[string]chan struct{})},
		executionStore: store,
	}
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.NoError(t, h.handleInput(context.Background(), env))
	requireInputAcks(t, conn, events.ExecutionStatusDelivered)
	require.True(t, conn.envelopes()[0].Event.Data.(events.InputAckData).Duplicate)
	sm.AssertExpectations(t)
	sm.AssertNotCalled(t, "GetWorker", mock.Anything)
}

func TestInputExecution_TimeoutBecomesUnknown(t *testing.T) {
	t.Parallel()
	store := &fakeExecutionStore{record: testExecutionRecord(execution.StatusAccepted)}
	timeoutErr := &worker.WorkerError{Kind: worker.ErrKindTimeout, Message: "worker response timed out"}
	h, sm, w, conn := newExecutionHandler(t, store, timeoutErr)
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.NoError(t, h.handleInput(context.Background(), env))
	requireInputAcks(t, conn, events.ExecutionStatusAccepted, events.ExecutionStatusUnknown)
	status, code, calls := store.snapshot()
	require.Equal(t, execution.StatusUnknown, status)
	require.Equal(t, string(events.ErrCodeExecutionTimeout), code)
	require.Equal(t, 1, calls)
	sm.AssertExpectations(t)
	w.AssertExpectations(t)
}

func TestInputExecution_TimeoutStillCapturesInboundTurn(t *testing.T) {
	t.Parallel()

	execStore := &fakeExecutionStore{record: testExecutionRecord(execution.StatusAccepted)}
	bridge, turnStore := newBridgeWithCollector(t)
	hub := newTestHub(t)
	bridge.hub = hub
	hub.seqGen.Init("s-exec", 0)
	hub.seqGen.MarkHydrated("s-exec")
	hub.JoinPlatformSession("s-exec", &mockPlatformConn{})

	sm := new(mockInputSM)
	w := new(mockWorkerForHandler)
	sm.On("Get", "s-exec").Return(&session.SessionInfo{
		State:    events.StateRunning,
		Platform: "webchat",
	}, nil).Maybe()
	sm.On("GetWorker", "s-exec").Return(w).Maybe()
	bridge.sm = sm
	bridge.workerRuns.Store("s-exec", workerRunBinding{
		worker:    w,
		id:        "run-worker",
		lifecycle: newWorkerRunLifecycle(nil),
	})

	allowAssistant := make(chan struct{})
	assistantDone := make(chan struct{})
	timeoutErr := &worker.WorkerError{Kind: worker.ErrKindTimeout, Message: "worker response timed out"}
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(timeoutErr).Run(func(mock.Arguments) {
		go func() {
			<-allowAssistant
			fc := &forwardContext{
				sessionID:     "s-exec",
				workerType:    worker.TypeClaudeCode,
				sessPlatform:  "webchat",
				firstEvent:    true,
				turnStartTime: time.Now(),
			}
			bridge.processForwardedEvent(
				events.NewEnvelope("assistant-message", "s-exec", 0, events.Message,
					map[string]any{"content": "assistant response"}),
				w, forwardOpts{}, fc,
			)
			bridge.processForwardedEvent(
				events.NewEnvelope("assistant-done", "s-exec", 0, events.Done,
					events.DoneData{Success: true}),
				w, forwardOpts{}, fc,
			)
			close(assistantDone)
		}()
	})

	h := &Handler{
		log:             testLogger(t),
		hub:             hub,
		sm:              sm,
		bridge:          bridge,
		executionStore:  execStore,
		ownerInstanceID: "gw-test",
	}
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.NoError(t, h.handleInput(context.Background(), env))
	close(allowAssistant)
	select {
	case <-assistantDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for assistant events")
	}

	require.NoError(t, bridge.collector.Close())
	turns, err := turnStore.QueryLatestTurns(context.Background(), "s-exec", 10)
	require.NoError(t, err)
	require.Len(t, turns, 2, "timeout delivery must retain both sides of the exchange")
	require.Equal(t, eventstore.RoleUser, turns[0].Role)
	require.Equal(t, "hello", turns[0].Content)
	require.Equal(t, "evt-client-1", turns[0].ClientMessageID)
	require.Equal(t, eventstore.RoleAssistant, turns[1].Role)
	require.Equal(t, "assistant response", turns[1].Content)
	require.Equal(t, turns[0].Generation, turns[1].Generation)
}

// TestInputExecution_AckReflectsIntendedStatusWhenSetStatusFails guards the
// C1 fix: when the durable SetStatus write fails, the terminal input.ack must
// still carry the intended outcome (unknown) rather than a stale 'accepted'.
// Otherwise the client would wait for a terminal ack that never arrives.
func TestInputExecution_AckReflectsIntendedStatusWhenSetStatusFails(t *testing.T) {
	t.Parallel()
	store := &fakeExecutionStore{
		record:    testExecutionRecord(execution.StatusAccepted),
		statusErr: errors.New("db unavailable"),
	}
	timeoutErr := &worker.WorkerError{Kind: worker.ErrKindTimeout, Message: "worker response timed out"}
	h, sm, w, conn := newExecutionHandler(t, store, timeoutErr)
	env := inputEnvelope("s-exec", "hello")
	env.ID = "evt-client-1"

	require.NoError(t, h.handleInput(context.Background(), env))
	requireInputAcks(t, conn, events.ExecutionStatusAccepted, events.ExecutionStatusUnknown)
	_, _, calls := store.snapshot()
	require.Equal(t, 1, calls)
	sm.AssertExpectations(t)
	w.AssertExpectations(t)
}
