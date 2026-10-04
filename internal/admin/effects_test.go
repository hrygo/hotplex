package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/effect"
)

// --- Effect mocks ---

type mockRuntimeEffects struct {
	listFn     func(ctx context.Context, f effect.OperatorListFilter) ([]*effect.Effect, error)
	getFn      func(ctx context.Context, id string) (*effect.Effect, error)
	attemptsFn func(ctx context.Context, id string) ([]*effect.Attempt, error)
	applyFn    func(ctx context.Context, req effect.OperatorActionRequest) (*effect.Effect, error)
	lastApply  effect.OperatorActionRequest
}

func (m *mockRuntimeEffects) ListForOperator(ctx context.Context, f effect.OperatorListFilter) ([]*effect.Effect, error) {
	if m.listFn != nil {
		return m.listFn(ctx, f)
	}
	return nil, nil
}

func (m *mockRuntimeEffects) GetByID(ctx context.Context, id string) (*effect.Effect, error) {
	if m.getFn != nil {
		return m.getFn(ctx, id)
	}
	return nil, effect.ErrEffectNotFound
}

func (m *mockRuntimeEffects) ListAttempts(ctx context.Context, id string) ([]*effect.Attempt, error) {
	if m.attemptsFn != nil {
		return m.attemptsFn(ctx, id)
	}
	return nil, nil
}

func (m *mockRuntimeEffects) ApplyOperatorAction(ctx context.Context, req effect.OperatorActionRequest) (*effect.Effect, error) {
	m.lastApply = req
	if m.applyFn != nil {
		return m.applyFn(ctx, req)
	}
	return nil, effect.ErrEffectNotFound
}

func unknownEffectRecord() *effect.Effect {
	return &effect.Effect{
		EffectID:     "eff-1",
		OccurrenceID: "occ-1",
		SessionID:    "sess-1",
		ExecutionID:  "exec-1",
		TargetKind:   "slack",
		TargetRef:    "C123",
		Status:       effect.StatusUnknown,
		Attempt:      1,
		ErrorCode:    "unproven",
		Reason:       "network failure",
		LeaseVersion: 1,
		CreatedAtMs:  1722800000000,
		UpdatedAtMs:  1722800001000,
	}
}

func withPathValue(r *http.Request, name, value string) *http.Request {
	r.SetPathValue(name, value)
	return r
}

// --- ListEffects ---

