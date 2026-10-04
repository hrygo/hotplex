package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/execution"
)

// --- Console mocks ---

type mockExecutionReader struct {
	records   []*execution.Record
	listErr   error
	byIDErr   error
	lastList  execution.ListFilter
	byIDCalls int
}

func (m *mockExecutionReader) ListRecent(_ context.Context, f execution.ListFilter) ([]*execution.Record, error) {
	m.lastList = f
	return m.records, m.listErr
}

func (m *mockExecutionReader) ByID(_ context.Context, executionID string) (*execution.Record, error) {
	m.byIDCalls++
	if m.byIDErr != nil {
		return nil, m.byIDErr
	}
	for _, r := range m.records {
		if r.ExecutionID == executionID {
			return r, nil
		}
	}
	return nil, execution.ErrNotFound
}

type mockSessionEvents struct {
	events  []EventFact
	hasMore bool
	err     error
}

func (m *mockSessionEvents) RecentEvents(context.Context, string, int) ([]EventFact, bool, error) {
	return m.events, m.hasMore, m.err
}

type mockExecutionEffects struct {
	effects []*effect.Effect
	err     error
	calls   int
}

func (m *mockExecutionEffects) EffectsForExecution(context.Context, string, int) ([]*effect.Effect, error) {
	m.calls++
	return m.effects, m.err
}

func record(id, session, delivery, runtime string) *execution.Record {
	return &execution.Record{
		ExecutionID:   id,
		SessionID:     session,
		Status:        execution.Status(delivery),
		RuntimeStatus: execution.RuntimeStatus(runtime),
		CreatedAt:     1000,
		UpdatedAt:     2000,
	}
}

func consoleAPI(execs ExecutionReader, events SessionEventReader, effects ExecutionEffectReader) *AdminAPI {
	api := newTestAPI()
	api.SetExecutionConsole(execs, events, effects)
	return api
}

func listRequest(api *AdminAPI, query string, scopes ...string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/executions"+query, nil)
	api.ListExecutions(w, withScope(r, scopes...))
	return w
}

func timelineRequest(api *AdminAPI, id, query string, scopes ...string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := withPathValue(httptest.NewRequest(http.MethodGet, "/admin/executions/"+id+"/timeline"+query, nil), "id", id)
	api.GetExecutionTimeline(w, withScope(r, scopes...))
	return w
}

// --- List ---

