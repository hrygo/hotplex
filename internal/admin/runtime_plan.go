package admin

import (
	"errors"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/web"
)

// RuntimePlanReport is the redacted wire shape of
// GET /admin/sessions/{id}/runtime-plan (#946 spec §6.6): the desired-state
// plan projection plus the observed bootstrap summary. It never carries
// prompts, secrets, full commands, or raw worker errors.
type RuntimePlanReport struct {
	Plan     agentspec.EffectiveRuntimePlanView `json:"plan"`
	Observed agentspec.ObservedSummary          `json:"observed"`
	// FromLaunch reports whether Plan is what this session's current run was
	// ACTUALLY launched under. False means it was re-resolved against the live
	// config — a different question, and one that must not be read as history.
	FromLaunch bool `json:"from_launch"`
	// PlanApplied reports whether the recorded run was started from its plan
	// (authoritative rollout) or only shadow-compared against it. A plan that
	// merely AGREED with the legacy parameters is not an applied plan.
	PlanApplied bool `json:"plan_applied"`
}

// HandleSessionRuntimePlan returns the redacted effective runtime plan for a
// session: plan hash, worker type, permission/sandbox summary, observed
// bootstrap state, warnings and blocked codes (#946 spec §6.6).
//
// Requires runtime:read AND session:read. The diagnostic projection is
// computed on demand from the session's persisted facts and the live config —
// no plan table, no second persisted truth (#946 spec §6.6).
//
// @Summary      Get session runtime plan
// @Description  Returns the redacted desired-state runtime plan and observed bootstrap summary for a session. Requires runtime:read and session:read scopes.
// @Tags         Admin API
// @Produce      json
// @Security     AdminBearerAuth
// @Param        id   path      string  true  "Session ID"
// @Success      200  {object}  RuntimePlanReport
// @Failure      400  {object}  ErrorResponse  "Missing session ID"
// @Failure      403  {object}  ErrorResponse  "Insufficient scope: need runtime:read and session:read"
// @Failure      404  {object}  ErrorResponse  "Session not found"
// @Failure      500  {object}  ErrorResponse  "Plan resolution failed"
// @Router       /admin/sessions/{id}/runtime-plan [get]
func (a *AdminAPI) HandleSessionRuntimePlan(w http.ResponseWriter, r *http.Request) {
	if !hasScope(r, ScopeRuntimeRead) {
		web.WriteAppError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "insufficient scope: need runtime:read")
		return
	}
	if !hasScope(r, ScopeSessionRead) {
		web.WriteAppError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "insufficient scope: need session:read")
		return
	}

	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		web.WriteAppError(w, http.StatusBadRequest, "INVALID_REQUEST", "session id is required")
		return
	}

	raw, err := a.sm.Get(r.Context(), id)
	if err != nil {
		if r.Context().Err() != nil {
			web.WriteAppError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "request cancelled")
			return
		}
		web.WriteAppError(w, http.StatusNotFound, "NOT_FOUND", "session not found")
		return
	}
	si, ok := raw.(*session.SessionInfo)
	if !ok || si == nil {
		web.WriteAppError(w, http.StatusNotFound, "NOT_FOUND", "session not found")
		return
	}

	var cfg *config.Config
	if a.cfg != nil {
		cfg = a.cfg.Get()
	}
	plan, fromLaunch, err := a.resolvedPlanFor(si, cfg)
	if err != nil && !errors.Is(err, agentspec.ErrPlanBlocked) {
		// A blocked plan is a valid diagnostic projection (its Blocked reasons
		// are the payload); any other failure is an internal error.
		a.log.Warn("admin: runtime plan resolution failed", "session_id", id, "err", err)
		web.WriteAppError(w, http.StatusInternalServerError, "INTERNAL", "failed to resolve runtime plan")
		return
	}

	observed := observedSummaryFor(si, fromLaunch)
	observability.RuntimePlanObserved().Add(r.Context(), 1,
		metric.WithAttributes(attribute.String("state", observed.State)))
	// Tell the caller which question they are looking at. A re-resolved plan
	// describes the CURRENT config, not this session's history; without this
	// flag the two are indistinguishable in the response.
	respondJSON(w, RuntimePlanReport{
		Plan:        plan.Redacted(),
		Observed:    observed,
		FromLaunch:  fromLaunch,
		PlanApplied: fromLaunch && a.launchPlans != nil && a.launchPlans.LaunchPlanApplied(si.ID),
	})
}

