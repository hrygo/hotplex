package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/internal/worker"
	noopworker "github.com/hrygo/hotplex/internal/worker/noop"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

type captureAuditStore struct {
	mu         sync.Mutex
	activities []audit.UserActivity
}

func (s *captureAuditStore) BeginTx(context.Context) (audit.Tx, error) {
	return &captureAuditTx{store: s}, nil
}

func (s *captureAuditStore) Query(_ context.Context, _ audit.Query) ([]audit.UserActivity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	activities := make([]audit.UserActivity, len(s.activities))
	copy(activities, s.activities)
	return activities, nil
}

func (s *captureAuditStore) Stats(context.Context, audit.Query) (audit.ActivityStats, error) {
	return audit.ActivityStats{}, nil
}

func (s *captureAuditStore) QueryAsc(context.Context, int64, int) ([]audit.UserActivity, error) {
	return nil, nil
}

func (s *captureAuditStore) DeleteBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func (s *captureAuditStore) SaveCheckpoint(context.Context, audit.Checkpoint) error {
	return nil
}

func (s *captureAuditStore) LatestCheckpoint(context.Context) (*audit.Checkpoint, error) {
	return nil, nil
}

func (s *captureAuditStore) ListIdentityLinks(context.Context, string) ([]audit.IdentityLink, error) {
	return nil, nil
}

func (s *captureAuditStore) UpsertIdentityLink(context.Context, audit.IdentityLink) error {
	return nil
}

func (s *captureAuditStore) DeleteIdentityLink(context.Context, string) error {
	return nil
}

func (s *captureAuditStore) Close() error {
	return nil
}

func (s *captureAuditStore) Dialect() dbutil.Dialect {
	return dbutil.DialectSQLite
}

type captureAuditTx struct {
	store *captureAuditStore
}

func (tx *captureAuditTx) Append(ctx context.Context, activity *audit.UserActivity) error {
	return tx.AppendBatch(ctx, []*audit.UserActivity{activity})
}

func (tx *captureAuditTx) AppendBatch(_ context.Context, activities []*audit.UserActivity) error {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	for _, activity := range activities {
		tx.store.activities = append(tx.store.activities, *activity)
	}
	return nil
}

func (tx *captureAuditTx) SaveCheckpoint(context.Context, audit.Checkpoint) error {
	return nil
}

func (tx *captureAuditTx) TailHash(context.Context) (string, error) {
	return "", nil
}

func (tx *captureAuditTx) LastRowBefore(context.Context, time.Time) (int64, string, error) {
	return 0, "", nil
}

func (tx *captureAuditTx) DeleteByIDLEQ(context.Context, int64) (int64, error) {
	return 0, nil
}

func (tx *captureAuditTx) RowCount(context.Context) (int64, error) {
	return 0, nil
}

func (tx *captureAuditTx) Commit() error {
	return nil
}

func (tx *captureAuditTx) Rollback() error {
	return nil
}

// TestHandler_ReceivedEventLogAndAuditExcludeBody ensures neither operational
// logs nor audit facts create a second plaintext copy of chat content.
func TestHandler_ReceivedEventLogAndAuditExcludeBody(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	store := &captureAuditStore{}
	collector := audit.NewCollector(store, nil, nil, logger, audit.CollectorConfig{
		ChannelCap:    1,
		BatchSize:     1,
		BatchInterval: time.Hour,
	})
	collector.Start(context.Background())
	t.Cleanup(func() { require.NoError(t, collector.Close(context.Background())) })

	handler := NewHandler(HandlerDeps{Log: logger, Hub: newTestHub(t)})
	handler.SetAuditCollector(collector)
	secret := "customer-secret-message"
	data := map[string]any{"content": secret}
	env := events.NewEnvelope(aep.NewID(), "session-1", 7, events.Kind("unknown"), data)

	require.Error(t, handler.Handle(context.Background(), env))

	encoded, err := json.Marshal(data)
	require.NoError(t, err)
	sum := sha256.Sum256(encoded)
	logOutput := logs.String()
	require.NotContains(t, logOutput, secret)
	require.Contains(t, logOutput, `"data_size":`+strconv.Itoa(len(encoded)))
	require.Contains(t, logOutput, `"data_sha256":"`+hex.EncodeToString(sum[:])[:16]+`"`)

	handler.emitAudit(audit.OutcomeSuccess, "user-1", "feishu", "session-1", secret)
	require.Eventually(t, func() bool {
		activities, queryErr := store.Query(context.Background(), audit.Query{})
		return queryErr == nil && len(activities) == 1
	}, time.Second, 10*time.Millisecond)

	activities, err := store.Query(context.Background(), audit.Query{})
	require.NoError(t, err)
	var detail map[string]any
	require.NoError(t, json.Unmarshal([]byte(activities[0].DetailJSON), &detail))
	require.NotContains(t, activities[0].DetailJSON, secret)
	require.NotContains(t, detail, "content")
}

// mockHandlerSM is a mock session manager for handler tests.
type mockHandlerSM struct {
	mock.Mock
}

func (m *mockHandlerSM) ValidateOwnership(ctx context.Context, sessionID, userID, adminUserID string) error {
	args := m.Called(ctx, sessionID, userID, adminUserID)
	return args.Error(0)
}

func (m *mockHandlerSM) ClearContext(ctx context.Context, sessionID string) error {
	args := m.Called(ctx, sessionID)
	return args.Error(0)
}

func (m *mockHandlerSM) TransitionWithReason(ctx context.Context, id string, to events.SessionState, termReason string) error {
	args := m.Called(ctx, id, to, termReason)
	return args.Error(0)
}

func (m *mockHandlerSM) GetWorker(id string) worker.Worker {
	args := m.Called(id)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(worker.Worker)
}

func (m *mockHandlerSM) DetachWorker(id string) {
	m.Called(id)
}

func (m *mockHandlerSM) Get(_ context.Context, id string) (*session.SessionInfo, error) {
	args := m.Called(id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*session.SessionInfo), args.Error(1)
}

// mockHandlerHub captures sent envelopes for verification.
type mockHandlerHub struct {
	mu      sync.Mutex
	sent    []*events.Envelope
	nextSeq int64
}

func newMockHub() *mockHandlerHub {
	return &mockHandlerHub{nextSeq: 1}
}

func (h *mockHandlerHub) NextSeq(_ string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq := h.nextSeq
	h.nextSeq++
	return seq
}

func (h *mockHandlerHub) SendToSession(_ context.Context, env *events.Envelope, _ ...func()) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = append(h.sent, env)
	return nil
}

func (h *mockHandlerHub) Sent() []*events.Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sent
}

func (h *mockHandlerHub) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = nil
}

// testableHandler gives us access to handleReset/handleGC for testing
// by embedding the handler with a custom sm/hub.
type testableHandler struct {
	log *slog.Logger
	cfg *config.Config
	sm  interface {
		ValidateOwnership(ctx context.Context, sessionID, userID, adminUserID string) error
		ClearContext(ctx context.Context, sessionID string) error
		TransitionWithReason(ctx context.Context, id string, to events.SessionState, termReason string) error
		GetWorker(id string) worker.Worker
		DetachWorker(id string)
		Get(ctx context.Context, id string) (*session.SessionInfo, error)
	}
	hub interface {
		NextSeq(sessionID string) int64
		SendToSession(ctx context.Context, env *events.Envelope, afterDrain ...func()) error
	}
}

func (h *testableHandler) sendState(ctx context.Context, sessionID string, state events.SessionState, message string) error {
	env := events.NewEnvelope(aep.NewID(), sessionID, h.hub.NextSeq(sessionID), events.State, events.StateData{
		State:   state,
		Message: message,
	})
	return h.hub.SendToSession(ctx, env)
}

