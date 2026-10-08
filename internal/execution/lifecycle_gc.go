package execution

import (
	"context"
	"fmt"
	"time"
)

// LifecycleFactsGCStore exposes bounded compaction of settled execution
// details while preserving the client message key and payload fingerprint.
type LifecycleFactsGCStore interface {
	CompactSettledFacts(ctx context.Context, finishedBefore time.Time, limit int) (int64, error)
}

const compactSettledFactsSQL = `UPDATE execution_inputs
	SET error_code = '',
	    owner_instance_id = '',
	    lease_until = 0,
	    runtime_error_code = '',
	    started_at = NULL,
	    fence_created_at = NULL,
	    turn_started_at = NULL,
	    turn_deadline_at = NULL,
	    turn_policy_revision = ''
	WHERE execution_id IN (
		SELECT e.execution_id
		FROM execution_inputs e
		WHERE e.status IN ('delivered', 'failed')
		  AND e.runtime_status IN ('completed', 'failed')
		  AND e.finished_at IS NOT NULL
		  AND e.finished_at <= ?
		  AND e.fence_reason = ''
		  AND NOT (e.status = 'failed' AND e.runtime_status = 'completed')
		  AND NOT EXISTS (
			SELECT 1 FROM execution_queue q
			WHERE q.execution_id = e.execution_id
		  )
		  AND (
			e.error_code <> ''
			OR e.owner_instance_id <> ''
			OR e.lease_until <> 0
			OR e.runtime_error_code <> ''
			OR e.started_at IS NOT NULL
			OR e.fence_created_at IS NOT NULL
			OR e.turn_started_at IS NOT NULL
			OR e.turn_deadline_at IS NOT NULL
			OR e.turn_policy_revision <> ''
		  )
		ORDER BY e.finished_at, e.execution_id
		LIMIT ?
	)`

// CompactSettledFacts removes operational details after their retention window.
// It preserves the idempotency key, payload hash, final delivery/runtime states,
// and finished_at so a late duplicate is still recognized and facts are not
// mistaken for missing input.
func (s *SQLStore) CompactSettledFacts(
	ctx context.Context, finishedBefore time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var compacted int64
	err := s.withWriteLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("execution: begin lifecycle fact compaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		result, err := tx.ExecContext(ctx, s.rebind(compactSettledFactsSQL),
			finishedBefore.UnixMilli(), limit)
		if err != nil {
			return fmt.Errorf("execution: compact settled facts: %w", err)
		}
		compacted, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("execution: compacted fact rows: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("execution: commit lifecycle fact compaction: %w", err)
		}
		return nil
	})
	return compacted, err
}
