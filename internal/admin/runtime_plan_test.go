package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

var runtimePlanHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func runtimePlanRequest(id string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/admin/sessions/sess/runtime-plan", nil)
	r.SetPathValue("id", id)
	return withScope(r, ScopeRuntimeRead, ScopeSessionRead)
}

func TestHandleSessionRuntimePlan_Forbidden(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		scopes []string
	}{
		{"no scopes", nil},
		{"missing runtime:read", []string{ScopeSessionRead}},
		{"missing session:read", []string{ScopeRuntimeRead}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newTestAPI()
			w := httptest.NewRecorder()
			r := withScope(httptest.NewRequest(http.MethodGet, "/admin/sessions/sess-1/runtime-plan", nil), tc.scopes...)
			r.SetPathValue("id", "sess-1")

			api.HandleSessionRuntimePlan(w, r)

			require.Equal(t, http.StatusForbidden, w.Code)
		})
	}
}

func TestHandleSessionRuntimePlan_MissingID(t *testing.T) {
	t.Parallel()
	api := newTestAPI()
	w := httptest.NewRecorder()
	r := runtimePlanRequest("   ")

	api.HandleSessionRuntimePlan(w, r)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleSessionRuntimePlan_NotFound(t *testing.T) {
	t.Parallel()
	api := newTestAPI() // default mockSessionManager returns ErrSessionNotFound
	w := httptest.NewRecorder()
	r := runtimePlanRequest("sess-missing")

	api.HandleSessionRuntimePlan(w, r)

	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleSessionRuntimePlan_NonSessionValue_NotFound(t *testing.T) {
	t.Parallel()
	api := newTestAPI(func(d *Deps) {
		d.SessionMgr = &mockSessionManager{
			getFn: func(id string) (any, error) { return &worker.SessionStartParams{}, nil },
		}
	})
	w := httptest.NewRecorder()
	r := runtimePlanRequest("sess-1")

	api.HandleSessionRuntimePlan(w, r)

	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestHandleSessionRuntimePlan_Success_RedactedView covers the happy path: a
// session without a dispatched worker type resolves a warned (not blocked)
// plan, and the response is the redacted view only.
func TestHandleSessionRuntimePlan_Success_RedactedView(t *testing.T) {
	t.Parallel()
	si := &session.SessionInfo{ID: "sess-1", UserID: "u1", WorkspaceID: "ws1"}
	api := newTestAPI(func(d *Deps) {
		d.SessionMgr = &mockSessionManager{
			getFn: func(id string) (any, error) { return si, nil },
		}
	})
	w := httptest.NewRecorder()
	r := runtimePlanRequest("sess-1")

	api.HandleSessionRuntimePlan(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	var report RuntimePlanReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))

	require.Regexp(t, runtimePlanHashRe, report.Plan.PlanHash)
	require.Empty(t, report.Plan.Blocked)
	require.Equal(t, agentspec.ObservedPlanned, report.Observed.State)

	// Redaction: internal contract fields never surface on the wire shape.
	for _, key := range []string{`"command"`, `"model"`, `"budget"`} {
		require.NotContains(t, body, key, "internal plan field leaked into the admin response")
	}
}

// TestHandleSessionRuntimePlan_BlockedPlan_StillDiagnostic: a blocked plan is
// a valid diagnostic payload (§6.6) — 200 with the blocked reasons and NO hash.
// The test binary's worker registry is empty, so any non-empty worker type on
// the session is rejected fail-closed by the shared resolver.
func TestHandleSessionRuntimePlan_BlockedPlan_StillDiagnostic(t *testing.T) {
	t.Parallel()
	si := &session.SessionInfo{
		ID:         "sess-2",
		WorkerType: worker.WorkerType("codex_cli"),
		State:      events.StateTerminated,
	}
	api := newTestAPI(func(d *Deps) {
		d.SessionMgr = &mockSessionManager{
			getFn: func(id string) (any, error) { return si, nil },
		}
	})
	w := httptest.NewRecorder()
	r := runtimePlanRequest("sess-2")

	api.HandleSessionRuntimePlan(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	var report RuntimePlanReport
	require.NoError(t, json.NewDecoder(w.Body).Decode(&report))

	require.Empty(t, report.Plan.PlanHash, "a blocked plan carries no hash")
	require.NotEmpty(t, report.Plan.Blocked)
	require.Equal(t, agentspec.BlockUnknownWorkerType, report.Plan.Blocked[0].Code)
	require.Equal(t, "codex_cli", report.Observed.WorkerType)
	require.Equal(t, agentspec.ObservedPlanned, report.Observed.State)
}

func TestObservedSummaryFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		si        *session.SessionInfo
		hasLaunch bool
		want      agentspec.ObservedSummary
	}{
		{
			name: "declared ceiling wins over active state",
			si: &session.SessionInfo{
				WorkerType:        worker.TypeClaudeCode,
				State:             events.StateRunning,
				PermissionCeiling: "default",
			},
			hasLaunch: true,
			want: agentspec.ObservedSummary{
				State:             agentspec.ObservedDeclared,
				WorkerType:        "claude_code",
				PermissionCeiling: "default",
			},
		},
		{
			name:      "active without ceiling is unknown",
			si:        &session.SessionInfo{WorkerType: worker.TypeCodexCLI, State: events.StateRunning},
			hasLaunch: true,
			want:      agentspec.ObservedSummary{State: agentspec.ObservedUnknown, WorkerType: "codex_cli"},
		},
		{
			name:      "inactive without ceiling is planned",
			si:        &session.SessionInfo{State: events.StateTerminated},
			hasLaunch: true,
			want:      agentspec.ObservedSummary{State: agentspec.ObservedPlanned},
		},
		{
			// D3: with no recorded launch there is nothing observed at all.
			// Reporting "unknown" here would describe a gap in our knowledge
			// as a fact about the runtime — and an ACTIVE session that never
			// launched must not be reported as running under an unproven
			// bootstrap.
			name: "no launch is planned even when the session looks active",
			si: &session.SessionInfo{
				WorkerType: worker.TypeClaudeCode,
				State:      events.StateRunning,
			},
			hasLaunch: false,
			want:      agentspec.ObservedSummary{State: agentspec.ObservedPlanned, WorkerType: "claude_code"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, observedSummaryFor(tc.si, tc.hasLaunch))
		})
	}
}

