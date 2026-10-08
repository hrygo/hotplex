package effect

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// ErrLeaseLost means a conditional write matched no row: someone else owns
// this attempt, the lease moved on, or the effect already reached a state
// this caller could not legally write from.
//
// It is never a transient error to retry blindly. The caller must re-read the
// effect and decide from what is actually recorded.
var ErrLeaseLost = errors.New("effect: lease lost")

// ErrNotClaimable means the effect is not in a state that may start a send.
var ErrNotClaimable = errors.New("effect: not claimable")

// ErrNoNewToken reports a failure to mint a lease token. Claiming stops rather
// than proceeding without one, because the token is what proves ownership.
var ErrNoNewToken = errors.New("effect: could not mint a lease token")

// AttemptOutcome is what one send attempt actually established.
//
// It is finer-grained than the effect's status because the two do not always
// move together: a rate-limited attempt leaves the effect retryable, while an
// unprovable attempt ends it.
type AttemptOutcome string

const (
	// AttemptAccepted means the provider returned a receipt.
	AttemptAccepted AttemptOutcome = "accepted"
	// AttemptRejected means the provider refused and said nothing was created.
	AttemptRejected AttemptOutcome = "rejected"
	// AttemptUnknown means the commit could not be determined either way.
	AttemptUnknown AttemptOutcome = "unknown"
	// AttemptNotSent means the request never reached the provider, so nothing
	// was created and the delivery is still owed.
	AttemptNotSent AttemptOutcome = "not_sent"
)

// Claim is the proof that one executor owns one attempt.
type Claim struct {
	Effect *Effect
	// Attempt is the attempt number this claim opens.
	Attempt int64
	// LeaseVersion fences older owners.
	LeaseVersion int64
	// LeaseToken is random per attempt. Completion must present it verbatim:
	// version alone would let a writer that guessed the version complete an
	// attempt it no longer owns.
	LeaseToken string
}

// ClaimRequest asks to become the single owner of the next send attempt.
type ClaimRequest struct {
	EffectID        string
	OwnerInstanceID string
	// LeaseUntil bounds how long this claim is trusted. An expired lease is
	// not proof that nothing was sent — see ExpireLeases.
	LeaseUntil time.Time
	// ExpectedAttempt is the attempt the caller believes it is claiming.
	ExpectedAttempt int64
	Now             time.Time
}

// RetryRequest asks to open the attempt after a rejection the provider
// explicitly declared safe to repeat.
type RetryRequest struct {
	EffectID        string
	OwnerInstanceID string
	LeaseUntil      time.Time
	// ExpectedAttempt is the attempt that was rejected. The new attempt is
	// this one plus one.
	ExpectedAttempt int64
	// MaxAttempts caps total sends. An effect at the cap is never retried
	// again.
	MaxAttempts int64
	Now         time.Time
}

// Completion records what one attempt established.
type Completion struct {
	EffectID string
	Attempt  int64
	// LeaseToken must match the token minted for this attempt.
	LeaseToken string
	// LeaseVersion is recorded alongside the outcome for traceability. The
	// token is what authorises the write; the version explains which claim it
	// belonged to.
	LeaseVersion int64

	Outcome        AttemptOutcome
	RejectionClass string
	ErrorCode      string
	// Reason is bounded and human-readable. Never a raw provider or worker
	// error, and never message content.
	Reason string
	// ProviderRef and EvidenceRef are set only for an accepted attempt.
	ProviderRef string
	EvidenceRef string
	// RetryAfter schedules the next attempt when the rejection was explicitly
	// safe to repeat. Ignored for every other outcome.
	RetryAfter time.Duration
	Now        time.Time
}

// effectStatusFor derives the effect transition from an attempt outcome.
//
// The mapping lives here so a caller cannot record an outcome and a status
// that disagree — the exact split that produced blind resends before.
func effectStatusFor(c Completion) (Status, error) {
	switch c.Outcome {
	case AttemptAccepted:
		return StatusDelivered, nil
	case AttemptUnknown:
		return StatusUnknown, nil
	case AttemptNotSent:
		// Nothing was created, so the delivery is still owed. Returning the
		// effect to planned is what makes it claimable again without ever
		// having been ambiguous.
		return StatusPlanned, nil
	case AttemptRejected:
		switch c.RejectionClass {
		case "safe_retry":
			// The effect waits in started with no lease. It is not failed:
			// the provider said "later", not "never".
			return StatusStarted, nil
		case "permanent", "unspecified", "":
			return StatusFailed, nil
		default:
			return "", fmt.Errorf("%w: unknown rejection class %q", ErrNotClaimable, c.RejectionClass)
		}
	default:
		return "", fmt.Errorf("%w: unknown attempt outcome %q", ErrNotClaimable, c.Outcome)
	}
}