func (h *testableHandler) handleReset(ctx context.Context, sessionID, ownerID string) error {
	// 1. Ownership check
	if err := h.sm.ValidateOwnership(ctx, sessionID, ownerID, ""); err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return errors.New("SESSION_NOT_FOUND")
		}
		return errors.New("UNAUTHORIZED")
	}
	// 1b. State precondition: reset only valid for active states.
	si, err := h.sm.Get(context.Background(), sessionID)
	if err != nil {
		return errors.New("SESSION_NOT_FOUND")
	}
	if si.State != events.StateCreated && si.State != events.StateRunning && si.State != events.StateIdle {
		return errors.New("PROTOCOL_VIOLATION")
	}
	// 2. Clear Context
	if err := h.sm.ClearContext(ctx, sessionID); err != nil {
		return err
	}
	// 3. Worker reset
	w := h.sm.GetWorker(sessionID)
	if w != nil {
		if _, err := w.ResetContext(ctx); err != nil {
			return err
		}
	}
	// 4. Transition to RUNNING
	if err := h.sm.TransitionWithReason(ctx, sessionID, events.StateRunning, "reset"); err != nil {
		return err
	}
	// 5. Send state notification
	return h.sendState(ctx, sessionID, events.StateRunning, "context_reset")
}

func (h *testableHandler) handleGC(ctx context.Context, sessionID, ownerID string) error {
	// 1. Ownership check
	if err := h.sm.ValidateOwnership(ctx, sessionID, ownerID, ""); err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return errors.New("SESSION_NOT_FOUND")
		}
		return errors.New("UNAUTHORIZED")
	}
	// 2. Get current state for idempotency check.
	si, err := h.sm.Get(context.Background(), sessionID)
	if err != nil {
		return errors.New("SESSION_NOT_FOUND")
	}
	// Idempotent: already terminated — return success without transitioning.
	if si.State == events.StateTerminated {
		return nil
	}
	// 3. Terminate + detach worker
	if w := h.sm.GetWorker(sessionID); w != nil {
		_ = w.Terminate(ctx)
		h.sm.DetachWorker(sessionID)
	}
	// 4. Re-read after worker cleanup to avoid stale-snapshot race.
	if fresh, err := h.sm.Get(context.Background(), sessionID); err == nil && fresh.State == events.StateTerminated {
		return nil
	}
	// 5. Transition to TERMINATED
	if err := h.sm.TransitionWithReason(ctx, sessionID, events.StateTerminated, "gc"); err != nil {
		return err
	}
	// 6. Send state notification
	return h.sendState(ctx, sessionID, events.StateTerminated, "session_archived")
}

// mockWorkerForHandler implements worker.Worker for handler tests.
type mockWorkerForHandler struct {
	mock.Mock
	conn    worker.SessionConn
	stopped atomic.Bool
}

func (m *mockWorkerForHandler) Type() worker.WorkerType   { return worker.TypeClaudeCode }
func (m *mockWorkerForHandler) SupportsResume() bool      { return true }
func (m *mockWorkerForHandler) CanResumeTerminated() bool { return true }
func (m *mockWorkerForHandler) SupportsStreaming() bool   { return true }
func (m *mockWorkerForHandler) SupportsTools() bool       { return true }
func (m *mockWorkerForHandler) EnvBlocklist() []string    { return nil }
func (m *mockWorkerForHandler) SessionStoreDir() string   { return "" }
func (m *mockWorkerForHandler) MaxTurns() int             { return 0 }
func (m *mockWorkerForHandler) Modalities() []string      { return []string{"text"} }

func (m *mockWorkerForHandler) Start(ctx context.Context, session worker.SessionInfo) error {
	args := m.Called(ctx, session)
	return args.Error(0)
}
func (m *mockWorkerForHandler) Input(ctx context.Context, content string, metadata map[string]any) error {
	m.stopped.Store(false)
	args := m.Called(ctx, content, metadata)
	return args.Error(0)
}
func (m *mockWorkerForHandler) Resume(ctx context.Context, session worker.SessionInfo) error {
	args := m.Called(ctx, session)
	return args.Error(0)
}
func (m *mockWorkerForHandler) Terminate(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}
func (m *mockWorkerForHandler) Kill() error { return nil }
func (m *mockWorkerForHandler) Wait() (int, error) {
	args := m.Called()
	return args.Int(0), args.Error(1)
}
func (m *mockWorkerForHandler) Conn() worker.SessionConn { return m.conn }
func (m *mockWorkerForHandler) Health() worker.WorkerHealth {
	args := m.Called()
	return args.Get(0).(worker.WorkerHealth)
}
func (m *mockWorkerForHandler) LastIO() time.Time {
	args := m.Called()
	return args.Get(0).(time.Time)
}
func (m *mockWorkerForHandler) ResetContext(ctx context.Context) (worker.ResetResult, error) {
	args := m.Called(ctx)
	return args.Get(0).(worker.ResetResult), args.Error(1)
}
func (m *mockWorkerForHandler) StopCurrentTurn(ctx context.Context) error {
	args := m.Called(ctx)
	err := args.Error(0)
	if err == nil {
		m.stopped.Store(true)
	}
	return err
}
func (m *mockWorkerForHandler) IsStopped() bool {
	return m.stopped.Load()
}

// ─── handleReset tests ──────────────────────────────────────────────────────

// mockControlWorker implements both worker.Worker and worker.ControlRequester.
type mockControlWorker struct {
	mockWorkerForHandler
	controlResp    map[string]any
	controlErr     error
	controlCalled  bool
	controlSubtype string
}

func (m *mockControlWorker) SendControlRequest(_ context.Context, subtype string, _ map[string]any) (map[string]any, error) {
	m.controlCalled = true
	m.controlSubtype = subtype
	return m.controlResp, m.controlErr
}

func (m *mockControlWorker) Terminate(_ context.Context) error {
	return nil
}

// mockCommanderWorker implements worker.Worker and worker.WorkerCommander.
type mockCommanderWorker struct {
	mockWorkerForHandler
	compactCalled bool
	clearCalled   bool
	rewindCalled  bool
}

func (m *mockCommanderWorker) Compact(_ context.Context, _ map[string]any) error {
	m.compactCalled = true
	return nil
}

func (m *mockCommanderWorker) Clear(_ context.Context) error {
	m.clearCalled = true
	return nil
}

func (m *mockCommanderWorker) Rewind(_ context.Context, _ string) error {
	m.rewindCalled = true
	return nil
}

func (m *mockCommanderWorker) Terminate(_ context.Context) error {
	return nil
}
func (m *mockCommanderWorker) SendControlRequest(_ context.Context, _ string, _ map[string]any) (map[string]any, error) {
	return nil, nil
}

// ─── handleWorkerCommand tests ──────────────────────────────────────────────

type workerCommandTestCtx struct {
	w            worker.Worker
	controlWkr   *mockControlWorker
	commanderWkr *mockCommanderWorker
	mockWkr      *mockWorkerForHandler
}

type workerCommandTestCase struct {
	name       string
	setup      func(tc *workerCommandTestCtx)
	command    events.WorkerStdioCommand
	rawData    map[string]any
	state      events.SessionState
	wantErr    bool
	errContain string
	verify     func(t *testing.T, tc *workerCommandTestCtx)
}

