package effect

import (
	"context"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// ReconcileOutcome is the late-arriving evidence verdict for an unknown effect.
//
// A reconcile never creates a second effect: it converges the SAME effect row
// to a reconciled terminal state, conditioned on it still being unknown.
// Any concurrent change (a claim, another reconcile, an operator decision)
// makes the update match no rows and surfaces as a conflict (#947).
type ReconcileOutcome string

const (
	// ReconcileSucceeded means late evidence proved the provider accepted it.
	ReconcileSucceeded ReconcileOutcome = "succeeded"
	// ReconcileFailed means late evidence proved the provider never committed.
	ReconcileFailed ReconcileOutcome = "failed"
)

// ReconcileRequest converges one unknown effect on late evidence.
type ReconcileRequest struct {
	EffectID string
	Outcome  ReconcileOutcome
	// ExpectedStatus must be StatusUnknown. Fenced is terminal on purpose:
	// a quarantined effect never re-enters the lifecycle.
	ExpectedStatus Status
	// ProviderRef/EvidenceRef carry the late evidence pointers (bounded,
	// secret-free, same contract as Completion).
	ProviderRef string
	EvidenceRef string
	Reason      string
	Now         time.Time
}

// reconcileStatusFor maps the evidence verdict to the terminal state.
func reconcileStatusFor(o ReconcileOutcome) (Status, error) {
	switch o {
	case ReconcileSucceeded:
		return StatusReconciledSucceeded, nil
	case ReconcileFailed:
		return StatusReconciledFailed, nil
	default:
		return "", fmt.Errorf("%w: unknown reconcile outcome %q", ErrNotClaimable, o)
	}
}

// ValidateReconcileRequest rejects a malformed reconcile before any write.
func (r ReconcileRequest) Validate() error {
	if r.EffectID == "" {
		return fmt.Errorf("%w: effect id is required", ErrNotClaimable)
	}
	if r.ExpectedStatus != StatusUnknown {
		return fmt.Errorf("%w: only unknown effects can be reconciled, got %q",
			ErrNotClaimable, r.ExpectedStatus)
	}
	if _, err := reconcileStatusFor(r.Outcome); err != nil {
		return err
	}
	if len(r.ProviderRef) > 256 || len(r.EvidenceRef) > 256 {
		return fmt.Errorf("%w: provider/evidence ref must be at most 256 characters", ErrNotClaimable)
	}
	if len(r.Reason) > 512 {
		return fmt.Errorf("%w: reason must be at most 512 characters", ErrNotClaimable)
	}
	return nil
}

const reconcileUpdateSQL = `UPDATE effects SET
		status = ?,
		error_code = ?,
		reason = ?,
		provider_ref = ?,
		evidence_ref = ?,
		owner_instance_id = '',
		lease_until = NULL,
		next_attempt_at = NULL,
		updated_at = ?
	WHERE effect_id = ?
	  AND status = ?`

// FenceRequest quarantines one effect: it will never be claimed or retried.
// Only unknown (or failed) effects accept it; reconciled/delivered are decided.
type FenceRequest struct {
	EffectID       string
	ExpectedStatus Status
	Reason         string
	Now            time.Time
}

// ValidateFenceRequest rejects a malformed fence before any write.
func (r FenceRequest) Validate() error {
	if r.EffectID == "" {
		return fmt.Errorf("%w: effect id is required", ErrNotClaimable)
	}
	if r.ExpectedStatus != StatusUnknown && r.ExpectedStatus != StatusFailed {
		return fmt.Errorf("%w: only unknown or failed effects can be fenced, got %q",
			ErrNotClaimable, r.ExpectedStatus)
	}
	if r.Reason == "" || len(r.Reason) > 512 {
		return fmt.Errorf("%w: reason must be 1-512 characters", ErrNotClaimable)
	}
	return nil
}

const fenceUpdateSQL = `UPDATE effects SET
		status = ?,
		error_code = ?,
		reason = ?,
		owner_instance_id = '',
		lease_until = NULL,
		next_attempt_at = NULL,
		updated_at = ?
	WHERE effect_id = ?
	  AND status = ?`

func applyReconcile(ctx context.Context, exec func(context.Context, string, ...any) (int64, error), req ReconcileRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	target, err := reconcileStatusFor(req.Outcome)
	if err != nil {
		return err
	}
	errorCode := "reconciled_" + string(req.Outcome)
	n, err := exec(ctx, reconcileUpdateSQL,
		string(target), errorCode, req.Reason, req.ProviderRef, req.EvidenceRef,
		req.Now.UnixMilli(), req.EffectID, string(req.ExpectedStatus))
	if err != nil {
		return fmt.Errorf("effect: reconcile: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: effect %s is no longer %s", ErrLeaseLost, req.EffectID, req.ExpectedStatus)
	}
	return nil
}

func applyFence(ctx context.Context, exec func(context.Context, string, ...any) (int64, error), req FenceRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	n, err := exec(ctx, fenceUpdateSQL,
		string(StatusFenced), "fenced", "fenced: "+req.Reason,
		req.Now.UnixMilli(), req.EffectID, string(req.ExpectedStatus))
	if err != nil {
		return fmt.Errorf("effect: fence: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: effect %s is no longer %s", ErrLeaseLost, req.EffectID, req.ExpectedStatus)
	}
	return nil
}

// ReconcileUnknown converges one unknown effect on late evidence (SQLite).
func (s *SQLiteStore) ReconcileUnknown(ctx context.Context, req ReconcileRequest) (*Effect, error) {
	err := applyReconcile(ctx, func(ctx context.Context, q string, args ...any) (int64, error) {
		res, err := s.db.ExecContext(ctx, dbutil.DialectSQLite.Rebind(q), args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}, req)
	if err != nil {
		return nil, err
	}
	return s.GetByID(ctx, req.EffectID)
}

// Fence quarantines one effect (SQLite).
func (s *SQLiteStore) Fence(ctx context.Context, req FenceRequest) (*Effect, error) {
	err := applyFence(ctx, func(ctx context.Context, q string, args ...any) (int64, error) {
		res, err := s.db.ExecContext(ctx, dbutil.DialectSQLite.Rebind(q), args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}, req)
	if err != nil {
		return nil, err
	}
	return s.GetByID(ctx, req.EffectID)
}

// ReconcileUnknown converges one unknown effect on late evidence (PostgreSQL).
func (s *PGStore) ReconcileUnknown(ctx context.Context, req ReconcileRequest) (*Effect, error) {
	err := applyReconcile(ctx, func(ctx context.Context, q string, args ...any) (int64, error) {
		res, err := s.db.ExecContext(ctx, s.db.Dialect().Rebind(q), args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}, req)
	if err != nil {
		return nil, err
	}
	return s.GetByID(ctx, req.EffectID)
}

// Fence quarantines one effect (PostgreSQL).
func (s *PGStore) Fence(ctx context.Context, req FenceRequest) (*Effect, error) {
	err := applyFence(ctx, func(ctx context.Context, q string, args ...any) (int64, error) {
		res, err := s.db.ExecContext(ctx, s.db.Dialect().Rebind(q), args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}, req)
	if err != nil {
		return nil, err
	}
	return s.GetByID(ctx, req.EffectID)
}