// ClaimSend moves an effect from planned to started under an owner lease and
// opens its first attempt.
//
// Both writes happen in one transaction: an attempt row without its effect
// transition, or an effect in started with no attempt to complete, would each
// strand a delivery.
func (s *SQLiteStore) ClaimSend(ctx context.Context, req ClaimRequest) (*Claim, error) {
	var claim *Claim
	err := s.writeMu.WithLock(func() error {
		return s.inTx(ctx, func(tx *sql.Tx) error {
			c, err := claimSend(ctx, tx, dbutil.DialectSQLite, req)
			claim = c
			return err
		})
	})
	return claim, err
}

// CompleteSend records what an attempt established and moves the effect to the
// status that outcome implies.
func (s *SQLiteStore) CompleteSend(ctx context.Context, c Completion) error {
	return s.writeMu.WithLock(func() error {
		return s.inTx(ctx, func(tx *sql.Tx) error {
			return completeSend(ctx, tx, dbutil.DialectSQLite, c)
		})
	})
}

// ClaimRetry opens the next attempt after a rejection the provider declared
// safe to repeat.
func (s *SQLiteStore) ClaimRetry(ctx context.Context, req RetryRequest) (*Claim, error) {
	var claim *Claim
	err := s.writeMu.WithLock(func() error {
		return s.inTx(ctx, func(tx *sql.Tx) error {
			c, err := claimRetry(ctx, tx, dbutil.DialectSQLite, req)
			claim = c
			return err
		})
	})
	return claim, err
}

// ExpireLeases moves sends whose lease elapsed into unknown.
//
// A lease expiring does not prove the message was never sent — the executor
// may have died between the provider accepting it and writing the receipt.
// Moving to unknown is the only honest answer; moving back to planned would
// authorise exactly the duplicate this ledger exists to prevent.
func (s *SQLiteStore) ExpireLeases(ctx context.Context, now time.Time, limit int) ([]*Effect, error) {
	var expired []*Effect
	err := s.writeMu.WithLock(func() error {
		return s.inTx(ctx, func(tx *sql.Tx) error {
			var err error
			expired, err = expireLeases(ctx, tx, dbutil.DialectSQLite, now, limit)
			return err
		})
	})
	return expired, err
}

// ClaimSend moves an effect from planned to started under an owner lease.
func (s *PGStore) ClaimSend(ctx context.Context, req ClaimRequest) (*Claim, error) {
	var claim *Claim
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := claimSend(ctx, tx, s.db.Dialect(), req)
		claim = c
		return err
	})
	return claim, err
}

// CompleteSend records what an attempt established.
func (s *PGStore) CompleteSend(ctx context.Context, c Completion) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return completeSend(ctx, tx, s.db.Dialect(), c)
	})
}

// ClaimRetry opens the next attempt after a safe rejection.
func (s *PGStore) ClaimRetry(ctx context.Context, req RetryRequest) (*Claim, error) {
	var claim *Claim
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := claimRetry(ctx, tx, s.db.Dialect(), req)
		claim = c
		return err
	})
	return claim, err
}

// ExpireLeases moves sends whose lease elapsed into unknown.
func (s *PGStore) ExpireLeases(ctx context.Context, now time.Time, limit int) ([]*Effect, error) {
	var expired []*Effect
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		expired, err = expireLeases(ctx, tx, s.db.Dialect(), now, limit)
		return err
	})
	return expired, err
}

func (s *SQLiteStore) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("effect: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("effect: commit: %w", err)
	}
	return nil
}

func (s *PGStore) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("effect: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("effect: commit: %w", err)
	}
	return nil
}

const claimSendSQL = `UPDATE effects SET
		status = 'started',
		owner_instance_id = ?,
		lease_until = ?,
		settled_at = NULL,
		lease_version = lease_version + 1,
		updated_at = ?
	WHERE effect_id = ?
	  AND status = 'planned'
	  AND attempt = ?`

const claimRetrySQL = `UPDATE effects SET
		attempt = attempt + 1,
		owner_instance_id = ?,
		lease_until = ?,
		lease_version = lease_version + 1,
		next_attempt_at = NULL,
		settled_at = NULL,
		updated_at = ?
	WHERE effect_id = ?
	  AND status = 'started'
	  AND attempt = ?
	  AND next_attempt_at IS NOT NULL
	  AND next_attempt_at <= ?
	  AND attempt + 1 < ?`

const insertAttemptSQL = `INSERT INTO effect_attempts
	(attempt_id, effect_id, attempt, owner_instance_id, lease_version, lease_token, started_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)`

