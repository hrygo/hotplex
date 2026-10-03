package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/internal/worker"
	noopworker "github.com/hrygo/hotplex/internal/worker/noop"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

// ─── Mock Store (for tests that need to stub specific store methods) ─────────────

type mockStore struct {
	mock.Mock
}

func (m *mockStore) Upsert(ctx context.Context, info *session.SessionInfo) error {
	args := m.Called(ctx, info)
	if args.Error(0) == nil {
		if ms, ok := args.Get(0).(*session.SessionInfo); ok {
			*info = *ms
		}
	}
	return args.Error(0)
}

func (m *mockStore) UpdateWorkerSessionIDSQL(ctx context.Context, id, workerSessionID string) error {
	args := m.Called(ctx, id, workerSessionID)
	return args.Error(0)
}

func (m *mockStore) SetPermissionCeilingIfEmpty(ctx context.Context, id, ceiling string) (string, error) {
	args := m.Called(ctx, id, ceiling)
	return args.String(0), args.Error(1)
}

func (m *mockStore) UpdateSpecSnapshot(ctx context.Context, id string, snapshot *agentspec.EffectiveAgentSpecSnapshot) error {
	args := m.Called(ctx, id, snapshot)
	return args.Error(0)
}

func (m *mockStore) Get(ctx context.Context, id string) (*session.SessionInfo, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*session.SessionInfo), args.Error(1)
}

func (m *mockStore) List(ctx context.Context, userID, platform, workspaceID string, limit, offset int) ([]*session.SessionInfo, error) {
	args := m.Called(ctx, userID, platform, workspaceID, limit, offset)
	return args.Get(0).([]*session.SessionInfo), args.Error(1)
}

func (m *mockStore) GetExpiredMaxLifetime(ctx context.Context, now time.Time) ([]string, error) {
	args := m.Called(ctx, now)
	return args.Get(0).([]string), args.Error(1)
}

func (m *mockStore) GetExpiredIdle(ctx context.Context, now time.Time) ([]string, error) {
	args := m.Called(ctx, now)
	return args.Get(0).([]string), args.Error(1)
}

func (m *mockStore) DeleteTerminated(ctx context.Context, cronCutoff, defaultCutoff time.Time) ([]*session.SessionInfo, error) {
	args := m.Called(ctx, cronCutoff, defaultCutoff)
	if len(args) == 1 {
		return nil, args.Error(0)
	}
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*session.SessionInfo), args.Error(1)
}

func (m *mockStore) DeletePhysical(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *mockStore) GetSessionsByState(ctx context.Context, state events.SessionState) ([]string, error) {
	args := m.Called(ctx, state)
	return args.Get(0).([]string), args.Error(1)
}

func (m *mockStore) Close() error {
	return m.Called().Error(0)
}

// ─── Test Hub Factory ─────────────────────────────────────────────────────────

func newCtrlHub(t *testing.T) *Hub {
	t.Helper()
	return NewHub(slog.Default(), config.NewConfigStore(&config.Config{}, nil))
}

// ─── Real-store Handler Factory (for session-state-dependent tests) ──────────

func newHandlerWithRealStore(t *testing.T) (*Handler, *session.Manager, *Hub, func()) {
	t.Helper()
	h := newCtrlHub(t)
	cfg := config.Default()

	f, err := os.CreateTemp("", "gateway_ctrl_*.db")
	require.NoError(t, err)
	f.Close()
	tmpPath := f.Name()
	cleanup := func() { os.Remove(tmpPath) }

	cfg.DB.Path = tmpPath
	cfg.DB.SQLite.Path = tmpPath
	writeMu := sqlutil.NewWriteMu(sqlutil.DialectSQLite)
	store, err := session.NewSQLiteStore(context.Background(), cfg, writeMu)
	require.NoError(t, err)
	mgr, err := session.NewManager(context.Background(), slog.Default(), cfg, nil, store)
	require.NoError(t, err)
	t.Cleanup(func() {
		mgr.Close()
		store.Close()
		cleanup()
	})
	return NewHandler(HandlerDeps{Log: slog.Default(), Hub: h, SM: mgr}), mgr, h, cleanup
}

// ─── Mock-store Handler Factory (for tests that stub store methods) ───────────

