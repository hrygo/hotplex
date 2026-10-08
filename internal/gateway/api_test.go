package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/security"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

// ─── Mock SessionManager for API tests ─────────────────────────────────────────

type mockAPISM struct {
	mock.Mock
}

func (m *mockAPISM) CreateWithBot(ctx context.Context, id, userID, botID, _ string, wt worker.WorkerType, allowedTools []string, platform string, platformKey map[string]string, workspaceID, workDir, title, clientKey string) (*session.SessionInfo, error) {
	args := m.Called(ctx, id, userID, botID, wt, allowedTools, platform, platformKey, workspaceID, workDir, title, clientKey)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*session.SessionInfo), args.Error(1)
}

func (m *mockAPISM) AttachWorker(_ context.Context, id string, w worker.Worker) error {
	return m.Called(id, w).Error(0)
}

func (m *mockAPISM) DetachWorker(id string) { m.Called(id) }

func (m *mockAPISM) DetachWorkerIf(id string, expected worker.Worker) bool {
	return m.Called(id, expected).Bool(0)
}

func (m *mockAPISM) Transition(ctx context.Context, id string, to events.SessionState) error {
	return m.Called(ctx, id, to).Error(0)
}

func (m *mockAPISM) Get(_ context.Context, id string) (*session.SessionInfo, error) {
	args := m.Called(id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*session.SessionInfo), args.Error(1)
}

func (m *mockAPISM) GetPurgeStatus(ctx context.Context, sessionID, userID string) (*session.PurgeStatus, error) {
	args := m.Called(ctx, sessionID, userID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*session.PurgeStatus), args.Error(1)
}

func (m *mockAPISM) GetWorker(id string) worker.Worker {
	args := m.Called(id)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(worker.Worker)
}

func (m *mockAPISM) Delete(ctx context.Context, id string) error {
	return m.Called(ctx, id).Error(0)
}

func (m *mockAPISM) DeletePhysical(ctx context.Context, id string) error {
	return m.Called(ctx, id).Error(0)
}

func (m *mockAPISM) List(ctx context.Context, userID, platform, workspaceID string, limit, offset int) ([]*session.SessionInfo, error) {
	args := m.Called(ctx, userID, platform, workspaceID, limit, offset)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*session.SessionInfo), args.Error(1)
}

func (m *mockAPISM) UpdateWorkerSessionID(ctx context.Context, id, workerSessionID string) error {
	return m.Called(ctx, id, workerSessionID).Error(0)
}

func (m *mockAPISM) EnsureWorkerSessionID(ctx context.Context, id, workerSessionID string) error {
	return m.Called(ctx, id, workerSessionID).Error(0)
}

func (m *mockAPISM) SetPermissionCeilingIfEmpty(ctx context.Context, id, ceiling string) (string, error) {
	args := m.Called(ctx, id, ceiling)
	return args.String(0), args.Error(1)
}

func (m *mockAPISM) ResetExpiry(ctx context.Context, id string) error {
	return m.Called(ctx, id).Error(0)
}

func (m *mockAPISM) UpdateWorkDir(ctx context.Context, id, workDir string) error {
	return m.Called(ctx, id, workDir).Error(0)
}

func (m *mockAPISM) TransitionWithInput(ctx context.Context, id string, to events.SessionState, content string, metadata map[string]any) error {
	return m.Called(ctx, id, to, content, metadata).Error(0)
}

func (m *mockAPISM) TransitionWithReason(ctx context.Context, id string, to events.SessionState, termReason string) error {
	return m.Called(ctx, id, to, termReason).Error(0)
}

func (m *mockAPISM) ValidateOwnership(ctx context.Context, sessionID, userID, adminUserID string) error {
	return m.Called(ctx, sessionID, userID, adminUserID).Error(0)
}

var _ apiSM = (*mockAPISM)(nil)

// ─── Mock SessionStarter for API tests ─────────────────────────────────────────

type mockAPIBridge struct {
	mock.Mock
}

func (m *mockAPIBridge) StartSession(ctx context.Context, p worker.SessionStartParams) error {
	return m.Called(ctx, p).Error(0)
}

func (m *mockAPIBridge) ResumeSession(ctx context.Context, id string, workDir string) error {
	return m.Called(ctx, id, workDir).Error(0)
}

func (m *mockAPIBridge) SwitchWorkDir(ctx context.Context, oldSessionID, newWorkDir string) (*SwitchWorkDirResult, error) {
	args := m.Called(ctx, oldSessionID, newWorkDir)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*SwitchWorkDirResult), args.Error(1)
}

