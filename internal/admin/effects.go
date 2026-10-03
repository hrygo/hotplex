package admin

// Operator delivery-effect endpoints. They are deliberately SEPARATE from the
// execution fence endpoints: an execution fence asks "may this run accept
// more input", while an unknown effect asks "was this message delivered".
// Sharing one endpoint would let a fence decision silently rewrite delivery
// history, or a delivery decision unblock a fenced run.
//
// Security contract:
//   - actor comes from the auth context (ActorFromRequest), never the body
//   - request bodies carry only decision, expected status, and bounded
//     reason/evidence_ref text; message content and credentials are never
//     accepted, stored, or echoed
//   - responses expose the content-free effect and attempt projections only

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/web"
)

const (
	// effectListDefaultLimit / effectListMaxLimit bound the operator effect
	// list. They match the fence list: deliveries are rare enough that a
	// tighter cap costs nothing and keeps the response bounded.
	effectListDefaultLimit = 100
	effectListMaxLimit     = 500

	// effectReasonMaxLen / effectEvidenceMaxLen match the fence action bounds
	// so operators learn one rule, not two.
	effectReasonMaxLen   = 512
	effectEvidenceMaxLen = 256
)

// EffectListItem is the wire shape of one delivery effect. Deliberately
// narrow: no content, no payload body, nothing derived from a message.
type EffectListItem struct {
	EffectID       string `json:"effect_id"`
	OccurrenceID   string `json:"occurrence_id"`
	SessionID      string `json:"session_id,omitempty"`
	ExecutionID    string `json:"execution_id,omitempty"`
	DeliveryStatus string `json:"delivery_status"`
	TargetKind     string `json:"target_kind"`
	TargetRef      string `json:"target_ref,omitempty"`
	Attempt        int64  `json:"attempt"`
	ErrorCode      string `json:"error_code,omitempty"`
	Reason         string `json:"reason,omitempty"`
	ProviderRef    string `json:"provider_ref,omitempty"`
	EvidenceRef    string `json:"evidence_ref,omitempty"`
	LeaseVersion   int64  `json:"lease_version"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// EffectAttemptItem is the wire shape of one send attempt. The lease token is
// a write credential and is never projected.
type EffectAttemptItem struct {
	Attempt        int64  `json:"attempt"`
	OwnerInstance  string `json:"owner_instance_id"`
	LeaseVersion   int64  `json:"lease_version"`
	StartedAt      int64  `json:"started_at"`
	FinishedAt     *int64 `json:"finished_at,omitempty"`
	InFlight       bool   `json:"in_flight"`
	Outcome        string `json:"outcome,omitempty"`
	RejectionClass string `json:"rejection_class,omitempty"`
	ProviderRef    string `json:"provider_ref,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

func effectListItem(e *effect.Effect) EffectListItem {
	return EffectListItem{
		EffectID:       e.EffectID,
		OccurrenceID:   e.OccurrenceID,
		SessionID:      e.SessionID,
		ExecutionID:    e.ExecutionID,
		DeliveryStatus: string(e.Status),
		TargetKind:     e.TargetKind,
		TargetRef:      e.TargetRef,
		Attempt:        e.Attempt,
		ErrorCode:      e.ErrorCode,
		Reason:         e.Reason,
		ProviderRef:    e.ProviderRef,
		EvidenceRef:    e.EvidenceRef,
		LeaseVersion:   e.LeaseVersion,
		CreatedAt:      e.CreatedAtMs,
		UpdatedAt:      e.UpdatedAtMs,
	}
}

func effectAttemptItem(a *effect.Attempt) EffectAttemptItem {
	return EffectAttemptItem{
		Attempt:        a.Attempt,
		OwnerInstance:  a.OwnerInstanceID,
		LeaseVersion:   a.LeaseVersion,
		StartedAt:      a.StartedAtMs,
		FinishedAt:     a.FinishedAtMs,
		InFlight:       a.InFlight(),
		Outcome:        string(a.Outcome),
		RejectionClass: a.RejectionClass,
		ProviderRef:    a.ProviderRef,
		ErrorCode:      a.ErrorCode,
		Reason:         a.Reason,
	}
}

func parseEffectPagination(r *http.Request) (limit, offset int) {
	limit = effectListDefaultLimit
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	if limit > effectListMaxLimit {
		limit = effectListMaxLimit
	}
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o >= 0 {
		offset = o
	}
	return limit, offset
}

// HandleListEffects lists delivery effects for operator inspection.
//
// @Summary      List delivery effects
// @Description  Lists external-delivery effects with their lifecycle state. Requires runtime:read scope.
// @Tags         Admin API
// @Produce      json
// @Security     AdminBearerAuth
// @Param        status  query    string  false  "Filter by delivery status"
// @Param        limit   query    int     false  "Max results (default 100, max 500)"
// @Param        offset  query    int     false  "Pagination offset"
// @Success      200  {object}  map[string]any  "effects list"
// @Failure      403  {object}  ErrorResponse  "Insufficient scope: need runtime:read"
// @Failure      503  {object}  ErrorResponse  "Effect store not configured"
// @Router       /admin/effects [get]
func (a *AdminAPI) HandleListEffects(w http.ResponseWriter, r *http.Request) {
	if !hasScope(r, ScopeRuntimeRead) {
		web.WriteAppError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE",
			"insufficient scope: need runtime:read")
		return
	}
	if a.runtimeEffects == nil {
		web.WriteAppError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"effect store not configured")
		return
	}

	limit, offset := parseEffectPagination(r)
	filter := effect.OperatorListFilter{
		Status: effect.Status(strings.TrimSpace(r.URL.Query().Get("status"))),
		Limit:  limit,
		Offset: offset,
	}

	effects, err := a.runtimeEffects.ListForOperator(r.Context(), filter)
	if err != nil {
		a.log.Error("admin: list effects", "err", err)
		web.WriteAppError(w, http.StatusInternalServerError, "INTERNAL", "failed to list effects")
		return
	}

	items := make([]EffectListItem, 0, len(effects))
	for _, e := range effects {
		items = append(items, effectListItem(e))
	}
	respondJSON(w, map[string]any{
		"effects": items,
		"limit":   limit,
		"offset":  offset,
	})
}

