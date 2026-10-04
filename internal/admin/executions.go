package admin

// Execution console: a bounded, redacted view of one run's history (plan §5.9,
// issue #868).
//
// The console answers "where did this run stop, and what is safe to do next?".
// Three constraints shape everything here:
//
//   - Bounded. Every query is capped and every response says whether it was
//     truncated. A timeline that silently returns the first N rows of an
//     unbounded scan is worse than one that admits it stopped.
//   - Historical. A run's facts are read from the record of THAT run. The
//     session's current state is never substituted for what happened then; a
//     console that overwrites history with "now" teaches operators to distrust
//     it.
//   - Content-free. Event payloads, prompts, tool arguments and provider
//     responses never cross this boundary. Events are projected to their type,
//     direction, source and time only.
//
// The UI is not trusted to decide what is safe. Actions are computed here from
// the record's current state and version, and returned as a list the UI can
// only render — there is deliberately no "force success".

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/web"
)

const (
	execListDefaultLimit = 50
	execListMaxLimit     = 100

	// Timeline caps. These are per response, not per item: the projection
	// fetches each source once, bounded.
	timelineMaxEvents  = 200
	timelineMaxEffects = 50

	// A time window wider than this is refused rather than silently clamped:
	// an operator asking for "everything" should be told the window is too wide
	// instead of receiving a page that looks complete.
	timelineMaxWindowMs = int64(7 * 24 * 60 * 60 * 1000)
)

// ExecutionReader is the read-only execution view the console needs.
type ExecutionReader interface {
	ListRecent(ctx context.Context, filter execution.ListFilter) ([]*execution.Record, error)
	ByID(ctx context.Context, executionID string) (*execution.Record, error)
}

// EventFact is a content-free projection of one stored event. It deliberately
// omits Data: an event payload can carry assistant text or tool arguments.
type EventFact struct {
	Seq       int64
	Type      string
	Direction string
	Source    string
	CreatedAt int64
}

// SessionEventReader returns bounded event facts for a session, plus whether
// more exist beyond the bound.
type SessionEventReader interface {
	RecentEvents(ctx context.Context, sessionID string, limit int) (events []EventFact, hasMore bool, err error)
}

// ExecutionEffectReader returns the bounded external deliveries of one run.
type ExecutionEffectReader interface {
	EffectsForExecution(ctx context.Context, executionID string, limit int) ([]*effect.Effect, error)
}

