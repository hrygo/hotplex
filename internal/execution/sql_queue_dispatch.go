package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// settleQueuedTx promotes a queued execution to a terminal FAILED runtime and
// delivery state with the given bounded reason, and removes its queue row — all
// inside the caller's transaction so the two never disagree.
//
// It refuses unless the execution is still queued (runtime_status='queued'). If
// it already crossed the dispatch boundary (pending/running), we must NOT
// settle it here: a running input has to go through the ordinary stop/unknown
// path, and pretending otherwise would hide a turn that actually ran.
func (s *SQLStore) settleQueuedTx(ctx context.Context, tx *sql.Tx, executionID, reason string, now int64) (bool, error) {
	result, err := tx.ExecContext(ctx, s.rebind(`
		UPDATE execution_inputs
		SET status = 'failed',
		    error_code = ?,
		    runtime_status = 'failed',
		    runtime_error_code = ?,
		    finished_at = ?,
		    updated_at = ?
		WHERE execution_id = ? AND runtime_status = 'queued'`),
		reason, reason, now, now, executionID)
	if err != nil {
		return false, fmt.Errorf("execution: settle queued execution: %w", err)
	}
	settled, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("execution: settle queued rows affected: %w", err)
	}
	if settled == 0 {
		return false, nil // not queued (or already settled) — caller decides
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM execution_queue WHERE execution_id = ?`, executionID); err != nil {
		return false, fmt.Errorf("execution: remove settled queue row: %w", err)
	}
	return true, nil
}

// ClaimQueued promotes the session's queue head to the dispatch boundary.
//
// The transition queued -> pending, the owner lease, and the removal of the
// queue row are ONE transaction. Past this point the input has been handed to
// a run, so a lost Worker response is unknown plus a fence — never a silent
// resend. The single-active index is the authority on whether the session may
// start a turn at all; a busy session leaves its queue intact for later.
func (s *SQLStore) ClaimQueued(ctx context.Context, request ClaimQueuedRequest) (*Record, *QueueEntry, error) {
	if request.SessionID == "" || request.OwnerInstanceID == "" || request.WorkerRunID == "" {
		return nil, nil, errors.New("execution: session id, owner instance id, and worker run id are required")
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var claimed *Record
	var entry *QueueEntry
	var busy, moved, stale, notFound bool
	err := s.withWriteLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("execution: begin queue claim: %w", err)
		}
		fail := func(e error) error {
			_ = tx.Rollback()
			return e
		}

		// Serialize per session so two dispatchers cannot both promote the head.
		if _, err := tx.ExecContext(ctx, s.rebind(`
			UPDATE execution_queue_counters SET next_seq = next_seq WHERE session_id = ?`),
			request.SessionID); err != nil {
			return fail(fmt.Errorf("execution: lock queue counter for claim: %w", err))
		}

		head, err := scanQueueEntry(tx.QueryRowContext(ctx, s.rebind(`
			SELECT `+queueEntryColumns+`
			FROM execution_queue
			WHERE session_id = ?
			ORDER BY queue_seq ASC
			LIMIT 1`), request.SessionID))
		if errors.Is(err, sql.ErrNoRows) {
			notFound = true
			return tx.Commit()
		}
		if err != nil {
			return fail(fmt.Errorf("execution: read queue head: %w", err))
		}

		if request.ExpectedExecutionID != "" && head.ExecutionID != request.ExpectedExecutionID {
			// The item the caller validated is no longer the head. Refuse so it
			// re-reads and re-checks rather than dispatching an unvalidated item.
			moved = true
			return tx.Commit()
		}
		if request.LifecycleRevision > 0 && head.LifecycleRevision != request.LifecycleRevision {
			stale = true
			return tx.Commit()
		}

		now := time.Now().UnixMilli()
		leaseSeconds := request.LeaseTTLSeconds
		if leaseSeconds <= 0 {
			leaseSeconds = LeaseTTL
		}

		result, err := tx.ExecContext(ctx, s.rebind(`
			UPDATE execution_inputs
			SET runtime_status = 'pending',
			    owner_instance_id = ?,
			    worker_run_id = ?,
			    lease_until = ?,
			    updated_at = ?
			WHERE execution_id = ? AND runtime_status = 'queued'`),
			request.OwnerInstanceID, request.WorkerRunID, now+leaseSeconds*1000, now,
			head.ExecutionID)
		if err != nil {
			if s.dialect.IsUniqueViolation(err) {
				// The single-active index fired: this session already has a
				// pending/running/fenced execution. Leave the queue intact.
				busy = true
				return tx.Commit()
			}
			return fail(fmt.Errorf("execution: promote queued to pending: %w", err))
		}
		promoted, _ := result.RowsAffected()
		if promoted == 0 {
			notFound = true
			return tx.Commit()
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM execution_queue WHERE execution_id = ?`, head.ExecutionID); err != nil {
			return fail(fmt.Errorf("execution: remove claimed queue row: %w", err))
		}

		claimed = &Record{
			ExecutionID:     head.ExecutionID,
			SessionID:       request.SessionID,
			Status:          StatusAccepted,
			CreatedAt:       head.EnqueuedAt,
			UpdatedAt:       now,
			OwnerInstanceID: request.OwnerInstanceID,
			WorkerRunID:     request.WorkerRunID,
			RuntimeStatus:   RuntimePending,
		}
		entry = head
		return tx.Commit()
	})
	if err != nil {
		return nil, nil, err
	}
	switch {
	case notFound:
		return nil, nil, ErrNotFound
	case busy:
		return nil, nil, ErrSessionBusy
	case moved:
		return nil, nil, ErrQueueHeadMoved
	case stale:
		return nil, nil, ErrQueueLifecycleStale
	}
	// Read the committed row back rather than returning the row the
	// transaction assembled: the stored record carries the input key and payload
	// hash the dispatcher needs to correlate the turn, and returning a partial
	// hand-built copy would invite a caller to trust an empty field.
	stored, err := s.getByID(ctx, claimed.ExecutionID)
	if err != nil {
		return nil, nil, err
	}
	return stored, entry, nil
}