// HandleGetEffect returns one effect with its per-attempt history.
//
// @Summary      Inspect a delivery effect
// @Description  Returns one effect plus every send attempt, so an operator can see what each try actually established. Requires runtime:read scope.
// @Tags         Admin API
// @Produce      json
// @Security     AdminBearerAuth
// @Param        id  path  string  true  "Effect ID"
// @Success      200  {object}  map[string]any  "effect and attempts"
// @Failure      403  {object}  ErrorResponse  "Insufficient scope: need runtime:read"
// @Failure      404  {object}  ErrorResponse  "Effect not found"
// @Failure      503  {object}  ErrorResponse  "Effect store not configured"
// @Router       /admin/effects/{id} [get]
func (a *AdminAPI) HandleGetEffect(w http.ResponseWriter, r *http.Request) {
	if !hasScope(r, ScopeRuntimeRead) {
		web.WriteAppError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE",
			"insufficient scope: need runtime:read")
		return
	}
	if a.runtimeEffects == nil {
		web.WriteAppError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"effect store not configured")
		return
	}

	effectID := strings.TrimSpace(r.PathValue("id"))
	if effectID == "" {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", "effect id is required")
		return
	}

	e, err := a.runtimeEffects.GetByID(r.Context(), effectID)
	if err != nil {
		a.writeEffectError(w, r, effectID, "inspect", err)
		return
	}
	attempts, err := a.runtimeEffects.ListAttempts(r.Context(), effectID)
	if err != nil {
		a.writeEffectError(w, r, effectID, "inspect", err)
		return
	}

	items := make([]EffectAttemptItem, 0, len(attempts))
	for _, at := range attempts {
		items = append(items, effectAttemptItem(at))
	}
	respondJSON(w, map[string]any{
		"effect":   effectListItem(e),
		"attempts": items,
	})
}

// effectActionBody is the operator decision payload. Bounded free-text only.
type effectActionBody struct {
	Decision string `json:"decision"`
	// ExpectedStatus is the status the operator saw. The write is refused
	// unless the effect is still in exactly that state.
	ExpectedStatus string `json:"expected_status"`
	Reason         string `json:"reason"`
	EvidenceRef    string `json:"evidence_ref"`
}