func (tc workerCommandTestCase) run(t *testing.T) {
	t.Helper()

	handler, mgr, _, _ := newHandlerWithRealStore(t)
	ctx := context.Background()
	const sid = "sess_wc"

	testCtx := &workerCommandTestCtx{}
	tc.setup(testCtx)

	_, err := mgr.Create(ctx, sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)

	targetState := tc.state
	if targetState == "" {
		targetState = events.StateRunning
	}
	require.NoError(t, mgr.Transition(ctx, sid, targetState))

	if testCtx.w != nil && targetState == events.StateRunning {
		require.NoError(t, mgr.AttachWorker(context.Background(), sid, testCtx.w))
	}

	var data any = events.WorkerCommandData{Command: tc.command}
	if tc.rawData != nil {
		data = tc.rawData
	}

	env := &events.Envelope{
		SessionID: sid,
		OwnerID:   "user1",
		Event: events.Event{
			Type: events.WorkerCmd,
			Data: data,
		},
	}

	err = handler.handleWorkerCommand(ctx, env)
	if tc.wantErr {
		require.Error(t, err)
		require.Contains(t, err.Error(), tc.errContain)
	} else {
		require.NoError(t, err)
	}
	if tc.verify != nil {
		tc.verify(t, testCtx)
	}
}