func (m *mockAPIBridge) GetWorkspaceByID(ctx context.Context, id string) (*session.Workspace, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*session.Workspace), args.Error(1)
}

// ─── Mock TurnsReader for API tests ────────────────────────────────

type mockTurnsStore struct {
	mock.Mock
}

func (m *mockTurnsStore) QueryTurns(ctx context.Context, sessionID string, limit, offset int) ([]*eventstore.TurnRecord, error) {
	args := m.Called(ctx, sessionID, limit, offset)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*eventstore.TurnRecord), args.Error(1)
}

func (m *mockTurnsStore) QueryTurnsBefore(ctx context.Context, sessionID string, beforeID int64, limit int) ([]*eventstore.TurnRecord, error) {
	args := m.Called(ctx, sessionID, beforeID, limit)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*eventstore.TurnRecord), args.Error(1)
}

func (m *mockTurnsStore) QueryLatestTurns(ctx context.Context, sessionID string, limit int) ([]*eventstore.TurnRecord, error) {
	args := m.Called(ctx, sessionID, limit)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*eventstore.TurnRecord), args.Error(1)
}

func (m *mockTurnsStore) QueryTurnStats(ctx context.Context, sessionID string) (*eventstore.TurnStats, error) {
	args := m.Called(ctx, sessionID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*eventstore.TurnStats), args.Error(1)
}

func (m *mockTurnsStore) LatestGeneration(ctx context.Context, sessionID string) (int64, error) {
	args := m.Called(ctx, sessionID)
	return args.Get(0).(int64), args.Error(1)
}

func (m *mockTurnsStore) LatestTurnNum(ctx context.Context, sessionID string, generation int64) (int, error) {
	args := m.Called(ctx, sessionID, generation)
	return args.Get(0).(int), args.Error(1)
}

func (m *mockTurnsStore) DeleteExpiredTurns(ctx context.Context, cutoff time.Time) (int64, error) {
	args := m.Called(ctx, cutoff)
	return args.Get(0).(int64), args.Error(1)
}

func (m *mockTurnsStore) LatestSeq(ctx context.Context, sessionID string) (int64, error) {
	args := m.Called(ctx, sessionID)
	return args.Get(0).(int64), args.Error(1)
}

type mockAPIEventStore struct {
	mock.Mock
}

func (m *mockAPIEventStore) QueryBySession(ctx context.Context, sessionID string, cursor int64, dir eventstore.CursorDirection, limit int) (*eventstore.EventPage, error) {
	args := m.Called(ctx, sessionID, cursor, dir, limit)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*eventstore.EventPage), args.Error(1)
}

// ─── Test helpers ───────────────────────────────────────────────────────────────

func newTestAuth(t *testing.T) *security.Authenticator {
	t.Helper()
	return security.NewAuthenticator(&config.SecurityConfig{})
}

func newTestAPI(t *testing.T, sm *mockAPISM, bridge *mockAPIBridge) *GatewayAPI {
	t.Helper()
	return NewGatewayAPI(slog.Default(), newTestAuth(t), sm, bridge, config.NewConfigStore(&config.Config{}, nil), nil, nil, nil)
}

func newTestAPIWithTurns(t *testing.T, sm *mockAPISM, bridge *mockAPIBridge, turnsStore *mockTurnsStore) *GatewayAPI {
	t.Helper()
	return NewGatewayAPI(slog.Default(), newTestAuth(t), sm, bridge, config.NewConfigStore(&config.Config{}, nil), turnsStore, nil, nil)
}

// mockAPIWorkspace mocks WorkspaceReader for CreateSession (workspace ownership) tests.
type mockAPIWorkspace struct {
	mock.Mock
}

func (m *mockAPIWorkspace) GetWorkspaceByID(ctx context.Context, id string) (*session.Workspace, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*session.Workspace), args.Error(1)
}

var _ WorkspaceReader = (*mockAPIWorkspace)(nil)

// newTestAPIWithWorkspace wires a mock WorkspaceReader so CreateSession can run
// its workspace ownership check.
func newTestAPIWithWorkspace(t *testing.T, sm *mockAPISM, bridge *mockAPIBridge, ws *mockAPIWorkspace) *GatewayAPI {
	t.Helper()
	return NewGatewayAPI(slog.Default(), newTestAuth(t), sm, bridge, config.NewConfigStore(&config.Config{}, nil), nil, nil, ws)
}