func newHandlerWithMockStore(t *testing.T, store *mockStore) (*Handler, *Hub) {
	t.Helper()
	h := newCtrlHub(t)
	cfg := config.Default()
	store.On("Close").Return(nil)
	mgr, err := session.NewManager(context.Background(), slog.Default(), cfg, nil, store)
	require.NoError(t, err)
	t.Cleanup(func() { mgr.Close() })
	return NewHandler(HandlerDeps{Log: slog.Default(), Hub: h, SM: mgr}), h
}

// ─── Envelope Helpers ─────────────────────────────────────────────────────────

func inputEnvelope(sessionID, content string) *events.Envelope {
	return events.NewEnvelope(aep.NewID(), sessionID, 1, events.Input, map[string]any{
		"content": content,
	})
}

func inputEnvelopeWithMetadata(sessionID, content string, metadata map[string]any) *events.Envelope {
	return events.NewEnvelope(aep.NewID(), sessionID, 1, events.Input, map[string]any{
		"content":  content,
		"metadata": metadata,
	})
}

func pingEnvelope(sessionID string) *events.Envelope {
	return events.NewEnvelope(aep.NewID(), sessionID, 1, events.Ping, nil)
}

func controlEnvelope(sessionID, action string) *events.Envelope {
	return events.NewEnvelope(aep.NewID(), sessionID, 1, events.Control, map[string]any{
		"action": action,
	})
}

// ─── handleInput tests ────────────────────────────────────────────────────────

func TestHandleInput_Success(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_input_ok"
	// Create session in StateCreated so transition CREATED→RUNNING succeeds.
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	w := new(mockWorkerForHandler)
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(nil)
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	env := inputEnvelope(sid, "hello")
	err = handler.handleInput(context.Background(), env)
	require.NoError(t, err)
}

func TestHandleInput_BlankContentIsIgnored(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "empty", content: ""},
		{name: "spaces", content: "   "},
		{name: "tabs", content: "\t\t"},
		{name: "newlines", content: "\n\r\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler, mgr, _, _ := newHandlerWithRealStore(t)
			sid := "sess_blank_" + tt.name
			_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
			require.NoError(t, err)

			w := new(mockWorkerForHandler)
			w.On("Terminate", mock.Anything).Return(nil).Maybe()
			require.NoError(t, mgr.AttachWorker(context.Background(), sid, w))

			require.NoError(t, handler.handleInput(context.Background(), inputEnvelope(sid, tt.content)))
			w.AssertNotCalled(t, "Input", mock.Anything, mock.Anything, mock.Anything)

			info, err := mgr.Get(context.Background(), sid)
			require.NoError(t, err)
			require.Equal(t, events.StateCreated, info.State)
		})
	}
}

func TestHandleInput_SessionNotFound(t *testing.T) {
	t.Parallel()
	store := new(mockStore)
	store.Test(t)
	bg := context.Background()
	store.On("Get", bg, "sess_missing").Return(nil, session.ErrSessionNotFound).Maybe()

	handler, _ := newHandlerWithMockStore(t, store)
	env := inputEnvelope("sess_missing", "hello")
	err := handler.handleInput(bg, env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "session not found")
}