func TestHandleWorkerCommand(t *testing.T) {
	t.Parallel()

	tests := []workerCommandTestCase{
		{
			name:    "passthrough_compact",
			command: events.StdioCompact,
			setup: func(tc *workerCommandTestCtx) {
				w := &mockCommanderWorker{}
				tc.w = w
				tc.commanderWkr = w
			},
			verify: func(t *testing.T, tc *workerCommandTestCtx) {
				t.Helper()
				require.True(t, tc.commanderWkr.compactCalled, "compact should be called")
			},
		},
		{
			name:    "passthrough_clear",
			command: events.StdioClear,
			setup: func(tc *workerCommandTestCtx) {
				w := &mockCommanderWorker{}
				tc.w = w
				tc.commanderWkr = w
			},
			verify: func(t *testing.T, tc *workerCommandTestCtx) {
				t.Helper()
				require.True(t, tc.commanderWkr.clearCalled, "clear should be called")
			},
		},
		{
			name:    "passthrough_rewind",
			command: events.StdioRewind,
			setup: func(tc *workerCommandTestCtx) {
				w := &mockCommanderWorker{}
				tc.w = w
				tc.commanderWkr = w
			},
			verify: func(t *testing.T, tc *workerCommandTestCtx) {
				t.Helper()
				require.True(t, tc.commanderWkr.rewindCalled, "rewind should be called")
			},
		},
		{
			name:    "control_context_usage",
			command: events.StdioContextUsage,
			setup: func(tc *workerCommandTestCtx) {
				w := &mockControlWorker{
					controlResp: map[string]any{
						"totalTokens": float64(50000),
						"maxTokens":   float64(200000),
						"percentage":  float64(25),
						"model":       "claude-sonnet-4",
						"categories":  []any{},
					},
				}
				tc.w = w
				tc.controlWkr = w
			},
			verify: func(t *testing.T, tc *workerCommandTestCtx) {
				t.Helper()
				require.True(t, tc.controlWkr.controlCalled, "SendControlRequest should be called")
				require.Equal(t, "get_context_usage", tc.controlWkr.controlSubtype)
			},
		},
		{
			name:    "control_mcp_status",
			command: events.StdioMCPStatus,
			setup: func(tc *workerCommandTestCtx) {
				w := &mockControlWorker{
					controlResp: map[string]any{
						"servers": []any{
							map[string]any{"name": "context7", "status": "connected"},
						},
					},
				}
				tc.w = w
				tc.controlWkr = w
			},
			verify: func(t *testing.T, tc *workerCommandTestCtx) {
				t.Helper()
				require.Equal(t, "mcp_status", tc.controlWkr.controlSubtype)
			},
		},
		{
			name:    "fallback_to_input",
			command: events.StdioCommit,
			setup: func(tc *workerCommandTestCtx) {
				w := new(mockWorkerForHandler)
				w.On("Input", mock.Anything, "/commit", mock.Anything).Return(nil)
				w.On("Terminate", mock.Anything).Return(nil)
				tc.w = w
				tc.mockWkr = w
			},
			verify: func(t *testing.T, tc *workerCommandTestCtx) {
				t.Helper()
				tc.mockWkr.AssertCalled(t, "Input", mock.Anything, "/commit", mock.Anything)
			},
		},
		{
			name:    "map_data_context_usage",
			command: "",
			rawData: map[string]any{"command": "context_usage"},
			setup: func(tc *workerCommandTestCtx) {
				w := &mockControlWorker{
					controlResp: map[string]any{
						"totalTokens": float64(50000),
						"maxTokens":   float64(200000),
						"categories":  []any{},
					},
				}
				tc.w = w
				tc.controlWkr = w
			},
			verify: func(t *testing.T, tc *workerCommandTestCtx) {
				t.Helper()
				require.True(t, tc.controlWkr.controlCalled)
			},
		},
		{
			name:       "terminated_session",
			command:    events.StdioCompact,
			state:      events.StateTerminated,
			wantErr:    true,
			errContain: "SESSION_BUSY",
			setup: func(tc *workerCommandTestCtx) {
				w := new(mockWorkerForHandler)
				tc.w = w
			},
		},
		{
			name:       "no_worker_attached",
			command:    events.StdioCompact,
			wantErr:    true,
			errContain: "no worker attached",
			setup: func(tc *workerCommandTestCtx) {
				tc.w = nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

// TestHandleWorkerCommand_Clear_ReconcilesGatewayState verifies that a
// successful worker-level /clear reconciles gateway state: the client must
// receive a terminal `done` event (otherwise the webchat spinner stays on) and
// the in-flight execution runtime must be finalized (otherwise the next input
// is rejected with SESSION_BUSY). Regression for "✅ 会话已清空" shown while the
// session stays blocked.
func TestHandleWorkerCommand_Clear_ReconcilesGatewayState(t *testing.T) {
	t.Parallel()

	handler, mgr, hub, _ := newHandlerWithRealStore(t)
	ctx := context.Background()
	const sid = "sess_clear_reconcile"

	_, err := mgr.Create(ctx, sid, "user1", worker.TypeClaudeCode, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, mgr.Transition(ctx, sid, events.StateRunning))

	w := &mockCommanderWorker{}
	require.NoError(t, mgr.AttachWorker(ctx, sid, w))

	// Wire a real execution store (sharing the session migrations) so a genuine
	// in-flight runtime can be finalized by the gateway.
	execStore := newTestExecutionStore(t, sid)
	rec, _, err := execStore.Accept(ctx, execution.AcceptRequest{
		SessionID:       sid,
		ClientMessageID: "msg-clear",
		PayloadHash:     "h",
		OwnerInstanceID: "gw-test",
		WorkerRunID:     "run-clear",
	})
	require.NoError(t, err)
	require.NoError(t, execStore.MarkRunning(ctx, rec.ExecutionID, "gw-test", "run-clear"))
	handler.executionStore = execStore

	// Capture envelopes sent to the client.
	pc := &mockPlatformConn{}
	hub.JoinPlatformSession(sid, pc)

	env := &events.Envelope{
		SessionID: sid,
		OwnerID:   "user1",
		Event: events.Event{
			Type: events.WorkerCmd,
			Data: events.WorkerCommandData{Command: events.StdioClear},
		},
	}

	require.NoError(t, handler.handleWorkerCommand(ctx, env))
	require.True(t, w.clearCalled, "worker Clear() must be invoked")

	// 1) A terminal done event must be sent so the client clears isRunning.
	require.Eventually(t, func() bool {
		for _, e := range pc.envelopes() {
			if e.Event.Type == events.Done {
				if d, ok := e.Event.Data.(events.DoneData); ok && d.Reason == "cleared_by_user" {
					return true
				}
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "expected a done event with reason cleared_by_user")

	// 2) The in-flight execution runtime must be finalized, so the next input
	//    is not rejected with SESSION_BUSY.
	_, err = execStore.ActiveBySession(ctx, sid)
	require.ErrorIs(t, err, execution.ErrNotFound, "active runtime should be finalized after /clear")
}

// newTestExecutionStore builds a real execution.Store backed by a session
// SQLite store (which runs the execution_inputs migration) on a temp db. The
// given session is upserted so the execution_inputs FK constraint holds.
func newTestExecutionStore(t *testing.T, sessionID string) execution.Store {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.DB.Path = filepath.Join(t.TempDir(), "clear_exec.db")
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

	store, err := execution.NewSQLStore(ctx, sessionStore.DB(), dbutil.DialectSQLite, writeMu, slog.Default())
	require.NoError(t, err)
	return store
}

func TestHandleWorkerCommandSessionNotFound(t *testing.T) {
	t.Parallel()

	handler, _, _, _ := newHandlerWithRealStore(t)

	env := &events.Envelope{
		SessionID: "sess-missing",
		OwnerID:   "user1",
		Event: events.Event{
			Type: events.WorkerCmd,
			Data: events.WorkerCommandData{
				Command: events.StdioCompact,
			},
		},
	}

	err := handler.handleWorkerCommand(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SESSION_NOT_FOUND")
}

// ─── handleReset tests ──────────────────────────────────────────────────────

func TestHandler_HandleReset_Unauthorized(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	sm.On("ValidateOwnership", ctx, "sess1", "wrong_user", "").Return(session.ErrOwnershipMismatch)

	err := h.handleReset(ctx, "sess1", "wrong_user")
	require.Error(t, err)
	require.Contains(t, err.Error(), "UNAUTHORIZED")
	sm.AssertExpectations(t)
}

func TestHandler_HandleReset_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	sm.On("ValidateOwnership", ctx, "sess1", "user1", "").Return(session.ErrSessionNotFound)

	err := h.handleReset(ctx, "sess1", "user1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "SESSION_NOT_FOUND")
	sm.AssertExpectations(t)
}

func TestHandler_HandleReset_Success(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	w := new(mockWorkerForHandler)

	sm.On("ValidateOwnership", ctx, "sess1", "user1", "").Return(nil)
	sm.On("Get", "sess1").Return(&session.SessionInfo{ID: "sess1", State: events.StateRunning}, nil)
	sm.On("ClearContext", ctx, "sess1").Return(nil)
	sm.On("GetWorker", "sess1").Return(w)
	w.On("ResetContext", ctx).Return(worker.ResetResult{}, nil)
	sm.On("TransitionWithReason", ctx, "sess1", events.StateRunning, "reset").Return(nil)

	err := h.handleReset(ctx, "sess1", "user1")
	require.NoError(t, err)

	// Verify state notification was sent
	sent := hub.Sent()
	require.Len(t, sent, 1)
	require.Equal(t, events.State, sent[0].Event.Type)
	stateData, ok := sent[0].Event.Data.(events.StateData)
	require.True(t, ok)
	require.Equal(t, events.StateRunning, stateData.State)
	require.Equal(t, "context_reset", stateData.Message)

	sm.AssertExpectations(t)
	w.AssertExpectations(t)
}

func TestHandler_HandleReset_NoWorker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	sm.On("ValidateOwnership", ctx, "sess1", "user1", "").Return(nil)
	sm.On("Get", "sess1").Return(&session.SessionInfo{ID: "sess1", State: events.StateRunning}, nil)
	sm.On("ClearContext", ctx, "sess1").Return(nil)
	sm.On("GetWorker", "sess1").Return(nil) // no worker attached
	sm.On("TransitionWithReason", ctx, "sess1", events.StateRunning, "reset").Return(nil)

	err := h.handleReset(ctx, "sess1", "user1")
	require.NoError(t, err)

	sm.AssertExpectations(t)
}

func TestHandler_HandleReset_WorkerResetFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	w := new(mockWorkerForHandler)

	sm.On("ValidateOwnership", ctx, "sess1", "user1", "").Return(nil)
	sm.On("Get", "sess1").Return(&session.SessionInfo{ID: "sess1", State: events.StateRunning}, nil)
	sm.On("ClearContext", ctx, "sess1").Return(nil)
	sm.On("GetWorker", "sess1").Return(w)
	w.On("ResetContext", ctx).Return(worker.ResetResult{}, errors.New("worker reset failed"))

	err := h.handleReset(ctx, "sess1", "user1")
	require.Error(t, err)

	sm.AssertExpectations(t)
	w.AssertExpectations(t)
}

func TestHandler_HandleReset_TerminatedState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	sm.On("ValidateOwnership", ctx, "sess1", "user1", "").Return(nil)
	sm.On("Get", "sess1").Return(&session.SessionInfo{ID: "sess1", State: events.StateTerminated}, nil)

	err := h.handleReset(ctx, "sess1", "user1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "PROTOCOL_VIOLATION")

	sm.AssertExpectations(t)
}

// ─── handleGC tests ────────────────────────────────────────────────────────

func TestHandler_HandleGC_Unauthorized(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	sm.On("ValidateOwnership", ctx, "sess1", "wrong_user", "").Return(session.ErrOwnershipMismatch)

	err := h.handleGC(ctx, "sess1", "wrong_user")
	require.Error(t, err)
	require.Contains(t, err.Error(), "UNAUTHORIZED")
	sm.AssertExpectations(t)
}

func TestHandler_HandleGC_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	sm.On("ValidateOwnership", ctx, "sess1", "user1", "").Return(session.ErrSessionNotFound)

	err := h.handleGC(ctx, "sess1", "user1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "SESSION_NOT_FOUND")
	sm.AssertExpectations(t)
}

func TestHandler_HandleGC_Success(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	w := new(mockWorkerForHandler)

	sm.On("ValidateOwnership", ctx, "sess1", "user1", "").Return(nil)
	sm.On("Get", "sess1").Return(&session.SessionInfo{ID: "sess1", State: events.StateRunning}, nil)
	sm.On("GetWorker", "sess1").Return(w)
	w.On("Terminate", ctx).Return(nil)
	sm.On("DetachWorker", "sess1")
	sm.On("TransitionWithReason", ctx, "sess1", events.StateTerminated, "gc").Return(nil)

	err := h.handleGC(ctx, "sess1", "user1")
	require.NoError(t, err)

	// Verify state notification
	sent := hub.Sent()
	require.Len(t, sent, 1)
	require.Equal(t, events.State, sent[0].Event.Type)
	stateData, ok := sent[0].Event.Data.(events.StateData)
	require.True(t, ok)
	require.Equal(t, events.StateTerminated, stateData.State)
	require.Equal(t, "session_archived", stateData.Message)

	sm.AssertExpectations(t)
	w.AssertExpectations(t)
}

func TestHandler_HandleGC_NoWorker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	sm.On("ValidateOwnership", ctx, "sess1", "user1", "").Return(nil)
	sm.On("Get", "sess1").Return(&session.SessionInfo{ID: "sess1", State: events.StateIdle}, nil)
	sm.On("GetWorker", "sess1").Return(nil) // no worker attached
	sm.On("TransitionWithReason", ctx, "sess1", events.StateTerminated, "gc").Return(nil)

	err := h.handleGC(ctx, "sess1", "user1")
	require.NoError(t, err)

	sm.AssertExpectations(t)
}

func TestHandler_HandleGC_Idempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sm := new(mockHandlerSM)
	hub := newMockHub()

	h := &testableHandler{
		log: slog.Default(),
		cfg: config.Default(),
		sm:  sm,
		hub: hub,
	}

	// Session is already TERMINATED — gc should succeed without transitioning.
	sm.On("ValidateOwnership", ctx, "sess1", "user1", "").Return(nil)
	sm.On("Get", "sess1").Return(&session.SessionInfo{ID: "sess1", State: events.StateTerminated}, nil)
	// No GetWorker, no Terminate, no DetachWorker, no Transition — idempotent.

	err := h.handleGC(ctx, "sess1", "user1")
	require.NoError(t, err)

	// No state notifications sent (idempotent — no changes).
	require.Empty(t, hub.Sent())
	sm.AssertExpectations(t)
}

// ─── Worker.ResetContext implementation tests ─────────────────────────────────

func TestWorker_ResetContext_Noop(t *testing.T) {
	t.Parallel()
	w := noopworker.NewWorker()
	_, err := w.ResetContext(context.Background())
	require.NoError(t, err)
}

// ─── handleInput interaction response tests ──────────────────────────────────

// mockInputSM satisfies SessionManager for handleInput tests.
type mockInputSM struct {
	mock.Mock
}

func (m *mockInputSM) Get(_ context.Context, id string) (*session.SessionInfo, error) {
	args := m.Called(id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*session.SessionInfo), args.Error(1)
}
func (m *mockInputSM) GetWorker(id string) worker.Worker {
	args := m.Called(id)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(worker.Worker)
}
func (m *mockInputSM) CreateWithBot(_ context.Context, _ string, _ string, _ string, _ string, _ worker.WorkerType, _ []string, _ string, _ map[string]string, _ string, _ string, _ string, _ string) (*session.SessionInfo, error) {
	return nil, nil
}
func (m *mockInputSM) Delete(_ context.Context, _ string) error         { return nil }
func (m *mockInputSM) DeletePhysical(_ context.Context, _ string) error { return nil }
func (m *mockInputSM) Transition(_ context.Context, _ string, _ events.SessionState) error {
	return nil
}
func (m *mockInputSM) TransitionWithInput(_ context.Context, _ string, _ events.SessionState, _ string, _ map[string]any) error {
	return nil
}
func (m *mockInputSM) TransitionWithReason(_ context.Context, _ string, _ events.SessionState, _ string) error {
	return nil
}
func (m *mockInputSM) AttachWorker(_ context.Context, _ string, _ worker.Worker) error { return nil }
func (m *mockInputSM) DetachWorker(_ string)                                           {}
func (m *mockInputSM) DetachWorkerIf(_ string, _ worker.Worker) bool                   { return false }
func (m *mockInputSM) UpdateWorkerSessionID(_ context.Context, _ string, _ string) error {
	return nil
}
func (m *mockInputSM) EnsureWorkerSessionID(_ context.Context, _ string, _ string) error {
	return nil
}
func (m *mockInputSM) SetPermissionCeilingIfEmpty(_ context.Context, _ string, ceiling string) (string, error) {
	return ceiling, nil
}
func (m *mockInputSM) List(_ context.Context, _ string, _ string, _ string, _ int, _ int) ([]*session.SessionInfo, error) {
	return nil, nil
}
func (m *mockInputSM) ValidateOwnership(_ context.Context, _ string, _ string, _ string) error {
	return nil
}
func (m *mockInputSM) UpdateWorkDir(_ context.Context, _ string, _ string) error { return nil }
func (m *mockInputSM) ResetExpiry(_ context.Context, _ string) error             { return nil }

func newInputHandler(t *testing.T, sm *mockInputSM) *Handler {
	t.Helper()
	return &Handler{
		log: slog.Default(),
		hub: nil, // interaction path does not use hub
		sm:  sm,
	}
}

func TestHandleInput_InteractionResponse_RoutesToWorker(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
	}{
		{"permission_response", map[string]any{"permission_response": map[string]any{"tool_call_id": "tc1", "decision": "allow"}}},
		{"question_response", map[string]any{"question_response": map[string]any{"answer": "yes"}}},
		{"elicitation_response", map[string]any{"elicitation_response": map[string]any{"value": "/tmp"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := new(mockInputSM)
			w := new(mockWorkerForHandler)
			sm.On("GetWorker", "s1").Return(w)
			w.On("Input", mock.Anything, "my response", mock.Anything).Return(nil)

			h := newInputHandler(t, sm)
			err := h.handleInput(context.Background(), inputEnvelopeWithMetadata("s1", "my response", tt.metadata))

			require.NoError(t, err)
			sm.AssertExpectations(t)
			w.AssertExpectations(t)

			// Verify Input received the interaction metadata (not nil).
			calls := w.Calls
			require.Len(t, calls, 1)
			md, ok := calls[0].Arguments.Get(2).(map[string]any)
			require.True(t, ok)
			require.NotNil(t, md[tt.name])
		})
	}
}

func TestHandleInput_NoWorker_ReturnsError(t *testing.T) {
	sm := new(mockInputSM)
	sm.On("GetWorker", "s1").Return(nil) // no worker

	h := newInputHandler(t, sm)
	err := h.handleInput(context.Background(), inputEnvelopeWithMetadata("s1", "ok", map[string]any{
		"permission_response": map[string]any{"decision": "allow"},
	}))

	require.Error(t, err)
	require.Contains(t, err.Error(), "interaction response dropped")
	sm.AssertExpectations(t)
}

func TestHandleInput_WorkerInputError_ReturnsError(t *testing.T) {
	sm := new(mockInputSM)
	w := new(mockWorkerForHandler)
	sm.On("GetWorker", "s1").Return(w)
	w.On("Input", mock.Anything, "ok", mock.Anything).Return(errors.New("worker busy"))

	h := newInputHandler(t, sm)
	err := h.handleInput(context.Background(), inputEnvelopeWithMetadata("s1", "ok", map[string]any{
		"question_response": map[string]any{"answer": "yes"},
	}))

	require.Error(t, err)
	require.Contains(t, err.Error(), "interaction response failed")
	sm.AssertExpectations(t)
	w.AssertExpectations(t)
}

func TestHandleInteractionResponseEvent_SendsCorrelatedSuccessAck(t *testing.T) {
	t.Parallel()

	sm := new(mockInputSM)
	w := new(mockWorkerForHandler)
	sm.On("Get", "s-interaction-ack").Return(&session.SessionInfo{Platform: "webchat"}, nil)
	sm.On("GetWorker", "s-interaction-ack").Return(w)
	w.On("Input", mock.Anything, "", mock.MatchedBy(func(metadata map[string]any) bool {
		response, ok := metadata["permission_response"].(map[string]any)
		return ok && response["request_id"] == "perm-ack-1" && response["allowed"] == true
	})).Return(nil)

	hub := newTestHub(t)
	platformConn := &mockPlatformConn{}
	hub.JoinPlatformSession("s-interaction-ack", platformConn)
	h := &Handler{log: slog.Default(), hub: hub, sm: sm}
	env := events.NewEnvelope("response-env", "s-interaction-ack", 7, events.PermissionResponse, map[string]any{
		"id":      "perm-ack-1",
		"allowed": true,
	})
	env.OwnerID = "user-1"

	require.NoError(t, h.handleInteractionResponseEvent(context.Background(), env))
	require.Eventually(t, func() bool {
		return len(platformConn.envelopes()) == 1
	}, time.Second, 10*time.Millisecond)

	ack := platformConn.envelopes()[0]
	require.Equal(t, events.PermissionResponse, ack.Event.Type)
	data, ok := ack.Event.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "perm-ack-1", data["id"])
	require.Equal(t, true, data["allowed"])
	sm.AssertExpectations(t)
	w.AssertExpectations(t)
}

func TestHandleInput_NonInteractionMetadata_FallsThrough(t *testing.T) {
	sm := new(mockInputSM)
	w := new(mockWorkerForHandler)

	// Normal path: Get returns active session, GetWorker returns worker,
	// worker.Input called with nil metadata (not interaction metadata).
	sm.On("Get", "s1").Return(&session.SessionInfo{State: events.StateRunning}, nil)
	sm.On("GetWorker", "s1").Return(w)
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(nil)

	h := newInputHandler(t, sm)
	err := h.handleInput(context.Background(), inputEnvelopeWithMetadata("s1", "hello", map[string]any{
		"platform": "slack", // not an interaction key
	}))

	require.NoError(t, err)
	sm.AssertExpectations(t)
	w.AssertExpectations(t)
}

func TestHandleInput_NoMetadata_FallsThrough(t *testing.T) {
	sm := new(mockInputSM)
	w := new(mockWorkerForHandler)

	sm.On("Get", "s1").Return(&session.SessionInfo{State: events.StateRunning}, nil)
	sm.On("GetWorker", "s1").Return(w)
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(nil)

	h := newInputHandler(t, sm)
	err := h.handleInput(context.Background(), inputEnvelope("s1", "hello"))

	require.NoError(t, err)
	sm.AssertExpectations(t)
	w.AssertExpectations(t)
}

// ─── CaptureInbound tests ────────────────────────────────────────────────────

// newBridgeWithCollector creates a Bridge with a real Collector backed by an
// in-memory SQLite store, for verifying CaptureInbound persistence.
func newBridgeWithCollector(t *testing.T) (*Bridge, *eventstore.SQLiteStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open(sqlutil.DriverName, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL,
		direction TEXT NOT NULL DEFAULT 'outbound',
		source TEXT NOT NULL DEFAULT 'normal'
			CHECK(source IN ('normal', 'crash', 'timeout', 'fresh_start')),
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL DEFAULT 0
	)`)
	require.NoError(t, err)
	createEventStoreSessionBarrierSchema(t, db)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS turns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		client_message_id TEXT,
		generation INTEGER NOT NULL DEFAULT 1,
		turn_num INTEGER NOT NULL,
		seq INTEGER NOT NULL DEFAULT 0,
		role TEXT NOT NULL,
		content TEXT NOT NULL DEFAULT '',
		platform TEXT NOT NULL DEFAULT '',
		user_id TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		success INTEGER,
		source TEXT NOT NULL DEFAULT 'normal',
		tools_json TEXT,
		tool_count INTEGER NOT NULL DEFAULT 0,
		tokens_input INTEGER NOT NULL DEFAULT 0,
		tokens_cache_write INTEGER NOT NULL DEFAULT 0,
		tokens_cache_read INTEGER NOT NULL DEFAULT 0,
		tokens_out INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0.0,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL DEFAULT 0
	)`)
	require.NoError(t, err)

	store := eventstore.NewSQLiteStore(db, nil)
	collector := eventstore.NewCollector(store, slog.Default())

	return &Bridge{log: slog.Default(), collector: collector, accum: make(map[string]*sessionAccumulator), turnsQuerier: store}, store
}

// IES-2: interaction response (permission/question/elicitation) triggers CaptureInbound.
func TestHandleInput_InteractionResponse_CaptureInbound(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
	}{
		{"permission_response", map[string]any{"permission_response": map[string]any{"tool_call_id": "tc1", "decision": "allow"}}},
		{"question_response", map[string]any{"question_response": map[string]any{"answer": "yes"}}},
		{"elicitation_response", map[string]any{"elicitation_response": map[string]any{"value": "/tmp"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := new(mockInputSM)
			w := new(mockWorkerForHandler)
			sm.On("GetWorker", "s1").Return(w)
			w.On("Input", mock.Anything, "ok", mock.Anything).Return(nil)

			br, store := newBridgeWithCollector(t)
			h := &Handler{
				log:    slog.Default(),
				hub:    nil,
				sm:     sm,
				bridge: br,
			}

			env := inputEnvelopeWithMetadata("s1", "ok", tt.metadata)
			env.Seq = 42
			err := h.handleInput(context.Background(), env)
			require.NoError(t, err)

			// Close collector to flush pending events, then verify inbound was stored.
			require.NoError(t, br.collector.Close())
			page, err := store.QueryBySession(context.Background(), "s1", 0, eventstore.CursorLatest, 10)
			require.NoError(t, err)
			require.Len(t, page.Events, 1, "expected exactly one inbound event")
			require.Equal(t, "inbound", page.Events[0].Direction)
			require.Equal(t, int64(42), page.Events[0].Seq)
			require.Equal(t, string(events.Input), page.Events[0].Type)
		})
	}
}

// IES-3: normal input triggers CaptureInbound.
func TestHandleInput_NormalInput_CaptureInbound(t *testing.T) {
	sm := new(mockInputSM)
	w := new(mockWorkerForHandler)
	sm.On("Get", "s1").Return(&session.SessionInfo{State: events.StateRunning}, nil)
	sm.On("GetWorker", "s1").Return(w)
	w.On("Input", mock.Anything, "hello", mock.Anything).Return(nil)

	br, store := newBridgeWithCollector(t)
	h := &Handler{
		log:    slog.Default(),
		hub:    nil,
		sm:     sm,
		bridge: br,
	}

	env := inputEnvelope("s1", "hello")
	env.Seq = 7
	err := h.handleInput(context.Background(), env)
	require.NoError(t, err)

	require.NoError(t, br.collector.Close())
	page, err := store.QueryBySession(context.Background(), "s1", 0, eventstore.CursorLatest, 10)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	require.Equal(t, "inbound", page.Events[0].Direction)
	require.Equal(t, int64(7), page.Events[0].Seq)
}

// ─── handleSupplementOnBusy (SESSION_BUSY mid-turn) tests ────────────────────

// mockMidTurnWorker extends mockWorkerForHandler with InjectMidTurn, satisfying
// worker.MidTurnInjector so the handler's SESSION_BUSY branch takes the
// passthrough path.
type mockMidTurnWorker struct {
	mockWorkerForHandler
	injectErr   error
	injected    string
	injectCount int
}

func (m *mockMidTurnWorker) InjectMidTurn(_ context.Context, content string, _ map[string]any) error {
	m.injectCount++
	m.injected = content
	return m.injectErr
}

// newBusyTestHandler wires a Handler with a mock SessionManager, a real Hub
// (so NextSeq/SendToSession work without Run()), and a real Bridge (so the
// PendingBuffer is exercised). The returned sm is pre-set to return the given
// worker from GetWorker; callers can substitute it via sm.Mock expectations.
func newBusyTestHandler(t *testing.T, w worker.Worker) (*Handler, *mockInputSM, *Hub, *Bridge) {
	t.Helper()
	sm := new(mockInputSM)
	sm.Test(t)
	if w != nil {
		sm.On("GetWorker", mock.Anything).Return(w).Maybe()
	}
	hub := newCtrlHub(t)
	bridge := NewBridge(BridgeDeps{Log: slog.Default(), Hub: hub, SM: sm})
	h := &Handler{
		log:    slog.Default(),
		hub:    hub,
		sm:     sm,
		bridge: bridge,
	}
	return h, sm, hub, bridge
}

func TestHandleSupplementOnBusy_Passthrough(t *testing.T) {
	t.Parallel()
	mw := &mockMidTurnWorker{}
	h, _, _, bridge := newBusyTestHandler(t, mw)
	env := newInputEnvelope(t, "s", "追问")
	env.Seq = 5

	require.NoError(t, h.handleSupplementOnBusy(t.Context(), env, "追问", nil))
	require.Equal(t, "追问", mw.injected, "InjectMidTurn must receive the supplement")
	require.Equal(t, 1, mw.injectCount, "InjectMidTurn must be called exactly once")

	// Passthrough must NOT buffer the supplement.
	_, _, ok := bridge.pending.DrainAndMerge("s")
	require.False(t, ok, "passthrough path must not buffer")
}

func TestHandleSupplementOnBusy_AcknowledgesAndDeduplicates(t *testing.T) {
	t.Parallel()
	mw := &mockMidTurnWorker{}
	h, _, hub, _ := newBusyTestHandler(t, mw)
	h.executionStore = &fakeExecutionStore{activeRecord: testExecutionRecord(execution.StatusDelivered)}
	conn := &mockPlatformConn{}
	hub.JoinPlatformSession("s", conn)

	env := newInputEnvelope(t, "s", "追问")
	env.Seq = 5

	require.NoError(t, h.handleSupplementOnBusy(t.Context(), env, "追问", nil))
	require.NoError(t, h.handleSupplementOnBusy(t.Context(), env, "追问", nil))
	require.Equal(t, 1, mw.injectCount, "same client_message_id must not be injected twice")

	require.Eventually(t, func() bool {
		acks := 0
		duplicateSeen := false
		for _, got := range conn.envelopes() {
			if got.Event.Type != events.InputAck {
				continue
			}
			data, ok := got.Event.Data.(events.InputAckData)
			if !ok ||
				data.ClientMessageID != clientMessageID(env) ||
				data.Status != events.ExecutionStatusDelivered ||
				data.InputMode != events.InputModeInjected ||
				data.Durability != events.InputDurabilityVolatile ||
				data.ParentExecutionID != "exec_test" {
				return false
			}
			acks++
			duplicateSeen = duplicateSeen || data.Duplicate
		}
		return acks == 2 && duplicateSeen
	}, time.Second, 10*time.Millisecond)
}

func TestHandleSupplementOnBusy_RejectsSameIDWithDifferentPayload(t *testing.T) {
	t.Parallel()
	mw := &mockMidTurnWorker{}
	h, _, hub, _ := newBusyTestHandler(t, mw)
	conn := &mockPlatformConn{}
	hub.JoinPlatformSession("s", conn)

	first := newInputEnvelope(t, "s", "first")
	require.NoError(t, h.handleSupplementOnBusy(t.Context(), first, "first", nil))
	conflict := events.Clone(first)
	conflict.Event.Data.(map[string]any)["content"] = "different"
	err := h.handleSupplementOnBusy(t.Context(), conflict, "different", nil)
	require.Error(t, err)
	require.Equal(t, 1, mw.injectCount, "payload conflict must not be injected")

	require.Eventually(t, func() bool {
		for _, got := range conn.envelopes() {
			if got.Event.Type != events.Error {
				continue
			}
			data, ok := got.Event.Data.(events.ErrorData)
			return ok && data.Code == events.ErrCodeInvalidMessage
		}
		return false
	}, time.Second, 10*time.Millisecond)
}

func TestHandleSupplementOnBusy_FallbackBuffer(t *testing.T) {
	t.Parallel()
	// mockWorkerForHandler does NOT implement worker.MidTurnInjector, so the
	// handler must fall back to the pending buffer.
	w := new(mockWorkerForHandler)
	h, _, _, bridge := newBusyTestHandler(t, w)
	env := newInputEnvelope(t, "s", "追问")
	env.Seq = 5

	require.NoError(t, h.handleSupplementOnBusy(t.Context(), env, "追问", nil))

	merged, _, ok := bridge.pending.DrainAndMerge("s")
	require.True(t, ok, "fallback path must buffer the supplement")
	require.Equal(t, "追问", merged)
}

func TestMidTurnContract_AllWorkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		workerType worker.WorkerType
		native     bool
		mode       string
	}{
		{name: "W-C", workerType: worker.TypeClaudeCode, native: true, mode: "injected"},
		{name: "W-O", workerType: worker.TypeOpenCodeSrv, native: false, mode: "buffered"},
		{name: "W-X", workerType: worker.TypeCodexCLI, native: true, mode: "injected"},
		{name: "W-A", workerType: worker.TypeACP, native: false, mode: "buffered"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				w       worker.Worker
				nativeW *mockMidTurnWorker
			)
			if tc.native {
				nativeW = &mockMidTurnWorker{}
				w = nativeW
			} else {
				w = new(mockWorkerForHandler)
			}
			h, _, hub, bridge := newBusyTestHandler(t, w)
			h.executionStore = &fakeExecutionStore{activeRecord: testExecutionRecord(execution.StatusDelivered)}
			sessionID := "mid-turn-" + tc.name
			conn := &mockPlatformConn{}
			hub.JoinPlatformSession(sessionID, conn)
			env := newInputEnvelope(t, sessionID, "supplement-"+tc.name)
			env.Event.Data.(map[string]any)["client_message_id"] = "client-" + tc.name

			require.NoError(t, h.handleSupplementOnBusy(t.Context(), env, "supplement-"+tc.name, nil))
			require.Eventually(t, func() bool {
				for _, got := range conn.envelopes() {
					if got.Metadata == nil {
						continue
					}
					if got.Metadata["supplement_mode"] == tc.mode {
						return true
					}
				}
				return false
			}, time.Second, 10*time.Millisecond)

			modeCount := 0
			ackCount := 0
			for _, got := range conn.envelopes() {
				if got.Event.Type == events.InputAck {
					data, ok := got.Event.Data.(events.InputAckData)
					require.True(t, ok)
					require.Equal(t, "exec_test", data.ParentExecutionID)
					require.Equal(t, events.InputDurabilityVolatile, data.Durability)
					if tc.native {
						require.Equal(t, events.InputModeInjected, data.InputMode)
						require.Equal(t, events.ExecutionStatusDelivered, data.Status)
					} else {
						require.Equal(t, events.InputModeBuffered, data.InputMode)
						require.Equal(t, events.ExecutionStatusAccepted, data.Status)
					}
					ackCount++
				}
				if got.Metadata != nil && got.Metadata["supplement_mode"] == tc.mode {
					modeCount++
				}
			}
			require.Equal(t, 1, ackCount)
			require.GreaterOrEqual(t, modeCount, 1, "the platform must observe the contract mode")

			bridge.pending.mu.Lock()
			bufferedCount := pendingCount(bridge.pending.items[sessionID])
			bridge.pending.mu.Unlock()
			if tc.native {
				require.Equal(t, 1, nativeW.injectCount)
				require.Equal(t, "supplement-"+tc.name, nativeW.injected)
				require.Zero(t, bufferedCount, "Native workers must not enter the fallback buffer")
			} else {
				w.(*mockWorkerForHandler).AssertNotCalled(t, "Input", mock.Anything, mock.Anything, mock.Anything)
				require.Equal(t, 1, bufferedCount, "fallback workers buffer the current-turn supplement")
			}
		})
	}
}

