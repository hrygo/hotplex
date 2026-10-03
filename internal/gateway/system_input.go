package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/cron"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/pkg/events"
)

// ErrSystemInputRejected is returned when a system-originated input cannot be
// accepted. Callers receive a typed error rather than a client-facing error
// event: there is no client channel to report on.
var ErrSystemInputRejected = errors.New("gateway: system input rejected")

// systemInputOwnerID attributes system-originated inputs in the audit trail.
// Cron runs already carry the real owner on the session, so this only marks the
// origin of the input, not the acting user.
const systemInputOwnerID = "system"

// systemInputPrefix namespaces system input identities so they can never
// collide with a client-supplied envelope ID.
const systemInputPrefix = "sys-"

// errStillRunning tells the wait loop to keep polling.
var errStillRunning = errors.New("gateway: execution still running")

// DispatchSystemInput accepts an internal, trusted input through the same
// durable path a client input uses: the execution ledger, the single-active
// gate, the owner lease, the turn stop fence and terminal correlation.
//
// It exists so a cron firing no longer calls Worker.Input directly, which would
// bypass the execution ledger and leave the run unattributable and
// undeduplicable. It deliberately does NOT reuse the client path's skill
// resolution, command dispatch or busy-supplement handling: a system input owns
// its session exclusively, and its prompt is Agent input rather than a user
// command.
func (h *Handler) DispatchSystemInput(ctx context.Context, req cron.SystemInputRequest) (cron.SystemInputResult, error) {
	var out cron.SystemInputResult

	if h.executionStore == nil {
		// Without the ledger there is no accept, no lease and no terminal
		// correlation, so the run could not be explained afterwards.
		return out, fmt.Errorf("%w: execution store unavailable", ErrSystemInputRejected)
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return out, fmt.Errorf("%w: missing session id", ErrSystemInputRejected)
	}
	if strings.TrimSpace(req.OccurrenceID) == "" {
		return out, fmt.Errorf("%w: missing occurrence id", ErrSystemInputRejected)
	}
	if strings.TrimSpace(req.Content) == "" {
		return out, fmt.Errorf("%w: empty content", ErrSystemInputRejected)
	}
	if _, err := h.sm.Get(ctx, req.SessionID); err != nil {
		return out, fmt.Errorf("%w: session not found", ErrSystemInputRejected)
	}

	// A stable per-occurrence identity is what makes a retry of the same firing
	// resolve to the recorded execution rather than a second dispatch.
	inputID := systemInputPrefix + req.OccurrenceID
	env := events.NewEnvelope(inputID, req.SessionID, 0, events.Input, events.InputData{Content: req.Content})
	env.Metadata = map[string]any{"system_input": true, "occurrence_id": req.OccurrenceID}

	unlockDispatch := h.dispatchGate.Lock(req.SessionID)
	defer unlockDispatch()

	record, duplicate, err := h.acceptInputExecution(ctx, env)
	if err != nil {
		if errors.Is(err, execution.ErrPayloadConflict) {
			return out, fmt.Errorf("%w: occurrence %s already used with different input",
				ErrSystemInputRejected, req.OccurrenceID)
		}
		if errors.Is(err, execution.ErrSessionBusy) {
			return out, fmt.Errorf("%w: session %s already has an active execution",
				ErrSystemInputRejected, req.SessionID)
		}
		return out, fmt.Errorf("%w: accept input: %w", ErrSystemInputRejected, err)
	}
	out.ExecutionID = record.ExecutionID
	if duplicate {
		// Already accepted: report the recorded outcome and dispatch nothing.
		out.Duplicate = true
		return out, nil
	}

	finish := func(status execution.Status, code events.ErrorCode) {
		if err := h.finishInputExecution(ctx, record, status, code); err != nil {
			h.log.Error("gateway: persist system input outcome failed", "err", err,
				"session_id", req.SessionID, observability.KeyExecutionID, record.ExecutionID,
				"status", status)
		}
	}
	fail := func(code events.ErrorCode, cause error) (cron.SystemInputResult, error) {
		finish(execution.StatusFailed, code)
		return cron.SystemInputResult{ExecutionID: record.ExecutionID},
			fmt.Errorf("%w: %w", ErrSystemInputRejected, cause)
	}

	var w = h.sm.GetWorker(req.SessionID)
	workerRunID := ""
	if h.bridge != nil {
		bound, runID, ok := h.bridge.CurrentWorkerBinding(req.SessionID)
		if ok {
			w = bound
			workerRunID = runID
		}
	}
	if w == nil {
		return fail(events.ErrCodeSessionNotFound, errors.New("no worker attached to session"))
	}
	if workerRunID == "" {
		workerRunID = record.WorkerRunID
	}

	// Register the dispatch against the durable execution before the Worker
	// sees the prompt, so an interrupted run is still attributable.
	if err := h.executionStore.MarkRunning(ctx, record.ExecutionID, h.ownerInstanceID, workerRunID); err != nil {
		return fail(events.ErrCodeInternalError, fmt.Errorf("dispatch registration: %w", err))
	}

	if si, err := h.sm.Get(ctx, req.SessionID); err == nil && si.State == events.StateIdle {
		if err := h.sm.TransitionWithInput(ctx, req.SessionID, events.StateRunning, req.Content, nil); err != nil {
			return fail(events.ErrCodeSessionBusy, fmt.Errorf("transition: %w", err))
		}
	}

	if h.bridge != nil {
		h.bridge.RecordTurnStart(req.SessionID)
	}
	h.stopFence.BeginTurn(req.SessionID, workerRunID, record.ExecutionID)

	if err := w.Input(ctx, req.Content, nil); err != nil {
		return fail(events.ErrCodeInternalError, fmt.Errorf("worker input: %w", err))
	}

	finish(execution.StatusDelivered, "")
	h.emitAudit(audit.OutcomeSuccess, systemInputOwnerID, "cron", req.SessionID, req.Content)
	return out, nil
}