// buildSessionPlan reconstructs the desired-state plan from the session's
// persisted facts plus the live config snapshot. The session's dispatched
// worker type and tool whitelist enter as top-precedence init metadata; the
// config chain below them mirrors the original resolution. Pure — no I/O.
func buildSessionPlan(cfg *config.Config, si *session.SessionInfo) (agentspec.EffectiveRuntimePlan, error) {
	in := agentspec.Input{
		Cfg: cfg,
		InitMeta: agentspec.InitMetadata{
			WorkerType:   string(si.WorkerType),
			AllowedTools: si.AllowedTools,
		},
		BotName:     si.BotName,
		Platform:    si.Platform,
		PlatformKey: si.PlatformKey,
		UserID:      si.UserID,
		WorkspaceID: si.WorkspaceID,
	}
	return (agentspec.Resolver{}).ResolvePlan(in)
}

// LaunchPlanProvider exposes the plan a session's CURRENT run was actually
// launched under (#946 D3).
//
// The diagnostic prefers this over re-resolving. A re-resolution answers "what
// would launch now"; the recorded plan answers "what did this run launch
// under". Reporting the first as the second is how a historical fact becomes a
// plausible fiction — and the difference matters most exactly when a config
// changed after the run started.
type LaunchPlanProvider interface {
	LaunchPlanFor(sessionID string) (agentspec.EffectiveRuntimePlan, bool)
	LaunchPlanApplied(sessionID string) bool
}

// SetLaunchPlanProvider wires the recorded-launch-plan source. nil is safe:
// the diagnostic then falls back to re-resolution and says so.
func (a *AdminAPI) SetLaunchPlanProvider(p LaunchPlanProvider) { a.launchPlans = p }

// resolvedPlanFor returns the plan to report, preferring what the run actually
// launched under and falling back to a re-resolution against the live config.
// The bool reports whether the answer came from a real launch.
func (a *AdminAPI) resolvedPlanFor(
	si *session.SessionInfo, cfg *config.Config,
) (agentspec.EffectiveRuntimePlan, bool, error) {
	if a.launchPlans != nil {
		if recorded, ok := a.launchPlans.LaunchPlanFor(si.ID); ok {
			return recorded, true, nil
		}
	}
	plan, err := buildSessionPlan(cfg, si)
	return plan, false, err
}

// observedSummaryFor maps the session's verifiable facts onto the observed
// bootstrap states (#946 spec §6.5). A Worker-reported permission ceiling is
// DECLARED (not independently enforced → never "enforced").
//
// hasLaunch is the gate that keeps this honest: with no recorded launch there
// is nothing observed at all, so the answer is ObservedPlanned. An active
// session that has never launched is not "unknown" — reporting unknown would
// describe a gap in our knowledge as a fact about the runtime.
func observedSummaryFor(si *session.SessionInfo, hasLaunch bool) agentspec.ObservedSummary {
	observed := agentspec.ObservedSummary{WorkerType: string(si.WorkerType)}
	switch {
	case !hasLaunch:
		observed.State = agentspec.ObservedPlanned
	case si.PermissionCeiling != "":
		observed.State = agentspec.ObservedDeclared
		observed.PermissionCeiling = si.PermissionCeiling
	case si.State.IsActive():
		observed.State = agentspec.ObservedUnknown
	default:
		observed.State = agentspec.ObservedPlanned
	}
	return observed
}
