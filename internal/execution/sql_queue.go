package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const queueEntryColumns = `execution_id, session_id, queue_seq, enqueued_at, expires_at,
	lifecycle_revision, payload_ref, payload_bytes`

// defaultQueueListLimit bounds QueueBySession when the caller asks for
// everything. A queue read is a projection input, not a dump.
const defaultQueueListLimit = 100

// querier is satisfied by both *sql.DB and *sql.Tx, so the record lookups can
// run inside the accepting transaction or outside it.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func scanQueueEntry(sc interface{ Scan(dest ...any) error }) (*QueueEntry, error) {
	e := new(QueueEntry)
	if err := sc.Scan(
		&e.ExecutionID, &e.SessionID, &e.QueueSeq, &e.EnqueuedAt, &e.ExpiresAt,
		&e.LifecycleRevision, &e.PayloadRef, &e.PayloadBytes,
	); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *SQLStore) getByClientMessageFrom(ctx context.Context, q querier, sessionID, clientMessageID string) (*Record, error) {
	r, err := s.scanRecord(q.QueryRowContext(ctx, s.rebind(`
		SELECT `+executionColumns+`
		FROM execution_inputs
		WHERE session_id = ? AND client_message_id = ?`), sessionID, clientMessageID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("execution: get by client message: %w", err)
	}
	return r, nil
}

func (s *SQLStore) queueEntryByExecution(ctx context.Context, q querier, executionID string) (*QueueEntry, error) {
	e, err := scanQueueEntry(q.QueryRowContext(ctx, s.rebind(`
		SELECT `+queueEntryColumns+`
		FROM execution_queue
		WHERE execution_id = ?`), executionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("execution: queue entry by execution: %w", err)
	}
	return e, nil
}

// AcceptQueued durably accepts an input into the bounded queue. Everything the
// caller is promised — durable record, dispatch ordinal, capacity — is decided
// and committed in one transaction. Nothing is written when a limit is hit.
func (s *SQLStore) AcceptQueued(
	ctx context.Context, request QueuedRequest, limits QueueLimits,
) (*Record, *QueueEntry, bool, error) {
	if request.SessionID == "" || request.ClientMessageID == "" || request.PayloadHash == "" {
		return nil, nil, false, errors.New("execution: session id, client message id, and payload hash are required")
	}
	if request.PayloadBytes < 0 {
		return nil, nil, false, errors.New("execution: payload bytes cannot be negative")
	}
	limits = limits.withDefaults()
	if request.PayloadBytes > limits.MaxPayloadBytes {
		return nil, nil, false, fmt.Errorf("%w: %d bytes exceeds the %d byte limit",
			ErrQueuePayloadTooLarge, request.PayloadBytes, limits.MaxPayloadBytes)
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var (
		stored    *Record
		entry     *QueueEntry
		duplicate bool
		// uniqueRace records that another transaction took the input key while
		// this one was deciding. The decision is then re-read from committed
		// state instead of being guessed.
		uniqueRace bool
	)
	err := s.withWriteLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("execution: begin queue accept: %w", err)
		}
		fail := func(e error) error {
			_ = tx.Rollback()
			return e
		}

		// Input idempotency is checked first, inside the transaction, so a
		// retry of the same message never consumes queue capacity or an
		// ordinal.
		existing, err := s.getByClientMessageFrom(ctx, tx, request.SessionID, request.ClientMessageID)
		if err == nil {
			if existing.PayloadHash != request.PayloadHash {
				// Same contract as Accept: the key was taken, so the caller is
				// told this was a duplicate AND that it conflicts. Reporting a
				// plain rejection would hide whether the earlier input is still
				// there.
				duplicate = true
				return fail(ErrPayloadConflict)
			}
			// A duplicate may name a queue row, a dispatched execution, or a
			// settled one. Only the first is still waiting.
			found, entryErr := s.queueEntryByExecution(ctx, tx, existing.ExecutionID)
			if entryErr != nil && !errors.Is(entryErr, ErrNotFound) {
				return fail(entryErr)
			}
			if entryErr == nil {
				entry = found
			}
			stored, duplicate = existing, true
			return tx.Commit()
		}
		if !errors.Is(err, ErrNotFound) {
			return fail(err)
		}

		// The global budget row is written first and unconditionally: that write
		// is the cross-instance lock for the global limit. A lock-free COUNT
		// would let two gateways both see room and both insert.
		if _, err := tx.ExecContext(ctx,
			`UPDATE execution_queue_budget SET used = used WHERE budget_id = 1`); err != nil {
			return fail(fmt.Errorf("execution: lock queue budget: %w", err))
		}
		var depth int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM execution_queue`).Scan(&depth); err != nil {
			return fail(fmt.Errorf("execution: count queue depth: %w", err))
		}
		if depth >= int64(limits.Global) {
			return fail(ErrQueueFull)
		}

		// Then the per-session ordinal allocator. Locking it is what serialises
		// concurrent enqueues for one session, so the depth check below cannot
		// be overtaken by a sibling transaction. The order (budget, then
		// session) is fixed so two enqueues can never take them in reverse.
		if _, err := tx.ExecContext(ctx, s.rebind(`
			INSERT INTO execution_queue_counters (session_id, next_seq)
			VALUES (?, 1)
			ON CONFLICT (session_id) DO NOTHING`), request.SessionID); err != nil {
			return fail(fmt.Errorf("execution: seed queue counter: %w", err))
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`
			UPDATE execution_queue_counters SET next_seq = next_seq + 1 WHERE session_id = ?`),
			request.SessionID); err != nil {
			return fail(fmt.Errorf("execution: advance queue counter: %w", err))
		}
		var nextSeq int64
		if err := tx.QueryRowContext(ctx, s.rebind(`
			SELECT next_seq FROM execution_queue_counters WHERE session_id = ?`),
			request.SessionID).Scan(&nextSeq); err != nil {
			return fail(fmt.Errorf("execution: read queue counter: %w", err))
		}
		queueSeq := nextSeq - 1

		var sessionDepth int64
		if err := tx.QueryRowContext(ctx, s.rebind(`
			SELECT COUNT(*) FROM execution_queue WHERE session_id = ?`),
			request.SessionID).Scan(&sessionDepth); err != nil {
			return fail(fmt.Errorf("execution: count session queue depth: %w", err))
		}
		if sessionDepth >= int64(limits.PerSession) {
			return fail(ErrQueueFull)
		}

		now := time.Now().UnixMilli()
		expiresAt := now + limits.TTL.Milliseconds()
		executionID := "exec_" + uuid.NewString()

		// A queued execution holds no lease and names no worker run: it has not
		// crossed the dispatch boundary, so claiming otherwise would make lease
		// recovery fence an input nobody ever sent.
		if _, err := tx.ExecContext(ctx, s.rebind(`
			INSERT INTO execution_inputs
				(execution_id, session_id, client_message_id, payload_hash, status, error_code,
				 created_at, updated_at, owner_instance_id, worker_run_id, lease_until,
				 runtime_status, runtime_error_code, fence_reason)
			VALUES (?, ?, ?, ?, 'accepted', '', ?, ?, ?, ?, 0, 'queued', '', '')`),
			executionID, request.SessionID, request.ClientMessageID, request.PayloadHash,
			now, now, request.OwnerInstanceID, ""); err != nil {
			if s.dialect.IsUniqueViolation(err) {
				// Another transaction took (session_id, client_message_id)
				// between the check above and this insert. Undo everything this
				// transaction reserved and let the caller re-read the winner's
				// facts.
				uniqueRace = true
				return fail(ErrSessionBusy)
			}
			return fail(fmt.Errorf("execution: insert queued execution: %w", err))
		}

		if _, err := tx.ExecContext(ctx, s.rebind(`
			INSERT INTO execution_queue
				(execution_id, session_id, queue_seq, enqueued_at, expires_at,
				 lifecycle_revision, payload_ref, payload_bytes)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			executionID, request.SessionID, queueSeq, now, expiresAt,
			request.LifecycleRevision, request.PayloadRef, request.PayloadBytes); err != nil {
			return fail(fmt.Errorf("execution: insert queue entry: %w", err))
		}

		// Refresh the depth mirror so an operator reading the budget row sees a
		// truthful number. It is never consulted for the capacity decision, so a
		// stale value here cannot refuse an enqueue.
		if _, err := tx.ExecContext(ctx,
			`UPDATE execution_queue_budget SET used = ? WHERE budget_id = 1`,
			depth+1); err != nil {
			return fail(fmt.Errorf("execution: refresh queue budget mirror: %w", err))
		}

		record := &Record{
			ExecutionID:     executionID,
			SessionID:       request.SessionID,
			ClientMessageID: request.ClientMessageID,
			PayloadHash:     request.PayloadHash,
			Status:          StatusAccepted,
			CreatedAt:       now,
			UpdatedAt:       now,
			OwnerInstanceID: request.OwnerInstanceID,
			RuntimeStatus:   RuntimeQueued,
		}
		stored = record
		entry = &QueueEntry{
			ExecutionID:       executionID,
			SessionID:         request.SessionID,
			QueueSeq:          queueSeq,
			EnqueuedAt:        now,
			ExpiresAt:         expiresAt,
			LifecycleRevision: request.LifecycleRevision,
			PayloadRef:        request.PayloadRef,
			PayloadBytes:      int64(request.PayloadBytes),
		}
		return tx.Commit()
	})
	if err != nil {
		if uniqueRace {
			// Re-read committed state rather than reporting a busy session: the
			// input key exists, so this is a duplicate or a conflict, and the
			// caller deserves that distinction.
			existing, readErr := s.getByClientMessage(ctx, request.SessionID, request.ClientMessageID)
			if readErr != nil {
				return nil, nil, false, readErr
			}
			if existing.PayloadHash != request.PayloadHash {
				return nil, nil, true, ErrPayloadConflict
			}
			found, entryErr := s.queueEntryByExecution(ctx, s.db, existing.ExecutionID)
			if entryErr != nil && !errors.Is(entryErr, ErrNotFound) {
				return nil, nil, true, entryErr
			}
			if entryErr != nil {
				found = nil
			}
			return existing, found, true, nil
		}
		// duplicate survives a conflict rejection: the key exists, and the
		// caller needs to know its earlier input is still there.
		return nil, nil, duplicate, err
	}
	return stored, entry, duplicate, nil
}

