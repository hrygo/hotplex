package admin

// Operator endpoints for the bounded durable input queue (plan Q2).
//
// The queue is a promise the gateway made to a client: "this input is stored
// and will be dispatched". These endpoints let an operator withdraw that
// promise while it is still withdrawable — that is, before the dispatch
// boundary. After dispatch, the input is a running execution and cancelling it
// is a different operation (stop), so the handler answers 409 rather than a
// success that never happened.
//
// Security contract, matching the fence endpoints:
//   - actor comes from the auth context (ActorFromRequest), never the body
//   - request bodies carry only bounded reason/evidence_ref text; input
//     content, prompts and credentials are never accepted or echoed
//   - responses expose counts and execution/session IDs only

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/web"
)

type queueActionBody struct {
	Reason      string `json:"reason"`
	EvidenceRef string `json:"evidence_ref"`
}

// errAlreadyDispatched marks the audit-only outcome of a cancel that lost the
// race with dispatch. It never escapes a handler: the HTTP response is 409.
var errAlreadyDispatched = errors.New("input already dispatched")

// HandleCancelQueuedInput withdraws the promise for one queued input.
//
// @Summary      Cancel one queued input
// @Description  Settles a durable-queue input that has NOT been dispatched yet. Conditional on the item still being queued: once the dispatch boundary is crossed the input may be running or finished, and the call returns 409 instead of a false success. Requires runtime:write scope.
// @Tags         Admin API
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Param        id    path   string             true  "Execution ID"
// @Param        body  body   queueActionBody  true  "Operator rationale"
// @Success      200   {object}  map[string]any  "Queued input cancelled"
// @Failure      400   {object}  ErrorResponse  "Invalid reason or evidence_ref"
// @Failure      403   {object}  ErrorResponse  "Insufficient scope: need runtime:write"
// @Failure      409   {object}  ErrorResponse  "Input is no longer queued: it has been dispatched"
// @Failure      503   {object}  ErrorResponse  "Queue dispatcher not configured"
// @Router       /admin/executions/{id}/queue-cancel [post]
func (a *AdminAPI) HandleCancelQueuedInput(w http.ResponseWriter, r *http.Request) {
	if !hasScope(r, ScopeRuntimeWrite) {
		web.WriteAppError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE",
			"insufficient scope: need runtime:write")
		return
	}
	if a.runtimeQueue == nil {
		web.WriteAppError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"input queue dispatcher not configured")
		return
	}

	executionID := strings.TrimSpace(r.PathValue("id"))
	if executionID == "" {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", "execution id is required")
		return
	}
	reason, evidenceRef, ok := decodeQueueActionBody(w, r)
	if !ok {
		return
	}

	cancelled, err := a.runtimeQueue.CancelQueuedInput(r.Context(), executionID)
	// A refusal is a rejected action, not a failure and not a success: the
	// store worked fine and told us the input had already been dispatched.
	auditErr := err
	if auditErr == nil && !cancelled {
		auditErr = errAlreadyDispatched
	}
	a.auditQueueAction(r, AuditRuntimeQueueCancel, executionID, reason, evidenceRef, auditErr)
	if err != nil {
		a.writeQueueActionError(w, r, "cancel", err)
		return
	}
	if !cancelled {
		// The store says this input is not queued. It was dispatched, so it is
		// somebody else's execution now; answering 200 would invent a
		// cancellation that did not happen.
		web.WriteAppError(w, http.StatusConflict, "INPUT_NOT_QUEUED",
			"input is no longer queued: it has been dispatched, use the stop action instead")
		return
	}

	observability.RuntimeQueueOperatorActions().Add(r.Context(), 1, metric.WithAttributes(
		attribute.String("action", "cancel"),
		attribute.String("result", "ok"),
	))
	a.log.Info("admin: queued input cancelled",
		"execution_id", executionID,
		"actor", ActorFromRequest(r),
	)

	respondJSON(w, map[string]any{"execution_id": executionID, "cancelled": true})
}