// CancelQueued settles one undispatched input. It refuses once the item has
// been dispatched: a caller that wanted a running input stopped must use the
// ordinary stop path, which records what actually happened.
func (s *SQLStore) CancelQueued(ctx context.Context, executionID, reason string) (*Record, error) {
	if executionID == "" {
		return nil, errors.New("execution: execution id is required")
	}
	if reason == "" {
		reason = QueueReasonCancelled
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var settled *Record
	err := s.withWriteLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("execution: begin queue cancel: %w", err)
		}
		now := time.Now().UnixMilli()
		ok, err := s.settleQueuedTx(ctx, tx, executionID, reason, now)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if !ok {
			_ = tx.Rollback()
			return ErrQueueNotQueued
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("execution: commit queue cancel: %w", err)
		}
		settled, err = s.getByID(ctx, executionID)
		if err != nil {
			return fmt.Errorf("execution: read back cancelled execution: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return settled, nil
}

// ClearQueue settles every undispatched input for a session. This is what
// /reset and session delete call, so a queue belonging to an abandoned turn
// never dispatches afterwards. Already-dispatched inputs are left alone.
func (s *SQLStore) ClearQueue(ctx context.Context, sessionID, reason string) (int64, error) {
	if sessionID == "" {
		return 0, errors.New("execution: session id is required")
	}
	if reason == "" {
		reason = QueueReasonCancelled
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var cleared int64
	err := s.withWriteLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("execution: begin queue clear: %w", err)
		}
		fail := func(e error) error {
			_ = tx.Rollback()
			return e
		}
		// Serialize per session so a concurrent claim cannot slip between the
		// head read and the settle.
		if _, err := tx.ExecContext(ctx, s.rebind(`
			UPDATE execution_queue_counters SET next_seq = next_seq WHERE session_id = ?`),
			sessionID); err != nil {
			return fail(fmt.Errorf("execution: lock queue counter for clear: %w", err))
		}

		ids, err := queueExecutionIDs(ctx, s, tx, sessionID)
		if err != nil {
			return fail(err)
		}

		now := time.Now().UnixMilli()
		for _, id := range ids {
			ok, err := s.settleQueuedTx(ctx, tx, id, reason, now)
			if err != nil {
				return fail(err)
			}
			if ok {
				cleared++
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("execution: commit queue clear: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return cleared, nil
}

// queueExecutionIDs lists a session's queued execution IDs in dispatch order.
func queueExecutionIDs(ctx context.Context, s *SQLStore, tx *sql.Tx, sessionID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, s.rebind(`
		SELECT execution_id FROM execution_queue WHERE session_id = ?
		ORDER BY queue_seq ASC`), sessionID)
	if err != nil {
		return nil, fmt.Errorf("execution: list queue ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("execution: scan queue id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("execution: iterate queue ids: %w", err)
	}
	return ids, nil
}

// ExpireQueued settles undispatched inputs whose TTL has elapsed, oldest
// first, up to limit. A queued input that waited longer than its bound is no
// longer what the user asked for, so it is settled rather than silently sent.
func (s *SQLStore) ExpireQueued(ctx context.Context, now time.Time, limit int) ([]*Record, error) {
	if limit <= 0 {
		limit = defaultQueueListLimit
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var expired []*Record
	var settledIDs []string
	err := s.withWriteLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("execution: begin queue expire: %w", err)
		}
		fail := func(e error) error {
			_ = tx.Rollback()
			return e
		}

		nowMillis := now.UnixMilli()
		rows, err := tx.QueryContext(ctx, s.rebind(`
			SELECT execution_id FROM execution_queue
			WHERE expires_at <= ?
			ORDER BY expires_at ASC
			LIMIT ?`), nowMillis, limit)
		if err != nil {
			return fail(fmt.Errorf("execution: list expired queue: %w", err))
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fail(fmt.Errorf("execution: scan expired queue: %w", err))
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fail(fmt.Errorf("execution: iterate expired queue: %w", err))
		}
		_ = rows.Close()

		for _, id := range ids {
			ok, err := s.settleQueuedTx(ctx, tx, id, QueueReasonExpired, nowMillis)
			if err != nil {
				return fail(err)
			}
			if ok {
				settledIDs = append(settledIDs, id)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("execution: commit queue expire: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Read the settled rows back so the caller reports what is actually stored,
	// not what the sweep intended.
	for _, id := range settledIDs {
		record, err := s.getByID(ctx, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		expired = append(expired, record)
	}
	return expired, nil
}

// QueueDepthBySession returns the number of undispatched inputs for one
// session.
func (s *SQLStore) QueueDepthBySession(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, errors.New("execution: session id is required")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var depth int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM execution_queue WHERE session_id = ?`, sessionID).Scan(&depth); err != nil {
		return 0, fmt.Errorf("execution: session queue depth: %w", err)
	}
	return depth, nil
}
