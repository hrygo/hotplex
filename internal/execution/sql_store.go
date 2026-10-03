package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

const storeTimeout = 5 * time.Second

// SQLStore persists execution ingress records in the gateway database.
type SQLStore struct {
	db      *sql.DB
	dialect dbutil.Dialect
	writeMu *sqlutil.WriteMu
	log     *slog.Logger
}

var _ Store = (*SQLStore)(nil)

func NewSQLStore(_ context.Context, db *sql.DB, dialect dbutil.Dialect, writeMu *sqlutil.WriteMu, log *slog.Logger) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("execution: nil database")
	}
	if log == nil {
		log = slog.Default()
	}
	return &SQLStore{
		db:      db,
		dialect: dialect,
		writeMu: writeMu,
		log:     log.With("component", "execution_store"),
	}, nil
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, storeTimeout)
}

func (s *SQLStore) rebind(query string) string {
	return s.dialect.Rebind(query)
}

func (s *SQLStore) dbNowMillisExpr() string {
	if s.dialect == dbutil.DialectPostgres {
		return `CAST(EXTRACT(EPOCH FROM clock_timestamp()) * 1000 AS BIGINT)`
	}
	return `CAST(strftime('%s','now') AS INTEGER) * 1000`
}

func (s *SQLStore) withWriteLock(fn func() error) error {
	if s.writeMu == nil {
		return fn()
	}
	return s.writeMu.WithLock(fn)
}

const executionColumns = `execution_id, session_id, client_message_id, payload_hash, status,
	error_code, created_at, updated_at, delivered_at,
	owner_instance_id, worker_run_id, lease_until,
	runtime_status, runtime_error_code, started_at, finished_at, fence_reason,
	fence_version, fence_created_at`

// ListRecent bounds. A caller that passes no limit gets a page, not a table
// dump: the operator console is a browsing surface, and "return everything"
// is never a query this store should be able to answer.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