func TestHandleInput_SessionNotActive(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_inactive"
	// Create session and transition to TERMINATED state.
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	err = mgr.Transition(context.Background(), sid, events.StateTerminated)
	require.NoError(t, err)

	env := inputEnvelope(sid, "hello")
	err = handler.handleInput(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not active")
}

func TestHandleInput_RunningSession_AcceptsInput(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_inv_trans"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	// Transition CREATED→RUNNING (valid).
	err = mgr.Transition(context.Background(), sid, events.StateRunning)
	require.NoError(t, err)

	// When session is RUNNING, handleInput delivers input to worker without error.
	// This tests the fix: handleInput no longer tries TransitionWithInput for RUNNING,
	// avoiding the invalid transition error.
	w := new(mockWorkerForHandler)
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(nil)
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	env := inputEnvelope(sid, "hello")
	err = handler.handleInput(context.Background(), env)
	// No error — input is accepted and delivered to worker.
	require.NoError(t, err)
}

// ─── handleInput: interaction response routing tests ────────────────────────

func TestHandleInput_InteractionResponse_Permission(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_perm_resp"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(mockWorkerForHandler)
	w.On("Input", mock.Anything, "", mock.Anything).Return(nil)
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	md := map[string]any{
		"permission_response": map[string]any{
			"request_id": "req_1",
			"allowed":    true,
		},
	}
	env := inputEnvelopeWithMetadata(sid, "", md)
	err = handler.handleInput(context.Background(), env)
	require.NoError(t, err)
	w.AssertCalled(t, "Input", mock.Anything, "", mock.MatchedBy(func(md map[string]any) bool {
		pr, ok := md["permission_response"].(map[string]any)
		return ok && pr["request_id"] == "req_1"
	}))
}

func TestHandleInput_InteractionResponse_Question(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_q_resp"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(mockWorkerForHandler)
	w.On("Input", mock.Anything, "", mock.Anything).Return(nil)
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	md := map[string]any{
		"question_response": map[string]any{
			"id":      "q_1",
			"answers": map[string]string{"_": "yes"},
		},
	}
	env := inputEnvelopeWithMetadata(sid, "", md)
	err = handler.handleInput(context.Background(), env)
	require.NoError(t, err)
	w.AssertCalled(t, "Input", mock.Anything, "", mock.Anything)
}

func TestHandleInput_InteractionResponse_Elicitation(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_elic_resp"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(mockWorkerForHandler)
	w.On("Input", mock.Anything, "", mock.Anything).Return(nil)
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	md := map[string]any{
		"elicitation_response": map[string]any{
			"id":     "e_1",
			"action": "accept",
		},
	}
	env := inputEnvelopeWithMetadata(sid, "", md)
	err = handler.handleInput(context.Background(), env)
	require.NoError(t, err)
	w.AssertCalled(t, "Input", mock.Anything, "", mock.Anything)
}

func TestHandleInput_InteractionResponse_NoWorker(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_no_worker"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))
	// No worker attached — interaction response should fail instead of silently
	// acknowledging a response that cannot reach the waiting worker.

	md := map[string]any{
		"permission_response": map[string]any{"request_id": "req_1", "allowed": true},
	}
	env := inputEnvelopeWithMetadata(sid, "", md)
	err = handler.handleInput(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "interaction response dropped")
}

func TestHandleInput_PlatformMetadataNotRouted(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_platform_md"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(mockWorkerForHandler)
	// Normal input path: handler calls w.Input(ctx, content, nil) — third arg is nil.
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(nil)
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	// Platform context metadata (from messaging/bridge MakeEnvelope) should NOT
	// trigger the interaction response short-circuit.
	md := map[string]any{
		"platform":   "slack",
		"user_id":    "U123",
		"channel_id": "C456",
	}
	env := inputEnvelopeWithMetadata(sid, "hello", md)
	err = handler.handleInput(context.Background(), env)
	require.NoError(t, err)
	// Verify Input was called — the key assertion is that it received "hello",
	// proving the platform metadata did NOT trigger the interaction shortcut.
	w.AssertCalled(t, "Input", mock.Anything, "hello", mock.Anything)
}

// ─── handlePing tests ─────────────────────────────────────────────────────────

func TestHandlePing_KnownSession(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_ping"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	conn, _ := newTestWSConnPair(t)
	t.Cleanup(func() { conn.Close() })
	hub.JoinSession(sid, newConn(hub, conn, sid, nil))

	env := pingEnvelope(sid)
	err = handler.handlePing(context.Background(), env)
	require.NoError(t, err)
}

func TestHandlePing_UnknownSession(t *testing.T) {
	t.Parallel()
	store := new(mockStore)
	store.Test(t)
	store.On("Get", mock.Anything, "sess_ping_missing").Return(nil, session.ErrSessionNotFound)

	handler, _ := newHandlerWithMockStore(t, store)

	env := pingEnvelope("sess_ping_missing")
	err := handler.handlePing(context.Background(), env)
	// Ping always succeeds even if session unknown; state falls back to "unknown".
	require.NoError(t, err)
}

// ─── handleControl tests ──────────────────────────────────────────────────────

func TestHandleControl_Terminate_Success(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_term"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	err = mgr.Transition(context.Background(), sid, events.StateRunning)
	require.NoError(t, err)

	conn, _ := newTestWSConnPair(t)
	t.Cleanup(func() { conn.Close() })
	hub.JoinSession(sid, newConn(hub, conn, sid, nil))

	env := controlEnvelope(sid, string(events.ControlActionTerminate))
	env.OwnerID = "user1"
	err = handler.handleControl(context.Background(), env)
	require.NoError(t, err)
}

func TestHandleControl_Stop_Success(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_stop"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	err = mgr.Transition(context.Background(), sid, events.StateRunning)
	require.NoError(t, err)

	w := new(mockWorkerForHandler)
	w.conn = noopworker.NewConn(sid, "user1")
	w.On("StopCurrentTurn", mock.Anything).Return(nil)
	w.On("Terminate", mock.Anything).Return(nil)
	w.On("Wait").Return(0, nil)
	mgr.AttachWorker(context.Background(), sid, w)
	bridge := NewBridge(BridgeDeps{Log: slog.Default(), Hub: hub, SM: mgr})
	handler.bridge = bridge
	runBinding := bridge.bindWorkerRun(sid, w, "run-stop-success", launchPlan{})
	bridge.fwdWg.Add(1)
	go func() {
		defer bridge.fwdWg.Done()
		defer close(runBinding.lifecycle.done)
		defer bridge.clearWorkerRun(sid, w, runBinding.id)
		bridge.launchForwarderLocked(runBinding, sid, forwardOpts{ctx: context.Background(), workerRunID: runBinding.id})
	}()

	// The stop must converge the pending execution runtime (acceptance A3:
	// Gateway stopped_by_user closed loop).
	execStore := &fakeExecutionStore{openRecord: testExecutionRecord(execution.StatusDelivered)}
	handler.executionStore = execStore

	clientConn, serverConn := newTestWSConnPair(t)
	t.Cleanup(func() { clientConn.Close() })
	hub.JoinSession(sid, newConn(hub, clientConn, sid, nil))

	env := controlEnvelope(sid, string(events.ControlActionStop))
	env.OwnerID = "user1"
	err = handler.handleControl(context.Background(), env)
	require.NoError(t, err)

	// The stop must reach the worker, not just return locally.
	w.AssertCalled(t, "StopCurrentTurn", mock.Anything)

	// The pending execution runtime is converged to failed/terminated.
	execStore.mu.Lock()
	require.Equal(t, execution.RuntimeFailed, execStore.finishStatus)
	execStore.mu.Unlock()

	// The client must observe the terminal done event with the stop reason.
	// finishRuntimeOnStop emits an additive runtime.execution.failed event
	// before the done envelope, so read until done (bounded).
	var doneEnv events.Envelope
	require.Eventually(t, func() bool {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, data, err := serverConn.ReadMessage()
		if err != nil {
			return false
		}
		if err := json.Unmarshal(data, &doneEnv); err != nil {
			return false
		}
		return doneEnv.Event.Type == events.Done
	}, 3*time.Second, 100*time.Millisecond)
	require.Equal(t, events.Done, doneEnv.Event.Type)
	doneData, ok := doneEnv.Event.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "stopped_by_user", doneData["reason"])
}

