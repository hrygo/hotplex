package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func purgeItemColumns() string {
	return "id, session_id, kind, attempts, next_attempt_at, lease_until, COALESCE(lease_token, '')"
}

func scanPurgeItemTask(sc interface{ Scan(...any) error }) (PurgeItemTask, error) {
	var task PurgeItemTask
	var leaseUntil sql.NullTime
	if err := sc.Scan(&task.ID, &task.SessionID, &task.Kind, &task.Attempts, &task.NextAttemptAt, &leaseUntil, &task.LeaseToken); err != nil {
		return PurgeItemTask{}, err
	}
	if leaseUntil.Valid {
		task.LeaseUntil = &leaseUntil.Time
	}
	return task, nil
}

func purgeItemSessionID(ctx context.Context, tx *sql.Tx, rebind func(string) string, itemID, leaseToken string) (string, error) {
	var sessionID string
	err := tx.QueryRowContext(ctx, rebind(`SELECT session_id FROM session_purge_items
		WHERE id = ? AND kind = ? AND status = ? AND lease_token = ?`),
		itemID, PurgeItemConversationContent, PurgeItemRunning, leaseToken).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrCleanupLeaseLost
	}
	if err != nil {
		return "", fmt.Errorf("session purge: load item session: %w", err)
	}
	return sessionID, nil
}

func markPurgeItemRunning(ctx context.Context, tx *sql.Tx, rebind func(string) string, task *PurgeItemTask, leaseUntil time.Time, leaseToken string, now time.Time) error {
	result, err := tx.ExecContext(ctx, rebind(`UPDATE session_purge_items
		SET status = ?, attempts = attempts + 1, lease_until = ?, lease_token = ?,
		    completed_at = NULL, error_code = '', updated_at = ?
		WHERE id = ? AND kind = ? AND status IN (?, ?, ?)
		  AND (lease_until IS NULL OR lease_until <= ?)`),
		string(PurgeItemRunning), leaseUntil, leaseToken, now, task.ID,
		PurgeItemConversationContent, string(PurgeItemPending), string(PurgeItemRetrying), string(PurgeItemRunning), now)
	if err != nil {
		return fmt.Errorf("session purge: lease content item: %w", err)
	}
	if err := cleanupLeaseResult(result); err != nil {
		return err
	}
	task.Attempts++
	task.LeaseUntil = &leaseUntil
	task.LeaseToken = leaseToken
	return refreshPurgeJobStatus(ctx, tx, rebind, task.SessionID, now)
}

func (s *SQLiteStore) ClaimPurgeItems(ctx context.Context, now, leaseUntil time.Time, limit int) ([]PurgeItemTask, error) {
	if limit <= 0 {
		return nil, nil
	}
	tasks := make([]PurgeItemTask, 0, limit)
	err := s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		rows, err := tx.QueryContext(ctx, `SELECT `+purgeItemColumns()+`
			FROM session_purge_items
			WHERE kind = ? AND status IN (?, ?, ?)
			  AND next_attempt_at <= ? AND (lease_until IS NULL OR lease_until <= ?)
			ORDER BY next_attempt_at, created_at LIMIT ?`,
			PurgeItemConversationContent, string(PurgeItemPending), string(PurgeItemRetrying), string(PurgeItemRunning),
			now, now, limit)
		if err != nil {
			return fmt.Errorf("session purge: select due content items: %w", err)
		}
		for rows.Next() {
			task, err := scanPurgeItemTask(rows)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("session purge: scan due content item: %w", err)
			}
			token := uuid.NewString()
			if err := markPurgeItemRunning(ctx, tx, identityRebind, &task, leaseUntil, token, now); err != nil {
				_ = rows.Close()
				return err
			}
			tasks = append(tasks, task)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("session purge: iterate due content items: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("session purge: close due content items: %w", err)
		}
		return tx.Commit()
	})
	return tasks, err
}