// WaitForExecution blocks until the given execution reaches a terminal runtime
// state or the timeout expires.
//
// It correlates on the execution, not on the session's global IDLE state: a
// session can read idle while the run this caller owns is still executing, and
// waiting on IDLE would declare such a run complete while it is still running.
// The session state is used only as a secondary signal when the execution is
// no longer the session's latest (superseded or closed by another path).
func (h *Handler) WaitForExecution(
	ctx context.Context, sessionID, executionID string, timeout time.Duration,
) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		err := h.executionFinished(timeoutCtx, sessionID, executionID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errStillRunning) {
			return err
		}
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("cron executor: timeout waiting for execution %s: %w",
				executionID, timeoutCtx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// executionFinished reports whether the execution has reached a terminal
// runtime state. When the execution is no longer the session's latest, the
// execution store cannot say more, so the session state decides.
func (h *Handler) executionFinished(ctx context.Context, sessionID, executionID string) error {
	if h.executionStore != nil && executionID != "" {
		rec, err := h.executionStore.LatestBySession(ctx, sessionID)
		switch {
		case err == nil && rec != nil && rec.ExecutionID == executionID:
			// This is our execution. A terminal runtime status is authoritative.
			switch rec.RuntimeStatus {
			case execution.RuntimeCompleted, execution.RuntimeFailed, execution.RuntimeUnknown:
				return nil
			}
			// Otherwise it is still pending/running; keep waiting.
			return errStillRunning
		case err == nil && rec != nil && rec.ExecutionID != executionID:
			// Our execution is no longer the latest: another path closed or
			// superseded it. Fall through to the session-state check.
		case errors.Is(err, execution.ErrNotFound):
			// Nothing recorded; fall through to the session-state check.
		default:
			if err != nil {
				h.log.Warn("gateway: execution lookup during wait failed",
					"session_id", sessionID, "execution_id", executionID, "err", err)
			}
		}
	}

	si, err := h.sm.Get(ctx, sessionID)
	if err == nil && si.State != events.StateRunning && si.State != events.StateCreated {
		return nil
	}
	return errStillRunning
}