type recordingPendingReplayer struct {
	mu        sync.Mutex
	envelopes []*events.Envelope
}

func (r *recordingPendingReplayer) DeliverReplay(_ context.Context, env *events.Envelope) error {
	r.mu.Lock()
	r.envelopes = append(r.envelopes, events.Clone(env))
	r.mu.Unlock()
	return nil
}

func (r *recordingPendingReplayer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.envelopes)
}

func TestMidTurnFallback_ReplaysOnceAfterDuplicateTerminal(t *testing.T) {
	t.Parallel()

	w := new(mockWorkerForHandler)
	h, _, _, bridge := newBusyTestHandler(t, w)
	replayer := &recordingPendingReplayer{}
	bridge.SetPendingReplayer(replayer)
	sessionID := "mid-turn-replay"
	env := newInputEnvelope(t, sessionID, "replay-once")
	env.Event.Data.(map[string]any)["client_message_id"] = "replay-client"

	require.NoError(t, h.handleSupplementOnBusy(t.Context(), env, "replay-once", nil))
	bridge.replayPending(sessionID)
	// A duplicate terminal notification must not drain or deliver the same
	// fallback supplement a second time.
	bridge.replayPending(sessionID)
	bridge.WaitPendingReplays(t.Context())

	require.Equal(t, 1, replayer.count())
	bridge.pending.mu.Lock()
	bufferedCount := pendingCount(bridge.pending.items[sessionID])
	bridge.pending.mu.Unlock()
	require.Zero(t, bufferedCount)
	w.AssertNotCalled(t, "Input", mock.Anything, mock.Anything, mock.Anything)
}