// HandleEffectAction applies an operator decision to an uncertain delivery.
//
// @Summary      Apply operator effect action
// @Description  Abandons, confirms or requeues an unknown delivery effect. Conditional on expected_status; conflicts return 409 so the operator re-inspects instead of double-deciding. Requires runtime:write scope.
// @Tags         Admin API
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Param        id    path   string            true  "Effect ID"
// @Param        body  body   effectActionBody  true  "Operator decision"
// @Success      200   {object}  map[string]any  "Applied decision and updated effect fact"
// @Failure      400   {object}  ErrorResponse  "Invalid decision, status, reason or evidence_ref"
// @Failure      403   {object}  ErrorResponse  "Insufficient scope: need runtime:write"
// @Failure      404   {object}  ErrorResponse  "Effect not found"
// @Failure      409   {object}  ErrorResponse  "Effect is no longer unknown: re-inspect before retrying"
// @Failure      503   {object}  ErrorResponse  "Effect store not configured or timed out"
// @Router       /admin/effects/{id}/action [post]
func (a *AdminAPI) HandleEffectAction(w http.ResponseWriter, r *http.Request) {
	if !hasScope(r, ScopeRuntimeWrite) {
		web.WriteAppError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE",
			"insufficient scope: need runtime:write")
		return
	}
	if a.runtimeEffects == nil {
		web.WriteAppError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"effect store not configured")
		return
	}

	effectID := strings.TrimSpace(r.PathValue("id"))
	if effectID == "" {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", "effect id is required")
		return
	}

	var body effectActionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body")
		return
	}

	decision := effect.OperatorDecision(strings.TrimSpace(body.Decision))
	switch decision {
	case effect.OperatorAbandon, effect.OperatorMarkDelivered, effect.OperatorRequeue:
	default:
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST",
			"decision must be abandon, mark_delivered or requeue")
		return
	}
	expected := effect.Status(strings.TrimSpace(body.ExpectedStatus))
	if expected != effect.StatusUnknown {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST",
			"expected_status must be unknown: only an uncertain delivery accepts a decision")
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" || len(reason) > effectReasonMaxLen {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST",
			"reason must be 1-512 characters")
		return
	}
	evidenceRef := strings.TrimSpace(body.EvidenceRef)
	if len(evidenceRef) > effectEvidenceMaxLen {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST",
			"evidence_ref must be at most 256 characters")
		return
	}

	updated, err := a.runtimeEffects.ApplyOperatorAction(r.Context(), effect.OperatorActionRequest{
		EffectID:       effectID,
		Decision:       decision,
		ExpectedStatus: expected,
		Reason:         reason,
		EvidenceRef:    evidenceRef,
		Now:            time.Now(),
	})
	a.auditEffectAction(r, effectID, decision, reason, evidenceRef, err)
	if err != nil {
		a.writeEffectError(w, r, effectID, string(decision), err)
		return
	}

	observability.RuntimeEffectActions().Add(r.Context(), 1, metric.WithAttributes(
		attribute.String("decision", string(decision)),
		attribute.String("result", "ok"),
	))
	a.log.Info("admin: effect action applied",
		"decision", string(decision),
		"effect_id", effectID,
		"occurrence_id", updated.OccurrenceID,
		"status", updated.Status,
		"actor", ActorFromRequest(r),
	)

	respondJSON(w, map[string]any{
		"decision": string(decision),
		"effect":   effectListItem(updated),
	})
}

// writeEffectError maps ledger errors to operator-actionable statuses.
func (a *AdminAPI) writeEffectError(
	w http.ResponseWriter, r *http.Request, effectID, action string, err error,
) {
	status := http.StatusInternalServerError
	code := "INTERNAL"
	msg := "failed to read or change the delivery effect"
	result := "error"
	switch {
	case errors.Is(err, effect.ErrEffectNotFound):
		status, code, msg = http.StatusNotFound, "EFFECT_NOT_FOUND", "effect not found"
	case errors.Is(err, effect.ErrLeaseLost):
		// 409, never an automatic retry: the operator must re-inspect and
		// decide again against what is actually recorded.
		status, code, msg = http.StatusConflict, "EFFECT_CONFLICT",
			"effect is no longer unknown: re-inspect before deciding"
		result = "conflict"
	case errors.Is(err, effect.ErrNotClaimable):
		status, code, msg = http.StatusBadRequest, "BAD_REQUEST", "invalid effect decision"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, msg = http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "effect store timeout"
	}

	if action != "inspect" {
		observability.RuntimeEffectActions().Add(r.Context(), 1, metric.WithAttributes(
			attribute.String("decision", action),
			attribute.String("result", result),
		))
	}
	if result == "conflict" {
		observability.RuntimeEffectConflicts().Add(r.Context(), 1)
	}
	if status >= 500 {
		a.log.Error("admin: effect operation failed", "err", err,
			"effect_id", effectID, "action", action)
	}
	web.WriteAppError(w, status, code, msg)
}

// auditEffectAction records the operator decision into the tamper-evident
// user_activity table with bounded rationale. The reason reaches this audit
// row and the effect's operator-attributed reason field — never the message
// content, and never as something the provider said.
func (a *AdminAPI) auditEffectAction(
	r *http.Request, effectID string, decision effect.OperatorDecision,
	reason, evidenceRef string, applyErr error,
) {
	if a.auditCollector == nil {
		return
	}
	action := AuditRuntimeEffectAbandon
	switch decision {
	case effect.OperatorMarkDelivered:
		action = AuditRuntimeEffectMarkDelivered
	case effect.OperatorRequeue:
		action = AuditRuntimeEffectRequeue
	}
	outcome := audit.OutcomeSuccess
	result := AuditResultOk
	if applyErr != nil {
		outcome = audit.OutcomeFailure
		result = AuditResultFailed
	}
	detail, _ := json.Marshal(map[string]any{
		"effect_id":    effectID,
		"decision":     string(decision),
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
		ResourceType: "runtime_effect",
		Outcome:      outcome,
		DetailJSON:   string(detail),
		IP:           clientIP(r),
		UserAgent:    r.UserAgent(),
	})
}
