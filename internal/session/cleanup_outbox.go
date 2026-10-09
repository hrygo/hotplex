package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

// CleanupTask is a durable request to delete a worker-owned remote session.
// WorkerSessionID is captured at deletion time and is never read from a later
// HotPlex session row.
type CleanupTask struct {
	ID              string
	SessionID       string
	WorkerType      worker.WorkerType
	WorkerSessionID string
	Attempts        int
	NextAttemptAt   time.Time
	LeaseUntil      *time.Time
	LeaseToken      string
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CleanupTaskStore is implemented by persistent session stores. Keeping this
// separate from Store preserves lightweight manager test doubles.
type CleanupTaskStore interface {
	MarkDeletedWithCleanup(ctx context.Context, info *SessionInfo) error
	DeletePhysicalWithCleanup(ctx context.Context, id string) (*SessionInfo, error)
	ClaimCleanupTasks(ctx context.Context, now, leaseUntil time.Time, limit int) ([]CleanupTask, error)
	CompleteCleanupTask(ctx context.Context, taskID, leaseToken string) error
	RetryCleanupTask(ctx context.Context, taskID, leaseToken string, nextAttemptAt time.Time, lastError string) error
	HasPendingCleanup(ctx context.Context, sessionID string) (bool, error)
}

// CleanupExecutor dispatches a task to the worker-type-specific cleaner.
type CleanupExecutor func(context.Context, worker.WorkerType, string) error

type unsupportedCleanupTaskStore interface {
	FailUnsupportedCleanupTask(ctx context.Context, taskID, leaseToken string) error
}

const (
	cleanupBatchSize      = 16
	cleanupLeaseDuration  = 30 * time.Second
	cleanupAttemptTimeout = 10 * time.Second
	cleanupPollInterval   = time.Second
	cleanupRetryMax       = 5 * time.Minute
	cleanupFailureCode    = "cleanup_failed"
)

// CleanupRunner leases and executes persistent cleanup tasks until ctx ends.
type CleanupRunner struct {
	log     *slog.Logger
	store   CleanupTaskStore
	execute CleanupExecutor
	now     func() time.Time
}

func NewCleanupRunner(log *slog.Logger, store CleanupTaskStore, execute CleanupExecutor) *CleanupRunner {
	if log == nil {
		log = slog.Default()
	}
	return &CleanupRunner{log: log.With("component", "session_cleanup_outbox"), store: store, execute: execute, now: time.Now}
}

// Run drains due work at startup and then polls. A failed task never prevents
// later tasks from running.
func (r *CleanupRunner) Run(ctx context.Context) {
	if r == nil || r.store == nil || r.execute == nil {
		return
	}
	r.RunOnce(ctx)
	ticker := time.NewTicker(cleanupPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RunOnce(ctx)
		}
	}
}

// RunOnce executes one leased batch. It is exported for deterministic tests.
func (r *CleanupRunner) RunOnce(ctx context.Context) {
	if r == nil || r.store == nil || r.execute == nil {
		return
	}
	now := r.now()
	tasks, err := r.store.ClaimCleanupTasks(ctx, now, now.Add(cleanupLeaseDuration), cleanupBatchSize)
	if err != nil {
		r.log.Warn("session cleanup: claim tasks failed", "err", err)
		return
	}
	for _, task := range tasks {
		attemptCtx, cancel := context.WithTimeout(ctx, cleanupAttemptTimeout)
		err := r.execute(attemptCtx, task.WorkerType, task.WorkerSessionID)
		cancel()
		if errors.Is(err, worker.ErrSessionCleanupUnsupported) {
			if store, ok := r.store.(unsupportedCleanupTaskStore); ok {
				if failureErr := store.FailUnsupportedCleanupTask(ctx, task.ID, task.LeaseToken); failureErr != nil {
					r.log.Warn("session cleanup: record unsupported capability failed", "task_id", task.ID, "err", failureErr)
				}
				continue
			}
		}
		if err == nil {
			if completeErr := r.store.CompleteCleanupTask(ctx, task.ID, task.LeaseToken); completeErr != nil {
				r.log.Warn("session cleanup: complete task failed", "task_id", task.ID, "session_id", task.SessionID, "err", completeErr)
			}
			continue
		}
		next := r.now().Add(cleanupBackoff(task.Attempts))
		if retryErr := r.store.RetryCleanupTask(ctx, task.ID, task.LeaseToken, next, cleanupFailureCode); retryErr != nil {
			r.log.Warn("session cleanup: retry scheduling failed", "task_id", task.ID, "session_id", task.SessionID, "err", retryErr)
			continue
		}
		r.log.Warn("session cleanup: remote delete failed; retry scheduled", "task_id", task.ID, "session_id", task.SessionID, "worker_type", task.WorkerType, "attempt", task.Attempts, "next_attempt_at", next, "error_code", cleanupFailureCode)
	}
}

func cleanupBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	shift := min(attempts-1, 8)
	delay := time.Second * time.Duration(math.Pow(2, float64(shift)))
	if delay > cleanupRetryMax {
		return cleanupRetryMax
	}
	return delay
}

func newCleanupTask(info *SessionInfo, now time.Time) *CleanupTask {
	if info == nil || info.WorkerSessionID == "" {
		return nil
	}
	return &CleanupTask{ID: uuid.NewString(), SessionID: info.ID, WorkerType: info.WorkerType, WorkerSessionID: info.WorkerSessionID, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
}

// upsertSessionArgs passes time.Time values to the driver unformatted. The
// modernc/sqlite driver already serializes them to RFC3339Nano (verified in the
// running DB, e.g. "2026-07-14T22:42:20.215036468+08:00") and binds both the
// stored column and the get_expired_* comparison params in that same format,
// keeping the lexicographic TEXT comparison correct. Manually UTC-formatting
// only the write side (a prior attempt at issue #879 #4) broke that invariant:
// GC expiry queries silently returned no rows. #879 #4's premise (driver uses
// time.Time.String()) does not hold on modernc v1.51.0 — the format is already
// canonical, so no write-side reformatting is applied here.
func upsertSessionArgs(info *SessionInfo, ctxJSON, pkJSON []byte) []any {
	policy := info.LifecyclePolicy
	if policy == "" {
		policy = "legacy"
	}
	return []any{info.ID, info.UserID, info.OwnerID, info.BotID, info.BotName, info.WorkerSessionID, info.WorkerType, string(info.State),
		info.Platform, string(pkJSON), info.WorkDir, info.Title, info.CreatedAt, info.UpdatedAt, info.ExpiresAt, info.IdleExpiresAt,
		string(ctxJSON), info.Source, info.ClientKey, nullableString(info.WorkspaceID),
		policy, info.LifecyclePolicyRevision, info.LastInputAt, info.RuntimeFinishedAt, info.ArchiveAt,
		info.ConversationExpiresAt, info.LastContentExpiresAt, info.HistoryExpiresAt, info.DeletedAt, info.ID}
}

func cleanupTaskColumns() string {
	return "id, session_id, worker_type, worker_session_id, attempts, next_attempt_at, lease_until, COALESCE(lease_token, ''), last_error, created_at, updated_at"
}

func scanCleanupTask(sc interface{ Scan(...any) error }) (CleanupTask, error) {
	var task CleanupTask
	var lease sql.NullTime
	err := sc.Scan(&task.ID, &task.SessionID, &task.WorkerType, &task.WorkerSessionID, &task.Attempts, &task.NextAttemptAt, &lease, &task.LeaseToken, &task.LastError, &task.CreatedAt, &task.UpdatedAt)
	if err != nil {
		return CleanupTask{}, err
	}
	if lease.Valid {
		task.LeaseUntil = &lease.Time
	}
	return task, nil
}

func insertCleanupTask(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, info *SessionInfo, now time.Time) error {
	task := newCleanupTask(info, now)
	if task == nil {
		return nil
	}
	_, err := execer.ExecContext(ctx, `INSERT INTO session_cleanup_tasks
		(id, session_id, worker_type, worker_session_id, attempts, next_attempt_at, lease_until, lease_token, last_error, created_at, updated_at)
		SELECT ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM session_purge_items WHERE session_id = ? AND kind = ? AND status IN (?, ?)
		)
		ON CONFLICT(session_id) DO NOTHING`,
		task.ID, task.SessionID, task.WorkerType, task.WorkerSessionID, task.Attempts, task.NextAttemptAt,
		task.LastError, task.CreatedAt, task.UpdatedAt, task.SessionID, PurgeItemWorkerSession,
		string(PurgeItemComplete), string(PurgeItemUnsupported))
	if err != nil {
		return fmt.Errorf("session cleanup: enqueue: %w", err)
	}
	return nil
}

func isCleanupPendingError(err error) bool {
	return errors.Is(err, ErrSessionCleanupPending) || (err != nil && containsCleanupPending(err.Error()))
}

func containsCleanupPending(message string) bool {
	return strings.Contains(message, "session cleanup pending") || strings.Contains(message, "session_cleanup_tasks")
}

type cleanupExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type purgeQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func identityRebind(query string) string { return query }

func insertPurgeJobAndItems(ctx context.Context, tx *sql.Tx, rebind func(string) string, info *SessionInfo, now time.Time) error {
	policyRevision := info.LifecyclePolicyRevision
	if policyRevision == "" {
		policyRevision = "legacy"
	}
	jobID := uuid.NewString()
	_, err := tx.ExecContext(ctx, rebind(`INSERT INTO session_purge_jobs
		(id, session_id, user_id, owner_id, workspace_id, policy_revision, status, requested_at, completed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
		ON CONFLICT(session_id) DO NOTHING`),
		jobID, info.ID, info.UserID, info.OwnerID, info.WorkspaceID, policyRevision,
		string(PurgeJobPending), now, now, now)
	if err != nil {
		return fmt.Errorf("session purge: create job: %w", err)
	}
	if err := tx.QueryRowContext(ctx, rebind(`SELECT id FROM session_purge_jobs WHERE session_id = ?`), info.ID).Scan(&jobID); err != nil {
		return fmt.Errorf("session purge: load job: %w", err)
	}

	workerState := PurgeItemComplete
	var workerCompletedAt *time.Time
	if info.WorkerSessionID != "" {
		workerState = PurgeItemPending
	} else {
		workerCompletedAt = &now
	}
	items := []struct {
		kind        string
		state       PurgeItemState
		completedAt *time.Time
	}{
		{kind: PurgeItemSessionMetadata, state: PurgeItemComplete, completedAt: &now},
		{kind: PurgeItemConversationContent, state: PurgeItemPending},
		{kind: PurgeItemWorkerSession, state: workerState, completedAt: workerCompletedAt},
	}
	for _, item := range items {
		_, err := tx.ExecContext(ctx, rebind(`INSERT INTO session_purge_items
			(id, job_id, session_id, kind, status, attempts, next_attempt_at, lease_until, lease_token, completed_at, error_code, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 0, ?, NULL, NULL, ?, '', ?, ?)
			ON CONFLICT(job_id, kind) DO NOTHING`),
			uuid.NewString(), jobID, info.ID, item.kind, string(item.state), now, item.completedAt, now, now)
		if err != nil {
			return fmt.Errorf("session purge: create %s item: %w", item.kind, err)
		}
	}
	return refreshPurgeJobStatus(ctx, tx, rebind, info.ID, now)
}

func refreshPurgeJobStatus(ctx context.Context, execer cleanupExecer, rebind func(string) string, sessionID string, now time.Time) error {
	_, err := execer.ExecContext(ctx, rebind(`UPDATE session_purge_jobs
		SET status = CASE
			WHEN EXISTS (
				SELECT 1 FROM session_purge_items
				WHERE session_id = ? AND status IN ('blocked', 'unsupported')
			) THEN 'blocked'
			WHEN EXISTS (
				SELECT 1 FROM session_purge_items
				WHERE session_id = ? AND status IN ('pending', 'running', 'retrying')
			) THEN 'in_progress'
			ELSE 'complete'
		END,
		completed_at = CASE
			WHEN NOT EXISTS (
				SELECT 1 FROM session_purge_items
				WHERE session_id = ? AND status <> 'complete'
			) THEN COALESCE(completed_at, ?)
			ELSE NULL
		END,
		updated_at = ?
		WHERE session_id = ?`), sessionID, sessionID, sessionID, now, now, sessionID)
	if err != nil {
		return fmt.Errorf("session purge: refresh job status: %w", err)
	}
	return nil
}

func getPurgeStatus(ctx context.Context, queryer purgeQueryer, rebind func(string) string, sessionID, userID string) (*PurgeStatus, error) {
	status := &PurgeStatus{SessionID: sessionID, Items: make([]PurgeItemStatus, 0)}
	var completedAt sql.NullTime
	err := queryer.QueryRowContext(ctx, rebind(`SELECT id, status, requested_at, updated_at, workspace_id, completed_at
		FROM session_purge_jobs WHERE session_id = ? AND user_id = ?`), sessionID, userID).
		Scan(&status.JobID, &status.Status, &status.RequestedAt, &status.UpdatedAt, &status.WorkspaceID, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPurgeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("session purge: load job status: %w", err)
	}
	if completedAt.Valid {
		status.CompletedAt = &completedAt.Time
	}

	rows, err := queryer.QueryContext(ctx, rebind(`SELECT kind, status, attempts, next_attempt_at, completed_at, error_code
		FROM session_purge_items WHERE session_id = ? ORDER BY kind`), sessionID)
	if err != nil {
		return nil, fmt.Errorf("session purge: load item status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item PurgeItemStatus
		var nextAttemptAt, itemCompletedAt sql.NullTime
		if err := rows.Scan(&item.Kind, &item.Status, &item.Attempts, &nextAttemptAt, &itemCompletedAt, &item.ErrorCode); err != nil {
			return nil, fmt.Errorf("session purge: scan item status: %w", err)
		}
		if nextAttemptAt.Valid {
			item.NextAttemptAt = &nextAttemptAt.Time
		}
		if itemCompletedAt.Valid {
			item.CompletedAt = &itemCompletedAt.Time
		}
		if item.Status == PurgeItemComplete {
			item.NextAttemptAt = nil
		}
		status.Items = append(status.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session purge: iterate item status: %w", err)
	}
	return status, nil
}

func markCleanupItemRunning(ctx context.Context, tx *sql.Tx, rebind func(string) string, sessionID string, attempts int, nextAttemptAt, leaseUntil time.Time, leaseToken string, now time.Time) error {
	_, err := tx.ExecContext(ctx, rebind(`UPDATE session_purge_items
		SET status = ?, attempts = ?, next_attempt_at = ?, lease_until = ?, lease_token = ?, completed_at = NULL, error_code = '', updated_at = ?
		WHERE session_id = ? AND kind = ? AND status <> ?`),
		string(PurgeItemRunning), attempts, nextAttemptAt, leaseUntil, leaseToken, now,
		sessionID, PurgeItemWorkerSession, string(PurgeItemComplete))
	if err != nil {
		return fmt.Errorf("session purge: mark worker item running: %w", err)
	}
	return refreshPurgeJobStatus(ctx, tx, rebind, sessionID, now)
}

func markCleanupItemComplete(ctx context.Context, tx *sql.Tx, rebind func(string) string, sessionID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, rebind(`UPDATE session_purge_items
		SET status = ?, lease_until = NULL, lease_token = NULL, completed_at = ?, error_code = '', updated_at = ?
		WHERE session_id = ? AND kind = ? AND status <> ?`),
		string(PurgeItemComplete), now, now, sessionID, PurgeItemWorkerSession, string(PurgeItemComplete))
	if err != nil {
		return fmt.Errorf("session purge: mark worker item complete: %w", err)
	}
	return refreshPurgeJobStatus(ctx, tx, rebind, sessionID, now)
}

func failUnsupportedCleanupTask(ctx context.Context, tx *sql.Tx, rebind func(string) string, taskID, leaseToken string) error {
	sessionID, err := cleanupTaskSessionID(ctx, tx, rebind, taskID, leaseToken)
	if err != nil {
		return err
	}
	now := time.Now()
	_, err = tx.ExecContext(ctx, rebind(`UPDATE session_purge_items
		SET status = ?, lease_until = NULL, lease_token = NULL, completed_at = NULL,
		    error_code = 'cleanup_unsupported', updated_at = ?
		WHERE session_id = ? AND kind = ? AND status <> ?`),
		string(PurgeItemUnsupported), now, sessionID, PurgeItemWorkerSession, string(PurgeItemComplete))
	if err != nil {
		return fmt.Errorf("session purge: mark worker capability unsupported: %w", err)
	}
	result, err := tx.ExecContext(ctx, rebind(`DELETE FROM session_cleanup_tasks WHERE id = ? AND lease_token = ?`), taskID, leaseToken)
	if err != nil {
		return err
	}
	if err := cleanupLeaseResult(result); err != nil {
		return err
	}
	return refreshPurgeJobStatus(ctx, tx, rebind, sessionID, now)
}

func (s *SQLiteStore) FailUnsupportedCleanupTask(ctx context.Context, taskID, leaseToken string) error {
	return s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := failUnsupportedCleanupTask(ctx, tx, identityRebind, taskID, leaseToken); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *pgStore) FailUnsupportedCleanupTask(ctx context.Context, taskID, leaseToken string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := failUnsupportedCleanupTask(ctx, tx, s.dialect.Rebind, taskID, leaseToken); err != nil {
		return err
	}
	return tx.Commit()
}

func markCleanupItemRetrying(ctx context.Context, tx *sql.Tx, rebind func(string) string, sessionID string, nextAttemptAt, now time.Time) error {
	_, err := tx.ExecContext(ctx, rebind(`UPDATE session_purge_items
		SET status = ?, next_attempt_at = ?, lease_until = NULL, lease_token = NULL, completed_at = NULL, error_code = 'cleanup_failed', updated_at = ?
		WHERE session_id = ? AND kind = ? AND status <> ?`),
		string(PurgeItemRetrying), nextAttemptAt, now, sessionID, PurgeItemWorkerSession, string(PurgeItemComplete))
	if err != nil {
		return fmt.Errorf("session purge: mark worker item retrying: %w", err)
	}
	return refreshPurgeJobStatus(ctx, tx, rebind, sessionID, now)
}

func cleanupTaskSessionID(ctx context.Context, tx *sql.Tx, rebind func(string) string, taskID, leaseToken string) (string, error) {
	var sessionID string
	err := tx.QueryRowContext(ctx, rebind(`SELECT session_id FROM session_cleanup_tasks WHERE id = ? AND lease_token = ?`), taskID, leaseToken).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrCleanupLeaseLost
	}
	if err != nil {
		return "", fmt.Errorf("session cleanup: load task session: %w", err)
	}
	return sessionID, nil
}

func markDeletedAndEnqueueCleanup(ctx context.Context, tx *sql.Tx, execer cleanupExecer, getSessionQuery, sessionID string, now time.Time, rebind func(string) string) error {
	locked, err := lockSessionRowForLifecycleChange(ctx, tx, rebind, sessionID)
	if err != nil {
		return err
	}
	if !locked {
		return nil
	}
	return markDeletedAndEnqueueCleanupLocked(ctx, tx, execer, getSessionQuery, sessionID, now, rebind)
}

func markDeletedAndEnqueueCleanupLocked(ctx context.Context, tx *sql.Tx, execer cleanupExecer, getSessionQuery, sessionID string, now time.Time, rebind func(string) string) error {
	info, err := scanSession(tx.QueryRowContext(ctx, getSessionQuery, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("session cleanup: load session for deletion: %w", err)
	}
	if err := insertCleanupTask(ctx, execer, info, now); err != nil {
		return err
	}
	if err := insertPurgeJobAndItems(ctx, tx, rebind, info, now); err != nil {
		return err
	}
	if _, err := execer.ExecContext(ctx, `UPDATE sessions
		SET state = ?, deleted_at = COALESCE(deleted_at, ?),
		    updated_at = CASE WHEN updated_at < ? THEN ? ELSE updated_at END,
		    title = '', work_dir = '', context_json = NULL, platform_key_json = '',
		    client_key = '', worker_session_id = ''
		WHERE id = ?`,
		string(events.StateDeleted), now, now, now, sessionID); err != nil {
		return fmt.Errorf("session cleanup: mark deleted: %w", err)
	}
	info.State = events.StateDeleted
	if info.DeletedAt == nil {
		info.DeletedAt = &now
	}
	if info.UpdatedAt.Before(now) {
		info.UpdatedAt = now
	}
	return nil
}

func lockSessionRowForLifecycleChange(ctx context.Context, tx *sql.Tx, rebind func(string) string, sessionID string) (bool, error) {
	// The no-op update is the common row lock for lifecycle writes and input
	// acceptance. SQLite serializes it through its database writer lock; on
	// PostgreSQL it takes a row lock until the transaction ends.
	result, err := tx.ExecContext(ctx, rebind(`UPDATE sessions SET state = state WHERE id = ?`), sessionID)
	if err != nil {
		return false, fmt.Errorf("session lifecycle: lock session row: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("session lifecycle: locked rows affected: %w", err)
	}
	return rows > 0, nil
}

func lifecycleRetirementBlocked(ctx context.Context, queryer purgeQueryer, rebind func(string) string, sessionID string) (bool, error) {
	var blocked bool
	err := queryer.QueryRowContext(ctx, rebind(`SELECT
		EXISTS (
			SELECT 1 FROM execution_inputs
			WHERE session_id = ?
			  AND (
				status IN ('accepted', 'unknown')
				OR runtime_status IN ('queued', 'pending', 'running', 'unknown')
				OR fence_reason <> ''
			  )
		)
		OR EXISTS (SELECT 1 FROM execution_queue WHERE session_id = ?)
		OR EXISTS (SELECT 1 FROM session_cleanup_tasks WHERE session_id = ?)`),
		sessionID, sessionID, sessionID).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("session lifecycle: check retirement blockers: %w", err)
	}
	return blocked, nil
}

func lifecycleSessionExpired(info *SessionInfo, now time.Time) bool {
	if info == nil ||
		info.LifecyclePolicy != config.LifecyclePolicyV2 ||
		info.DeletedAt != nil ||
		info.State == events.StateRunning ||
		info.State == events.StateDeleted ||
		info.ConversationExpiresAt == nil ||
		info.HistoryExpiresAt == nil {
		return false
	}
	return !info.ConversationExpiresAt.After(now) &&
		(info.LastContentExpiresAt == nil || !info.LastContentExpiresAt.After(now)) &&
		!info.HistoryExpiresAt.After(now)
}

func retireExpiredLifecycleSession(
	ctx context.Context,
	tx *sql.Tx,
	getSessionQuery string,
	rebind func(string) string,
	id string,
	now time.Time,
) (*SessionInfo, error) {
	locked, err := lockSessionRowForLifecycleChange(ctx, tx, rebind, id)
	if err != nil || !locked {
		return nil, err
	}
	info, err := scanSession(tx.QueryRowContext(ctx, getSessionQuery, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session lifecycle: load retirement candidate: %w", err)
	}
	if !lifecycleSessionExpired(info, now) {
		return nil, nil
	}
	blocked, err := lifecycleRetirementBlocked(ctx, tx, rebind, id)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, nil
	}
	if err := markDeletedAndEnqueueCleanupLocked(
		ctx, tx, pgExec{tx: tx, rebind: rebind}, getSessionQuery, id, now, rebind,
	); err != nil {
		return nil, err
	}
	info.State = events.StateDeleted
	info.UpdatedAt = now
	info.DeletedAt = &now
	return info, nil
}

func ensureSQLiteLifecycleLock(ctx context.Context, tx *sql.Tx, sessionID string) error {
	// SQLiteStore already holds its process-wide writeMu for every caller of
	// this helper. The transaction therefore has the required lifecycle lock
	// without retaining a row after session retention GC.
	return nil
}

func ensurePGLifecycleLock(ctx context.Context, tx *sql.Tx, rebind func(string) string, sessionID string) error {
	// Transaction-scoped advisory locks disappear on commit/rollback. A hash
	// collision only adds contention; it cannot permit conflicting lifecycle
	// writes to run concurrently.
	_, err := tx.ExecContext(ctx, rebind(`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`), sessionID)
	return err
}

func (s *SQLiteStore) HasPendingCleanup(ctx context.Context, sessionID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_cleanup_tasks WHERE session_id = ?)`, sessionID).Scan(&exists)
	return exists, err
}

func (s *SQLiteStore) GetPurgeStatus(ctx context.Context, sessionID, userID string) (*PurgeStatus, error) {
	return getPurgeStatus(ctx, s.db, identityRebind, sessionID, userID)
}

func (s *SQLiteStore) MarkDeletedWithCleanup(ctx context.Context, info *SessionInfo) error {
	return s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := ensureSQLiteLifecycleLock(ctx, tx, info.ID); err != nil {
			return err
		}
		if err := markDeletedAndEnqueueCleanup(ctx, tx, tx, queries["store.get_session"], info.ID, time.Now(), identityRebind); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *SQLiteStore) DeletePhysicalWithCleanup(ctx context.Context, id string) (*SessionInfo, error) {
	var deleted *SessionInfo
	err := s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := ensureSQLiteLifecycleLock(ctx, tx, id); err != nil {
			return err
		}
		locked, err := lockSessionRowForLifecycleChange(ctx, tx, identityRebind, id)
		if err != nil {
			return err
		}
		if !locked {
			return tx.Commit()
		}
		info, err := scanSession(tx.QueryRowContext(ctx, queries["store.get_session"], id))
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit()
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, queries["store.delete_physical"], id); err != nil {
			return fmt.Errorf("session cleanup: physical delete: %w", err)
		}
		if err := insertCleanupTask(ctx, tx, info, time.Now()); err != nil {
			return err
		}
		deleted = info
		return tx.Commit()
	})
	return deleted, err
}

func (s *SQLiteStore) RetireExpiredLifecycleSession(ctx context.Context, id string, now time.Time) (*SessionInfo, error) {
	var retired *SessionInfo
	err := s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := ensureSQLiteLifecycleLock(ctx, tx, id); err != nil {
			return err
		}
		retired, err = retireExpiredLifecycleSession(ctx, tx, queries["store.get_session"], identityRebind, id, now)
		if err != nil {
			return err
		}
		return tx.Commit()
	})
	return retired, err
}

func (s *SQLiteStore) ClaimCleanupTasks(ctx context.Context, now, leaseUntil time.Time, limit int) ([]CleanupTask, error) {
	if limit <= 0 {
		return nil, nil
	}
	tasks := make([]CleanupTask, 0, limit)
	err := s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		rows, err := tx.QueryContext(ctx, `SELECT `+cleanupTaskColumns()+` FROM session_cleanup_tasks WHERE next_attempt_at <= ? AND (lease_until IS NULL OR lease_until <= ?) ORDER BY next_attempt_at, created_at LIMIT ?`, now, now, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			task, err := scanCleanupTask(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			leaseToken := uuid.NewString()
			result, err := tx.ExecContext(ctx, `UPDATE session_cleanup_tasks SET attempts = attempts + 1, lease_until = ?, lease_token = ?, updated_at = ? WHERE id = ? AND (lease_until IS NULL OR lease_until <= ?)`, leaseUntil, leaseToken, now, task.ID, now)
			if err != nil {
				_ = rows.Close()
				return err
			}
			updated, err := result.RowsAffected()
			if err != nil {
				_ = rows.Close()
				return err
			}
			if updated == 1 {
				task.Attempts++
				task.LeaseUntil = &leaseUntil
				task.LeaseToken = leaseToken
				task.UpdatedAt = now
				if err := markCleanupItemRunning(ctx, tx, identityRebind, task.SessionID, task.Attempts, task.NextAttemptAt, leaseUntil, leaseToken, now); err != nil {
					_ = rows.Close()
					return err
				}
				tasks = append(tasks, task)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		return tx.Commit()
	})
	return tasks, err
}

func (s *SQLiteStore) CompleteCleanupTask(ctx context.Context, taskID, leaseToken string) error {
	return s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		sessionID, err := cleanupTaskSessionID(ctx, tx, identityRebind, taskID, leaseToken)
		if err != nil {
			return err
		}
		now := time.Now()
		if err := markCleanupItemComplete(ctx, tx, identityRebind, sessionID, now); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM session_cleanup_tasks WHERE id = ? AND lease_token = ?`, taskID, leaseToken)
		if err != nil {
			return err
		}
		if err := cleanupLeaseResult(result); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *SQLiteStore) RetryCleanupTask(ctx context.Context, taskID, leaseToken string, nextAttemptAt time.Time, lastError string) error {
	return s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		sessionID, err := cleanupTaskSessionID(ctx, tx, identityRebind, taskID, leaseToken)
		if err != nil {
			return err
		}
		now := time.Now()
		result, err := tx.ExecContext(ctx, `UPDATE session_cleanup_tasks SET next_attempt_at = ?, lease_until = NULL, lease_token = NULL, last_error = ?, updated_at = ? WHERE id = ? AND lease_token = ?`, nextAttemptAt, normalizeCleanupErrorCode(lastError), now, taskID, leaseToken)
		if err != nil {
			return err
		}
		if err := cleanupLeaseResult(result); err != nil {
			return err
		}
		if err := markCleanupItemRetrying(ctx, tx, identityRebind, sessionID, nextAttemptAt, now); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *pgStore) HasPendingCleanup(ctx context.Context, sessionID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`SELECT EXISTS(SELECT 1 FROM session_cleanup_tasks WHERE session_id = ?)`), sessionID).Scan(&exists)
	return exists, err
}