// ownedWorkspaceMock returns a WorkspaceReader mock whose GetWorkspaceByID always
// yields a workspace owned by the given user (default "anonymous" in dev mode).
func ownedWorkspaceMock(owner, workDir string) *mockAPIWorkspace {
	ws := new(mockAPIWorkspace)
	ws.On("GetWorkspaceByID", mock.Anything, mock.Anything).Return(&session.Workspace{
		ID: "ws-test", OwnerUserID: owner, WorkDir: workDir, Status: "active",
	}, nil)
	return ws
}

func authedReq(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("X-API-Key", "test-key")
	return r
}

// setupMux creates a ServeMux with API routes for tests that need r.PathValue.
func setupMux(api *GatewayAPI) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", api.ListSessions)
	mux.HandleFunc("POST /api/sessions", api.CreateSession)
	mux.HandleFunc("GET /api/sessions/{id}", api.GetSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", api.DeleteSession)
	mux.HandleFunc("GET /api/sessions/{id}/cleanup", api.GetSessionCleanupStatus)
	mux.HandleFunc("POST /api/sessions/{id}/cd", api.SwitchWorkDir)
	mux.HandleFunc("GET /api/sessions/{id}/history", api.GetHistory)
	mux.HandleFunc("GET /api/sessions/{id}/events", api.GetEvents)
	mux.HandleFunc("GET /api/workers", api.ListWorkers)
	return mux
}

// ─── CreateSession tests ────────────────────────────────────────────────────────

func TestCreateSession_WithClientSessionID(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPIWithWorkspace(t, sm, bridge, ownedWorkspaceMock("anonymous", "/tmp/hotplex/proj"))

	// Get returns not found → no idempotency path
	sm.On("Get", mock.Anything).Return(nil, session.ErrSessionNotFound)
	bridge.On("StartSession", mock.Anything, mock.Anything).Return(nil)

	body := strings.NewReader(`{"workspace_id":"ws-test","client_session_id":"client-1","title":"my-title"}`)
	w := httptest.NewRecorder()
	api.CreateSession(w, authedReq("POST", "/api/sessions", body))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.NotEmpty(t, resp["session_id"])
	bridge.AssertExpectations(t)
}

func TestCreateSession_MissingClientSessionID(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	w := httptest.NewRecorder()
	api.CreateSession(w, authedReq("POST", "/api/sessions", nil))

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "client_session_id is required")
}

// 方案 1（issue #824）：workspace_id 可选。不传时走 legacy 路径（v1.29.0 契约），
// 必须返回 200，不得回退到 400 "workspace_id is required"。
func TestCreateSession_LegacyNoWorkspace(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	sm.On("Get", mock.Anything).Return(nil, session.ErrSessionNotFound)
	bridge.On("StartSession", mock.Anything, mock.Anything).Return(nil)

	w := httptest.NewRecorder()
	api.CreateSession(w, authedReq("POST", "/api/sessions?client_session_id=c1", nil))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "session_id")
	bridge.AssertExpectations(t)
}

func TestCreateSession_WorkspaceForbidden(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	ws := new(mockAPIWorkspace)
	ws.On("GetWorkspaceByID", mock.Anything, "ws-other").Return(&session.Workspace{
		ID: "ws-other", OwnerUserID: "someone-else", WorkDir: "/tmp/p", Status: "active",
	}, nil)
	api := newTestAPIWithWorkspace(t, sm, bridge, ws)

	w := httptest.NewRecorder()
	api.CreateSession(w, authedReq("POST", "/api/sessions?workspace_id=ws-other&client_session_id=c1", nil))

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "WORKSPACE_FORBIDDEN")
}

func TestCreateSession_ClientSessionIDOnlyNoTitle(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPIWithWorkspace(t, sm, bridge, ownedWorkspaceMock("anonymous", "/tmp/hotplex/proj"))

	// title is optional, client_session_id alone should work
	sm.On("Get", mock.Anything).Return(nil, session.ErrSessionNotFound)
	bridge.On("StartSession", mock.Anything, mock.Anything).Return(nil)

	w := httptest.NewRecorder()
	api.CreateSession(w, authedReq("POST", "/api/sessions?workspace_id=ws-test&client_session_id=csid-only", nil))

	require.Equal(t, http.StatusOK, w.Code)
	bridge.AssertExpectations(t)
}

func TestCreateSession_IdempotentActiveSession(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPIWithWorkspace(t, sm, bridge, ownedWorkspaceMock("anonymous", "/tmp/hotplex/proj"))

	active := &session.SessionInfo{ID: "existing-id", State: events.StateRunning}
	sm.On("Get", mock.Anything).Return(active, nil)
	// bridge.StartSession should NOT be called

	w := httptest.NewRecorder()
	api.CreateSession(w, authedReq("POST", "/api/sessions?workspace_id=ws-test&client_session_id=test&title=test", nil))

	require.Equal(t, http.StatusOK, w.Code)
	bridge.AssertNotCalled(t, "StartSession", mock.Anything)
}

