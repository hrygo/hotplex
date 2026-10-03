package effect

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// Attempt is what one send attempt actually did.
//
// It is kept separately from the effect because an effect is a delivery and
// an attempt is one try at it. Collapsing them loses the only evidence that
// distinguishes "the provider refused and said later" from "we never found
// out" once the effect has settled.
type Attempt struct {
	AttemptID       string
	EffectID        string
	Attempt         int64
	OwnerInstanceID string
	LeaseVersion    int64
	// LeaseToken is a write credential and is never returned to a reader.
	LeaseToken  string
	StartedAtMs int64
	// FinishedAtMs is nil while the attempt is still in flight.
	FinishedAtMs   *int64
	Outcome        AttemptOutcome
	RejectionClass string
	ProviderRef    string
	EvidenceRef    string
	ErrorCode      string
	Reason         string
}

// InFlight reports whether the attempt was claimed but never finished.
func (a *Attempt) InFlight() bool { return a.FinishedAtMs == nil }

const attemptColumns = `attempt_id, effect_id, attempt, owner_instance_id, lease_version,
		lease_token, started_at, finished_at, outcome, rejection_class,
		provider_ref, evidence_ref, error_code, reason`

func scanAttempt(row rowScanner) (*Attempt, error) {
	var (
		a        Attempt
		finished sql.NullInt64
	)
	if err := row.Scan(
		&a.AttemptID, &a.EffectID, &a.Attempt, &a.OwnerInstanceID, &a.LeaseVersion,
		&a.LeaseToken, &a.StartedAtMs, &finished, &a.Outcome, &a.RejectionClass,
		&a.ProviderRef, &a.EvidenceRef, &a.ErrorCode, &a.Reason,
	); err != nil {
		return nil, fmt.Errorf("effect: scan attempt: %w", err)
	}
	if finished.Valid {
		v := finished.Int64
		a.FinishedAtMs = &v
	}
	return &a, nil
}

const listRecoverableSQL = `SELECT ` + effectColumns + ` FROM effects
	WHERE status = 'planned'
	ORDER BY created_at
	LIMIT ?`

// ListRecoverable returns effects that are durably owed a send.
//
// Only 'planned' qualifies. An effect in 'started' is owned or awaiting a
// retry, and one in 'unknown' may or may not exist at the provider — neither
// may be picked up as fresh work.
func (s *SQLiteStore) ListRecoverable(ctx context.Context, limit int) ([]*Effect, error) {
	return listRecoverable(ctx, s.db, dbutil.DialectSQLite, limit)
}

// ListRecoverable returns effects that are durably owed a send.
func (s *PGStore) ListRecoverable(ctx context.Context, limit int) ([]*Effect, error) {
	return listRecoverable(ctx, s.db, s.db.Dialect(), limit)
}

func listRecoverable(
	ctx context.Context, q queryer, dialect dbutil.Dialect, limit int,
) ([]*Effect, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, dialect.Rebind(listRecoverableSQL), limit)
	if err != nil {
		return nil, fmt.Errorf("effect: list recoverable: %w", err)
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
		return nil, fmt.Errorf("effect: scan recoverable: %w", err)
	}
	return out, nil
}

const listAttemptsSQL = `SELECT ` + attemptColumns + ` FROM effect_attempts
	WHERE effect_id = ?
	ORDER BY attempt`

// ListAttempts returns the per-attempt facts for an effect, oldest first.
func (s *SQLiteStore) ListAttempts(ctx context.Context, effectID string) ([]*Attempt, error) {
	rows, err := s.db.QueryContext(ctx, listAttemptsSQL, effectID)
	if err != nil {
		return nil, fmt.Errorf("effect: list attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanAttempts(rows)
}

// ListAttempts returns the per-attempt facts for an effect, oldest first.
func (s *PGStore) ListAttempts(ctx context.Context, effectID string) ([]*Attempt, error) {
	rows, err := s.db.QueryContext(ctx, s.db.Dialect().Rebind(listAttemptsSQL), effectID)
	if err != nil {
		return nil, fmt.Errorf("effect: list attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanAttempts(rows)
}

func scanAttempts(rows *sql.Rows) ([]*Attempt, error) {
	var out []*Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("effect: scan attempts: %w", err)
	}
	return out, nil
}

// RetryBudget is the first-slice delivery budget, carried over unchanged from
// the in-memory retry queue so switching an owner does not silently change how
// often a target is contacted.
type RetryBudget struct {
	// MaxAttempts counts sends, not rejections.
	MaxAttempts int64
	Initial     time.Duration
	Max         time.Duration
}

// DefaultRetryBudget is the budget the first slice uses: at most three sends,
// 30s before the first retry, capped at five minutes.
func DefaultRetryBudget() RetryBudget {
	return RetryBudget{MaxAttempts: 3, Initial: 30 * time.Second, Max: 5 * time.Minute}
}

// Backoff returns the wait before the attempt following a rejection.
//
// Backoff is a lower bound, not a scheduler: a provider-supplied RetryAfter
// wins when it is longer, because ignoring the provider's own hint is how a
// retry turns a rate limit into a block.
func (b RetryBudget) Backoff(attempt int64, providerHint time.Duration) time.Duration {
	wait := b.Initial
	for i := int64(1); i < attempt && wait < b.Max; i++ {
		wait *= 2
	}
	if wait > b.Max {
		wait = b.Max
	}
	if providerHint > wait {
		wait = providerHint
	}
	return wait
}
