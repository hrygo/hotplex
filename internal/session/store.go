package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/pkg/events"
)

// Store defines the interface for session persistence.
type Store interface {
	Upsert(ctx context.Context, info *SessionInfo) error
	UpdateWorkerSessionIDSQL(ctx context.Context, id, workerSessionID string) error
	SetPermissionCeilingIfEmpty(ctx context.Context, id, ceiling string) (string, error)
	UpdateSpecSnapshot(ctx context.Context, id string, snapshot *agentspec.EffectiveAgentSpecSnapshot) error
	Get(ctx context.Context, id string) (*SessionInfo, error)
	List(ctx context.Context, userID, platform, workspaceID string, limit, offset int) ([]*SessionInfo, error)
	GetExpiredMaxLifetime(ctx context.Context, now time.Time) ([]string, error)
	GetExpiredIdle(ctx context.Context, now time.Time) ([]string, error)
	DeleteTerminated(ctx context.Context, cronCutoff, defaultCutoff time.Time) ([]*SessionInfo, error)
	DeletePhysical(ctx context.Context, id string) error
	GetSessionsByState(ctx context.Context, state events.SessionState) ([]string, error)
	Close() error
}

type lifecycleDeadlineStore interface {
	AdvanceLifecycleDeadlines(
		ctx context.Context,
		id, policyRevision string,
		lastInputAt, archiveAt, conversationExpiresAt, historyExpiresAt, updatedAt time.Time,
	) error
}

type lifecycleRetirementStore interface {
	ListExpiredLifecycleSessions(ctx context.Context, now time.Time, limit int) ([]string, error)
	RetireExpiredLifecycleSession(ctx context.Context, id string, now time.Time) (*SessionInfo, error)
}

type lifecycleGCStatus struct {
	eligible             int64
	blocked              int64
	unknownExecutionHold int64
	eligibleLag          time.Duration
	blockedLag           time.Duration
}

type lifecycleGCStatusStore interface {
	GetLifecycleGCStatus(ctx context.Context, now time.Time) (lifecycleGCStatus, error)
}

var _ Store = (*SQLiteStore)(nil)

// SQLiteStore implements Store using SQLite.
type SQLiteStore struct {
	db      *sql.DB
	log     *slog.Logger
	writeMu *sqlutil.WriteMu
}

// DB returns the underlying *sql.DB for sharing with other stores (e.g., eventstore).
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// NewSQLiteStore creates and initializes a new SQLiteStore.
// If writeMu is non-nil, all write operations are serialized through it.
func NewSQLiteStore(ctx context.Context, cfg *config.Config, writeMu *sqlutil.WriteMu) (*SQLiteStore, error) {
	db, err := openSQLiteDB(cfg, dbOpenOpts{
		Label:       "session",
		MaxOpen:     cfg.DB.MaxOpenConns,
		MaxIdle:     cfg.DB.MaxOpenConns,
		MaxLifetime: 0,
		MaxIdleTime: 5 * time.Minute,
	})
	if err != nil {
		return nil, err
	}

	if err := RunMigrations(ctx, db, dbutil.DialectSQLite); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &SQLiteStore{db: db, log: slog.Default().With("component", "session_store"), writeMu: writeMu}, nil
}

// marshalSessionJSON serializes the Context and PlatformKey fields for Upsert.
// The bound AgentIdentity (#848) and effective AgentSpec snapshot (#866), when
// present, are folded into the context_json blob under reserved keys — the
// in-memory Context is not mutated, so both survive a /reset (which clears
// Context) and re-persist on the next Upsert.
func marshalSessionJSON(info *SessionInfo) (ctxJSON, pkJSON []byte, err error) {
	ctx := info.Context
	if info.Identity != nil {
		ctx = agentspec.MergeIntoContext(ctx, info.Identity)
	}
	if info.SpecSnapshot != nil {
		ctx = agentspec.MergeSnapshotIntoContext(ctx, info.SpecSnapshot)
	}
	if ctx != nil {
		ctxJSON, err = json.Marshal(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("session store: marshal context: %w", err)
		}
	}
	if info.PlatformKey != nil {
		pkJSON, err = json.Marshal(info.PlatformKey)
		if err != nil {
			return nil, nil, fmt.Errorf("session store: marshal platform key: %w", err)
		}
	}
	return ctxJSON, pkJSON, nil
}

