package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// --- Queue dispatcher mock ---

type mockRuntimeQueue struct {
	cancelled    bool
	cancelErr    error
	cancelledIDs []string
	cleared      int64
	clearErr     error
	clearedIDs   []string
}

func (m *mockRuntimeQueue) CancelQueuedInput(_ context.Context, executionID string) (bool, error) {
	m.cancelledIDs = append(m.cancelledIDs, executionID)
	return m.cancelled, m.cancelErr
}

func (m *mockRuntimeQueue) ClearSessionQueue(_ context.Context, sessionID string) (int64, error) {
	m.clearedIDs = append(m.clearedIDs, sessionID)
	return m.cleared, m.clearErr
}

func queueCancelRequest(t *testing.T, api *AdminAPI, executionID, body string, scopes ...string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := withPathValue(httptest.NewRequest(http.MethodPost,
		"/admin/executions/"+executionID+"/queue-cancel", strings.NewReader(body)), "id", executionID)
	api.HandleCancelQueuedInput(w, withScope(r, scopes...))
	return w
}

func queueClearRequest(t *testing.T, api *AdminAPI, sessionID, body string, scopes ...string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := withPathValue(httptest.NewRequest(http.MethodPost,
		"/admin/sessions/"+sessionID+"/queue-clear", strings.NewReader(body)), "id", sessionID)
	api.HandleClearSessionQueue(w, withScope(r, scopes...))
	return w
}

// --- Cancel ---

func TestHandleCancelQueuedInput_Forbidden(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeQueue(&mockRuntimeQueue{cancelled: true})

	w := queueCancelRequest(t, api, "exec-1", `{"reason":"operator request"}`, ScopeRuntimeRead)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandleCancelQueuedInput_DispatcherNotConfigured(t *testing.T) {
	t.Parallel()
	api := newTestAPI()

	w := queueCancelRequest(t, api, "exec-1", `{"reason":"operator request"}`, ScopeRuntimeWrite)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestHandleCancelQueuedInput_RequiresReason(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeQueue(&mockRuntimeQueue{cancelled: true})

	for name, body := range map[string]string{
		"empty reason":      `{"reason":"   "}`,
		"missing reason":    `{}`,
		"oversize reason":   `{"reason":"` + strings.Repeat("x", fenceReasonMaxLen+1) + `"}`,
		"oversize evidence": `{"reason":"ok","evidence_ref":"` + strings.Repeat("y", fenceEvidenceMaxLen+1) + `"}`,
		"malformed":         `{`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := queueCancelRequest(t, api, "exec-1", body, ScopeRuntimeWrite)
			require.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestHandleCancelQueuedInput_Cancelled(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	queue := &mockRuntimeQueue{cancelled: true}
	api.SetRuntimeQueue(queue)

	w := queueCancelRequest(t, api, "exec-1", `{"reason":"wrong batch","evidence_ref":"ticket-9"}`, ScopeRuntimeWrite)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, []string{"exec-1"}, queue.cancelledIDs)
	var resp struct {
		ExecutionID string `json:"execution_id"`
		Cancelled   bool   `json:"cancelled"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Cancelled)
	require.Equal(t, "exec-1", resp.ExecutionID)
}

// A dispatched input must never be reported as cancelled. This is the whole
// reason the endpoint can return 409.
func TestHandleCancelQueuedInput_AlreadyDispatchedConflicts(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeQueue(&mockRuntimeQueue{cancelled: false})

	w := queueCancelRequest(t, api, "exec-1", `{"reason":"operator request"}`, ScopeRuntimeWrite)

	require.Equal(t, http.StatusConflict, w.Code)
	require.NotContains(t, w.Body.String(), `"cancelled":true`)
}

func TestHandleCancelQueuedInput_StoreError(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeQueue(&mockRuntimeQueue{cancelErr: context.DeadlineExceeded})

	w := queueCancelRequest(t, api, "exec-1", `{"reason":"operator request"}`, ScopeRuntimeWrite)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// --- Clear ---

func TestHandleClearSessionQueue_Forbidden(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeQueue(&mockRuntimeQueue{cleared: 3})

	w := queueClearRequest(t, api, "sess-1", `{"reason":"reset the session"}`, ScopeRuntimeRead)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandleClearSessionQueue_DispatcherNotConfigured(t *testing.T) {
	t.Parallel()
	api := newTestAPI()

	w := queueClearRequest(t, api, "sess-1", `{"reason":"reset the session"}`, ScopeRuntimeWrite)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestHandleClearSessionQueue_ReturnsCount(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	queue := &mockRuntimeQueue{cleared: 4}
	api.SetRuntimeQueue(queue)

	w := queueClearRequest(t, api, "sess-1", `{"reason":"reset the session"}`, ScopeRuntimeWrite)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, []string{"sess-1"}, queue.clearedIDs)
	var resp struct {
		SessionID string `json:"session_id"`
		Cleared   int64  `json:"cleared"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "sess-1", resp.SessionID)
	require.Equal(t, int64(4), resp.Cleared)
}

// An empty queue is a successful no-op, not a conflict: there is nothing
// left to withdraw, and the operator's intent is already satisfied.
func TestHandleClearSessionQueue_EmptyQueueIsOK(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeQueue(&mockRuntimeQueue{cleared: 0})

	w := queueClearRequest(t, api, "sess-1", `{"reason":"reset the session"}`, ScopeRuntimeWrite)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"cleared":0`)
}

func TestHandleClearSessionQueue_StoreErrorIsNotSuccess(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeQueue(&mockRuntimeQueue{clearErr: context.DeadlineExceeded})

	w := queueClearRequest(t, api, "sess-1", `{"reason":"reset the session"}`, ScopeRuntimeWrite)

	// A failed clear must not be reported as "cleared: 0" — that reads to the
	// operator as a verified empty queue.
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotContains(t, w.Body.String(), `"cleared"`)
}

// --- Audit routing ---

// /queue-clear lives under /admin/sessions, so the generic session audit
// mapping would otherwise file it as a session action.
func TestAdminActionFor_QueueRoutes(t *testing.T) {
	t.Parallel()
	require.Equal(t, AuditRuntimeQueueCancel,
		adminActionFor(http.MethodPost, "/admin/executions/exec-1/queue-cancel"))
	require.Equal(t, AuditRuntimeQueueClear,
		adminActionFor(http.MethodPost, "/admin/sessions/sess-1/queue-clear"))
}