func TestHandleSupplementOnBusy_FullBufferReturnsFailureWithoutAck(t *testing.T) {
	t.Parallel()
	w := new(mockWorkerForHandler)
	h, _, hub, bridge := newBusyTestHandler(t, w)
	conn := &mockPlatformConn{}
	hub.JoinPlatformSession("s", conn)
	for i := 0; i < maxPendingPerSession; i++ {
		env := newInputEnvelope(t, "s", fmt.Sprintf("pending-%d", i))
		require.True(t, bridge.pending.Append("s", fmt.Sprintf("pending-%d", i), env))
	}

	overflow := newInputEnvelope(t, "s", "overflow")
	overflow.Event.Data.(map[string]any)["client_message_id"] = "overflow-id"
	err := h.handleSupplementOnBusy(t.Context(), overflow, "overflow", nil)
	require.Error(t, err)

	require.Eventually(t, func() bool {
		errorSeen := false
		for _, got := range conn.envelopes() {
			switch got.Event.Type {
			case events.InputAck:
				data, ok := got.Event.Data.(events.InputAckData)
				if ok && data.ClientMessageID == "overflow-id" {
					return false
				}
			case events.Error:
				errorSeen = true
			}
		}
		return errorSeen
	}, time.Second, 10*time.Millisecond)
}

