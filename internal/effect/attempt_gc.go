package effect

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// AttemptGCStore exposes bounded cleanup of detailed send-attempt evidence.
// The effect row remains as the idempotency marker after its attempts expire.
type AttemptGCStore interface {
	DeleteSettledAttempts(ctx context.Context, settledBefore time.Time, limit int) (int64, error)
}

// Only finished attempts belonging to old, settled terminal effects are
// eligible. A missing settlement clock, live lease, nonterminal state, or
// unfinished attempt keeps the evidence intact.
const deleteSettledAttemptsSQL = `DELETE FROM effect_attempts
	WHERE attempt_id IN (
		SELECT a.attempt_id
		FROM effect_attempts a
		JOIN effects e ON e.effect_id = a.effect_id
		WHERE a.finished_at IS NOT NULL
		  AND e.status IN ('delivered', 'failed', 'reconciled_succeeded',
		                   'reconciled_failed', 'fenced')
		  AND e.settled_at IS NOT NULL
		  AND e.facts_retention_ms IS NOT NULL
		  AND e.settled_at + e.facts_retention_ms <= ?
		  AND e.lease_until IS NULL
		  AND e.owner_instance_id = ''
		  AND NOT EXISTS (
			SELECT 1 FROM effect_attempts pending
			WHERE pending.effect_id = e.effect_id
			  AND pending.finished_at IS NULL
		  )
		ORDER BY a.finished_at, a.attempt_id
		LIMIT ?
	)`

func deleteSettledAttempts(
	ctx context.Context, tx *sql.Tx, dialect dbutil.Dialect, settledBefore time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, dialect.Rebind(deleteSettledAttemptsSQL),
		settledBefore.UnixMilli(), limit)
	if err != nil {
		return 0, fmt.Errorf("effect: expire settled attempts: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("effect: expired attempt rows: %w", err)
	}
	return deleted, nil
}

func (s *SQLiteStore) DeleteSettledAttempts(
	ctx context.Context, settledBefore time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	var deleted int64
	err := s.writeMu.WithLock(func() error {
		return s.inTx(ctx, func(tx *sql.Tx) error {
			var err error
			deleted, err = deleteSettledAttempts(ctx, tx, dbutil.DialectSQLite, settledBefore, limit)
			return err
		})
	})
	return deleted, err
}

func (s *PGStore) DeleteSettledAttempts(
	ctx context.Context, settledBefore time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	var deleted int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		deleted, err = deleteSettledAttempts(ctx, tx, s.db.Dialect(), settledBefore, limit)
		return err
	})
	return deleted, err
}