func TestHandleListEffects_Forbidden(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeEffects(&mockRuntimeEffects{})
	w := httptest.NewRecorder()
	r := withScope(httptest.NewRequest(http.MethodGet, "/admin/effects", nil), ScopeSessionRead)

	api.HandleListEffects(w, r)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandleListEffects_ProviderNotConfigured(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	w := httptest.NewRecorder()
	r := withScope(httptest.NewRequest(http.MethodGet, "/admin/effects", nil), ScopeRuntimeRead)

	api.HandleListEffects(w, r)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestHandleListEffects_NeverEchoesMessageContent is the privacy contract: the
// console shows delivery FACTS, never what was said.
func TestHandleListEffects_NeverEchoesMessageContent(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeEffects(&mockRuntimeEffects{
		listFn: func(context.Context, effect.OperatorListFilter) ([]*effect.Effect, error) {
			return []*effect.Effect{unknownEffectRecord()}, nil
		},
	})
	w := httptest.NewRecorder()
	r := withScope(httptest.NewRequest(http.MethodGet, "/admin/effects?status=unknown", nil),
		ScopeRuntimeRead)

	api.HandleListEffects(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.Contains(t, body, "eff-1")
	require.NotContains(t, strings.ToLower(body), "content")
	require.NotContains(t, body, "prompt")
}

// --- GetEffect ---

func TestHandleGetEffect_IncludesAttemptHistory(t *testing.T) {
	t.Parallel()
	finished := int64(1722800002000)
	api := newTestAPI()
	api.SetRuntimeEffects(&mockRuntimeEffects{
		getFn: func(context.Context, string) (*effect.Effect, error) {
			return unknownEffectRecord(), nil
		},
		attemptsFn: func(context.Context, string) ([]*effect.Attempt, error) {
			return []*effect.Attempt{
				{
					AttemptID: "att-1", EffectID: "eff-1", Attempt: 0,
					Outcome: effect.AttemptAccepted, ProviderRef: "C123:1",
					FinishedAtMs: &finished,
				},
				{
					AttemptID: "att-2", EffectID: "eff-1", Attempt: 1,
					Outcome: effect.AttemptUnknown,
				},
			}, nil
		},
	})
	w := httptest.NewRecorder()
	r := withScope(withPathValue(
		httptest.NewRequest(http.MethodGet, "/admin/effects/eff-1", nil), "id", "eff-1"),
		ScopeRuntimeRead)

	api.HandleGetEffect(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.Contains(t, body, `"outcome":"accepted"`)
	require.Contains(t, body, `"outcome":"unknown"`)
	require.Contains(t, body, `"in_flight":true`)
	require.NotContains(t, body, "lease_token",
		"the lease token is a write credential and must never be projected")
}

func TestHandleGetEffect_NotFound(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeEffects(&mockRuntimeEffects{})
	w := httptest.NewRecorder()
	r := withScope(withPathValue(
		httptest.NewRequest(http.MethodGet, "/admin/effects/missing", nil), "id", "missing"),
		ScopeRuntimeRead)

	api.HandleGetEffect(w, r)

	require.Equal(t, http.StatusNotFound, w.Code)
}

// --- Effect action ---

func TestHandleEffectAction_Forbidden(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeEffects(&mockRuntimeEffects{})
	w := httptest.NewRecorder()
	r := withScope(withPathValue(
		httptest.NewRequest(http.MethodPost, "/admin/effects/eff-1/action",
			strings.NewReader(`{"decision":"abandon","expected_status":"unknown","reason":"x"}`)),
		"id", "eff-1"), ScopeRuntimeRead)

	api.HandleEffectAction(w, r)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandleEffectAction_AppliesDecision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		decision string
	}{
		{name: "abandon", decision: "abandon"},
		{name: "mark delivered", decision: "mark_delivered"},
		{name: "requeue", decision: "requeue"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			provider := &mockRuntimeEffects{
				applyFn: func(_ context.Context, req effect.OperatorActionRequest) (*effect.Effect, error) {
					updated := unknownEffectRecord()
					updated.Status = effect.StatusFailed
					updated.ErrorCode = "operator_abandoned"
					updated.Reason = "operator: " + req.Reason
					return updated, nil
				},
			}
			api := newTestAPI()
			api.SetRuntimeEffects(provider)

			body := `{"decision":"` + tc.decision +
				`","expected_status":"unknown","reason":"checked by hand","evidence_ref":"ticket-42"}`
			w := httptest.NewRecorder()
			r := withScope(withPathValue(
				httptest.NewRequest(http.MethodPost, "/admin/effects/eff-1/action",
					strings.NewReader(body)), "id", "eff-1"), ScopeRuntimeWrite)

			api.HandleEffectAction(w, r)

			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, effect.OperatorDecision(tc.decision), provider.lastApply.Decision)
			require.Equal(t, effect.StatusUnknown, provider.lastApply.ExpectedStatus)
			require.Equal(t, "checked by hand", provider.lastApply.Reason)
			require.Equal(t, "ticket-42", provider.lastApply.EvidenceRef)
		})
	}
}

// TestHandleEffectAction_ConflictDoesNotRetry pins the operator contract: a
// 409 tells the operator to re-inspect, and nothing is written on the caller's
// behalf.
func TestHandleEffectAction_ConflictDoesNotRetry(t *testing.T) {
	t.Parallel()

	calls := 0
	api := newTestAPI()
	api.SetRuntimeEffects(&mockRuntimeEffects{
		applyFn: func(context.Context, effect.OperatorActionRequest) (*effect.Effect, error) {
			calls++
			return nil, effect.ErrLeaseLost
		},
	})
	w := httptest.NewRecorder()
	r := withScope(withPathValue(
		httptest.NewRequest(http.MethodPost, "/admin/effects/eff-1/action",
			strings.NewReader(`{"decision":"requeue","expected_status":"unknown","reason":"x"}`)),
		"id", "eff-1"), ScopeRuntimeWrite)

	api.HandleEffectAction(w, r)

	require.Equal(t, http.StatusConflict, w.Code)
	require.Equal(t, 1, calls, "a conflict must not be retried automatically")
}

func TestHandleEffectAction_ValidatesTheRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "unknown decision", body: `{"decision":"obliterate","expected_status":"unknown","reason":"x"}`, want: http.StatusBadRequest},
		{name: "no reason", body: `{"decision":"abandon","expected_status":"unknown"}`, want: http.StatusBadRequest},
		{
			name: "a decided effect accepts no decision",
			body: `{"decision":"abandon","expected_status":"delivered","reason":"x"}`,
			want: http.StatusBadRequest,
		},
		{name: "invalid json", body: `not json`, want: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			called := false
			api := newTestAPI()
			api.SetRuntimeEffects(&mockRuntimeEffects{
				applyFn: func(context.Context, effect.OperatorActionRequest) (*effect.Effect, error) {
					called = true
					return unknownEffectRecord(), nil
				},
			})
			w := httptest.NewRecorder()
			r := withScope(withPathValue(
				httptest.NewRequest(http.MethodPost, "/admin/effects/eff-1/action",
					strings.NewReader(tc.body)), "id", "eff-1"), ScopeRuntimeWrite)

			api.HandleEffectAction(w, r)

			require.Equal(t, tc.want, w.Code)
			require.False(t, called, "a rejected request must never reach the ledger")
		})
	}
}

func TestHandleEffectAction_NotFound(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	api.SetRuntimeEffects(&mockRuntimeEffects{})
	w := httptest.NewRecorder()
	r := withScope(withPathValue(
		httptest.NewRequest(http.MethodPost, "/admin/effects/missing/action",
			strings.NewReader(`{"decision":"abandon","expected_status":"unknown","reason":"x"}`)),
		"id", "missing"), ScopeRuntimeWrite)

	api.HandleEffectAction(w, r)

	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestEffectListItem_CarriesNoContent(t *testing.T) {
	t.Parallel()

	item := effectListItem(unknownEffectRecord())
	encoded, err := json.Marshal(item)
	require.NoError(t, err)

	body := string(encoded)
	require.NotContains(t, body, "payload_sha")
	require.NotContains(t, body, "lease_until")
	require.NotContains(t, body, "owner_instance")
}