func (s *SQLStore) scanRecord(sc interface {
	Scan(dest ...any) error
}) (*Record, error) {
	r := new(Record)
	var deliveredAt, startedAt, finishedAt, fenceCreatedAt sql.NullInt64
	err := sc.Scan(
		&r.ExecutionID, &r.SessionID, &r.ClientMessageID, &r.PayloadHash, &r.Status,
		&r.ErrorCode, &r.CreatedAt, &r.UpdatedAt, &deliveredAt,
		&r.OwnerInstanceID, &r.WorkerRunID, &r.LeaseUntil,
		&r.RuntimeStatus, &r.RuntimeErrorCode, &startedAt, &finishedAt, &r.FenceReason,
		&r.FenceVersion, &fenceCreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if deliveredAt.Valid {
		r.DeliveredAt = &deliveredAt.Int64
	}
	if startedAt.Valid {
		r.StartedAt = &startedAt.Int64
	}
	if finishedAt.Valid {
		r.FinishedAt = &finishedAt.Int64
	}
	if fenceCreatedAt.Valid {
		r.FenceCreatedAt = &fenceCreatedAt.Int64
	}
	return r, nil
}

// fenceEntryBump returns the SET fragments that increment fence_version and
// stamp fence_created_at when a row enters the fenced state. The CASE guards
// keep the bump idempotent: only the transition from a non-fenced row counts
// as fence entry. Empty when the update does not raise a fence.
func (s *SQLStore) fenceEntryBump(setsFence bool) string {
	if !setsFence {
		return ""
	}
	return `, fence_version = CASE WHEN fence_reason = '' THEN fence_version + 1 ELSE fence_version END,
	    fence_created_at = CASE WHEN fence_reason = '' THEN ` + s.dbNowMillisExpr() + ` ELSE fence_created_at END`
}

func (s *SQLStore) Accept(ctx context.Context, request AcceptRequest) (*Record, bool, error) {
	if request.SessionID == "" || request.ClientMessageID == "" || request.PayloadHash == "" {
		return nil, false, errors.New("execution: session id, client message id, and payload hash are required")
	}
	if request.OwnerInstanceID == "" {
		return nil, false, errors.New("execution: owner instance id is required")
	}
	if request.WorkerRunID == "" {
		return nil, false, errors.New("execution: worker run id is required")
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	now := time.Now().UnixMilli()
	record := &Record{
		ExecutionID:     "exec_" + uuid.NewString(),
		SessionID:       request.SessionID,
		ClientMessageID: request.ClientMessageID,
		PayloadHash:     request.PayloadHash,
		Status:          StatusAccepted,
		CreatedAt:       now,
		UpdatedAt:       now,
		OwnerInstanceID: request.OwnerInstanceID,
		WorkerRunID:     request.WorkerRunID,
		RuntimeStatus:   RuntimePending,
	}

	var inserted bool
	err := s.withWriteLock(func() error {
		result, err := s.db.ExecContext(ctx, s.rebind(`
			INSERT INTO execution_inputs
				(execution_id, session_id, client_message_id, payload_hash, status, error_code,
				 created_at, updated_at, owner_instance_id, worker_run_id, lease_until,
				 runtime_status, runtime_error_code, fence_reason)
			VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, `+s.dbNowMillisExpr()+` + ?, ?, '', '')
			ON CONFLICT(session_id, client_message_id) DO NOTHING`),
			record.ExecutionID, record.SessionID, record.ClientMessageID, record.PayloadHash,
			record.Status, record.CreatedAt, record.UpdatedAt,
			record.OwnerInstanceID, record.WorkerRunID, int64(LeaseTTL)*1000, record.RuntimeStatus)
		if err != nil {
			if s.dialect.IsUniqueViolation(err) {
				return ErrSessionBusy
			}
			return fmt.Errorf("execution: accept input: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("execution: accept rows affected: %w", err)
		}
		inserted = rows == 1
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if inserted {
		stored, err := s.getByID(ctx, record.ExecutionID)
		if err != nil {
			return nil, false, err
		}
		return stored, false, nil
	}

	existing, err := s.getByClientMessage(ctx, request.SessionID, request.ClientMessageID)
	if err != nil {
		return nil, false, err
	}
	if existing.PayloadHash != request.PayloadHash {
		return nil, true, ErrPayloadConflict
	}
	return existing, true, nil
}

func (s *SQLStore) getByClientMessage(ctx context.Context, sessionID, clientMessageID string) (*Record, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT `+executionColumns+`
		FROM execution_inputs
		WHERE session_id = ? AND client_message_id = ?`), sessionID, clientMessageID)
	r, err := s.scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("execution: get input: %w", err)
	}
	return r, nil
}

func (s *SQLStore) SetStatus(ctx context.Context, executionID string, status Status, errorCode string) error {
	if executionID == "" {
		return errors.New("execution: execution id is required")
	}
	if status != StatusDelivered && status != StatusUnknown && status != StatusFailed {
		return fmt.Errorf("execution: invalid terminal status %q", status)
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	now := time.Now().UnixMilli()
	var deliveredAt any
	if status == StatusDelivered {
		deliveredAt = now
	}

	var rows int64
	err := s.withWriteLock(func() error {
		result, err := s.db.ExecContext(ctx, s.rebind(`
			UPDATE execution_inputs
			SET status = ?, error_code = ?, updated_at = ?, delivered_at = ?
			WHERE execution_id = ? AND status = ?`),
			status, errorCode, now, deliveredAt, executionID, StatusAccepted)
		if err != nil {
			return fmt.Errorf("execution: set status: %w", err)
		}
		rows, _ = result.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		var current Status
		readCtx, readCancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
		err := s.db.QueryRowContext(readCtx, s.rebind(`
			SELECT status FROM execution_inputs WHERE execution_id = ?`), executionID).Scan(&current)
		readCancel()
		if err == nil && current == status {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("execution: get current status: %w", err)
		}
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) SetDelivery(ctx context.Context, executionID, ownerID string, status Status, errorCode string) error {
	if executionID == "" || ownerID == "" {
		return errors.New("execution: execution id and owner id are required")
	}
	if status != StatusDelivered && status != StatusUnknown && status != StatusFailed {
		return fmt.Errorf("execution: invalid delivery status %q", status)
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	now := time.Now().UnixMilli()
	var deliveredAt any
	if status == StatusDelivered {
		deliveredAt = now
	}

	var rows int64
	err := s.withWriteLock(func() error {
		result, err := s.db.ExecContext(ctx, s.rebind(`
			UPDATE execution_inputs
			SET status = ?, error_code = ?, updated_at = ?, delivered_at = ?
			WHERE execution_id = ? AND owner_instance_id = ? AND status = ?`),
			status, errorCode, now, deliveredAt, executionID, ownerID, StatusAccepted)
		if err != nil {
			return fmt.Errorf("execution: set delivery: %w", err)
		}
		rows, _ = result.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		r, err := s.getByID(ctx, executionID)
		if err != nil {
			return err
		}
		if r.Status == status {
			return nil
		}
		if r.OwnerInstanceID != ownerID {
			return ErrOwnerMismatch
		}
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) MarkRunning(ctx context.Context, executionID, ownerID, workerRunID string) error {
	if executionID == "" || ownerID == "" || workerRunID == "" {
		return errors.New("execution: execution id, owner id, and worker run id are required")
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	now := time.Now().UnixMilli()
	var rows int64
	err := s.withWriteLock(func() error {
		result, err := s.db.ExecContext(ctx, s.rebind(`
			UPDATE execution_inputs
			SET runtime_status = ?, worker_run_id = ?, started_at = ?, updated_at = ?
			WHERE execution_id = ? AND owner_instance_id = ? AND runtime_status = ?`),
			RuntimeRunning, workerRunID, now, now, executionID, ownerID, RuntimePending)
		if err != nil {
			return fmt.Errorf("execution: mark running: %w", err)
		}
		rows, _ = result.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		r, err := s.getByID(ctx, executionID)
		if err != nil {
			return err
		}
		if r.OwnerInstanceID != ownerID {
			return ErrOwnerMismatch
		}
		if r.RuntimeStatus == RuntimeRunning && r.WorkerRunID == workerRunID {
			return nil
		}
		return fmt.Errorf("execution: mark running: runtime_status is %q, expected pending", r.RuntimeStatus)
	}
	return nil
}

func (s *SQLStore) FinishRuntime(ctx context.Context, executionID, workerRunID string, status RuntimeStatus, errorCode string) error {
	if executionID == "" || workerRunID == "" {
		return errors.New("execution: execution id and worker run id are required")
	}
	if status != RuntimeCompleted && status != RuntimeFailed && status != RuntimeUnknown {
		return fmt.Errorf("execution: invalid terminal runtime status %q", status)
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	now := time.Now().UnixMilli()
	fenceReason := ""
	if status == RuntimeUnknown {
		fenceReason = "RUNTIME_AMBIGUOUS"
	}

	var currentFilter string
	if status == RuntimeCompleted || status == RuntimeFailed {
		currentFilter = "('pending', 'running', 'unknown')"
	} else {
		currentFilter = "('pending', 'running')"
	}

	var rows int64
	err := s.withWriteLock(func() error {
		result, err := s.db.ExecContext(ctx, s.rebind(`
			UPDATE execution_inputs
			SET runtime_status = ?, runtime_error_code = ?, finished_at = ?,
			    fence_reason = ?, updated_at = ?`+s.fenceEntryBump(fenceReason != "")+`
			WHERE execution_id = ? AND worker_run_id = ?
			  AND runtime_status IN `+currentFilter),
			status, errorCode, now, fenceReason, now,
			executionID, workerRunID)
		if err != nil {
			return fmt.Errorf("execution: finish runtime: %w", err)
		}
		rows, _ = result.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		r, err := s.getByID(ctx, executionID)
		if err != nil {
			return err
		}
		if r.RuntimeStatus == status && r.WorkerRunID == workerRunID {
			return nil
		}
		if r.WorkerRunID != workerRunID {
			return ErrRunMismatch
		}
		return fmt.Errorf("execution: finish runtime: current %q cannot transition to %q", r.RuntimeStatus, status)
	}
	return nil
}

// ConvergeDeliveryFailed rewrites a delivery recorded as failed to unknown when
// the runtime has since completed for the same worker run. A late Done proves
// the worker actually executed the input, so a failed delivery (e.g. a native
// skill invocation that timed out client-side while the server kept running)
// is a misclassification. The original error code is retained for diagnostics.
// This is the single explicit exception to the terminal-never-regresses
// invariant: the update only fires on a failed delivery whose runtime already
// reached completed, and unknown never triggers automatic redelivery. Any
// other combination is a no-op.
func (s *SQLStore) ConvergeDeliveryFailed(ctx context.Context, executionID, workerRunID string) error {
	if executionID == "" || workerRunID == "" {
		return errors.New("execution: execution id and worker run id are required")
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	err := s.withWriteLock(func() error {
		_, err := s.db.ExecContext(ctx, s.rebind(`
			UPDATE execution_inputs
			SET status = ?, updated_at = ?
			WHERE execution_id = ? AND worker_run_id = ?
			  AND status = ? AND runtime_status = ?`),
			StatusUnknown, time.Now().UnixMilli(),
			executionID, workerRunID, StatusFailed, RuntimeCompleted)
		return err
	})
	if err != nil {
		return fmt.Errorf("execution: converge delivery failed: %w", err)
	}
	return nil
}

func (s *SQLStore) ActiveBySession(ctx context.Context, sessionID string) (*Record, error) {
	if sessionID == "" {
		return nil, errors.New("execution: session id is required")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	row := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT `+executionColumns+`
		FROM execution_inputs
		WHERE session_id = ? AND runtime_status IN ('pending', 'running')
		ORDER BY created_at DESC LIMIT 1`), sessionID)
	r, err := s.scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("execution: active by session: %w", err)
	}
	return r, nil
}

func (s *SQLStore) OpenBySession(ctx context.Context, sessionID string) (*Record, error) {
	if sessionID == "" {
		return nil, errors.New("execution: session id is required")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	row := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT `+executionColumns+`
		FROM execution_inputs
		WHERE session_id = ? AND runtime_status IN ('pending', 'running', 'unknown')
		ORDER BY created_at DESC LIMIT 1`), sessionID)
	r, err := s.scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("execution: open by session: %w", err)
	}
	return r, nil
}

// LatestBySession returns the most recent execution record for the session
// regardless of runtime status (pending/running/unknown/completed/failed), or
// ErrNotFound if none exists. Used by the stop path to identify the turn an
// in-flight stop belongs to: the stop's own FinishRuntime closes the runtime
// BEFORE a duplicate stop arrives, so OpenBySession (open runtimes only) would
// resolve to a different (or no) record and the single-stop fence would admit
// the duplicate. The latest record is stable across a stopped turn's runtime
// closure and changes only when a NEW turn accepts a new execution.
func (s *SQLStore) LatestBySession(ctx context.Context, sessionID string) (*Record, error) {
	if sessionID == "" {
		return nil, errors.New("execution: session id is required")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	row := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT `+executionColumns+`
		FROM execution_inputs
		WHERE session_id = ?
		ORDER BY created_at DESC LIMIT 1`), sessionID)
	r, err := s.scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("execution: latest by session: %w", err)
	}
	return r, nil
}