func TestHandleSupplementOnBusy_InjectFailureFallsBack(t *testing.T) {
	t.Parallel()
	mw := &mockMidTurnWorker{injectErr: errors.New("turn ended")}
	h, _, _, bridge := newBusyTestHandler(t, mw)
	env := newInputEnvelope(t, "s", "追问")
	env.Seq = 5

	require.NoError(t, h.handleSupplementOnBusy(t.Context(), env, "追问", nil))
	require.Equal(t, 1, mw.injectCount, "InjectMidTurn must be attempted")

	merged, _, ok := bridge.pending.DrainAndMerge("s")
	require.True(t, ok, "failed inject must fall back to buffer")
	require.Equal(t, "追问", merged)
}

// TestHandleSupplementOnBusy_RaceGateStillHeld is the complement: the active
// gate is still held (Done did NOT win the race), so the re-check passes and
// InjectMidTurn proceeds normally.
func TestHandleSupplementOnBusy_RaceGateStillHeld(t *testing.T) {
	t.Parallel()
	mw := &mockMidTurnWorker{}
	h, _, _, bridge := newBusyTestHandler(t, mw)
	h.executionStore = &fakeExecutionStore{activeRecord: testExecutionRecord(execution.StatusDelivered)}

	env := newInputEnvelope(t, "s", "追问")
	env.Seq = 5

	require.NoError(t, h.handleSupplementOnBusy(t.Context(), env, "追问", nil))
	require.Equal(t, 1, mw.injectCount, "gate held → InjectMidTurn must run")
	require.Equal(t, "追问", mw.injected)

	_, _, ok := bridge.pending.DrainAndMerge("s")
	require.False(t, ok, "successful inject must not buffer")
}