func (s *pgStore) GetPurgeStatus(ctx context.Context, sessionID, userID string) (*PurgeStatus, error) {
	return getPurgeStatus(ctx, s.db, s.dialect.Rebind, sessionID, userID)
}

func (s *pgStore) MarkDeletedWithCleanup(ctx context.Context, info *SessionInfo) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensurePGLifecycleLock(ctx, tx, s.dialect.Rebind, info.ID); err != nil {
		return err
	}
	if err := markDeletedAndEnqueueCleanup(ctx, tx, pgExec{tx: tx, rebind: s.dialect.Rebind}, s.queries["store.get_session"], info.ID, time.Now(), s.dialect.Rebind); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *pgStore) DeletePhysicalWithCleanup(ctx context.Context, id string) (*SessionInfo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensurePGLifecycleLock(ctx, tx, s.dialect.Rebind, id); err != nil {
		return nil, err
	}
	locked, err := lockSessionRowForLifecycleChange(ctx, tx, s.dialect.Rebind, id)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, tx.Commit()
	}
	info, err := scanSession(tx.QueryRowContext(ctx, s.queries["store.get_session"], id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, s.queries["store.delete_physical"], id); err != nil {
		return nil, err
	}
	if err := insertCleanupTask(ctx, pgExec{tx: tx, rebind: s.dialect.Rebind}, info, time.Now()); err != nil {
		return nil, err
	}
	return info, tx.Commit()
}

