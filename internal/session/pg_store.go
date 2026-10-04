package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/pkg/events"
)

// pgStore implements Store using PostgreSQL.
type pgStore struct {
	db      *dbutil.DB
	dialect dbutil.Dialect
	queries map[string]string // Rebound queries ($N placeholders)
	log     *slog.Logger
}

// NewPGStore creates and initializes a new pgStore using the provided db connection.
func NewPGStore(ctx context.Context, db *dbutil.DB) (Store, error) {
	if err := RunMigrations(ctx, db.DB, dbutil.DialectPostgres); err != nil {
		return nil, fmt.Errorf("session store: pg migrations: %w", err)
	}

	// Copy and rebind all queries from ? to $N placeholders.
	q := make(map[string]string, len(queries))
	for k, v := range queries {
		q[k] = dbutil.DialectPostgres.Rebind(v)
	}

	return &pgStore{
		db:      db,
		dialect: dbutil.DialectPostgres,
		queries: q,
		log:     slog.Default().With("component", "session_pg_store"),
	}, nil
}

// Upsert inserts or updates a session record.
// Unlike SQLiteStore, no write serialization is needed — PG handles concurrency natively.
func (s *pgStore) Upsert(ctx context.Context, info *SessionInfo) error {
	ctx, cancel := upsertTimeout(ctx)
	defer cancel()

	ctxJSON, pkJSON, err := marshalSessionJSON(info)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensurePGLifecycleLock(ctx, tx, s.dialect.Rebind, info.ID); err != nil {
		return err
	}
	ctxJSON, err = preservePersistedSpecSnapshot(ctx, tx, s.queries["store.get_context_json"], info.ID, ctxJSON)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, s.queries["sessions.upsert_session"], upsertSessionArgs(info, ctxJSON, pkJSON)...)
	if err != nil {
		if isCleanupPendingError(err) {
			return ErrSessionCleanupPending
		}
		return fmt.Errorf("session store: upsert: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("session store: upsert rows affected: %w", err)
	}
	if updated == 0 {
		return ErrSessionCleanupPending
	}
	return tx.Commit()
}

// UpdateWorkerSessionIDSQL performs a targeted UPDATE on the worker_session_id
// column only, avoiding the full-row overwrite of Upsert.
// Concurrency safety: relies on PostgreSQL MVCC single-row UPDATE atomicity
// (no writeMu needed, unlike SQLite store which serializes via writeMu).
func (s *pgStore) UpdateWorkerSessionIDSQL(ctx context.Context, id, workerSessionID string) error {
	ctx, cancel := upsertTimeout(ctx)
	defer cancel()
	result, err := s.db.ExecContext(ctx, s.queries["sessions.update_worker_session_id"], workerSessionID, id)
	if err != nil {
		return fmt.Errorf("session store: update worker session id: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("session store: worker session id rows affected: %w", err)
	}
	if updated == 0 {
		pending, err := s.HasPendingCleanup(ctx, id)
		if err != nil {
			return fmt.Errorf("session store: check cleanup task: %w", err)
		}
		if pending {
			return ErrSessionCleanupPending
		}
	}
	return nil
}

// SetPermissionCeilingIfEmpty atomically captures the first effective Worker
// permission ceiling and returns the authoritative stored value.
func (s *pgStore) SetPermissionCeilingIfEmpty(ctx context.Context, id, ceiling string) (string, error) {
	ctx, cancel := upsertTimeout(ctx)
	defer cancel()

	if _, err := s.db.ExecContext(ctx, s.queries["sessions.set_permission_ceiling_if_empty"], ceiling, id); err != nil {
		return "", fmt.Errorf("session store: set permission ceiling: %w", err)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx, s.queries["store.get_permission_ceiling"], id).Scan(&stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrSessionNotFound
		}
		return "", fmt.Errorf("session store: get permission ceiling: %w", err)
	}
	return stored, nil
}

