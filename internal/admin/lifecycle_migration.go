package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hrygo/hotplex/internal/web"
)

var (
	ErrLifecycleMigrationConfirmationRequired = errors.New("admin: lifecycle migration confirmation required")
	ErrLifecycleMigrationPlanExpired          = errors.New("admin: lifecycle migration preview expired")
	ErrLifecycleMigrationPlanStale            = errors.New("admin: lifecycle migration preview is stale")
)

// LifecycleMigrationProvider keeps the admin package independent of the
// database-backed lifecycle implementation.
type LifecycleMigrationProvider interface {
	Preview(context.Context) (*LifecycleMigrationPreview, error)
	Apply(context.Context, string, string) (*LifecycleMigrationApplyResult, error)
}

type LifecycleMigrationSummary struct {
	Count          int64      `json:"count"`
	Bytes          int64      `json:"bytes"`
	OldestDeadline *time.Time `json:"oldest_deadline,omitempty"`
}

type LifecycleMigrationBlocked struct {
	LegacySessionsMissingInputClock int64 `json:"legacy_sessions_missing_input_clock"`
	OrphanedEvents                  int64 `json:"orphaned_events"`
	OrphanedTurns                   int64 `json:"orphaned_turns"`
	ContentOnSessionsMissingClock   int64 `json:"content_on_sessions_missing_input_clock"`
	UnknownExecutionRecords         int64 `json:"unknown_execution_records"`
	ActiveExecutionRecords          int64 `json:"active_execution_records"`
}

type LifecycleMigrationPreview struct {
	PlanID         string                    `json:"plan_id"`
	CreatedAt      time.Time                 `json:"created_at"`
	ExpiresAt      time.Time                 `json:"expires_at"`
	PolicyRevision string                    `json:"policy_revision"`
	BatchSize      int                       `json:"batch_size"`
	Sessions       LifecycleMigrationSummary `json:"sessions"`
	Events         LifecycleMigrationSummary `json:"events"`
	Turns          LifecycleMigrationSummary `json:"turns"`
	Blocked        LifecycleMigrationBlocked `json:"blocked"`
}

type LifecycleMigrationApplyResult struct {
	Sessions int64 `json:"sessions"`
	Events   int64 `json:"events"`
	Turns    int64 `json:"turns"`
}

type lifecycleMigrationApplyRequest struct {
	PlanID        string `json:"plan_id"`
	ConfirmPlanID string `json:"confirm_plan_id"`
}

// SetLifecycleMigration injects the explicitly invoked legacy retention
// migration surface. Nil disables both endpoints with a 503 response.
func (a *AdminAPI) SetLifecycleMigration(provider LifecycleMigrationProvider) {
	a.lifecycleMigration = provider
}

// HandleLifecycleMigrationPreview creates a read-only aggregate preview. The
// resulting short-lived plan can be applied only through the separate write
// endpoint with its exact plan ID repeated as confirmation.
func (a *AdminAPI) HandleLifecycleMigrationPreview(w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, ScopeAdminRead) {
		return
	}
	if a.lifecycleMigration == nil {
		web.WriteAppError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "lifecycle migration is not available")
		return
	}
	preview, err := a.lifecycleMigration.Preview(r.Context())
	if err != nil {
		a.logLifecycleMigrationError("preview", err)
		web.WriteAppError(w, http.StatusInternalServerError, "INTERNAL", "failed to preview lifecycle migration")
		return
	}
	respondJSON(w, preview)
}

// HandleLifecycleMigrationApply applies one bounded batch from a previously
// previewed snapshot after explicit plan-ID confirmation.
func (a *AdminAPI) HandleLifecycleMigrationApply(w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, ScopeAdminWrite) {
		return
	}
	if a.lifecycleMigration == nil {
		web.WriteAppError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "lifecycle migration is not available")
		return
	}
	var request lifecycleMigrationApplyRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		web.WriteAppError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid lifecycle migration request")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		web.WriteAppError(w, http.StatusBadRequest, "INVALID_REQUEST", "request must contain a single JSON object")
		return
	}
	request.PlanID = strings.TrimSpace(request.PlanID)
	request.ConfirmPlanID = strings.TrimSpace(request.ConfirmPlanID)
	if request.PlanID == "" || request.PlanID != request.ConfirmPlanID {
		web.WriteAppError(w, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "plan_id and confirm_plan_id must match")
		return
	}
	result, err := a.lifecycleMigration.Apply(r.Context(), request.PlanID, request.ConfirmPlanID)
	switch {
	case errors.Is(err, ErrLifecycleMigrationConfirmationRequired):
		web.WriteAppError(w, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "plan_id and confirm_plan_id must match")
	case errors.Is(err, ErrLifecycleMigrationPlanExpired):
		web.WriteAppError(w, http.StatusConflict, "PREVIEW_EXPIRED", "lifecycle migration preview expired; create a new preview")
	case errors.Is(err, ErrLifecycleMigrationPlanStale):
		web.WriteAppError(w, http.StatusConflict, "PREVIEW_STALE", "lifecycle data or policy changed; create a new preview")
	case err != nil:
		a.logLifecycleMigrationError("apply", err)
		web.WriteAppError(w, http.StatusInternalServerError, "INTERNAL", "failed to apply lifecycle migration")
	default:
		respondJSON(w, result)
	}
}

func (a *AdminAPI) logLifecycleMigrationError(operation string, err error) {
	if a.log != nil {
		a.log.Error("admin: lifecycle migration "+operation, "err", err)
	}
}