func TestHandleControl_Stop_AfterNaturalTerminalIsSilent(t *testing.T) {
	t.Parallel()

	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_stop_after_natural_terminal"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(mockWorkerForHandler)
	w.conn = noopworker.NewConn(sid, "user1")
	w.On("StopCurrentTurn", mock.Anything).Return(nil).Maybe()
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	bridge := NewBridge(BridgeDeps{Log: slog.Default(), Hub: hub, SM: mgr})
	handler.bridge = bridge
	binding := bridge.bindWorkerRun(sid, w, "run-stop-after-natural-terminal", launchPlan{})
	binding.lifecycle.terminalCommitted.Store(true)

	env := controlEnvelope(sid, string(events.ControlActionStop))
	env.OwnerID = "user1"
	require.NoError(t, handler.handleControl(context.Background(), env),
		"a natural terminal that won the event barrier must make the racing stop a silent no-op")
	w.AssertNotCalled(t, "StopCurrentTurn", mock.Anything)
	w.AssertNotCalled(t, "Terminate", mock.Anything)
	require.True(t, handler.stopFence.Claim(sid, binding.id, ""),
		"the no-op stop must roll back its claim instead of fencing the next turn")
}

func TestHandleControl_Stop_LedgerLookupFailureFailsClosed(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_stop_ledger_lookup_failure"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(context.Background(), sid, events.StateRunning))

	w := new(mockWorkerForHandler)
	w.conn = noopworker.NewConn(sid, "user1")
	w.On("StopCurrentTurn", mock.Anything).Return(errors.New("stop must not be called")).Maybe()
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	bridge := NewBridge(BridgeDeps{Log: slog.Default(), Hub: hub, SM: mgr})
	handler.bridge = bridge
	bridge.bindWorkerRun(sid, w, "run-stop-ledger-failure", launchPlan{})
	handler.executionStore = &fakeExecutionStore{latestErr: errors.New("injected ledger outage")}

	env := controlEnvelope(sid, string(events.ControlActionStop))
	env.OwnerID = "user1"
	err = handler.handleControl(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stop execution identity unavailable")
	w.AssertNotCalled(t, "StopCurrentTurn", mock.Anything)
	require.True(t, handler.stopFence.Claim(sid, "run-stop-ledger-failure", "exec-stop-ledger-failure"),
		"lookup failure must not mutate the exact stop fence")
}

func TestHandleControl_Terminate_Unauthorized(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_term_unauth"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	err = mgr.Transition(context.Background(), sid, events.StateRunning)
	require.NoError(t, err)

	conn, _ := newTestWSConnPair(t)
	t.Cleanup(func() { conn.Close() })
	hub.JoinSession(sid, newConn(hub, conn, sid, nil))

	env := controlEnvelope(sid, string(events.ControlActionTerminate))
	env.OwnerID = "other_user"
	err = handler.handleControl(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ownership required")
}

func TestHandleControl_Delete_Success(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_del"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	err = mgr.Transition(context.Background(), sid, events.StateRunning)
	require.NoError(t, err)

	env := controlEnvelope(sid, string(events.ControlActionDelete))
	env.OwnerID = "user1"
	err = handler.handleControl(context.Background(), env)
	require.NoError(t, err)
}

func TestHandleControl_Delete_Unauthorized(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_del_unauth"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	err = mgr.Transition(context.Background(), sid, events.StateRunning)
	require.NoError(t, err)

	conn, _ := newTestWSConnPair(t)
	t.Cleanup(func() { conn.Close() })
	hub.JoinSession(sid, newConn(hub, conn, sid, nil))

	env := controlEnvelope(sid, string(events.ControlActionDelete))
	env.OwnerID = "hacker"
	err = handler.handleControl(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ownership required")
}

func TestHandleControl_UnknownAction(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_unknown"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	err = mgr.Transition(context.Background(), sid, events.StateRunning)
	require.NoError(t, err)

	conn, _ := newTestWSConnPair(t)
	t.Cleanup(func() { conn.Close() })
	hub.JoinSession(sid, newConn(hub, conn, sid, nil))

	env := controlEnvelope(sid, "fly_to_the_moon")
	env.OwnerID = "user1"
	err = handler.handleControl(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown control action")
}

func TestHandleControl_InvalidData(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_bad"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	conn, _ := newTestWSConnPair(t)
	t.Cleanup(func() { conn.Close() })
	hub.JoinSession(sid, newConn(hub, conn, sid, nil))

	badEnv := events.NewEnvelope(aep.NewID(), sid, 1, events.Control, "not a map")
	badEnv.OwnerID = "user1"
	err = handler.handleControl(context.Background(), badEnv)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid data")
}

// ─── Handle (event router) tests ─────────────────────────────────────────────

func TestHandle_InputRoute(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_handle"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	w := new(mockWorkerForHandler)
	w.On("Input", mock.Anything, "test", mock.Anything).Return(nil)
	w.On("Terminate", mock.Anything).Return(nil).Maybe()
	mgr.AttachWorker(context.Background(), sid, w)

	env := inputEnvelope(sid, "test")
	err = handler.Handle(context.Background(), env)
	require.NoError(t, err)
}

func TestHandle_PingRoute(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_hping"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	conn, _ := newTestWSConnPair(t)
	t.Cleanup(func() { conn.Close() })
	hub.JoinSession(sid, newConn(hub, conn, sid, nil))

	env := pingEnvelope(sid)
	err = handler.Handle(context.Background(), env)
	require.NoError(t, err)
}

func TestHandle_ControlRoute(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_hctrl"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	err = mgr.Transition(context.Background(), sid, events.StateRunning)
	require.NoError(t, err)

	env := controlEnvelope(sid, string(events.ControlActionDelete))
	env.OwnerID = "user1"
	err = handler.Handle(context.Background(), env)
	require.NoError(t, err)
}

func TestHandle_UnknownEventType(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)

	const sid = "sess_unknown_evt"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	// Unknown event type: Handle now returns the error from sendErrorf.
	env := events.NewEnvelope(aep.NewID(), sid, 1, "fly_to_the_moon", nil)
	env.OwnerID = "user1"
	err = handler.Handle(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown event type")
}

func TestHandle_InputMalformedData(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_mal"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	conn, _ := newTestWSConnPair(t)
	t.Cleanup(func() { conn.Close() })
	hub.JoinSession(sid, newConn(hub, conn, sid, nil))

	badEnv := events.NewEnvelope(aep.NewID(), sid, 1, events.Input, "not a map")
	badEnv.OwnerID = "user1"
	err = handler.Handle(context.Background(), badEnv)
	require.Error(t, err)
}

// ─── SendControlToSession tests ───────────────────────────────────────────────

func TestSendControlToSession_Reconnect(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_reconn"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	clientConn, serverConn := newTestWSConnPair(t)
	t.Cleanup(func() { clientConn.Close() })
	hub.JoinSession(sid, newConn(hub, clientConn, sid, nil))

	err = handler.SendControlToSession(context.Background(), sid,
		events.ControlActionReconnect, "worker restarted", map[string]any{"delay_ms": 1000})
	require.NoError(t, err)

	_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, data, err := serverConn.ReadMessage()
	require.NoError(t, err)
	var env events.Envelope
	err = json.Unmarshal(data, &env)
	require.NoError(t, err)
	require.Equal(t, events.Control, env.Event.Type)
}

func TestSendControlToSession_SessionInvalid(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_inval"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	clientConn, serverConn := newTestWSConnPair(t)
	t.Cleanup(func() { clientConn.Close() })
	hub.JoinSession(sid, newConn(hub, clientConn, sid, nil))

	err = handler.SendSessionInvalid(context.Background(), sid, "session deleted", true)
	require.NoError(t, err)

	_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, data, err := serverConn.ReadMessage()
	require.NoError(t, err)
	var env events.Envelope
	err = json.Unmarshal(data, &env)
	require.NoError(t, err)
	require.Equal(t, events.Control, env.Event.Type)
}

func TestSendControlToSession_Throttle(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_throttle"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	clientConn, serverConn := newTestWSConnPair(t)
	t.Cleanup(func() { clientConn.Close() })
	hub.JoinSession(sid, newConn(hub, clientConn, sid, nil))

	err = handler.SendThrottle(context.Background(), sid, 5000, 10)
	require.NoError(t, err)

	_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, data, err := serverConn.ReadMessage()
	require.NoError(t, err)
	var env events.Envelope
	err = json.Unmarshal(data, &env)
	require.NoError(t, err)
	require.Equal(t, events.Control, env.Event.Type)
}

// ─── SendReconnect test ────────────────────────────────────────────────────────

func TestSendReconnect(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)

	const sid = "sess_send_reconn"
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	clientConn, serverConn := newTestWSConnPair(t)
	t.Cleanup(func() { clientConn.Close() })
	hub.JoinSession(sid, newConn(hub, clientConn, sid, nil))

	err = handler.SendReconnect(context.Background(), sid, "worker restarted", 500)
	require.NoError(t, err)

	_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, data, err := serverConn.ReadMessage()
	require.NoError(t, err)
	var env2 events.Envelope
	err = json.Unmarshal(data, &env2)
	require.NoError(t, err)
	require.Equal(t, events.Control, env2.Event.Type)
}

// ─── Heartbeat tests ──────────────────────────────────────────────────────────

func TestHeartbeat_MarkMissed_ResetsOnMax(t *testing.T) {
	t.Parallel()
	h := newHeartbeat(slog.Default())
	// First 2 misses should not trigger max
	require.False(t, h.MarkMissed())
	require.False(t, h.MarkMissed())
	// Third miss reaches max (maxMiss=3)
	require.True(t, h.MarkMissed())
	// After MarkAlive resets, counter goes back to 0
	h.MarkAlive()
	require.Equal(t, 0, h.MissedCount())
}

func TestHeartbeat_MarkMissed_AfterStop(t *testing.T) {
	t.Parallel()
	h := newHeartbeat(slog.Default())
	h.Stop()
	// After stop, MarkMissed returns false (max not exceeded)
	require.False(t, h.MarkMissed())
}

func TestHeartbeat_MarkAlive_ResetsCounter(t *testing.T) {
	t.Parallel()
	h := newHeartbeat(slog.Default())
	require.Equal(t, 0, h.MissedCount())
	h.MarkMissed()
	require.Equal(t, 1, h.MissedCount())
	h.MarkAlive()
	require.Equal(t, 0, h.MissedCount())
}

func TestHeartbeat_Stop_Idempotent(t *testing.T) {
	t.Parallel()
	h := newHeartbeat(slog.Default())
	h.Stop()
	// Second stop: should be no-op (idempotent)
	h.Stop()
	select {
	case _, ok := <-h.Stopped():
		require.False(t, ok, "channel should be closed")
	default:
		t.Fatal("expected channel to be closed")
	}
}

func TestHandleInput_HelpCommand(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)
	_ = hub
	const sid = "sess_help"
	_, _ = mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	_ = mgr.Transition(context.Background(), sid, events.StateRunning)

	env := inputEnvelope(sid, "$help")
	err := handler.Handle(context.Background(), env)
	require.NoError(t, err)
}

func TestHandleInput_ControlCommandGC(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)
	const sid = "sess_ctrl_gc"
	_, _ = mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	_ = mgr.Transition(context.Background(), sid, events.StateRunning)

	env := inputEnvelope(sid, "$gc")
	env.OwnerID = "user1"
	err := handler.Handle(context.Background(), env)
	require.NoError(t, err)

	si, _ := mgr.Get(context.Background(), sid)
	require.Equal(t, events.StateTerminated, si.State)
}

func TestHandleInput_SlashGC(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)
	const sid = "sess_slash_gc"
	_, _ = mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	_ = mgr.Transition(context.Background(), sid, events.StateRunning)

	env := inputEnvelope(sid, "/gc")
	env.OwnerID = "user1"
	err := handler.Handle(context.Background(), env)
	require.NoError(t, err)

	si, _ := mgr.Get(context.Background(), sid)
	require.Equal(t, events.StateTerminated, si.State)
}