// UpdateSpecSnapshot atomically replaces only the reserved AgentSpec value in
// context_json. PostgreSQL evaluates jsonb_set against the row version acquired
// by this UPDATE, so unrelated JSON keys are preserved.
func (s *pgStore) UpdateSpecSnapshot(ctx context.Context, id string, snapshot *agentspec.EffectiveAgentSpecSnapshot) error {
	ctx, cancel := upsertTimeout(ctx)
	defer cancel()

	snapshotJSON, err := marshalSpecSnapshot(snapshot)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensurePGLifecycleLock(ctx, tx, s.dialect.Rebind, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, s.queries["sessions.update_spec_snapshot_pg"], snapshotJSON, id)
	if err != nil {
		return fmt.Errorf("session store: update spec snapshot: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("session store: spec snapshot rows affected: %w", err)
	}
	if updated == 0 {
		return ErrSessionNotFound
	}
	return tx.Commit()
}

// Get loads a session by ID. Returns ErrSessionNotFound if not found.
func (s *pgStore) Get(ctx context.Context, id string) (*SessionInfo, error) {
	info, err := scanSession(s.db.QueryRowContext(ctx, s.queries["store.get_session"], id))
	if errors.Is(err, sql.ErrNoRows) {
		pending, pendingErr := s.HasPendingCleanup(ctx, id)
		if pendingErr != nil {
			return nil, fmt.Errorf("session store: check cleanup task: %w", pendingErr)
		}
		if pending {
			return nil, ErrSessionCleanupPending
		}
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("session store: load: %w", err)
	}
	return info, nil
}

// List returns sessions with pagination, excluding soft-deleted records.
func (s *pgStore) List(ctx context.Context, userID, platform, workspaceID string, limit, offset int) ([]*SessionInfo, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, s.queries["store.list_sessions"], userID, userID, platform, platform, workspaceID, workspaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("session store: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Non-nil empty slice so empty results JSON-serialize to `[]` not `null`
	// (frontend calls .filter() on the response — nil crashes it). See store.go.
	sessions := make([]*SessionInfo, 0)
	for rows.Next() {
		si, err := scanSession(rows)
		if err != nil {
			s.log.Warn("session store: skipping corrupted row", "err", err)
			continue
		}
		sessions = append(sessions, si)
	}
	return sessions, rows.Err()
}

// GetExpiredMaxLifetime returns session IDs that exceeded their max lifetime.
func (s *pgStore) GetExpiredMaxLifetime(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.queries["store.get_expired_max_lifetime"],
		string(events.StateCreated), string(events.StateRunning), string(events.StateIdle), now)
	if err != nil {
		return nil, fmt.Errorf("session store: get expired max lifetime: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectIDs(rows)
}

// GetExpiredIdle returns session IDs that exceeded their idle timeout.
func (s *pgStore) GetExpiredIdle(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.queries["store.get_expired_idle"], events.StateIdle, now)
	if err != nil {
		return nil, fmt.Errorf("session store: get expired idle: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectIDs(rows)
}

// DeleteTerminated removes terminated sessions older than the respective cutoffs.
func (s *pgStore) DeleteTerminated(ctx context.Context, cronCutoff, defaultCutoff time.Time) ([]*SessionInfo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("session store: delete terminated: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, s.queries["store.select_terminated_ids"], events.StateTerminated, cronCutoff, defaultCutoff)
	if err != nil {
		return nil, fmt.Errorf("session store: select terminated: %w", err)
	}
	ids, err := collectIDs(rows)
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	deleted := make([]*SessionInfo, 0, len(ids))
	now := time.Now()
	for _, id := range ids {
		if err := ensurePGLifecycleLock(ctx, tx, s.dialect.Rebind, id); err != nil {
			return nil, err
		}
		deletedRows, err := tx.QueryContext(ctx, s.queries["store.delete_terminated_by_id"], id, events.StateTerminated, cronCutoff, defaultCutoff)
		if err != nil {
			return nil, fmt.Errorf("session store: delete terminated: %w", err)
		}
		if deletedRows.Next() {
			info, err := scanSession(deletedRows)
			if err != nil {
				_ = deletedRows.Close()
				return nil, fmt.Errorf("session store: scan deleted session: %w", err)
			}
			// The scanned rows must be closed before the cleanup INSERT runs
			// on the same transaction. pgx (database/sql) multiplexes one
			// server connection per *sql.Tx, and an open result set holds it:
			// any further statement on the tx fails with
			// "driver: bad connection" instead of waiting. modernc/sqlite
			// buffers rows client-side, which is why this only fails on PG.
			if err := deletedRows.Close(); err != nil {
				return nil, fmt.Errorf("session store: close deleted session rows: %w", err)
			}
			deleted = append(deleted, info)
			if err := insertCleanupTask(ctx, pgExec{tx: tx, rebind: s.dialect.Rebind}, info, now); err != nil {
				return nil, err
			}
		} else {
			if err := deletedRows.Err(); err != nil {
				_ = deletedRows.Close()
				return nil, err
			}
			if err := deletedRows.Close(); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return deleted, nil
}

// DeletePhysical deletes a session by ID, bypassing the state machine.
func (s *pgStore) DeletePhysical(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.queries["store.delete_physical"], id)
	if err != nil {
		return fmt.Errorf("session store: delete physical: %w", err)
	}
	return nil
}

// GetSessionsByState returns all session IDs in the given state.
func (s *pgStore) GetSessionsByState(ctx context.Context, state events.SessionState) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.queries["store.get_sessions_by_state"], string(state))
	if err != nil {
		return nil, fmt.Errorf("session store: query by state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectIDs(rows)
}

// Close is a no-op for pgStore — the connection is managed by gatewayStores,
// which calls s.db.Close() on the shared *dbutil.DB after s.session.Close().
func (s *pgStore) Close() error {
	return nil
}

var _ Store = (*pgStore)(nil)