func (s *pgStore) RetireExpiredLifecycleSession(ctx context.Context, id string, now time.Time) (*SessionInfo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensurePGLifecycleLock(ctx, tx, s.dialect.Rebind, id); err != nil {
		return nil, err
	}
	retired, err := retireExpiredLifecycleSession(ctx, tx, s.queries["store.get_session"], s.dialect.Rebind, id, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return retired, nil
}

func (s *pgStore) ClaimCleanupTasks(ctx context.Context, now, leaseUntil time.Time, limit int) ([]CleanupTask, error) {
	if limit <= 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	query := s.dialect.Rebind(`SELECT ` + cleanupTaskColumns() + ` FROM session_cleanup_tasks WHERE next_attempt_at <= ? AND (lease_until IS NULL OR lease_until <= ?) ORDER BY next_attempt_at, created_at LIMIT ? FOR UPDATE SKIP LOCKED`)
	rows, err := tx.QueryContext(ctx, query, now, now, limit)
	if err != nil {
		return nil, err
	}
	tasks := make([]CleanupTask, 0, limit)
	update := s.dialect.Rebind(`UPDATE session_cleanup_tasks SET attempts = attempts + 1, lease_until = ?, lease_token = ?, updated_at = ? WHERE id = ?`)
	for rows.Next() {
		task, err := scanCleanupTask(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		leaseToken := uuid.NewString()
		if _, err := tx.ExecContext(ctx, update, leaseUntil, leaseToken, now, task.ID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		task.Attempts++
		task.LeaseUntil = &leaseUntil
		task.LeaseToken = leaseToken
		task.UpdatedAt = now
		if err := markCleanupItemRunning(ctx, tx, s.dialect.Rebind, task.SessionID, task.Attempts, task.NextAttemptAt, leaseUntil, leaseToken, now); err != nil {
			_ = rows.Close()
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return tasks, tx.Commit()
}

func (s *pgStore) CompleteCleanupTask(ctx context.Context, taskID, leaseToken string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	sessionID, err := cleanupTaskSessionID(ctx, tx, s.dialect.Rebind, taskID, leaseToken)
	if err != nil {
		return err
	}
	now := time.Now()
	if err := markCleanupItemComplete(ctx, tx, s.dialect.Rebind, sessionID, now); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, s.dialect.Rebind(`DELETE FROM session_cleanup_tasks WHERE id = ? AND lease_token = ?`), taskID, leaseToken)
	if err != nil {
		return err
	}
	if err := cleanupLeaseResult(result); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *pgStore) RetryCleanupTask(ctx context.Context, taskID, leaseToken string, nextAttemptAt time.Time, lastError string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	sessionID, err := cleanupTaskSessionID(ctx, tx, s.dialect.Rebind, taskID, leaseToken)
	if err != nil {
		return err
	}
	now := time.Now()
	result, err := tx.ExecContext(ctx, s.dialect.Rebind(`UPDATE session_cleanup_tasks SET next_attempt_at = ?, lease_until = NULL, lease_token = NULL, last_error = ?, updated_at = ? WHERE id = ? AND lease_token = ?`), nextAttemptAt, normalizeCleanupErrorCode(lastError), now, taskID, leaseToken)
	if err != nil {
		return err
	}
	if err := cleanupLeaseResult(result); err != nil {
		return err
	}
	if err := markCleanupItemRetrying(ctx, tx, s.dialect.Rebind, sessionID, nextAttemptAt, now); err != nil {
		return err
	}
	return tx.Commit()
}

type pgExec struct {
	tx     *sql.Tx
	rebind func(string) string
}

func (e pgExec) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return e.tx.ExecContext(ctx, e.rebind(query), args...)
}

func normalizeCleanupErrorCode(_ string) string {
	return cleanupFailureCode
}

func cleanupLeaseResult(result sql.Result) error {
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return ErrCleanupLeaseLost
	}
	return nil
}