func (s *SQLiteStore) CompletePurgeItem(ctx context.Context, itemID, leaseToken string) error {
	return s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		sessionID, err := purgeItemSessionID(ctx, tx, identityRebind, itemID, leaseToken)
		if err != nil {
			return err
		}
		now := time.Now()
		result, err := tx.ExecContext(ctx, `UPDATE session_purge_items
			SET status = ?, lease_until = NULL, lease_token = NULL,
			    completed_at = ?, error_code = '', updated_at = ?
			WHERE id = ? AND kind = ? AND status = ? AND lease_token = ?`,
			string(PurgeItemComplete), now, now, itemID, PurgeItemConversationContent, string(PurgeItemRunning), leaseToken)
		if err != nil {
			return fmt.Errorf("session purge: complete content item: %w", err)
		}
		if err := cleanupLeaseResult(result); err != nil {
			return err
		}
		if err := refreshPurgeJobStatus(ctx, tx, identityRebind, sessionID, now); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *SQLiteStore) RetryPurgeItem(ctx context.Context, itemID, leaseToken string, nextAttemptAt time.Time, errorCode string) error {
	return s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		sessionID, err := purgeItemSessionID(ctx, tx, identityRebind, itemID, leaseToken)
		if err != nil {
			return err
		}
		now := time.Now()
		result, err := tx.ExecContext(ctx, `UPDATE session_purge_items
			SET status = ?, next_attempt_at = ?, lease_until = NULL, lease_token = NULL,
			    completed_at = NULL, error_code = ?, updated_at = ?
			WHERE id = ? AND kind = ? AND status = ? AND lease_token = ?`,
			string(PurgeItemRetrying), nextAttemptAt, normalizeCleanupErrorCode(errorCode), now,
			itemID, PurgeItemConversationContent, string(PurgeItemRunning), leaseToken)
		if err != nil {
			return fmt.Errorf("session purge: retry content item: %w", err)
		}
		if err := cleanupLeaseResult(result); err != nil {
			return err
		}
		if err := refreshPurgeJobStatus(ctx, tx, identityRebind, sessionID, now); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *pgStore) ClaimPurgeItems(ctx context.Context, now, leaseUntil time.Time, limit int) ([]PurgeItemTask, error) {
	if limit <= 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	query := s.dialect.Rebind(`SELECT ` + purgeItemColumns() + `
		FROM session_purge_items
		WHERE kind = ? AND status IN (?, ?, ?)
		  AND next_attempt_at <= ? AND (lease_until IS NULL OR lease_until <= ?)
		ORDER BY next_attempt_at, created_at LIMIT ? FOR UPDATE SKIP LOCKED`)
	rows, err := tx.QueryContext(ctx, query,
		PurgeItemConversationContent, string(PurgeItemPending), string(PurgeItemRetrying), string(PurgeItemRunning),
		now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("session purge: select due content items: %w", err)
	}
	tasks := make([]PurgeItemTask, 0, limit)
	for rows.Next() {
		task, err := scanPurgeItemTask(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("session purge: scan due content item: %w", err)
		}
		token := uuid.NewString()
		if err := markPurgeItemRunning(ctx, tx, s.dialect.Rebind, &task, leaseUntil, token, now); err != nil {
			_ = rows.Close()
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("session purge: iterate due content items: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("session purge: close due content items: %w", err)
	}
	return tasks, tx.Commit()
}

func (s *pgStore) CompletePurgeItem(ctx context.Context, itemID, leaseToken string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	sessionID, err := purgeItemSessionID(ctx, tx, s.dialect.Rebind, itemID, leaseToken)
	if err != nil {
		return err
	}
	now := time.Now()
	result, err := tx.ExecContext(ctx, s.dialect.Rebind(`UPDATE session_purge_items
		SET status = ?, lease_until = NULL, lease_token = NULL,
		    completed_at = ?, error_code = '', updated_at = ?
		WHERE id = ? AND kind = ? AND status = ? AND lease_token = ?`),
		string(PurgeItemComplete), now, now, itemID, PurgeItemConversationContent, string(PurgeItemRunning), leaseToken)
	if err != nil {
		return fmt.Errorf("session purge: complete content item: %w", err)
	}
	if err := cleanupLeaseResult(result); err != nil {
		return err
	}
	if err := refreshPurgeJobStatus(ctx, tx, s.dialect.Rebind, sessionID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *pgStore) RetryPurgeItem(ctx context.Context, itemID, leaseToken string, nextAttemptAt time.Time, errorCode string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	sessionID, err := purgeItemSessionID(ctx, tx, s.dialect.Rebind, itemID, leaseToken)
	if err != nil {
		return err
	}
	now := time.Now()
	result, err := tx.ExecContext(ctx, s.dialect.Rebind(`UPDATE session_purge_items
		SET status = ?, next_attempt_at = ?, lease_until = NULL, lease_token = NULL,
		    completed_at = NULL, error_code = ?, updated_at = ?
		WHERE id = ? AND kind = ? AND status = ? AND lease_token = ?`),
		string(PurgeItemRetrying), nextAttemptAt, normalizeCleanupErrorCode(errorCode), now,
		itemID, PurgeItemConversationContent, string(PurgeItemRunning), leaseToken)
	if err != nil {
		return fmt.Errorf("session purge: retry content item: %w", err)
	}
	if err := cleanupLeaseResult(result); err != nil {
		return err
	}
	if err := refreshPurgeJobStatus(ctx, tx, s.dialect.Rebind, sessionID, now); err != nil {
		return err
	}
	return tx.Commit()
}