func TestCreateSession_DeletedSessionCannotBeReused(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPIWithWorkspace(t, sm, bridge, ownedWorkspaceMock("anonymous", "/tmp/hotplex/proj"))

	deleted := &session.SessionInfo{ID: "deleted-id", State: events.StateDeleted}
	sm.On("Get", mock.Anything).Return(deleted, nil)

	w := httptest.NewRecorder()
	api.CreateSession(w, authedReq("POST", "/api/sessions?workspace_id=ws-test&client_session_id=test-csid&title=test", nil))

	require.Equal(t, http.StatusGone, w.Code)
	sm.AssertNotCalled(t, "DeletePhysical", mock.Anything, mock.Anything)
	bridge.AssertNotCalled(t, "StartSession", mock.Anything, mock.Anything)
}

func TestCreateSession_BridgeError(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPIWithWorkspace(t, sm, bridge, ownedWorkspaceMock("anonymous", "/tmp/hotplex/proj"))

	sm.On("Get", mock.Anything).Return(nil, session.ErrSessionNotFound)
	bridge.On("StartSession", mock.Anything, mock.Anything).Return(errTestBridge)

	w := httptest.NewRecorder()
	api.CreateSession(w, authedReq("POST", "/api/sessions?workspace_id=ws-test&client_session_id=fail&title=fail", nil))

	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "failed to create session")
}

func TestCreateSession_WorkerTypeValidation(t *testing.T) {
	t.Parallel()

	// worker_type is validated at the boundary whether supplied via query param
	// (?worker_type=) or JSON body ({"worker_type":...}). Both parse paths
	// (api.go:267-270) are independent and must reject unknown types with 400
	// INVALID_WORKER_TYPE before reaching DeriveSessionKey / bridge.StartSession
	// (so mocks are intentionally unset for invalid cases). See spec ③ §7.2/§10.
	//
	// testNoopType ("noop_gateway_test") is the registered test worker — the 4
	// real constants are validated at the worker-package boundary (TestValidateType).
	newAPI := func(t *testing.T) (*GatewayAPI, *mockAPISM, *mockAPIBridge) {
		sm := new(mockAPISM)
		bridge := new(mockAPIBridge)
		return newTestAPIWithWorkspace(t, sm, bridge, ownedWorkspaceMock("anonymous", "/tmp/hotplex/proj")), sm, bridge
	}

	invalid := []struct {
		name string
		url  string
		body string
	}{
		{"query bogus", "/api/sessions?workspace_id=ws-test&client_session_id=cq1&worker_type=bogus", ""},
		{"query TypeUnknown", "/api/sessions?workspace_id=ws-test&client_session_id=cq2&worker_type=unknown", ""},
		{"query case sensitive", "/api/sessions?workspace_id=ws-test&client_session_id=cq3&worker_type=Claude_Code", ""},
		{"body bogus", "/api/sessions?workspace_id=ws-test&client_session_id=cb1", `{"worker_type":"bogus"}`},
		{"body TypeUnknown", "/api/sessions?workspace_id=ws-test&client_session_id=cb2", `{"worker_type":"unknown"}`},
		{"body case sensitive", "/api/sessions?workspace_id=ws-test&client_session_id=cb3", `{"worker_type":"Claude_Code"}`},
	}
	for _, tt := range invalid {
		t.Run("invalid/"+tt.name, func(t *testing.T) {
			t.Parallel()
			api, _, _ := newAPI(t)
			w := httptest.NewRecorder()
			var body io.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			}
			api.CreateSession(w, authedReq("POST", tt.url, body))
			require.Equal(t, http.StatusBadRequest, w.Code, "resp=%s", w.Body.String())
			require.Contains(t, w.Body.String(), "INVALID_WORKER_TYPE")
		})
	}

	valid := []struct {
		name string
		url  string
		body string
	}{
		{"query", "/api/sessions?workspace_id=ws-test&client_session_id=cvq&worker_type=" + string(testNoopType), ""},
		{"body", "/api/sessions?workspace_id=ws-test&client_session_id=cvb", `{"worker_type":"` + string(testNoopType) + `"}`},
	}
	for _, tt := range valid {
		t.Run("valid/"+tt.name, func(t *testing.T) {
			t.Parallel()
			api, sm, bridge := newAPI(t)
			sm.On("Get", mock.Anything).Return(nil, session.ErrSessionNotFound)
			bridge.On("StartSession", mock.Anything, mock.Anything).Return(nil)
			w := httptest.NewRecorder()
			var body io.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			}
			api.CreateSession(w, authedReq("POST", tt.url, body))
			require.Equal(t, http.StatusOK, w.Code, "resp=%s", w.Body.String())
		})
	}
}