// ExecutionSummary is the wire shape of one execution in the console. It is
// the same deliberately narrow projection the fence list uses.
type ExecutionSummary struct {
	ExecutionID      string `json:"execution_id"`
	SessionID        string `json:"session_id"`
	DeliveryStatus   string `json:"delivery_status"`
	RuntimeStatus    string `json:"runtime_status"`
	RuntimeErrorCode string `json:"runtime_error_code,omitempty"`
	WorkerRunID      string `json:"worker_run_id,omitempty"`
	FenceReason      string `json:"fence_reason,omitempty"`
	FenceVersion     int64  `json:"fence_version"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
	StartedAt        *int64 `json:"started_at,omitempty"`
	FinishedAt       *int64 `json:"finished_at,omitempty"`
	FenceCreatedAt   *int64 `json:"fence_created_at,omitempty"`
}

func executionSummary(rec *execution.Record) ExecutionSummary {
	return ExecutionSummary{
		ExecutionID:      rec.ExecutionID,
		SessionID:        rec.SessionID,
		DeliveryStatus:   string(rec.Status),
		RuntimeStatus:    string(rec.RuntimeStatus),
		RuntimeErrorCode: rec.RuntimeErrorCode,
		WorkerRunID:      rec.WorkerRunID,
		FenceReason:      rec.FenceReason,
		FenceVersion:     rec.FenceVersion,
		CreatedAt:        rec.CreatedAt,
		UpdatedAt:        rec.UpdatedAt,
		StartedAt:        rec.StartedAt,
		FinishedAt:       rec.FinishedAt,
		FenceCreatedAt:   rec.FenceCreatedAt,
	}
}

// evidenceState distinguishes "we looked and there is nothing" from "we never
// recorded it". The plan requires the latter to read as missing history rather
// than as success or failure.
type evidenceState string

const (
	evidenceRecorded   evidenceState = "recorded"
	evidenceNotStored  evidenceState = "not_recorded_for_this_run"
	evidenceNotQueried evidenceState = "unavailable"
)

// Notes are stable codes, not sentences. A client has to render them in its own
// language, and an operator comparing two runs needs the same string to mean
// the same thing; English prose in the payload would satisfy neither. Unknown
// codes are shown verbatim rather than swallowed.
const (
	noteEventsUnavailable    = "events_unavailable"
	noteEventsNotConfigured  = "events_not_configured"
	noteEventsTruncated      = "events_truncated"
	noteEffectsUnavailable   = "effects_unavailable"
	noteEffectsNotConfigured = "effects_not_configured"
	noteEffectsTruncated     = "effects_truncated"
	noteNoDeliveryPlanned    = "no_delivery_planned"
)

// TimelineItem is one bounded, redacted fact in a run's history.
//
// FactTime is when the thing happened according to the system that recorded
// it; ObservedTime is when this projection read it. They are kept apart
// because a delayed event (an effect receipt, a late Done) legitimately has
// the two differ, and collapsing them hides exactly the lag an operator is
// trying to diagnose.
type TimelineItem struct {
	Phase       string        `json:"phase"`
	Source      string        `json:"source"`
	Kind        string        `json:"kind"`
	FactTime    int64         `json:"fact_time"`
	ObservedAt  int64         `json:"observed_at"`
	ExecutionID string        `json:"execution_id,omitempty"`
	WorkerRunID string        `json:"worker_run_id,omitempty"`
	EffectID    string        `json:"effect_id,omitempty"`
	Evidence    string        `json:"evidence,omitempty"`
	EvidenceSt  evidenceState `json:"evidence_state,omitempty"`
	Truncated   bool          `json:"truncated,omitempty"`
}

// AvailableAction is an action the SERVER says is currently valid, with the
// conditional token it must be submitted with. The UI renders these; it never
// derives them.
type AvailableAction struct {
	Kind string `json:"kind"`
	// Target names what the action applies to: "execution" or an effect ID.
	Target string `json:"target"`
	// RequiresVersion is the fencing token the action must carry.
	RequiresVersion int64 `json:"requires_version,omitempty"`
	// Description is operator-facing text, bounded and free of user content.
	Description string `json:"description"`
}

// ExecutionTimeline is the detail projection for one run.
type ExecutionTimeline struct {
	Execution ExecutionSummary  `json:"execution"`
	Items     []TimelineItem    `json:"items"`
	Actions   []AvailableAction `json:"actions"`
	// PlanEvidence is explicit about absence: the launch plan is recorded per
	// session for the CURRENT run only, so a historical execution usually has
	// none. Substituting the session's present plan would rewrite history.
	PlanEvidence evidenceState `json:"plan_evidence"`
	// EffectEvidence is "not_recorded_for_this_run" when the run never planned
	// an external delivery, which is most runs.
	EffectEvidence evidenceState `json:"effect_evidence"`
	Truncated      bool          `json:"truncated"`
	// Notes are stable codes (see the note* constants), not prose.
	Notes []string `json:"notes,omitempty"`
}

// SetExecutionConsole wires the three read-only views. Each is optional; a
// missing one degrades that section to "unavailable" rather than failing the
// whole projection.
func (a *AdminAPI) SetExecutionConsole(
	executions ExecutionReader,
	events SessionEventReader,
	effects ExecutionEffectReader,
) {
	a.consoleExecutions = executions
	a.consoleEvents = events
	a.consoleEffects = effects
}

// SetConsoleEffects wires the delivery view separately so a deployment with
// effects disabled still gets an execution timeline, with its delivery section
// reported as unavailable rather than absent.
func (a *AdminAPI) SetConsoleEffects(effects ExecutionEffectReader) {
	a.consoleEffects = effects
}

func (a *AdminAPI) consoleEnabled() bool {
	return a.consoleExecutions != nil
}

// parseExecListQuery validates the list query. An invalid or oversized request
// is refused rather than silently narrowed: a console that quietly shows less
// than asked for is how an operator misses the run they were looking for.
func parseExecListQuery(r *http.Request) (execution.ListFilter, error) {
	q := r.URL.Query()
	filter := execution.ListFilter{
		SessionID:      strings.TrimSpace(q.Get("session_id")),
		DeliveryStatus: execution.Status(strings.TrimSpace(q.Get("delivery_status"))),
		RuntimeStatus:  execution.RuntimeStatus(strings.TrimSpace(q.Get("runtime_status"))),
		Limit:          execListDefaultLimit,
	}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return filter, errors.New("limit must be a positive integer")
		}
		if n > execListMaxLimit {
			return filter, errors.New("limit exceeds the maximum of 100")
		}
		filter.Limit = n
	}
	if raw := strings.TrimSpace(q.Get("before_created_at")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return filter, errors.New("before_created_at must be a non-negative epoch milliseconds value")
		}
		filter.BeforeCreatedAt = n
		filter.BeforeExecutionID = strings.TrimSpace(q.Get("before_execution_id"))
		if filter.BeforeExecutionID == "" {
			return filter, errors.New("before_execution_id is required with before_created_at")
		}
	}
	since, err := optionalMillis(q.Get("since_ms"))
	if err != nil {
		return filter, errors.New("since_ms must be epoch milliseconds")
	}
	until, err := optionalMillis(q.Get("until_ms"))
	if err != nil {
		return filter, errors.New("until_ms must be epoch milliseconds")
	}
	filter.SinceMs, filter.UntilMs = since, until
	if filter.SinceMs > 0 && filter.UntilMs > 0 && filter.UntilMs <= filter.SinceMs {
		return filter, errors.New("until_ms must be after since_ms")
	}
	if filter.DeliveryStatus != "" && !validDeliveryStatus(filter.DeliveryStatus) {
		return filter, errors.New("delivery_status is not a known status")
	}
	if filter.RuntimeStatus != "" && !validRuntimeStatus(filter.RuntimeStatus) {
		return filter, errors.New("runtime_status is not a known status")
	}
	return filter, nil
}

func optionalMillis(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("not a non-negative integer")
	}
	return n, nil
}

func validDeliveryStatus(s execution.Status) bool {
	switch s {
	case execution.StatusAccepted, execution.StatusDelivered,
		execution.StatusUnknown, execution.StatusFailed:
		return true
	}
	return false
}

func validRuntimeStatus(s execution.RuntimeStatus) bool {
	switch s {
	case execution.RuntimeQueued, execution.RuntimePending, execution.RuntimeRunning,
		execution.RuntimeCompleted, execution.RuntimeFailed, execution.RuntimeUnknown:
		return true
	}
	return false
}

// ListExecutions returns a bounded page of executions, newest first.
//
// @Summary      List executions
// @Description  Returns a bounded, newest-first page of execution records for the operator console. Supports keyset pagination and optional session/status/time filters. Requires runtime:read scope.
// @Tags         Admin API
// @Produce      json
// @Security     AdminBearerAuth
// @Param        session_id        query  string  false  "Filter by session"
// @Param        delivery_status   query  string  false  "Filter by delivery status"
// @Param        runtime_status    query  string  false  "Filter by runtime status"
// @Param        since_ms          query  integer false  "Only executions created at or after this epoch ms"
// @Param        until_ms          query  integer false  "Only executions created before this epoch ms"
// @Param        before_created_at query  integer false  "Keyset cursor: created_at"
// @Param        before_execution_id query string false "Keyset cursor: execution_id"
// @Param        limit             query  integer false  "Page size (default 50, max 100)"
// @Success      200  {object}  map[string]any  "Executions page"
// @Failure      400  {object}  ErrorResponse  "Invalid filter or pagination"
// @Failure      403  {object}  ErrorResponse  "Insufficient scope: need runtime:read"
// @Failure      503  {object}  ErrorResponse  "Execution store not configured"
// @Router       /admin/executions [get]
func (a *AdminAPI) ListExecutions(w http.ResponseWriter, r *http.Request) {
	if !hasScope(r, ScopeRuntimeRead) {
		web.WriteAppError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE",
			"insufficient scope: need runtime:read")
		return
	}
	if !a.consoleEnabled() {
		web.WriteAppError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"execution console not configured")
		return
	}

	filter, err := parseExecListQuery(r)
	if err != nil {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}

	records, err := a.consoleExecutions.ListRecent(r.Context(), filter)
	if err != nil {
		if errors.Is(err, execution.ErrNotFound) {
			web.WriteAppError(w, http.StatusNotFound, "NOT_FOUND", "execution not found")
			return
		}
		web.WriteAppError(w, http.StatusInternalServerError, "INTERNAL",
			"failed to list executions")
		return
	}

	items := make([]ExecutionSummary, 0, len(records))
	for _, rec := range records {
		items = append(items, executionSummary(rec))
	}

	// next_cursor is present only when another page may exist. It repeats the
	// last row's key so the client can page without guessing at an offset that
	// new rows would invalidate.
	var nextCursor map[string]any
	if len(records) == filter.Limit && len(records) > 0 {
		last := records[len(records)-1]
		nextCursor = map[string]any{
			"before_created_at":   last.CreatedAt,
			"before_execution_id": last.ExecutionID,
		}
	}

	observability.RuntimeConsoleQueries().Add(r.Context(), 1, metric.WithAttributes(
		attribute.String("view", "list")))

	respondJSON(w, map[string]any{
		"executions":  items,
		"next_cursor": nextCursor,
	})
}

// GetExecutionTimeline projects one run's history from three bounded sources.
//
// @Summary      Get one execution's timeline
// @Description  Returns a bounded, redacted projection of a single execution: its control facts, a capped window of session events, and the external deliveries planned for it. Reports explicitly when plan or effect evidence was never recorded for THIS run, and offers only the actions valid for the record's current state. The notes array carries stable codes (events_unavailable, events_not_configured, events_truncated, effects_unavailable, effects_not_configured, effects_truncated, no_delivery_planned) rather than prose. Requires runtime:read scope.
// @Tags         Admin API
// @Produce      json
// @Security     AdminBearerAuth
// @Param        id       path   string  true   "Execution ID"
// @Param        since_ms query  integer false  "Only events at or after this epoch ms"
// @Param        until_ms query  integer false  "Only events before this epoch ms"
// @Success      200  {object}  ExecutionTimeline
// @Failure      400  {object}  ErrorResponse  "Invalid time window"
// @Failure      403  {object}  ErrorResponse  "Insufficient scope: need runtime:read"
// @Failure      404  {object}  ErrorResponse  "Execution not found"
// @Failure      503  {object}  ErrorResponse  "Execution console not configured"
// @Router       /admin/executions/{id}/timeline [get]
func (a *AdminAPI) GetExecutionTimeline(w http.ResponseWriter, r *http.Request) {
	if !hasScope(r, ScopeRuntimeRead) {
		web.WriteAppError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE",
			"insufficient scope: need runtime:read")
		return
	}
	if !a.consoleEnabled() {
		web.WriteAppError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"execution console not configured")
		return
	}

	executionID := strings.TrimSpace(r.PathValue("id"))
	if executionID == "" {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", "execution id is required")
		return
	}

	q := r.URL.Query()
	since, err := optionalMillis(q.Get("since_ms"))
	if err != nil {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", "since_ms must be epoch milliseconds")
		return
	}
	until, err := optionalMillis(q.Get("until_ms"))
	if err != nil {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", "until_ms must be epoch milliseconds")
		return
	}
	if since > 0 && until > 0 && until <= since {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST", "until_ms must be after since_ms")
		return
	}
	if until-since > timelineMaxWindowMs {
		web.WriteAppError(w, http.StatusBadRequest, "BAD_REQUEST",
			"time window exceeds the maximum of 7 days; narrow it rather than receiving a truncated page")
		return
	}

	record, err := a.consoleExecutions.ByID(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, execution.ErrNotFound) {
			web.WriteAppError(w, http.StatusNotFound, "EXECUTION_NOT_FOUND", "execution not found")
			return
		}
		web.WriteAppError(w, http.StatusInternalServerError, "INTERNAL",
			"failed to load execution")
		return
	}

	timeline := a.buildTimeline(r.Context(), record, since, until)

	observability.RuntimeConsoleQueries().Add(r.Context(), 1, metric.WithAttributes(
		attribute.String("view", "timeline")))

	respondJSON(w, timeline)
}

// buildTimeline assembles the projection. Each source is read at most once and
// bounded; a source that is not configured degrades to "unavailable" rather
// than failing the whole view, because a partial honest timeline is more
// useful than none.
func (a *AdminAPI) buildTimeline(
	ctx context.Context,
	rec *execution.Record,
	sinceMs, untilMs int64,
) ExecutionTimeline {
	out := ExecutionTimeline{
		Execution: executionSummary(rec),
		Items:     []TimelineItem{},
		Actions:   []AvailableAction{},
		Notes:     []string{},
		// The launch plan is recorded per session for the CURRENT run only.
		// A historical execution therefore has none, and saying so is the
		// honest answer; borrowing the session's present plan would rewrite
		// what this run was launched under.
		PlanEvidence:   evidenceNotStored,
		EffectEvidence: evidenceNotStored,
	}

	now := nowMillis()
	out.Items = append(out.Items, controlFacts(rec, now)...)

	if a.consoleEvents != nil {
		events, hasMore, err := a.consoleEvents.RecentEvents(ctx, rec.SessionID, timelineMaxEvents)
		if err != nil {
			out.Notes = append(out.Notes, noteEventsUnavailable)
		} else {
			for _, ev := range events {
				if sinceMs > 0 && ev.CreatedAt < sinceMs {
					continue
				}
				if untilMs > 0 && ev.CreatedAt >= untilMs {
					continue
				}
				out.Items = append(out.Items, TimelineItem{
					Phase:      timelinePhaseFor(rec, ev.Type),
					Source:     "event_store",
					Kind:       ev.Type,
					FactTime:   ev.CreatedAt,
					ObservedAt: now,
					Evidence:   ev.Direction,
					EvidenceSt: evidenceRecorded,
				})
			}
			if hasMore {
				out.Truncated = true
				out.Items = append(out.Items, TimelineItem{
					Phase:      "truncated",
					Source:     "event_store",
					Kind:       "window_truncated",
					ObservedAt: now,
					Truncated:  true,
				})
				out.Notes = append(out.Notes, noteEventsTruncated)
			}
		}
	} else {
		out.Notes = append(out.Notes, noteEventsNotConfigured)
	}

	var loadedEffects []*effect.Effect
	if a.consoleEffects != nil {
		effects, err := a.consoleEffects.EffectsForExecution(ctx, rec.ExecutionID, timelineMaxEffects)
		loadedEffects = effects
		switch {
		case err != nil:
			out.Notes = append(out.Notes, noteEffectsUnavailable)
		case len(effects) == 0:
			out.Notes = append(out.Notes, noteNoDeliveryPlanned)
		default:
			out.EffectEvidence = evidenceRecorded
			for _, eff := range effects {
				out.Items = append(out.Items, effectItem(eff, rec, now))
			}
			if len(effects) == timelineMaxEffects {
				out.Truncated = true
				out.Notes = append(out.Notes, noteEffectsTruncated)
			}
		}
	} else {
		out.Notes = append(out.Notes, noteEffectsNotConfigured)
	}

	// Actions come from the record and the effects already loaded above, never a
	// second query: the projection must cost a fixed number of reads.
	out.Actions = availableActions(rec, loadedEffects)
	return out
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// controlFacts projects the execution record itself into timeline items. These
// are the authoritative facts: every other source is a projection of something
// that happened to this run, while these ARE the run.
func controlFacts(rec *execution.Record, observedAt int64) []TimelineItem {
	items := []TimelineItem{{
		Phase:       phaseForRuntime(rec.RuntimeStatus),
		Source:      "execution_store",
		Kind:        string(rec.Status),
		FactTime:    rec.CreatedAt,
		ObservedAt:  observedAt,
		ExecutionID: rec.ExecutionID,
		Evidence:    string(rec.RuntimeStatus),
		EvidenceSt:  evidenceRecorded,
	}}

	appendAt := func(phase, kind string, at int64, evidence string) {
		if at <= 0 {
			return
		}
		items = append(items, TimelineItem{
			Phase:       phase,
			Source:      "execution_store",
			Kind:        kind,
			FactTime:    at,
			ObservedAt:  observedAt,
			ExecutionID: rec.ExecutionID,
			WorkerRunID: rec.WorkerRunID,
			Evidence:    evidence,
			EvidenceSt:  evidenceRecorded,
		})
	}
	deref := func(v *int64) int64 {
		if v == nil {
			return 0
		}
		return *v
	}
	appendAt("dispatched", "worker_run_assigned", deref(rec.StartedAt), rec.WorkerRunID)
	appendAt("terminal", string(rec.RuntimeStatus), deref(rec.FinishedAt), rec.RuntimeErrorCode)

	if rec.FenceReason != "" {
		fenceAt := int64(0)
		if rec.FenceCreatedAt != nil {
			fenceAt = *rec.FenceCreatedAt
		}
		items = append(items, TimelineItem{
			Phase:       "fenced",
			Source:      "execution_store",
			Kind:        "fence_raised",
			FactTime:    fenceAt,
			ObservedAt:  observedAt,
			ExecutionID: rec.ExecutionID,
			Evidence:    rec.FenceReason,
			EvidenceSt:  evidenceRecorded,
		})
	}
	return items
}

func phaseForRuntime(status execution.RuntimeStatus) string {
	switch status {
	case execution.RuntimeQueued:
		return "queued"
	case execution.RuntimePending:
		return "dispatched"
	case execution.RuntimeRunning:
		return "running"
	case execution.RuntimeCompleted, execution.RuntimeFailed, execution.RuntimeUnknown:
		return "terminal"
	default:
		return "accepted"
	}
}

// timelinePhaseFor buckets an event type into the same phase vocabulary as the
// control facts, so a client can order mixed sources without special-casing
// each one.
func timelinePhaseFor(rec *execution.Record, eventType string) string {
	switch {
	case strings.Contains(eventType, "input.ack"):
		return "accepted"
	case strings.Contains(eventType, "queue"):
		return "queued"
	case strings.Contains(eventType, "turn.start"), strings.Contains(eventType, "worker"):
		return "running"
	case strings.Contains(eventType, "done"), strings.Contains(eventType, "failed"),
		strings.Contains(eventType, "error"):
		return "terminal"
	default:
		return phaseForRuntime(rec.RuntimeStatus)
	}
}

// effectItem projects one external delivery. Agent completion, provider
// acceptance and external verification are separate facts and get separate
// items; collapsing them is what makes "the agent finished" get read as "the
// message arrived".
func effectItem(eff *effect.Effect, rec *execution.Record, observedAt int64) TimelineItem {
	item := TimelineItem{
		Phase:       "delivery",
		Source:      "effect_store",
		Kind:        string(eff.Status),
		FactTime:    eff.UpdatedAtMs,
		ObservedAt:  observedAt,
		ExecutionID: rec.ExecutionID,
		WorkerRunID: rec.WorkerRunID,
		EffectID:    eff.EffectID,
		EvidenceSt:  evidenceRecorded,
	}
	switch eff.Status {
	case effect.StatusDelivered:
		// provider_ref is a provider-assigned identifier, not user content.
		item.Evidence = eff.ProviderRef
	case effect.StatusUnknown:
		item.Evidence = string(effect.StatusUnknown)
	case effect.StatusReconciledSucceeded, effect.StatusReconciledFailed:
		// #868: reconciled is late evidence converging an unknown effect,
		// not an on-attempt receipt. The evidence ref names what proved it.
		item.Evidence = eff.EvidenceRef
		if item.Evidence == "" {
			item.Evidence = string(eff.Status)
		}
	case effect.StatusFenced:
		// A quarantined effect never dispatches again; the error code names
		// the fence, not a provider outcome.
		item.Phase = "fenced"
		item.Evidence = eff.ErrorCode
	default:
		item.Evidence = eff.ErrorCode
	}
	return item
}

// availableActions derives the actions valid RIGHT NOW from the record's state
// and the effects' states. It is deliberately conservative: an action that
// would need a fact we do not have is not offered, and "mark this run
// successful" does not exist at any state.
func availableActions(rec *execution.Record, effects []*effect.Effect) []AvailableAction {
	actions := []AvailableAction{}

	if rec.FenceReason != "" {
		actions = append(actions,
			AvailableAction{
				Kind:            "fence_resolve",
				Target:          "execution",
				RequiresVersion: rec.FenceVersion,
				Description:     "Clear the fence; the run stays unknown.",
			},
			AvailableAction{
				Kind:            "fence_abandon",
				Target:          "execution",
				RequiresVersion: rec.FenceVersion,
				Description:     "Clear the fence and end the run as failed.",
			},
		)
	}
	if rec.RuntimeStatus == execution.RuntimeQueued {
		actions = append(actions, AvailableAction{
			Kind:        "queue_cancel",
			Target:      "execution",
			Description: "Withdraw this input before it is dispatched.",
		})
	}
	for _, eff := range effects {
		if eff == nil || eff.Status != effect.StatusUnknown {
			continue
		}
		for _, kind := range []string{"effect_abandon", "effect_mark_delivered", "effect_requeue"} {
			actions = append(actions, AvailableAction{
				Kind:            kind,
				Target:          eff.EffectID,
				RequiresVersion: eff.Attempt,
				Description:     effectActionDescription(kind),
			})
		}
	}
	return actions
}

func effectActionDescription(kind string) string {
	switch kind {
	case "effect_abandon":
		return "Give up on this delivery; it is recorded as failed."
	case "effect_mark_delivered":
		return "Record this delivery as sent, on your own external evidence."
	case "effect_requeue":
		return "Allow one more send attempt on the SAME delivery."
	default:
		return ""
	}
}