func claimSend(
	ctx context.Context, tx *sql.Tx, dialect dbutil.Dialect, req ClaimRequest,
) (*Claim, error) {
	if err := validateClaim(req.EffectID, req.OwnerInstanceID, req.LeaseUntil, req.Now); err != nil {
		return nil, err
	}
	token, err := newLeaseToken()
	if err != nil {
		return nil, err
	}
	now := req.Now

	res, err := tx.ExecContext(ctx, dialect.Rebind(claimSendSQL),
		req.OwnerInstanceID, req.LeaseUntil.UnixMilli(), now.UnixMilli(),
		req.EffectID, req.ExpectedAttempt)
	if err != nil {
		return nil, fmt.Errorf("effect: claim: %w", err)
	}
	if err := requireOneRow(res, "effect %s attempt %d", req.EffectID, req.ExpectedAttempt); err != nil {
		return nil, err
	}

	e, err := readEffect(ctx, tx, dialect, req.EffectID)
	if err != nil {
		return nil, err
	}
	if err := openAttempt(ctx, tx, dialect, req.EffectID, req.ExpectedAttempt,
		req.OwnerInstanceID, e.LeaseVersion, token, now); err != nil {
		return nil, err
	}
	return &Claim{
		Effect:       e,
		Attempt:      req.ExpectedAttempt,
		LeaseVersion: e.LeaseVersion,
		LeaseToken:   token,
	}, nil
}

func claimRetry(
	ctx context.Context, tx *sql.Tx, dialect dbutil.Dialect, req RetryRequest,
) (*Claim, error) {
	if err := validateClaim(req.EffectID, req.OwnerInstanceID, req.LeaseUntil, req.Now); err != nil {
		return nil, err
	}
	if req.MaxAttempts <= 0 {
		return nil, fmt.Errorf("%w: max attempts must be positive", ErrNotClaimable)
	}
	token, err := newLeaseToken()
	if err != nil {
		return nil, err
	}
	next := req.ExpectedAttempt + 1

	res, err := tx.ExecContext(ctx, dialect.Rebind(claimRetrySQL),
		req.OwnerInstanceID, req.LeaseUntil.UnixMilli(), req.Now.UnixMilli(),
		req.EffectID, req.ExpectedAttempt, req.Now.UnixMilli(), req.MaxAttempts)
	if err != nil {
		return nil, fmt.Errorf("effect: claim retry: %w", err)
	}
	if err := requireOneRow(res,
		"effect %s attempt %d is not awaiting a retry or is at the cap",
		req.EffectID, req.ExpectedAttempt); err != nil {
		return nil, err
	}

	e, err := readEffect(ctx, tx, dialect, req.EffectID)
	if err != nil {
		return nil, err
	}
	if err := openAttempt(ctx, tx, dialect, req.EffectID, next,
		req.OwnerInstanceID, e.LeaseVersion, token, req.Now); err != nil {
		return nil, err
	}
	return &Claim{
		Effect:       e,
		Attempt:      next,
		LeaseVersion: e.LeaseVersion,
		LeaseToken:   token,
	}, nil
}

func openAttempt(
	ctx context.Context, tx *sql.Tx, dialect dbutil.Dialect,
	effectID string, attempt int64, owner string, leaseVersion int64, token string, now time.Time,
) error {
	_, err := tx.ExecContext(ctx, dialect.Rebind(insertAttemptSQL),
		"att_"+uuid.NewString(), effectID, attempt, owner, leaseVersion, token, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("effect: open attempt: %w", err)
	}
	return nil
}

const finishAttemptSQL = `UPDATE effect_attempts SET
		finished_at = ?,
		outcome = ?,
		rejection_class = ?,
		provider_ref = ?,
		evidence_ref = ?,
		error_code = ?,
		reason = ?,
		lease_version = ?
	WHERE effect_id = ?
	  AND attempt = ?
	  AND lease_token = ?
	  AND finished_at IS NULL`

const applyCompletionSQL = `UPDATE effects SET
		status = ?,
		error_code = ?,
		reason = ?,
		provider_ref = ?,
		evidence_ref = ?,
		owner_instance_id = '',
		lease_until = NULL,
		settled_at = CASE WHEN ? IN ('delivered', 'failed', 'reconciled_succeeded',
			'reconciled_failed', 'fenced') THEN ? ELSE NULL END,
		next_attempt_at = ?,
		updated_at = ?
	WHERE effect_id = ?
	  AND status = 'started'
	  AND attempt = ?`