func TestCreateSession_StaleWorkerPreferenceDegradesToDefault(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	// Workspace carries a stale, unvalidated worker_preference (written before
	// the PATCH gate existed, or via a bypass write). It must NOT yield a 400
	// (the caller didn't supply worker_type) and must NOT reach worker launch
	// with the bad value — degrade to the default instead. See review P2.
	ws := new(mockAPIWorkspace)
	ws.On("GetWorkspaceByID", mock.Anything, mock.Anything).Return(&session.Workspace{
		ID: "ws-test", OwnerUserID: "anonymous", WorkDir: "/tmp/hotplex/proj",
		WorkerPreference: "bogus_stale", Status: "active",
	}, nil)
	api := newTestAPIWithWorkspace(t, sm, bridge, ws)

	sm.On("Get", mock.Anything).Return(nil, session.ErrSessionNotFound)
	bridge.On("StartSession", mock.Anything, mock.MatchedBy(func(p worker.SessionStartParams) bool {
		return p.WorkerType == worker.TypeClaudeCode
	})).Return(nil)

	w := httptest.NewRecorder()
	api.CreateSession(w, authedReq("POST", "/api/sessions?workspace_id=ws-test&client_session_id=stale", nil))

	require.Equal(t, http.StatusOK, w.Code, "resp=%s", w.Body.String())
	bridge.AssertExpectations(t)
}

var errTestBridge = fmt.Errorf("test bridge error")

// TestCreateSession_WorkDirFromWorkspace: work_dir comes from the workspace
// (immutable, spec §6.2), never from the request query.
func TestCreateSession_WorkDirFromWorkspace(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPIWithWorkspace(t, sm, bridge, ownedWorkspaceMock("anonymous", "/tmp/hotplex/ws-dir"))

	sm.On("Get", mock.Anything).Return(nil, session.ErrSessionNotFound)
	// Capture StartSession params to verify WorkDir == workspace's, not the query's.
	bridge.On("StartSession", mock.Anything, mock.MatchedBy(func(p worker.SessionStartParams) bool {
		return p.WorkDir == "/tmp/hotplex/ws-dir" && p.WorkspaceID == "ws-test"
	})).Return(nil)

	// work_dir query must be ignored.
	w := httptest.NewRecorder()
	api.CreateSession(w, authedReq("POST", "/api/sessions?workspace_id=ws-test&client_session_id=wd&work_dir=/tmp/IGNORED", nil))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	bridge.AssertExpectations(t)
}

// ─── DeleteSession tests ────────────────────────────────────────────────────────

func TestDeleteSession_UsesLifecycleDelete(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	sm.On("Get", "sess-1").Return(&session.SessionInfo{ID: "sess-1", UserID: "anonymous"}, nil)
	sm.On("Delete", mock.Anything, "sess-1").Return(nil)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedReq("DELETE", "/api/sessions/sess-1", nil))

	require.Equal(t, http.StatusNoContent, w.Code)
	sm.AssertCalled(t, "Delete", mock.Anything, "sess-1")
	sm.AssertNotCalled(t, "DeletePhysical", mock.Anything, "sess-1")
}

func TestDeleteSession_DeleteFailureReturnsError(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	sm.On("Get", "sess-2").Return(&session.SessionInfo{ID: "sess-2", UserID: "anonymous"}, nil)
	sm.On("Delete", mock.Anything, "sess-2").Return(errTestBridge)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedReq("DELETE", "/api/sessions/sess-2", nil))

	require.Equal(t, http.StatusInternalServerError, w.Code)
	sm.AssertCalled(t, "Delete", mock.Anything, "sess-2")
}

func TestDeleteSession_MissingID(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedReq("DELETE", "/api/sessions/", nil))

	// No {id} match → 404 from mux (no path value)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetSessionCleanupStatusUsesOwnershipSnapshot(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	status := &session.PurgeStatus{
		JobID: "purge-1", SessionID: "sess-deleted",
		Status: session.PurgeJobInProgress, WorkspaceID: "ws-test",
		Items: []session.PurgeItemStatus{{
			Kind: session.PurgeItemConversationContent, Status: session.PurgeItemPending,
		}},
	}
	sm.On("GetPurgeStatus", mock.Anything, "sess-deleted", "anonymous").Return(status, nil).Once()
	api := newTestAPIWithWorkspace(t, sm, bridge, ownedWorkspaceMock("anonymous", "/tmp/project"))
	w := httptest.NewRecorder()
	setupMux(api).ServeHTTP(w, authedReq("GET", "/api/sessions/sess-deleted/cleanup", nil))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&body))
	require.Equal(t, "purge-1", body["job_id"])
	require.Equal(t, "in_progress", body["status"])
	require.NotContains(t, body, "workspace_id",
		"the persisted authorization snapshot is not part of the public response")
	sm.AssertExpectations(t)
}