func (s *SQLStore) FenceBySession(ctx context.Context, sessionID string) (*Record, error) {
	if sessionID == "" {
		return nil, errors.New("execution: session id is required")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	row := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT `+executionColumns+`
		FROM execution_inputs
		WHERE session_id = ? AND fence_reason <> ''
		ORDER BY created_at DESC LIMIT 1`), sessionID)
	r, err := s.scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("execution: fence by session: %w", err)
	}
	return r, nil
}

func (s *SQLStore) ByID(ctx context.Context, executionID string) (*Record, error) {
	if executionID == "" {
		return nil, errors.New("execution: execution id is required")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	row := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT `+executionColumns+`
		FROM execution_inputs
		WHERE execution_id = ?`), executionID)
	r, err := s.scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("execution: by id: %w", err)
	}
	return r, nil
}

func (s *SQLStore) ListRecent(ctx context.Context, filter ListFilter) ([]*Record, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	query := `SELECT ` + executionColumns + ` FROM execution_inputs WHERE 1=1`
	args := []any{}
	if filter.SessionID != "" {
		query += ` AND session_id = ?`
		args = append(args, filter.SessionID)
	}
	if filter.DeliveryStatus != "" {
		query += ` AND status = ?`
		args = append(args, string(filter.DeliveryStatus))
	}
	if filter.RuntimeStatus != "" {
		query += ` AND runtime_status = ?`
		args = append(args, string(filter.RuntimeStatus))
	}
	if filter.SinceMs > 0 {
		query += ` AND created_at >= ?`
		args = append(args, filter.SinceMs)
	}
	if filter.UntilMs > 0 {
		query += ` AND created_at < ?`
		args = append(args, filter.UntilMs)
	}
	// Keyset pagination rather than OFFSET: rows arriving between pages would
	// otherwise shift the window and make a console page silently skip or
	// repeat executions.
	if filter.BeforeCreatedAt > 0 {
		query += ` AND (created_at < ? OR (created_at = ? AND execution_id < ?))`
		args = append(args, filter.BeforeCreatedAt, filter.BeforeCreatedAt, filter.BeforeExecutionID)
	}
	query += ` ORDER BY created_at DESC, execution_id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("execution: list recent: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]*Record, 0, limit)
	for rows.Next() {
		r, err := s.scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("execution: list recent scan: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("execution: list recent iterate: %w", err)
	}
	return records, nil
}

func (s *SQLStore) ClearFenceAfterFreshStart(ctx context.Context, executionID, reason, freshWorkerRunID string) error {
	if executionID == "" || reason == "" || freshWorkerRunID == "" {
		return errors.New("execution: execution id, reason, and fresh worker run id are required")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	now := time.Now().UnixMilli()
	var rows int64
	err := s.withWriteLock(func() error {
		result, err := s.db.ExecContext(ctx, s.rebind(`
			UPDATE execution_inputs
			SET fence_reason = '', worker_run_id = ?, updated_at = ?
			WHERE execution_id = ? AND fence_reason = ?`),
			freshWorkerRunID, now, executionID, reason)
		if err != nil {
			return fmt.Errorf("execution: clear fence: %w", err)
		}
		rows, _ = result.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		r, err := s.getByID(ctx, executionID)
		if err != nil {
			return err
		}
		if r.FenceReason == "" {
			return nil
		}
		return fmt.Errorf("execution: clear fence: fence_reason is %q, expected %q", r.FenceReason, reason)
	}
	return nil
}

// listFencesDefaultLimit bounds ListFences when the caller passes no limit.
const listFencesDefaultLimit = 100

func (s *SQLStore) ApplyFenceDecision(ctx context.Context, request FenceActionRequest) (*Record, error) {
	if request.ExecutionID == "" {
		return nil, errors.New("execution: execution id is required")
	}
	switch request.Decision {
	case FenceDecisionResolve, FenceDecisionAbandon:
	default:
		return nil, fmt.Errorf("execution: invalid fence decision %q", request.Decision)
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	now := time.Now().UnixMilli()
	var rows int64
	err := s.withWriteLock(func() error {
		var (
			result sql.Result
			err    error
		)
		if request.Decision == FenceDecisionResolve {
			// Resolve clears the fence and keeps runtime_status=unknown: it
			// never marks the execution delivered/completed.
			result, err = s.db.ExecContext(ctx, s.rebind(`
				UPDATE execution_inputs
				SET fence_reason = '', updated_at = ?
				WHERE execution_id = ? AND fence_reason <> '' AND fence_version = ?`),
				now, request.ExecutionID, request.ExpectedFenceVersion)
		} else {
			// Abandon terminates the runtime as failed. The runtime_status
			// predicate makes terminal non-regression explicit at the SQL
			// level: fenced rows are runtime-unknown by construction.
			result, err = s.db.ExecContext(ctx, s.rebind(`
				UPDATE execution_inputs
				SET fence_reason = '', runtime_status = ?, runtime_error_code = ?,
				    finished_at = ?, updated_at = ?
				WHERE execution_id = ? AND fence_reason <> '' AND fence_version = ?
				  AND runtime_status = ?`),
				RuntimeFailed, RuntimeErrorCodeOperatorAbandoned, now, now,
				request.ExecutionID, request.ExpectedFenceVersion, RuntimeUnknown)
		}
		if err != nil {
			return fmt.Errorf("execution: apply fence decision: %w", err)
		}
		rows, _ = result.RowsAffected()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		current, err := s.getByID(ctx, request.ExecutionID)
		if err != nil {
			return nil, err
		}
		return nil, fenceConflictFor(current, request.ExpectedFenceVersion)
	}
	return s.getByID(ctx, request.ExecutionID)
}

// fenceConflictFor classifies a failed conditional fence update so operators
// get an actionable 409 instead of a silent no-op.
func fenceConflictFor(current *Record, expectedVersion int64) error {
	if current.FenceReason == "" {
		return fmt.Errorf("%w: fence already cleared", ErrFenceConflict)
	}
	if current.FenceVersion != expectedVersion {
		return fmt.Errorf("%w: fence version is %d, expected %d", ErrFenceConflict, current.FenceVersion, expectedVersion)
	}
	return fmt.Errorf("%w: runtime status is %q", ErrFenceConflict, current.RuntimeStatus)
}

func (s *SQLStore) ListFences(ctx context.Context, sessionID string, limit, offset int) ([]*Record, error) {
	if limit <= 0 {
		limit = listFencesDefaultLimit
	}
	if offset < 0 {
		offset = 0
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	query := `
		SELECT ` + executionColumns + `
		FROM execution_inputs
		WHERE fence_reason <> ''`
	args := []any{}
	if sessionID != "" {
		query += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	// COALESCE keeps legacy rows (fenced before migration 031, created_at
	// NULL) ordered deterministically across dialects.
	query += ` ORDER BY COALESCE(fence_created_at, 0) DESC, created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("execution: list fences: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]*Record, 0, limit)
	for rows.Next() {
		r, err := s.scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("execution: list fences scan: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("execution: list fences iterate: %w", err)
	}
	return records, nil
}

func (s *SQLStore) RenewLeases(ctx context.Context, ownerID string, ttlSeconds int64, excludeExecutionIDs []string) (int64, error) {
	if ownerID == "" {
		return 0, errors.New("execution: owner id is required")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var rows int64
	err := s.withWriteLock(func() error {
		if len(excludeExecutionIDs) > 0 {
			var err error
			rows, err = s.renewLeasesExcluding(ctx, ownerID, ttlSeconds, excludeExecutionIDs)
			return err
		}
		query := `
			UPDATE execution_inputs
			SET lease_until = ` + s.dbNowMillisExpr() + ` + ?, updated_at = ` + s.dbNowMillisExpr() + `
			WHERE owner_instance_id = ? AND runtime_status IN ('pending', 'running')`
		args := []any{ttlSeconds * 1000, ownerID}
		result, err := s.db.ExecContext(ctx, s.rebind(query), args...)
		if err != nil {
			return fmt.Errorf("execution: renew leases: %w", err)
		}
		rows, _ = result.RowsAffected()
		return nil
	})
	return rows, err
}

const sqlIDBatchSize = 500

func (s *SQLStore) renewLeasesExcluding(ctx context.Context, ownerID string, ttlSeconds int64, excludedIDs []string) (int64, error) {
	excluded := make(map[string]struct{}, len(excludedIDs))
	for _, executionID := range excludedIDs {
		excluded[executionID] = struct{}{}
	}

	queryRows, err := s.db.QueryContext(ctx, s.rebind(`
		SELECT execution_id FROM execution_inputs
		WHERE owner_instance_id = ? AND runtime_status IN ('pending', 'running')`), ownerID)
	if err != nil {
		return 0, fmt.Errorf("execution: query renewable leases: %w", err)
	}
	var renewable []string
	for queryRows.Next() {
		var executionID string
		if err := queryRows.Scan(&executionID); err != nil {
			_ = queryRows.Close()
			return 0, fmt.Errorf("execution: scan renewable lease: %w", err)
		}
		if _, skip := excluded[executionID]; !skip {
			renewable = append(renewable, executionID)
		}
	}
	if err := queryRows.Err(); err != nil {
		_ = queryRows.Close()
		return 0, fmt.Errorf("execution: iterate renewable leases: %w", err)
	}
	if err := queryRows.Close(); err != nil {
		return 0, fmt.Errorf("execution: close renewable leases: %w", err)
	}

	var renewed int64
	for start := 0; start < len(renewable); start += sqlIDBatchSize {
		end := start + sqlIDBatchSize
		if end > len(renewable) {
			end = len(renewable)
		}
		batch := renewable[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch)+2)
		args = append(args, ttlSeconds*1000, ownerID)
		for i, executionID := range batch {
			placeholders[i] = "?"
			args = append(args, executionID)
		}
		result, err := s.db.ExecContext(ctx, s.rebind(`
			UPDATE execution_inputs
			SET lease_until = `+s.dbNowMillisExpr()+` + ?, updated_at = `+s.dbNowMillisExpr()+`
			WHERE owner_instance_id = ? AND runtime_status IN ('pending', 'running')
			  AND execution_id IN (`+strings.Join(placeholders, ",")+`)`), args...)
		if err != nil {
			return renewed, fmt.Errorf("execution: renew lease batch: %w", err)
		}
		count, _ := result.RowsAffected()
		renewed += count
	}
	return renewed, nil
}

func (s *SQLStore) RecoverExpiredLeases(ctx context.Context, trackedExecutionIDs []string) (LeaseRecoveryResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var recovery LeaseRecoveryResult
	err := s.withWriteLock(func() error {
		result, err := s.db.ExecContext(ctx, s.rebind(`
			UPDATE execution_inputs
			SET status = CASE WHEN status = 'accepted' THEN 'unknown' ELSE status END,
			    runtime_status = 'unknown',
			    error_code = CASE
			        WHEN status = 'accepted' THEN 'GATEWAY_LEASE_EXPIRED'
			        ELSE error_code
			    END,
			    runtime_error_code = 'GATEWAY_LEASE_EXPIRED',
			    fence_reason = 'GATEWAY_LEASE_EXPIRED',
			    finished_at = `+s.dbNowMillisExpr()+`,
			    updated_at = `+s.dbNowMillisExpr()+
			s.fenceEntryBump(true)+`
			WHERE runtime_status IN ('pending', 'running')
			  AND lease_until <= `+s.dbNowMillisExpr()))
		if err != nil {
			return fmt.Errorf("execution: recover expired leases: %w", err)
		}
		recovery.Recovered, _ = result.RowsAffected()

		active, err := s.activeExecutionIDs(ctx, trackedExecutionIDs)
		if err != nil {
			return err
		}
		for _, executionID := range trackedExecutionIDs {
			if _, ok := active[executionID]; !ok {
				recovery.ConvergedExecutionIDs = append(recovery.ConvergedExecutionIDs, executionID)
			}
		}
		return nil
	})
	return recovery, err
}

func (s *SQLStore) activeExecutionIDs(ctx context.Context, executionIDs []string) (map[string]struct{}, error) {
	active := make(map[string]struct{})
	for start := 0; start < len(executionIDs); start += sqlIDBatchSize {
		end := start + sqlIDBatchSize
		if end > len(executionIDs) {
			end = len(executionIDs)
		}
		batch := executionIDs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, executionID := range batch {
			placeholders[i] = "?"
			args[i] = executionID
		}
		rows, err := s.db.QueryContext(ctx, s.rebind(`
			SELECT execution_id FROM execution_inputs
			WHERE runtime_status IN ('pending', 'running')
			  AND execution_id IN (`+strings.Join(placeholders, ",")+`)`), args...)
		if err != nil {
			return nil, fmt.Errorf("execution: query active tracked leases: %w", err)
		}
		for rows.Next() {
			var executionID string
			if err := rows.Scan(&executionID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("execution: scan active tracked lease: %w", err)
			}
			active[executionID] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("execution: close active tracked leases: %w", err)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("execution: iterate active tracked leases: %w", err)
		}
	}
	return active, nil
}

func (s *SQLStore) TerminateOwnerLeases(ctx context.Context, ownerID, reason string) (int64, error) {
	if ownerID == "" {
		return 0, errors.New("execution: owner id is required")
	}
	if reason == "" {
		reason = "GATEWAY_SHUTDOWN"
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	now := time.Now().UnixMilli()
	var rows int64
	err := s.withWriteLock(func() error {
		result, err := s.db.ExecContext(ctx, s.rebind(`
			UPDATE execution_inputs
			SET status = CASE WHEN status = 'accepted' THEN 'unknown' ELSE status END,
			    error_code = CASE WHEN status = 'accepted' THEN ? ELSE error_code END,
			    runtime_status = 'unknown',
			    runtime_error_code = ?,
			    fence_reason = ?,
			    finished_at = ?,
			    updated_at = ?`+s.fenceEntryBump(true)+`
			WHERE owner_instance_id = ?
			  AND runtime_status IN ('pending', 'running')`),
			reason, reason, reason, now, now, ownerID)
		if err != nil {
			return fmt.Errorf("execution: terminate owner leases: %w", err)
		}
		rows, _ = result.RowsAffected()
		return nil
	})
	return rows, err
}

func (s *SQLStore) getByID(ctx context.Context, executionID string) (*Record, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT `+executionColumns+`
		FROM execution_inputs
		WHERE execution_id = ?`), executionID)
	r, err := s.scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("execution: get by id: %w", err)
	}
	return r, nil
}
