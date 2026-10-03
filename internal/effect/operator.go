package effect

import (
	"context"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// OperatorDecision is a human judgement applied to an uncertain delivery.
//
// Only 'unknown' effects accept one. delivered and failed are already decided
// by the system, and planned work is owned by the scheduler, not an operator.
type OperatorDecision string

const (
	// OperatorAbandon gives up on an unknown delivery without claiming it
	// succeeded or failed at the provider. The effect becomes failed with an
	// operator-attributed reason so the audit trail is honest.
	OperatorAbandon OperatorDecision = "abandon"
	// OperatorMarkDelivered records that an operator independently confirmed
	// the message exists at the provider.
	OperatorMarkDelivered OperatorDecision = "mark_delivered"
	// OperatorRequeue returns an unknown effect to planned so the delivery
	// machinery may retry it.
	OperatorRequeue OperatorDecision = "requeue"
)

// OperatorActionRequest is one conditional operator decision.
type OperatorActionRequest struct {
	EffectID string
	Decision OperatorDecision
	// ExpectedStatus must be StatusUnknown: the write is refused if the
	// effect is in any other state, so an operator cannot race the sender or
	// overwrite a receipt that arrived while they were deciding.
	ExpectedStatus Status
	// Reason is a bounded justification recorded in audit. It is stored on the
	// effect as an operator-attributed reason, never as provider truth.
	Reason string
	// EvidenceRef is a bounded pointer to whatever confirmed the decision
	// (a screenshot, a provider lookup, a ticket). It is stored, but is never
	// interpreted.
	EvidenceRef string
	Now         time.Time
}

// OperatorListFilter bounds the operator effect list.
type OperatorListFilter struct {
	// Status filters to one lifecycle state. Empty lists everything.
	Status Status
	Limit  int
	Offset int
}

const (
	operatorDefaultLimit = 100
	operatorMaxLimit     = 500
	operatorReasonMax    = 512
	operatorEvidenceMax  = 256
)

// ValidateOperatorAction rejects a malformed decision before any write.
func (r OperatorActionRequest) Validate() error {
	if r.EffectID == "" {
		return fmt.Errorf("%w: effect id is required", ErrNotClaimable)
	}
	if r.ExpectedStatus != StatusUnknown {
		return fmt.Errorf("%w: only unknown effects accept an operator decision, got %q",
			ErrNotClaimable, r.ExpectedStatus)
	}
	switch r.Decision {
	case OperatorAbandon, OperatorMarkDelivered, OperatorRequeue:
	default:
		return fmt.Errorf("%w: unknown decision %q", ErrNotClaimable, r.Decision)
	}
	reason := r.Reason
	if reason == "" || len(reason) > operatorReasonMax {
		return fmt.Errorf("%w: reason must be 1-%d characters", ErrNotClaimable, operatorReasonMax)
	}
	if len(r.EvidenceRef) > operatorEvidenceMax {
		return fmt.Errorf("%w: evidence_ref must be at most %d characters",
			ErrNotClaimable, operatorEvidenceMax)
	}
	return nil
}

// targetStatusForDecision maps a decision to the effect status it writes.
func targetStatusForDecision(d OperatorDecision) Status {
	if d == OperatorRequeue {
		return StatusPlanned
	}
	return StatusFailed
}

// operatorUpdateSQL applies a decision conditionally on the effect still
// being unknown. Any concurrent change — a late receipt, a fence, another
// operator — makes the update match no rows and surfaces as a conflict.
const operatorUpdateSQL = `UPDATE effects SET
		status = ?,
		error_code = ?,
		reason = ?,
		evidence_ref = ?,
		owner_instance_id = '',
		lease_until = NULL,
		next_attempt_at = NULL,
		updated_at = ?
	WHERE effect_id = ?
	  AND status = ?`

// operatorReasonPrefix attributes the reason to the operator so a later
// reader never mistakes it for something the provider or a worker said.
const operatorReasonPrefix = "operator: "

// ApplyOperatorAction records one conditional operator decision.
func (s *SQLiteStore) ApplyOperatorAction(ctx context.Context, req OperatorActionRequest) (*Effect, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	target := targetStatusForDecision(req.Decision)
	errorCode := "operator_abandoned"
	if req.Decision == OperatorMarkDelivered {
		errorCode = "operator_confirmed_delivered"
	} else if req.Decision == OperatorRequeue {
		errorCode = "operator_requeued"
	}

	res, err := s.db.ExecContext(ctx, dbutil.DialectSQLite.Rebind(operatorUpdateSQL),
		target, errorCode, operatorReasonPrefix+req.Reason, req.EvidenceRef,
		req.Now.UnixMilli(), req.EffectID, req.ExpectedStatus)
	if err != nil {
		return nil, fmt.Errorf("effect: operator action: %w", err)
	}
	if err := requireOneRow(res, "effect %s is no longer unknown", req.EffectID); err != nil {
		return nil, err
	}
	return s.GetByID(ctx, req.EffectID)
}

// ListForOperator returns effects for the operator console, newest first,
// optionally filtered by status. Content is never included.
func (s *SQLiteStore) ListForOperator(ctx context.Context, f OperatorListFilter) ([]*Effect, error) {
	return listForOperator(ctx, s.db, dbutil.DialectSQLite, f)
}

// ApplyOperatorAction records one conditional operator decision.
func (s *PGStore) ApplyOperatorAction(ctx context.Context, req OperatorActionRequest) (*Effect, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	target := targetStatusForDecision(req.Decision)
	errorCode := "operator_abandoned"
	if req.Decision == OperatorMarkDelivered {
		errorCode = "operator_confirmed_delivered"
	} else if req.Decision == OperatorRequeue {
		errorCode = "operator_requeued"
	}

	res, err := s.db.ExecContext(ctx, s.db.Dialect().Rebind(operatorUpdateSQL),
		target, errorCode, operatorReasonPrefix+req.Reason, req.EvidenceRef,
		req.Now.UnixMilli(), req.EffectID, req.ExpectedStatus)
	if err != nil {
		return nil, fmt.Errorf("effect: operator action: %w", err)
	}
	if err := requireOneRow(res, "effect %s is no longer unknown", req.EffectID); err != nil {
		return nil, err
	}
	return s.GetByID(ctx, req.EffectID)
}

// ListForOperator returns effects for the operator console, newest first.
func (s *PGStore) ListForOperator(ctx context.Context, f OperatorListFilter) ([]*Effect, error) {
	return listForOperator(ctx, s.db, s.db.Dialect(), f)
}

const operatorListSQL = `SELECT ` + effectColumns + ` FROM effects
	ORDER BY created_at DESC, effect_id
	LIMIT ? OFFSET ?`

const operatorListFilteredSQL = `SELECT ` + effectColumns + ` FROM effects
	WHERE status = ?
	ORDER BY created_at DESC, effect_id
	LIMIT ? OFFSET ?`

func listForOperator(ctx context.Context, q queryer, dialect dbutil.Dialect, f OperatorListFilter) ([]*Effect, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = operatorDefaultLimit
	}
	if limit > operatorMaxLimit {
		limit = operatorMaxLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	query := operatorListSQL
	args := []any{limit, f.Offset}
	if f.Status != "" {
		query = operatorListFilteredSQL
		args = []any{string(f.Status), limit, f.Offset}
	}

	rows, err := q.QueryContext(ctx, dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("effect: operator list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Effect
	for rows.Next() {
		e, err := scanEffect(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("effect: operator list scan: %w", err)
	}
	return out, nil
}