func TestGetSessionCleanupStatusHidesOtherOwners(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	sm.On("GetPurgeStatus", mock.Anything, "sess-private", "anonymous").
		Return(nil, session.ErrPurgeNotFound).Once()
	api := newTestAPI(t, sm, bridge)
	w := httptest.NewRecorder()
	setupMux(api).ServeHTTP(w, authedReq("GET", "/api/sessions/sess-private/cleanup", nil))

	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	sm.AssertExpectations(t)
}

// ─── ListSessions tests ─────────────────────────────────────────────────────────

func TestListSessions(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	now := time.Now()
	sessions := []*session.SessionInfo{
		{ID: "s1", State: events.StateRunning, CreatedAt: now},
		{ID: "s2", State: events.StateIdle, CreatedAt: now},
	}
	sm.On("List", mock.Anything, "anonymous", "webchat", mock.Anything, 100, 0).Return(sessions, nil)

	w := httptest.NewRecorder()
	api.ListSessions(w, authedReq("GET", "/api/sessions", nil))

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	list := resp["sessions"].([]any)
	require.Len(t, list, 2)
}

func TestListSessions_Unauthorized(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	w := httptest.NewRecorder()
	api.ListSessions(w, httptest.NewRequest("GET", "/api/sessions", nil))
	// No X-API-Key header → unauthorized

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// ─── GetSession tests ───────────────────────────────────────────────────────────

func TestGetSession(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	si := &session.SessionInfo{ID: "sess-x", State: events.StateRunning, Title: "my session", UserID: "anonymous"}
	sm.On("Get", "sess-x").Return(si, nil)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedReq("GET", "/api/sessions/sess-x", nil))

	require.Equal(t, http.StatusOK, w.Code)
	var got session.SessionInfo
	require.NoError(t, json.NewDecoder(w.Body).Decode(&got))
	require.Equal(t, "sess-x", got.ID)
	require.Equal(t, "my session", got.Title)
}

func TestGetSession_NotFound(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	sm.On("Get", "no-such").Return(nil, session.ErrSessionNotFound)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedReq("GET", "/api/sessions/no-such", nil))

	require.Equal(t, http.StatusNotFound, w.Code)
}

// ─── SwitchWorkDir tests ────────────────────────────────────────────────────────

func TestSwitchWorkDir_Success(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	si := &session.SessionInfo{ID: "sess-cd", State: events.StateRunning, UserID: "anonymous"}
	sm.On("Get", "sess-cd").Return(si, nil)
	bridge.On("SwitchWorkDir", mock.Anything, "sess-cd", mock.MatchedBy(func(p string) bool {
		return strings.HasSuffix(p, "tmp")
	})).Return(&SwitchWorkDirResult{OldSessionID: "sess-cd", NewSessionID: "sess-new", WorkDir: "/tmp"}, nil)

	mux := setupMux(api)
	body := strings.NewReader(`{"work_dir":"/tmp"}`)
	w := httptest.NewRecorder()
	req := authedReq("POST", "/api/sessions/sess-cd/cd", body)
	mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.Equal(t, "sess-new", resp["new_session_id"])
}

func TestSwitchWorkDir_EmptyBody(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	req := authedReq("POST", "/api/sessions/sess-cd/cd", strings.NewReader(`{}`))
	mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "work_dir is required")
}