// upsertTimeout ensures the context has a deadline, defaulting to 5 seconds.
func upsertTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, 5*time.Second)
}

func (s *SQLiteStore) Upsert(ctx context.Context, info *SessionInfo) error {
	ctx, cancel := upsertTimeout(ctx)
	defer cancel()

	ctxJSON, pkJSON, err := marshalSessionJSON(info)
	if err != nil {
		return err
	}

	return s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := ensureSQLiteLifecycleLock(ctx, tx, info.ID); err != nil {
			return err
		}
		ctxJSON, err = preservePersistedSpecSnapshot(ctx, tx, queries["store.get_context_json"], info.ID, ctxJSON)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, queries["sessions.upsert_session"], upsertSessionArgs(info, ctxJSON, pkJSON)...)
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
	})
}

// UpdateWorkerSessionIDSQL performs a targeted UPDATE on the worker_session_id
// column only, avoiding the full-row overwrite of Upsert.
func (s *SQLiteStore) UpdateWorkerSessionIDSQL(ctx context.Context, id, workerSessionID string) error {
	ctx, cancel := upsertTimeout(ctx)
	defer cancel()
	return s.writeMu.WithLock(func() error {
		result, err := s.db.ExecContext(ctx, queries["sessions.update_worker_session_id"], workerSessionID, id)
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
	})
}