func TestHandleInput_NormalTextNotCommand(t *testing.T) {
	t.Parallel()
	handler, mgr, _, _ := newHandlerWithRealStore(t)
	const sid = "sess_normal"
	_, _ = mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	_ = mgr.Transition(context.Background(), sid, events.StateRunning)

	_ = handler.Handle(context.Background(), inputEnvelope(sid, "hello world"))
	// Normal text should attempt worker delivery; no crash and no command parsing.
}

func TestHandleInput_AutoResumeActiveSessionLackingWorkerBinding(t *testing.T) {
	t.Parallel()
	handler, mgr, hub, _ := newHandlerWithRealStore(t)
	const sid = "sess_autoresume_active"

	// Create an active session in StateIdle
	_, err := mgr.Create(context.Background(), sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	err = mgr.Transition(context.Background(), sid, events.StateRunning)
	require.NoError(t, err)
	err = mgr.Transition(context.Background(), sid, events.StateIdle)
	require.NoError(t, err)

	// Verify that the worker is NOT attached to the session manager, and there is no worker run binding
	require.Nil(t, mgr.GetWorker(sid))

	// Create the bridge and mock worker
	mockWorker := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		conn:       &fakeWorkerConn{ch: make(chan *events.Envelope)},
	}
	b := NewBridge(BridgeDeps{Log: slog.Default(), Hub: hub, SM: mgr})
	b.SetWorkerFactory(&mockBridgeWorkerFactory{workers: []*mockBridgeWorker{mockWorker}})
	handler.bridge = b

	// Verify that CurrentWorkerBinding returns false initially
	_, _, ok := b.CurrentWorkerBinding(sid)
	require.False(t, ok)

	// Handle input: this should trigger auto-resume since session is active but has no worker binding
	env := inputEnvelope(sid, "hello")
	env.OwnerID = "user1"
	err = handler.Handle(context.Background(), env)
	require.NoError(t, err)

	// Verify that the worker is now attached and active worker run binding is present
	require.NotNil(t, mgr.GetWorker(sid))
	_, _, ok = b.CurrentWorkerBinding(sid)
	require.True(t, ok)
}