// TestHandleSupplementOnBusy_GateReleasedBeforeInject covers the TOCTOU race
// where the running turn's Done arrives between the SESSION_BUSY detection
// (acceptInputExecutionWithRetry) and the mid-turn inject. CC headless stays
// alive reading stdin after Done, so injecting then would start a ghost turn
// with no execution record. The ActiveBySession re-check must route the
// supplement to the normal delivery path (a proper new turn) instead. Here
// sm.Get fails, so delegation is observed as an error — the key assertion is
// that InjectMidTurn is never called once the gate is released.
func TestHandleSupplementOnBusy_GateReleasedBeforeInject(t *testing.T) {
	t.Parallel()
	mw := &mockMidTurnWorker{}
	h, sm, _, _ := newBusyTestHandler(t, mw)
	// ActiveBySession reports the gate as released (ErrNotFound): Done won the
	// race between SESSION_BUSY detection and the inject.
	h.executionStore = &fakeExecutionStore{}
	// deliverToWorker's first step is sm.Get; fail it so delegation is observed
	// as an error without standing up a full delivery pipeline.
	sm.On("Get", "s").Return(nil, errors.New("session gone")).Maybe()

	env := newInputEnvelope(t, "s", "追问")
	env.Seq = 5

	err := h.handleSupplementOnBusy(t.Context(), env, "追问", nil)
	require.Error(t, err, "released gate must route to deliverToWorker")
	require.Equal(t, 0, mw.injectCount, "must NOT inject once the active gate is released")
}

func TestHandleSupplementOnBusy_GateReleasedDuringInject(t *testing.T) {
	t.Parallel()
	mw := &mockMidTurnWorker{injectErr: errors.New("turn ended")}
	h, sm, _, bridge := newBusyTestHandler(t, mw)
	h.executionStore = &fakeExecutionStore{activeResults: []*execution.Record{
		testExecutionRecord(execution.StatusDelivered),
		nil,
	}}
	sm.On("Get", "s").Return(nil, errors.New("session gone")).Maybe()

	env := newInputEnvelope(t, "s", "追问")
	env.Seq = 5

	err := h.handleSupplementOnBusy(t.Context(), env, "追问", nil)
	require.Error(t, err, "gate released during inject must route through normal delivery")
	require.Equal(t, 1, mw.injectCount)
	_, _, ok := bridge.pending.DrainAndMerge("s")
	require.False(t, ok, "released gate must not leave the supplement buffered")
}

func TestDeliverReplay_BusyReturnsForOuterRequeue(t *testing.T) {
	t.Parallel()
	w := new(mockWorkerForHandler)
	h, sm, _, bridge := newBusyTestHandler(t, w)
	h.executionStore = &fakeExecutionStore{acceptErr: execution.ErrSessionBusy}
	sm.On("Get", "s").Return(&session.SessionInfo{ID: "s", State: events.StateRunning}, nil).Maybe()

	env := newInputEnvelope(t, "s", "replay")
	err := h.DeliverReplay(t.Context(), env)
	require.ErrorIs(t, err, execution.ErrSessionBusy)
	_, _, ok := bridge.pending.DrainAndMerge("s")
	require.False(t, ok, "DeliverReplay must not recursively enter supplement buffering")
}