// QueueByExecution returns the scheduling state of one queued input.
func (s *SQLStore) QueueByExecution(ctx context.Context, executionID string) (*QueueEntry, error) {
	if executionID == "" {
		return nil, errors.New("execution: execution id is required")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return s.queueEntryByExecution(ctx, s.db, executionID)
}

// QueueBySession returns a session's undispatched inputs in dispatch order.
func (s *SQLStore) QueueBySession(ctx context.Context, sessionID string, limit int) ([]*QueueEntry, error) {
	if sessionID == "" {
		return nil, errors.New("execution: session id is required")
	}
	if limit <= 0 {
		limit = defaultQueueListLimit
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, s.rebind(`
		SELECT `+queueEntryColumns+`
		FROM execution_queue
		WHERE session_id = ?
		ORDER BY queue_seq ASC
		LIMIT ?`), sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("execution: list queue by session: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := make([]*QueueEntry, 0, 8)
	for rows.Next() {
		entry, err := scanQueueEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("execution: scan queue entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("execution: iterate queue entries: %w", err)
	}
	return entries, nil
}

// QueueDepth reads the queue itself rather than a maintained counter.
func (s *SQLStore) QueueDepth(ctx context.Context) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var depth int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM execution_queue`).Scan(&depth); err != nil {
		return 0, fmt.Errorf("execution: queue depth: %w", err)
	}
	return depth, nil
}