func TestListExecutions_Forbidden(t *testing.T) {
	t.Parallel()
	api := consoleAPI(&mockExecutionReader{}, nil, nil)

	w := listRequest(api, "", ScopeSessionRead)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestListExecutions_NotConfigured(t *testing.T) {
	t.Parallel()
	api := newTestAPI()

	w := listRequest(api, "", ScopeRuntimeRead)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestListExecutions_ReturnsSummaries(t *testing.T) {
	t.Parallel()
	api := consoleAPI(&mockExecutionReader{
		records: []*execution.Record{record("exec-1", "sess-1", "accepted", "queued")},
	}, nil, nil)

	w := listRequest(api, "", ScopeRuntimeRead)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Executions []ExecutionSummary `json:"executions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Executions, 1)
	require.Equal(t, "exec-1", resp.Executions[0].ExecutionID)
	require.Equal(t, "queued", resp.Executions[0].RuntimeStatus)
}

// The default page must be a page, not the whole table.
func TestListExecutions_DefaultLimitIsBounded(t *testing.T) {
	t.Parallel()
	reader := &mockExecutionReader{}
	api := consoleAPI(reader, nil, nil)

	listRequest(api, "", ScopeRuntimeRead)

	require.Equal(t, execListDefaultLimit, reader.lastList.Limit)
}

func TestListExecutions_RejectsOversizedLimit(t *testing.T) {
	t.Parallel()
	api := consoleAPI(&mockExecutionReader{}, nil, nil)

	// Silently clamping would return a page that looks like the request was
	// honoured; an operator paging a large window must be told it was not.
	w := listRequest(api, "?limit=5000", ScopeRuntimeRead)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "maximum")
}

func TestListExecutions_RejectsBadFilters(t *testing.T) {
	t.Parallel()
	api := consoleAPI(&mockExecutionReader{}, nil, nil)

	for name, query := range map[string]string{
		"limit zero":          "?limit=0",
		"limit negative":      "?limit=-5",
		"limit not a number":  "?limit=abc",
		"bad delivery status": "?delivery_status=maybe",
		"bad runtime status":  "?runtime_status=thinking",
		"cursor without id":   "?before_created_at=1000",
		"window inverted":     "?since_ms=2000&until_ms=1000",
		"since not a number":  "?since_ms=abc",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := listRequest(api, query, ScopeRuntimeRead)
			require.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestListExecutions_PassesFiltersThrough(t *testing.T) {
	t.Parallel()
	reader := &mockExecutionReader{}
	api := consoleAPI(reader, nil, nil)

	listRequest(api, "?session_id=sess-9&runtime_status=unknown&limit=10", ScopeRuntimeRead)

	require.Equal(t, "sess-9", reader.lastList.SessionID)
	require.Equal(t, execution.RuntimeUnknown, reader.lastList.RuntimeStatus)
	require.Equal(t, 10, reader.lastList.Limit)
}

func TestListExecutions_NextCursorOnlyWhenFullPage(t *testing.T) {
	t.Parallel()
	full := make([]*execution.Record, execListDefaultLimit)
	for i := range full {
		full[i] = record("exec-"+string(rune('a'+i%26)), "sess-1", "accepted", "completed")
	}

	api := consoleAPI(&mockExecutionReader{records: full}, nil, nil)
	w := listRequest(api, "", ScopeRuntimeRead)
	require.Contains(t, w.Body.String(), "next_cursor")

	api2 := consoleAPI(&mockExecutionReader{records: full[:1]}, nil, nil)
	w2 := listRequest(api2, "", ScopeRuntimeRead)
	require.NotContains(t, w2.Body.String(), "before_created_at")
}

// --- Timeline ---

func TestGetExecutionTimeline_Forbidden(t *testing.T) {
	t.Parallel()
	api := consoleAPI(&mockExecutionReader{}, nil, nil)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeWrite)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetExecutionTimeline_NotConfigured(t *testing.T) {
	t.Parallel()
	api := newTestAPI()

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestGetExecutionTimeline_NotFound(t *testing.T) {
	t.Parallel()
	api := consoleAPI(&mockExecutionReader{}, nil, nil)

	w := timelineRequest(api, "missing", "", ScopeRuntimeRead)

	require.Equal(t, http.StatusNotFound, w.Code)
	require.Contains(t, w.Body.String(), "EXECUTION_NOT_FOUND")
}

func TestGetExecutionTimeline_RejectsOversizedWindow(t *testing.T) {
	t.Parallel()
	api := consoleAPI(&mockExecutionReader{}, nil, nil)

	w := timelineRequest(api, "exec-1", "?since_ms=0&until_ms=99999999999999", ScopeRuntimeRead)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetExecutionTimeline_ReportsControlFacts(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "delivered", "completed")
	started := int64(1500)
	rec.StartedAt = &started
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, nil)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	require.Equal(t, http.StatusOK, w.Code)
	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	require.Equal(t, "exec-1", tl.Execution.ExecutionID)

	phases := map[string]bool{}
	for _, item := range tl.Items {
		phases[item.Phase] = true
	}
	require.True(t, phases["dispatched"], "expected a dispatched fact, got %v", phases)
	require.True(t, phases["terminal"], "expected a terminal fact, got %v", phases)
}

// The launch plan is recorded per session for the CURRENT run. A historical run
// therefore has none, and the projection must say so rather than substituting
// the session's present plan — which would rewrite history.
func TestGetExecutionTimeline_PlanEvidenceIsExplicitlyAbsent(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "delivered", "completed")
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, nil)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	require.Equal(t, evidenceNotStored, tl.PlanEvidence)
}

func TestGetExecutionTimeline_NoEffectSourceIsANoteNotAFailure(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "accepted", "queued")
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, nil)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	require.Equal(t, http.StatusOK, w.Code)
	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	require.Equal(t, evidenceNotStored, tl.EffectEvidence)
	require.Contains(t, tl.Notes, noteEffectsNotConfigured)
}

func TestGetExecutionTimeline_EventsAreContentFree(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "delivered", "completed")
	events := &mockSessionEvents{events: []EventFact{
		{Seq: 4, Type: "assistant.message", Direction: "outbound", Source: "worker", CreatedAt: 1700},
	}}
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, events, nil)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.Contains(t, body, "assistant.message")
	// No event payload, prompt or tool argument may cross this boundary.
	require.NotContains(t, body, "content")
	require.NotContains(t, body, "text")
}

func TestGetExecutionTimeline_TruncationIsReported(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "delivered", "completed")
	events := &mockSessionEvents{
		events:  []EventFact{{Seq: 1, Type: "run.done", CreatedAt: 1700}},
		hasMore: true,
	}
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, events, nil)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	require.True(t, tl.Truncated)
	require.Contains(t, tl.Notes, noteEventsTruncated)
}

func TestGetExecutionTimeline_EffectSourcesAreReadOnce(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "delivered", "completed")
	effects := &mockExecutionEffects{effects: []*effect.Effect{
		{EffectID: "eff-1", Status: effect.StatusDelivered, ProviderRef: "C123", UpdatedAtMs: 1800},
	}}
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, effects)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	require.Equal(t, http.StatusOK, w.Code)
	// The projection derives actions from the effects it already loaded; a
	// second query would make the cost per timeline unbounded in sources.
	require.Equal(t, 1, effects.calls)
}

// Notes travel to clients that render them in their own language, so they must
// be codes. A sentence here would be untranslatable and would tie the payload
// to one wording.
func TestGetExecutionTimeline_NotesAreStableCodes(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "delivered", "completed")
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, nil)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	require.Equal(t, http.StatusOK, w.Code)
	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	require.NotEmpty(t, tl.Notes)
	code := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, note := range tl.Notes {
		require.Regexp(t, code, note, "note %q is not a stable code", note)
	}
}

func TestGetExecutionTimeline_DeliveryFactsAreSeparateFromCompletion(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "delivered", "completed")
	effects := &mockExecutionEffects{effects: []*effect.Effect{
		{EffectID: "eff-1", Status: effect.StatusUnknown, UpdatedAtMs: 1800},
	}}
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, effects)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	require.Equal(t, evidenceRecorded, tl.EffectEvidence)

	var sawTerminal, sawDelivery bool
	for _, item := range tl.Items {
		if item.Phase == "terminal" {
			sawTerminal = true
		}
		if item.Phase == "delivery" && string(effect.StatusUnknown) == item.Kind {
			sawDelivery = true
			require.Equal(t, "eff-1", item.EffectID)
		}
	}
	require.True(t, sawTerminal)
	require.True(t, sawDelivery, "an uncertain delivery must appear as its own fact, distinct from the agent finishing")
}

// --- Actions ---

func actionKinds(actions []AvailableAction) map[string]bool {
	out := map[string]bool{}
	for _, a := range actions {
		out[a.Kind] = true
	}
	return out
}

func TestGetExecutionTimeline_FencedRunOffersFenceActions(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "unknown", "unknown")
	rec.FenceReason = "lease_expired"
	rec.FenceVersion = 7
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, nil)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	kinds := actionKinds(tl.Actions)
	require.True(t, kinds["fence_resolve"])
	require.True(t, kinds["fence_abandon"])
	for _, a := range tl.Actions {
		require.Equal(t, int64(7), a.RequiresVersion, "fence actions must carry the fencing token")
	}
}

func TestGetExecutionTimeline_QueuedRunOffersCancel(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "accepted", "queued")
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, nil)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	require.True(t, actionKinds(tl.Actions)["queue_cancel"])
}

func TestGetExecutionTimeline_UncertainDeliveryOffersOperatorChoices(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "delivered", "completed")
	effects := &mockExecutionEffects{effects: []*effect.Effect{
		{EffectID: "eff-1", Status: effect.StatusUnknown, Attempt: 2, UpdatedAtMs: 1800},
	}}
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, effects)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	kinds := actionKinds(tl.Actions)
	require.True(t, kinds["effect_abandon"])
	require.True(t, kinds["effect_mark_delivered"])
	require.True(t, kinds["effect_requeue"])
}

// No state may ever offer a way to declare success the system cannot prove.
func TestGetExecutionTimeline_NeverOffersForceSuccess(t *testing.T) {
	t.Parallel()
	states := []struct{ delivery, runtime string }{
		{"accepted", "queued"},
		{"delivered", "running"},
		{"unknown", "unknown"},
		{"delivered", "completed"},
		{"failed", "failed"},
	}
	for _, s := range states {
		rec := record("exec-1", "sess-1", s.delivery, s.runtime)
		rec.FenceReason = "why_not"
		api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil,
			&mockExecutionEffects{effects: []*effect.Effect{
				{EffectID: "eff-1", Status: effect.StatusUnknown, UpdatedAtMs: 1800},
			}})

		w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)
		var tl ExecutionTimeline
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
		for _, a := range tl.Actions {
			require.NotContains(t, strings.ToLower(a.Kind), "force")
			require.NotContains(t, strings.ToLower(a.Kind), "success")
		}
	}
}

func TestGetExecutionTimeline_FinishedDeliveryOffersNoAction(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "delivered", "completed")
	effects := &mockExecutionEffects{effects: []*effect.Effect{
		{EffectID: "eff-1", Status: effect.StatusDelivered, UpdatedAtMs: 1800},
	}}
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, effects)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)

	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	require.Empty(t, tl.Actions, "a settled delivery has nothing left to decide")
}

// TestGetExecutionTimeline_ReconciledAndFencedAreDistinct proves #868:
// reconciled_* shows the late evidence (not the error code), fenced gets
// its own phase — unknown, delivered, reconciled and fenced never blur.
func TestGetExecutionTimeline_ReconciledAndFencedAreDistinct(t *testing.T) {
	t.Parallel()
	rec := record("exec-1", "sess-1", "delivered", "completed")
	effects := &mockExecutionEffects{effects: []*effect.Effect{
		{EffectID: "eff-r", Status: effect.StatusReconciledSucceeded, EvidenceRef: "lookup-1", ErrorCode: "reconciled_succeeded", UpdatedAtMs: 1800},
		{EffectID: "eff-f", Status: effect.StatusFenced, ErrorCode: "fenced", UpdatedAtMs: 1900},
	}}
	api := consoleAPI(&mockExecutionReader{records: []*execution.Record{rec}}, nil, effects)

	w := timelineRequest(api, "exec-1", "", ScopeRuntimeRead)
	require.Equal(t, http.StatusOK, w.Code)

	var tl ExecutionTimeline
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tl))
	byID := map[string]TimelineItem{}
	for _, item := range tl.Items {
		if item.EffectID != "" {
			byID[item.EffectID] = item
		}
	}
	r := byID["eff-r"]
	require.Equal(t, "delivery", r.Phase)
	require.Equal(t, "lookup-1", r.Evidence, "reconciled shows late evidence, not the error code")
	f := byID["eff-f"]
	require.Equal(t, "fenced", f.Phase)
}