func TestSwitchWorkDir_SessionNotFound(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	sm.On("Get", "no-sess").Return(nil, session.ErrSessionNotFound)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	req := authedReq("POST", "/api/sessions/no-sess/cd", strings.NewReader(`{"work_dir":"/tmp"}`))
	mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestSwitchWorkDir_WorkspaceBoundRejected 验证工作区绑定的 WebChat session 不能
// 通过 /cd 切换 work_dir——work_dir 来自工作区且不可变（spec §6.2）。切换会破坏
// "work_dir 来自 workspace" 不变量，并可能让另一工作区在 worker 仍在跑时被删
// （review fix）。
func TestSwitchWorkDir_WorkspaceBoundRejected(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	// WorkspaceID 非空 = 工作区绑定的 WebChat session。
	si := &session.SessionInfo{ID: "sess-ws", State: events.StateRunning, UserID: "anonymous", WorkspaceID: "ws-1"}
	sm.On("Get", "sess-ws").Return(si, nil)
	// bridge.SwitchWorkDir 必须不被调用。
	bridge.AssertNotCalled(t, "SwitchWorkDir")

	mux := setupMux(api)
	w := httptest.NewRecorder()
	req := authedReq("POST", "/api/sessions/sess-ws/cd", strings.NewReader(`{"work_dir":"/tmp"}`))
	mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "WORK_DIR_IMMUTABLE")
}

// ─── GetHistory tests ───────────────────────────────────────────────────────────

func TestGetHistory_Success(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	ts := new(mockTurnsStore)
	api := newTestAPIWithTurns(t, sm, bridge, ts)

	sm.On("Get", "sess-1").Return(&session.SessionInfo{ID: "sess-1", UserID: "anonymous"}, nil)
	records := []*eventstore.TurnRecord{
		{SessionID: "sess-1", Seq: 1, Role: "user", Content: "hello"},
	}
	ts.On("QueryLatestTurns", mock.Anything, "sess-1", 51).Return(records, nil)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	r := authedReq("GET", "/api/sessions/sess-1/history?limit=50", nil)
	mux.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Records []*eventstore.TurnRecord `json:"records"`
		HasMore bool                     `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Records, 1)
	require.False(t, resp.HasMore)
}

func TestGetHistory_DeletedSessionIsHidden(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	ts := new(mockTurnsStore)
	api := newTestAPIWithTurns(t, sm, bridge, ts)

	sm.On("Get", "sess-deleted").Return(&session.SessionInfo{
		ID: "sess-deleted", UserID: "anonymous", State: events.StateDeleted,
	}, nil)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedReq("GET", "/api/sessions/sess-deleted/history", nil))

	require.Equal(t, http.StatusNotFound, w.Code)
	ts.AssertNotCalled(t, "QueryLatestTurns", mock.Anything, mock.Anything, mock.Anything)
}

func TestGetHistory_ExpiredSessionStillUsesPerRecordExpiry(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	ts := new(mockTurnsStore)
	api := newTestAPIWithTurns(t, sm, bridge, ts)

	expiredAt := time.Now().Add(-time.Second)
	sm.On("Get", "sess-1").Return(&session.SessionInfo{
		ID: "sess-1", UserID: "anonymous", HistoryExpiresAt: &expiredAt,
	}, nil)
	ts.On("QueryLatestTurns", mock.Anything, "sess-1", 51).Return(nil, eventstore.ErrNotFound)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedReq("GET", "/api/sessions/sess-1/history", nil))

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Records []any `json:"records"`
		HasMore bool  `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Empty(t, resp.Records)
	require.False(t, resp.HasMore)
	ts.AssertExpectations(t)
}

func TestGetHistory_ReturnsClientMessageID(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	ts := new(mockTurnsStore)
	api := newTestAPIWithTurns(t, sm, bridge, ts)

	sm.On("Get", "sess-1").Return(&session.SessionInfo{ID: "sess-1", UserID: "anonymous"}, nil)
	records := []*eventstore.TurnRecord{
		{SessionID: "sess-1", Seq: 1, Role: "user", Content: "hello", ClientMessageID: "cm-history"},
	}
	ts.On("QueryLatestTurns", mock.Anything, "sess-1", 51).Return(records, nil)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedReq("GET", "/api/sessions/sess-1/history", nil))

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Records []map[string]any `json:"records"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Records, 1)
	require.Equal(t, "cm-history", resp.Records[0]["client_message_id"])
}

func TestGetHistory_HasMore(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	ts := new(mockTurnsStore)
	api := newTestAPIWithTurns(t, sm, bridge, ts)

	sm.On("Get", "sess-1").Return(&session.SessionInfo{ID: "sess-1", UserID: "anonymous"}, nil)
	// Store returns ASC (oldest first, newest last). With limit=2 and 3 records,
	// the API must keep the NEWEST 2 (Seq 2, 3) and report has_more=true.
	// Regression guard: previously the code sliced `records[:limit]`, which
	// returned the OLDEST 2 (Seq 1, 2) and dropped the latest exchange on refresh.
	records := []*eventstore.TurnRecord{
		{ID: 1, Seq: 1, Content: "oldest"},
		{ID: 2, Seq: 2, Content: "middle"},
		{ID: 3, Seq: 3, Content: "newest"},
	}
	ts.On("QueryLatestTurns", mock.Anything, "sess-1", 3).Return(records, nil)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	r := authedReq("GET", "/api/sessions/sess-1/history?limit=2", nil)
	mux.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Records []*eventstore.TurnRecord `json:"records"`
		HasMore bool                     `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Records, 2)
	require.True(t, resp.HasMore)
	require.Equal(t, int64(2), resp.Records[0].ID, "must keep middle record, not oldest")
	require.Equal(t, int64(3), resp.Records[1].ID, "must keep newest record, not drop it")
	require.Equal(t, "newest", resp.Records[1].Content)
}