func TestBuildSessionPlan_FactsRoundTrip(t *testing.T) {
	t.Parallel()
	si := &session.SessionInfo{
		ID:           "sess-3",
		UserID:       "u1",
		WorkspaceID:  "ws1",
		BotName:      "bot-1",
		Platform:     "webchat",
		AllowedTools: []string{"Read"},
	}

	plan, err := buildSessionPlan(nil, si)

	require.NoError(t, err)
	require.Regexp(t, runtimePlanHashRe, plan.PlanHash)

	// The session's persisted tool whitelist enters as init-metadata source.
	var sawTools bool
	for _, ref := range plan.SourceRefs {
		if ref.Field == "allowed_tools" {
			sawTools = true
			require.Equal(t, agentspec.PlanSourceInitMetadata, ref.Source)
		}
	}
	require.True(t, sawTools, "allowed_tools source ref missing")

	// No worker type resolvable for a webchat input without init metadata.
	require.NotEmpty(t, plan.Warnings)
	require.Equal(t, agentspec.WarnWorkerTypeUnresolved, plan.Warnings[0].Code)
}

func TestBuildSessionPlan_BlockedTyped(t *testing.T) {
	t.Parallel()
	si := &session.SessionInfo{WorkerType: worker.WorkerType("not-registered-in-test")}

	_, err := buildSessionPlan(nil, si)

	require.ErrorIs(t, err, agentspec.ErrPlanBlocked)
}