// AdvanceLifecycleDeadlines updates only the session-level lifecycle deadlines.
// It does not set content expiry because turns/events have no per-record policy
// or expiry timestamp yet.
func (s *SQLiteStore) AdvanceLifecycleDeadlines(
	ctx context.Context,
	id, policyRevision string,
	lastInputAt, archiveAt, conversationExpiresAt, historyExpiresAt, updatedAt time.Time,
) error {
	ctx, cancel := upsertTimeout(ctx)
	defer cancel()

	return s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := ensureSQLiteLifecycleLock(ctx, tx, id); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, queries["sessions.advance_lifecycle_deadlines"],
			lastInputAt, lastInputAt,
			archiveAt, archiveAt,
			conversationExpiresAt, conversationExpiresAt,
			historyExpiresAt, historyExpiresAt,
			updatedAt, updatedAt,
			id, config.LifecyclePolicyV2, policyRevision,
		)
		if err != nil {
			return fmt.Errorf("session store: advance lifecycle deadlines: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("session store: lifecycle deadline rows affected: %w", err)
		}
		if updated == 0 {
			var pending bool
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM session_cleanup_tasks WHERE session_id = ?)`, id,
			).Scan(&pending); err != nil {
				return fmt.Errorf("session store: check cleanup task: %w", err)
			}
			if pending {
				return ErrSessionCleanupPending
			}
			return ErrSessionNotFound
		}
		return tx.Commit()
	})
}

// SetPermissionCeilingIfEmpty atomically captures the first effective Worker
// permission ceiling and returns the authoritative stored value.
func (s *SQLiteStore) SetPermissionCeilingIfEmpty(ctx context.Context, id, ceiling string) (string, error) {
	ctx, cancel := upsertTimeout(ctx)
	defer cancel()

	var stored string
	err := s.writeMu.WithLock(func() error {
		if _, err := s.db.ExecContext(ctx, queries["sessions.set_permission_ceiling_if_empty"], ceiling, id); err != nil {
			return fmt.Errorf("session store: set permission ceiling: %w", err)
		}
		if err := s.db.QueryRowContext(ctx, queries["store.get_permission_ceiling"], id).Scan(&stored); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrSessionNotFound
			}
			return fmt.Errorf("session store: get permission ceiling: %w", err)
		}
		return nil
	})
	return stored, err
}

// UpdateSpecSnapshot atomically replaces only the reserved AgentSpec value in
// context_json. It deliberately avoids Upsert so a snapshot refresh cannot
// overwrite concurrent lifecycle or context changes with a stale SessionInfo.
func (s *SQLiteStore) UpdateSpecSnapshot(ctx context.Context, id string, snapshot *agentspec.EffectiveAgentSpecSnapshot) error {
	ctx, cancel := upsertTimeout(ctx)
	defer cancel()

	snapshotJSON, err := marshalSpecSnapshot(snapshot)
	if err != nil {
		return err
	}
	return s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := ensureSQLiteLifecycleLock(ctx, tx, id); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, queries["sessions.update_spec_snapshot"], snapshotJSON, id)
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
	})
}

func marshalSpecSnapshot(snapshot *agentspec.EffectiveAgentSpecSnapshot) (string, error) {
	if snapshot == nil {
		return "", fmt.Errorf("session store: marshal spec snapshot: %w", agentspec.ErrInvalidSnapshot)
	}
	if err := snapshot.Validate(); err != nil {
		return "", fmt.Errorf("session store: validate spec snapshot: %w", err)
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("session store: marshal spec snapshot: %w", err)
	}
	return string(b), nil
}

// preservePersistedSpecSnapshot copies the database's current reserved snapshot
// into an incoming context blob. Callers hold the per-session lifecycle lock, so
// a stale full-row Upsert cannot race a targeted snapshot refresh in either
// direction. Snapshot changes are exclusively performed by UpdateSpecSnapshot.
func preservePersistedSpecSnapshot(ctx context.Context, tx *sql.Tx, query, id string, incoming []byte) ([]byte, error) {
	var persistedJSON sql.NullString
	if err := tx.QueryRowContext(ctx, query, id).Scan(&persistedJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return incoming, nil
		}
		return nil, fmt.Errorf("session store: load persisted spec snapshot: %w", err)
	}
	if !persistedJSON.Valid || persistedJSON.String == "" {
		return incoming, nil
	}

	var persisted map[string]json.RawMessage
	if err := json.Unmarshal([]byte(persistedJSON.String), &persisted); err != nil {
		return nil, fmt.Errorf("session store: decode persisted context: %w", err)
	}
	rawSnapshot, ok := persisted[agentspec.SnapshotContextKey]
	if !ok {
		return incoming, nil
	}

	merged := make(map[string]json.RawMessage)
	if len(incoming) > 0 {
		if err := json.Unmarshal(incoming, &merged); err != nil {
			return nil, fmt.Errorf("session store: decode incoming context: %w", err)
		}
	}
	merged[agentspec.SnapshotContextKey] = rawSnapshot
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("session store: preserve spec snapshot: %w", err)
	}
	return out, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSession(sc rowScanner) (*SessionInfo, error) {
	var info SessionInfo
	var ctxJSON, platformKeyStr sql.NullString
	var expiresAt, idleExpiresAt sql.NullTime
	var lastInputAt, runtimeFinishedAt, archiveAt, conversationExpiresAt sql.NullTime
	var lastContentExpiresAt, historyExpiresAt, deletedAt sql.NullTime
	var createdAt, updatedAt time.Time

	err := sc.Scan(
		&info.ID, &info.UserID, &info.OwnerID, &info.WorkerSessionID, &info.WorkerType, &info.State, &info.BotID, &info.BotName,
		&info.Platform, &platformKeyStr, &info.WorkDir, &info.Title,
		&createdAt, &updatedAt, &expiresAt, &idleExpiresAt, &ctxJSON, &info.Source, &info.ClientKey, &info.WorkspaceID,
		&info.PermissionCeiling, &info.LifecyclePolicy, &info.LifecyclePolicyRevision,
		&lastInputAt, &runtimeFinishedAt, &archiveAt, &conversationExpiresAt,
		&lastContentExpiresAt, &historyExpiresAt, &deletedAt,
	)
	if err != nil {
		return nil, err
	}

	info.CreatedAt = createdAt
	info.UpdatedAt = updatedAt
	if expiresAt.Valid {
		info.ExpiresAt = &expiresAt.Time
	}
	if idleExpiresAt.Valid {
		info.IdleExpiresAt = &idleExpiresAt.Time
	}
	if lastInputAt.Valid {
		info.LastInputAt = &lastInputAt.Time
	}
	if runtimeFinishedAt.Valid {
		info.RuntimeFinishedAt = &runtimeFinishedAt.Time
	}
	if archiveAt.Valid {
		info.ArchiveAt = &archiveAt.Time
	}
	if conversationExpiresAt.Valid {
		info.ConversationExpiresAt = &conversationExpiresAt.Time
	}
	if lastContentExpiresAt.Valid {
		info.LastContentExpiresAt = &lastContentExpiresAt.Time
	}
	if historyExpiresAt.Valid {
		info.HistoryExpiresAt = &historyExpiresAt.Time
	}
	if deletedAt.Valid {
		info.DeletedAt = &deletedAt.Time
	}
	if ctxJSON.Valid && ctxJSON.String != "" {
		if err := json.Unmarshal([]byte(ctxJSON.String), &info.Context); err != nil {
			return nil, fmt.Errorf("session store: unmarshal context: %w", err)
		}
		// Pop the bound identity out of Context into the typed field. Legacy
		// rows without the reserved key are untouched (Identity stays nil); when
		// identity was the only entry the emptied map is normalized back to nil
		// so a context-less session still round-trips to a NULL column.
		if id := agentspec.ExtractFromContext(info.Context); id != nil {
			info.Identity = id
			if len(info.Context) == 0 {
				info.Context = nil
			}
		}
		// Pop the effective AgentSpec snapshot out of Context and, when present,
		// restore the effective tool whitelist (#866 AC1). AllowedTools is
		// in-memory only — it has no column — so without this restore a resumed
		// session would silently lose its tool boundary across a restart. Legacy
		// rows without the reserved key are untouched (SpecSnapshot stays nil,
		// AllowedTools stays empty = no restriction).
		snap, err := agentspec.ExtractSnapshotFromContext(info.Context)
		if err != nil {
			return nil, fmt.Errorf("session store: extract agent spec snapshot: %w", err)
		}
		if snap != nil {
			info.SpecSnapshot = snap
			if tools := snap.RestoreAllowedTools(); tools != nil {
				info.AllowedTools = tools
			}
			if len(info.Context) == 0 {
				info.Context = nil
			}
		}
	}
	if platformKeyStr.Valid && platformKeyStr.String != "" {
		if err := json.Unmarshal([]byte(platformKeyStr.String), &info.PlatformKey); err != nil {
			return nil, fmt.Errorf("session store: unmarshal platform key: %w", err)
		}
	}
	return &info, nil
}

func (s *SQLiteStore) Get(ctx context.Context, id string) (*SessionInfo, error) {
	info, err := scanSession(s.db.QueryRowContext(ctx, queries["store.get_session"], id))
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

func (s *SQLiteStore) List(ctx context.Context, userID, platform, workspaceID string, limit, offset int) ([]*SessionInfo, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, queries["store.list_sessions"], userID, userID, platform, platform, workspaceID, workspaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("session store: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Non-nil empty slice (not nil) so an empty result JSON-serializes to `[]`
	// rather than `null`. The webchat frontend calls .filter() directly on the
	// `sessions` field — a nil would crash it ("Cannot read properties of null").
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

func collectIDs(rows *sql.Rows) ([]string, error) {
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func (s *SQLiteStore) GetExpiredMaxLifetime(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, queries["store.get_expired_max_lifetime"],
		string(events.StateCreated), string(events.StateRunning), string(events.StateIdle), now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectIDs(rows)
}

func (s *SQLiteStore) GetExpiredIdle(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, queries["store.get_expired_idle"], events.StateIdle, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectIDs(rows)
}

func (s *SQLiteStore) ListExpiredLifecycleSessions(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, queries["store.select_expired_lifecycle_ids"], now, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("session store: select expired lifecycle sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectIDs(rows)
}

func (s *SQLiteStore) GetLifecycleGCStatus(ctx context.Context, now time.Time) (lifecycleGCStatus, error) {
	var status lifecycleGCStatus
	var oldestEligible, oldestBlocked sql.NullString
	err := s.db.QueryRowContext(ctx, queries["store.lifecycle_gc_status"], now, now, now).
		Scan(&status.eligible, &status.blocked, &status.unknownExecutionHold, &oldestEligible, &oldestBlocked)
	if err != nil {
		return lifecycleGCStatus{}, fmt.Errorf("session store: get lifecycle GC status: %w", err)
	}
	if oldestEligible.Valid {
		deadline, err := parseSQLiteDateTime(oldestEligible.String)
		if err != nil {
			return lifecycleGCStatus{}, fmt.Errorf("session store: parse oldest eligible lifecycle deadline: %w", err)
		}
		status.eligibleLag = lifecycleDeadlineLag(&deadline, now)
	}
	if oldestBlocked.Valid {
		deadline, err := parseSQLiteDateTime(oldestBlocked.String)
		if err != nil {
			return lifecycleGCStatus{}, fmt.Errorf("session store: parse oldest blocked lifecycle deadline: %w", err)
		}
		status.blockedLag = lifecycleDeadlineLag(&deadline, now)
	}
	return status, nil
}

func parseSQLiteDateTime(value string) (time.Time, error) {
	const sqliteDateTimeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"
	deadline, err := time.Parse(sqliteDateTimeLayout, value)
	if err == nil {
		return deadline, nil
	}
	if rfc3339, rfcErr := time.Parse(time.RFC3339Nano, value); rfcErr == nil {
		return rfc3339, nil
	}
	return time.Time{}, fmt.Errorf("unsupported SQLite datetime %q: %w", value, err)
}

// Events lifecycle is managed independently — session deletion does not cascade to events.
func (s *SQLiteStore) DeleteTerminated(ctx context.Context, cronCutoff, defaultCutoff time.Time) ([]*SessionInfo, error) {
	deleted := make([]*SessionInfo, 0)
	err := s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		rows, err := tx.QueryContext(ctx, queries["store.select_terminated_ids"], events.StateTerminated, cronCutoff, defaultCutoff)
		if err != nil {
			return fmt.Errorf("session store: select terminated: %w", err)
		}
		ids, err := collectIDs(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		now := time.Now()
		for _, id := range ids {
			if err := ensureSQLiteLifecycleLock(ctx, tx, id); err != nil {
				return err
			}
			locked, err := lockSessionRowForLifecycleChange(ctx, tx, identityRebind, id)
			if err != nil {
				return err
			}
			if !locked {
				continue
			}
			deletedRows, err := tx.QueryContext(ctx, queries["store.delete_terminated_by_id"], id, events.StateTerminated, cronCutoff, defaultCutoff)
			if err != nil {
				return fmt.Errorf("session store: delete terminated: %w", err)
			}
			if deletedRows.Next() {
				info, err := scanSession(deletedRows)
				if err != nil {
					_ = deletedRows.Close()
					return fmt.Errorf("session store: scan deleted session: %w", err)
				}
				deleted = append(deleted, info)
				if err := insertCleanupTask(ctx, tx, info, now); err != nil {
					_ = deletedRows.Close()
					return err
				}
			}
			if err := deletedRows.Err(); err != nil {
				_ = deletedRows.Close()
				return err
			}
			if err := deletedRows.Close(); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
	return deleted, err
}

func (s *SQLiteStore) DeletePhysical(ctx context.Context, id string) error {
	return s.writeMu.WithLock(func() error {
		_, err := s.db.ExecContext(ctx, queries["store.delete_physical"], id)
		if err != nil {
			return fmt.Errorf("session store: delete physical: %w", err)
		}
		return nil
	})
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) Compact(ctx context.Context, threshold float64) error {
	return s.writeMu.WithLock(func() error {
		var pageCount, freeCount int
		if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
			return fmt.Errorf("session store: compact page_count: %w", err)
		}
		if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freeCount); err != nil {
			return fmt.Errorf("session store: compact freelist_count: %w", err)
		}
		if pageCount == 0 || float64(freeCount)/float64(pageCount) < threshold {
			return nil
		}
		start := time.Now()
		s.log.Info("session store: VACUUM starting",
			"page_count", pageCount, "free_count", freeCount,
			"ratio", fmt.Sprintf("%.1f%%", float64(freeCount)/float64(pageCount)*100))
		if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return fmt.Errorf("session store: compact checkpoint: %w", err)
		}
		_, err := s.db.ExecContext(ctx, "VACUUM")
		s.log.Info("session store: VACUUM completed", "duration", time.Since(start))
		return err
	})
}

func (s *SQLiteStore) GetSessionsByState(ctx context.Context, state events.SessionState) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, queries["store.get_sessions_by_state"], string(state))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectIDs(rows)
}