func TestGetHistory_NoRecords(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	ts := new(mockTurnsStore)
	api := newTestAPIWithTurns(t, sm, bridge, ts)

	sm.On("Get", "sess-1").Return(&session.SessionInfo{ID: "sess-1", UserID: "anonymous"}, nil)
	ts.On("QueryLatestTurns", mock.Anything, "sess-1", 51).Return(nil, eventstore.ErrNotFound)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	r := authedReq("GET", "/api/sessions/sess-1/history", nil)
	mux.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Records []any `json:"records"`
		HasMore bool  `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Empty(t, resp.Records)
	require.False(t, resp.HasMore)
}

func TestGetEvents_ExpiredSessionStillUsesPerRecordExpiry(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)
	eventReader := new(mockAPIEventStore)
	api.eventStore = eventReader

	expiredAt := time.Now().Add(-time.Second)
	sm.On("Get", "sess-1").Return(&session.SessionInfo{
		ID: "sess-1", UserID: "anonymous", HistoryExpiresAt: &expiredAt,
	}, nil)
	eventReader.On("QueryBySession", mock.Anything, "sess-1", int64(0), eventstore.CursorLatest, 200).
		Return(nil, eventstore.ErrNotFound)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedReq("GET", "/api/sessions/sess-1/events", nil))

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Events   []any `json:"events"`
		HasOlder bool  `json:"has_older"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Empty(t, resp.Events)
	require.False(t, resp.HasOlder)
	eventReader.AssertExpectations(t)
}

func TestGetHistory_Unauthorized(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	ts := new(mockTurnsStore)
	api := newTestAPIWithTurns(t, sm, bridge, ts)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/sessions/sess-1/history", nil)
	mux.ServeHTTP(w, r)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestGetHistory_OwnershipCheck(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	ts := new(mockTurnsStore)
	api := newTestAPIWithTurns(t, sm, bridge, ts)

	sm.On("Get", "sess-1").Return(&session.SessionInfo{ID: "sess-1", UserID: "other-user"}, nil)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	r := authedReq("GET", "/api/sessions/sess-1/history", nil)
	mux.ServeHTTP(w, r)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetHistory_WithBeforeID(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	ts := new(mockTurnsStore)
	api := newTestAPIWithTurns(t, sm, bridge, ts)

	sm.On("Get", "sess-1").Return(&session.SessionInfo{ID: "sess-1", UserID: "anonymous"}, nil)
	records := []*eventstore.TurnRecord{
		{ID: 1},
	}
	ts.On("QueryTurnsBefore", mock.Anything, "sess-1", int64(5), 11).Return(records, nil)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	r := authedReq("GET", "/api/sessions/sess-1/history?before_id=5&limit=10", nil)
	mux.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestGetHistory_NilTurnsStore(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	sm.On("Get", "sess-1").Return(&session.SessionInfo{ID: "sess-1", UserID: "anonymous"}, nil)

	mux := setupMux(api)
	w := httptest.NewRecorder()
	r := authedReq("GET", "/api/sessions/sess-1/history", nil)
	mux.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Records []any `json:"records"`
		HasMore bool  `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Empty(t, resp.Records)
	require.False(t, resp.HasMore)
}

func TestListWorkers(t *testing.T) {
	t.Parallel()
	sm := new(mockAPISM)
	bridge := new(mockAPIBridge)
	api := newTestAPI(t, sm, bridge)

	// X-API-Key auth setup
	auth := newTestAuth(t)
	api.auth = auth

	mux := setupMux(api)
	w := httptest.NewRecorder()
	r := authedReq("GET", "/api/workers", nil)
	mux.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)

	var resp []WorkerInstallationStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp, 4)

	types := make(map[string]bool)
	for _, ws := range resp {
		types[ws.Type] = true
	}
	require.True(t, types["claude_code"])
	require.True(t, types["opencode_server"])
	require.True(t, types["codex_cli"])
	require.True(t, types["acp"])
}
