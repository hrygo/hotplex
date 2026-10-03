package effect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// ErrLeaseLost means a conditional update matched no row: someone else owns
// this attempt now, or the effect already reached a state this caller could
// not legally write from.
//
// It is never a transient error to retry blindly. The caller must re-read the
// effect and decide from what is actually recorded.
var ErrLeaseLost = errors.New("effect: lease lost")

// ErrNotClaimable means the effect is not in a state that may start a send.
var ErrNotClaimable = errors.New("effect: not claimable")

// ClaimRequest asks to become the single owner of one send attempt.
type ClaimRequest struct {
	EffectID string
	// OwnerInstanceID identifies this process. It is recorded so an operator
	// can tell which instance holds the effect.
	OwnerInstanceID string
	// LeaseUntil bounds how long this claim is trusted. An expired lease is
	// not proof that nothing was sent — see MarkUnknown.
	LeaseUntil time.Time
	// ExpectedAttempt is the attempt the caller believes it is claiming. A
	// mismatch means another attempt has already advanced the effect, so the
	// claim is refused rather than silently taking over.
	ExpectedAttempt int64
	Now             time.Time
}

// Completion records the typed outcome of one send attempt.
type Completion struct {
	EffectID string
	Attempt  int64
	// LeaseVersion is the version this owner claimed. A stale writer cannot
	// overwrite a newer verified result.
	LeaseVersion int64
	// Outcome must be one of delivered, failed or unknown. Writing planned or
	// started through this path is refused.
	Outcome   Status
	ErrorCode string
	Reason    string
	// ProviderRef and EvidenceRef are set only for a delivered effect.
	ProviderRef string
	EvidenceRef string
	Now         time.Time
}

// ClaimSend moves an effect from planned to started under an owner lease.
//
// This is the single-owner mechanism: exactly one caller can win the
// conditional update, so two instances recovering the same occurrence cannot
// both start sending. The update and the returned row are one fact — a caller
// that cannot prove it won must not send.
func (s *SQLiteStore) ClaimSend(ctx context.Context, req ClaimRequest) (*Effect, error) {
	var claimed *Effect
	err := s.writeMu.WithLock(func() error {
		var err error
		claimed, err = claimSend(ctx, s.db, dbutil.DialectSQLite, req, s.queryRow)
		return err
	})
	return claimed, err
}

// CompleteSend records the typed outcome of an attempt the caller owns.
//
// The conditional update is what makes a late or fenced writer harmless: if
// the lease moved on, the write affects no rows and the caller learns it lost
// the effect rather than overwriting a verified result.
func (s *SQLiteStore) CompleteSend(ctx context.Context, c Completion) error {
	return s.writeMu.WithLock(func() error {
		return completeSend(ctx, s.db, dbutil.DialectSQLite, c)
	})
}

// ClaimSend moves an effect from planned to started under an owner lease.
func (s *PGStore) ClaimSend(ctx context.Context, req ClaimRequest) (*Effect, error) {
	return claimSend(ctx, s.db, s.db.Dialect(), req, func(
		ctx context.Context, query string, args ...any,
	) Row {
		return s.db.QueryRowContext(ctx, s.db.Dialect().Rebind(query), args...)
	})
}

// CompleteSend records the typed outcome of an attempt the caller owns.
func (s *PGStore) CompleteSend(ctx context.Context, c Completion) error {
	return completeSend(ctx, s.db, s.db.Dialect(), c)
}

const claimSendSQL = `UPDATE effects SET
		status = 'started',
		owner_instance_id = ?,
		lease_until = ?,
		lease_version = lease_version + 1,
		updated_at = ?
	WHERE effect_id = ?
	  AND status = 'planned'
	  AND attempt = ?`

// claimSend is the shared body of both dialects; only placeholder binding and
// the write mutex differ.
func claimSend(
	ctx context.Context,
	db execer,
	dialect dbutil.Dialect,
	req ClaimRequest,
	queryRow rowQuerier,
) (*Effect, error) {
	if req.EffectID == "" || req.OwnerInstanceID == "" {
		return nil, fmt.Errorf("%w: effect id and owner are required", ErrNotClaimable)
	}
	if req.LeaseUntil.IsZero() || !req.LeaseUntil.After(req.Now) {
		// A lease that is already expired would create an effect nobody owns
		// and everybody must treat as uncertain.
		return nil, fmt.Errorf("%w: lease must extend past now", ErrNotClaimable)
	}

	res, err := db.ExecContext(ctx, dialect.Rebind(claimSendSQL),
		req.OwnerInstanceID, req.LeaseUntil.UnixMilli(), req.Now.UnixMilli(),
		req.EffectID, req.ExpectedAttempt)
	if err != nil {
		return nil, fmt.Errorf("effect: claim: %w", err)
	}
	affected, err := rowsAffected(res)
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		// Either the effect moved on or the attempt no longer matches. Both
		// mean this caller does not own the send.
		return nil, fmt.Errorf("%w: effect %s attempt %d", ErrLeaseLost, req.EffectID, req.ExpectedAttempt)
	}

	return scanEffect(queryRow(ctx, dialect.Rebind(
		`SELECT `+effectColumns+` FROM effects WHERE effect_id = ?`), req.EffectID))
}

const completeSendSQL = `UPDATE effects SET
		status = ?,
		error_code = ?,
		reason = ?,
		provider_ref = ?,
		evidence_ref = ?,
		owner_instance_id = '',
		lease_until = NULL,
		updated_at = ?
	WHERE effect_id = ?
	  AND status = 'started'
	  AND attempt = ?
	  AND lease_version = ?`

func completeSend(ctx context.Context, db execer, dialect dbutil.Dialect, c Completion) error {
	switch c.Outcome {
	case StatusDelivered, StatusFailed, StatusUnknown:
	default:
		// planned/started are lifecycle transitions, not outcomes. Allowing
		// them here would let a caller skip the claim protocol entirely.
		return fmt.Errorf("%w: %q is not a completion outcome", ErrNotClaimable, c.Outcome)
	}
	if c.EffectID == "" {
		return fmt.Errorf("%w: effect id is required", ErrNotClaimable)
	}

	res, err := db.ExecContext(ctx, dialect.Rebind(completeSendSQL),
		c.Outcome, c.ErrorCode, c.Reason, c.ProviderRef, c.EvidenceRef,
		c.Now.UnixMilli(), c.EffectID, c.Attempt, c.LeaseVersion)
	if err != nil {
		return fmt.Errorf("effect: complete: %w", err)
	}
	affected, err := rowsAffected(res)
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: effect %s attempt %d lease %d",
			ErrLeaseLost, c.EffectID, c.Attempt, c.LeaseVersion)
	}
	return nil
}

// execer is the write surface both *sql.DB and *dbutil.DB satisfy, which is
// what lets the SQLite and PostgreSQL stores share one implementation.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// rowQuerier adapts each store's own row-returning method, whose signature is
// fixed by database/sql and therefore cannot satisfy an interface directly.
type rowQuerier func(ctx context.Context, query string, args ...any) Row

func (s *SQLiteStore) queryRow(ctx context.Context, query string, args ...any) Row {
	return s.db.QueryRowContext(ctx, query, args...)
}