func completeSend(
	ctx context.Context, tx *sql.Tx, dialect dbutil.Dialect, c Completion,
) error {
	if c.EffectID == "" || c.Attempt < 0 || c.LeaseToken == "" {
		return fmt.Errorf("%w: effect id, attempt and lease token are required", ErrNotClaimable)
	}
	status, err := effectStatusFor(c)
	if err != nil {
		return err
	}

	// The attempt row is matched first, and on its token. A caller that lost
	// its lease fails here, before it can touch the effect at all.
	res, err := tx.ExecContext(ctx, dialect.Rebind(finishAttemptSQL),
		c.Now.UnixMilli(), c.Outcome, c.RejectionClass,
		c.ProviderRef, c.EvidenceRef, c.ErrorCode, c.Reason, c.LeaseVersion,
		c.EffectID, c.Attempt, c.LeaseToken)
	if err != nil {
		return fmt.Errorf("effect: finish attempt: %w", err)
	}
	if err := requireOneRow(res, "attempt %d of effect %s is not open under this lease",
		c.Attempt, c.EffectID); err != nil {
		return err
	}

	var nextAttemptAt any
	if status == StatusStarted {
		next := c.Now.Add(c.RetryAfter)
		nextAttemptAt = next.UnixMilli()
	}

	res, err = tx.ExecContext(ctx, dialect.Rebind(applyCompletionSQL),
		status, c.ErrorCode, c.Reason, c.ProviderRef, c.EvidenceRef,
		status, c.Now.UnixMilli(), nextAttemptAt, c.Now.UnixMilli(), c.EffectID, c.Attempt)
	if err != nil {
		return fmt.Errorf("effect: apply completion: %w", err)
	}
	return requireOneRow(res, "effect %s attempt %d is not in flight", c.EffectID, c.Attempt)
}

const expireLeasesSQL = `SELECT ` + effectColumns + ` FROM effects
	WHERE status = 'started'
	  AND lease_until IS NOT NULL
	  AND lease_until < ?
	ORDER BY lease_until
	LIMIT ?`

const expireEffectSQL = `UPDATE effects SET
		status = 'unknown',
		error_code = 'lease_expired',
		reason = 'send lease expired; the commit is unproven',
		owner_instance_id = '',
		lease_until = NULL,
		settled_at = NULL,
		updated_at = ?
	WHERE effect_id = ?
	  AND status = 'started'
	  AND lease_until IS NOT NULL
	  AND lease_until < ?`

const expireAttemptSQL = `UPDATE effect_attempts SET
		finished_at = ?,
		outcome = 'unknown',
		error_code = 'lease_expired',
		reason = 'send lease expired; the commit is unproven'
	WHERE effect_id = ?
	  AND attempt = ?
	  AND finished_at IS NULL`

func expireLeases(
	ctx context.Context, tx *sql.Tx, dialect dbutil.Dialect, now time.Time, limit int,
) ([]*Effect, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, dialect.Rebind(expireLeasesSQL), now.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("effect: list expired leases: %w", err)
	}
	var expired []*Effect
	for rows.Next() {
		e, err := scanEffect(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		expired = append(expired, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("effect: scan expired leases: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("effect: close expired leases: %w", err)
	}

	for _, e := range expired {
		if _, err := tx.ExecContext(ctx, dialect.Rebind(expireAttemptSQL),
			now.UnixMilli(), e.EffectID, e.Attempt); err != nil {
			return nil, fmt.Errorf("effect: expire attempt %s: %w", e.EffectID, err)
		}
		res, err := tx.ExecContext(ctx, dialect.Rebind(expireEffectSQL),
			now.UnixMilli(), e.EffectID, now.UnixMilli())
		if err != nil {
			return nil, fmt.Errorf("effect: expire lease %s: %w", e.EffectID, err)
		}
		// A concurrent owner may have completed it first. That is a correct
		// outcome, not an error: the effect is no longer ours to expire.
		if _, err := rowsAffected(res); err != nil {
			return nil, err
		}
	}
	return expired, nil
}

func readEffect(ctx context.Context, tx *sql.Tx, dialect dbutil.Dialect, effectID string) (*Effect, error) {
	return scanEffect(dbTx{tx}.QueryRowContext(ctx, dialect.Rebind(
		`SELECT `+effectColumns+` FROM effects WHERE effect_id = ?`), effectID))
}

func validateClaim(effectID, owner string, leaseUntil, now time.Time) error {
	if effectID == "" || owner == "" {
		return fmt.Errorf("%w: effect id and owner are required", ErrNotClaimable)
	}
	if leaseUntil.IsZero() || !leaseUntil.After(now) {
		// A lease that is already expired would create an effect nobody owns
		// and everybody must treat as uncertain.
		return fmt.Errorf("%w: lease must extend past now", ErrNotClaimable)
	}
	return nil
}

func requireOneRow(res sql.Result, format string, args ...any) error {
	n, err := rowsAffected(res)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrLeaseLost, fmt.Sprintf(format, args...))
	}
	return nil
}

// newLeaseToken mints the unforgeable proof that one executor owns one
// attempt. Claiming stops if it cannot be minted rather than proceeding
// without one.
func newLeaseToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoNewToken, err)
	}
	return "ltk_" + hex.EncodeToString(buf[:]), nil
}

// queryer is the read surface both *sql.DB and *dbutil.DB satisfy, which is
// what lets the SQLite and PostgreSQL stores share one implementation.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