// HandleClearSessionQueue settles every undispatched input for a session.
// Running and dispatched turns are untouched: this endpoint only withdraws
// promises that have not been handed to a Worker yet.
//
// @Summary      Clear a session's undispatched input queue
// @Description  Cancels every queued-but-not-dispatched input for a session. Inputs already dispatched (running, finished or fenced) are left alone. Requires runtime:write scope.
// @Tags         Admin API
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Param        id    path   string             true  "Session ID"
// @Param        body  body   queueActionBody  true  "Operator rationale"
// @Success      200   {object}  map[string]any  "Number of queued inputs settled"
// @Failure      400   {object}  ErrorResponse  "Invalid session id, reason or evidence_ref"
// @Failure      403   {object}  ErrorResponse  "Insufficient scope: need runtime:write"
// @Failure      503   {object}  ErrorResponse  "Queue dispatcher not configured"
// @Router       /admin/sessions/{id}/queue-clear [post]
func (a *AdminAPI) HandleClearSessionQueue(w http.ResponseWriter, r *http.Request) {
	if !hasScope(r, ScopeRuntimeWrite) {
		web.WriteAppError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE",
			"insufficient scope: need runtime:write")
		return
	}
	if a.runtimeQueue == nil {
		web.WriteAppError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"input queue dispatcher not configured")
		return
	}

	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", "session id is required")
		return
	}
	reason, evidenceRef, ok := decodeQueueActionBody(w, r)
	if !ok {
		return
	}

	cleared, err := a.runtimeQueue.ClearSessionQueue(r.Context(), sessionID)
	a.auditQueueAction(r, AuditRuntimeQueueClear, sessionID, reason, evidenceRef, err)
	if err != nil {
		a.writeQueueActionError(w, r, "clear", err)
		return
	}

	observability.RuntimeQueueOperatorActions().Add(r.Context(), 1, metric.WithAttributes(
		attribute.String("action", "clear"),
		attribute.String("result", "ok"),
	))
	a.log.Info("admin: session input queue cleared",
		"session_id", sessionID,
		"cleared", cleared,
		"actor", ActorFromRequest(r),
	)

	respondJSON(w, map[string]any{"session_id": sessionID, "cleared": cleared})
}

// decodeQueueActionBody validates the bounded rationale both queue endpoints
// require. An empty body is a valid JSON decode error path (400), but a body
// with no reason is rejected: an operator withdrawing a promise to a client
// must be able to say why.
func decodeQueueActionBody(w http.ResponseWriter, r *http.Request) (reason, evidenceRef string, ok bool) {
	var body queueActionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body")
		return "", "", false
	}
	reason = strings.TrimSpace(body.Reason)
	if reason == "" || len(reason) > fenceReasonMaxLen {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST",
			"reason must be 1-512 characters")
		return "", "", false
	}
	evidenceRef = strings.TrimSpace(body.EvidenceRef)
	if len(evidenceRef) > fenceEvidenceMaxLen {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST",
			"evidence_ref must be at most 256 characters")
		return "", "", false
	}
	return reason, evidenceRef, true
}

// writeQueueActionError maps dispatcher failures to operator-actionable
// statuses. The store never surfaces input content through these errors.
func (a *AdminAPI) writeQueueActionError(w http.ResponseWriter, r *http.Request, action string, err error) {
	status := http.StatusInternalServerError
	code := "INTERNAL"
	msg := "failed to apply queue action"
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, code, msg = http.StatusServiceUnavailable, "TIMEOUT", "queue action timed out"
	}
	observability.RuntimeQueueOperatorActions().Add(r.Context(), 1, metric.WithAttributes(
		attribute.String("action", action),
		attribute.String("result", "error"),
	))
	web.WriteAppError(w, status, code, msg)
}

// auditQueueAction records the operator decision into the tamper-evident
// user_activity table. The reason/evidence never reach the execution store.
func (a *AdminAPI) auditQueueAction(r *http.Request, action, targetID, reason, evidenceRef string, applyErr error) {
	if a.auditCollector == nil {
		return
	}
	outcome := audit.OutcomeSuccess
	result := AuditResultOk
	switch {
	case errors.Is(applyErr, errAlreadyDispatched):
		result = "rejected"
	case applyErr != nil:
		outcome, result = audit.OutcomeFailure, AuditResultFailed
	}
	detail, _ := json.Marshal(map[string]any{
		"target_id":    targetID,
		"reason":       reason,
		"evidence_ref": evidenceRef,
		"result":       result,
	})
	userID, userIDType := actorIdentity(ActorFromRequest(r))
	_ = a.auditCollector.Enqueue(context.Background(), &audit.UserActivity{
		Ts:           time.Now().UnixMilli(),
		UserID:       userID,
		UserIDType:   userIDType,
		Platform:     audit.PlatformAdmin,
		Action:       "admin." + action,
		ResourceType: "runtime",
		Outcome:      outcome,
		DetailJSON:   string(detail),
		IP:           clientIP(r),
		UserAgent:    r.UserAgent(),
	})
}
