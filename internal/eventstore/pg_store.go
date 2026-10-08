package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// pgStore implements EventStore + TurnQuerier using PostgreSQL.
type pgStore struct {
	db        *dbutil.DB
	dialect   dbutil.Dialect
	sql       map[string]string // Rebound query cache (PG $N placeholders)
	log       *slog.Logger
	retention RetentionPolicy
}

// pgEventTx is a PostgreSQL-backed transaction for batch event and turn writes.
type pgEventTx struct {
	tx        *sql.Tx
	sql       map[string]string // Reference to pgStore's rebound queries
	dialect   dbutil.Dialect
	retention RetentionPolicy
}

// Interface checks.
var (
	_ EventStore  = (*pgStore)(nil)
	_ TurnQuerier = (*pgStore)(nil)
)

// NewPGStore creates a PostgreSQL-backed event store. All embedded SQL queries
// are rebound to PG $N placeholders. The turns.insert query gets an additional
// RETURNING id clause since PG does not support LastInsertId.
func NewPGStore(db *dbutil.DB, log *slog.Logger) *pgStore {
	return NewPGStoreWithRetention(db, log, RetentionPolicy{})
}

// NewPGStoreWithRetention creates a PostgreSQL store with explicit content
// retention. Existing rows retain their zero per-record deadline and use the
// legacy window.
func NewPGStoreWithRetention(db *dbutil.DB, log *slog.Logger, retention RetentionPolicy) *pgStore {
	d := db.Dialect()
	s := &pgStore{
		db:        db,
		dialect:   d,
		sql:       make(map[string]string, len(queries)),
		log:       log,
		retention: retention.normalized(),
	}
	for k, v := range queries {
		s.sql[k] = d.Rebind(v)
	}
	// Override turns.insert with RETURNING id for PG auto-increment.
	// Strip trailing semicolons to prevent RETURNING from being appended after a statement terminator.
	s.sql["turns.insert"] = d.Rebind(strings.TrimRight(strings.TrimSpace(queries["turns.insert"]), ";")) + " RETURNING id"
	return s
}

// ---------------------------------------------------------------------------
// EventStore implementation
// ---------------------------------------------------------------------------

func (s *pgStore) Append(ctx context.Context, event *StoredEvent) error {
	tx, err := s.BeginTx(ctx)
	if err != nil {
		return err
	}
	if err := tx.Append(ctx, event); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("eventstore: append: %w", err)
	}
	return nil
}

func (s *pgStore) BeginTx(ctx context.Context) (EventTx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("eventstore: begin tx: %w", err)
	}
	return &pgEventTx{tx: tx, sql: s.sql, dialect: s.dialect, retention: s.retention}, nil
}

func (s *pgStore) QueryBySession(ctx context.Context, sessionID string, cursor int64, dir CursorDirection, limit int) (*EventPage, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	nowMS, legacyCutoffMS := s.retention.readWindow(time.Now())
	return queryBySession(ctx, s.db, s.sql, sessionID, cursor, dir, limit, nowMS, legacyCutoffMS)
}

func (s *pgStore) DeleteBySession(ctx context.Context, sessionID string) error {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, s.sql["delete_by_session"], sessionID)
	if err != nil {
		return fmt.Errorf("eventstore: delete by session: %w", err)
	}
	return nil
}

// DeleteConversationBySession atomically removes both event payloads and
// materialized turns so a partial purge cannot leave a readable copy behind.
func (s *pgStore) DeleteConversationBySession(ctx context.Context, sessionID string) error {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("eventstore: begin conversation deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, query := range []string{s.sql["delete_by_session"], s.sql["turns.delete_by_session"]} {
		if _, err := tx.ExecContext(ctx, query, sessionID); err != nil {
			return fmt.Errorf("eventstore: delete conversation: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("eventstore: commit conversation deletion: %w", err)
	}
	return nil
}

func (s *pgStore) DeleteExpired(ctx context.Context, legacyCutoff time.Time) (int64, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	res, err := s.db.ExecContext(ctx, s.sql["delete_expired"], time.Now().UnixMilli(), legacyCutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("eventstore: delete expired: %w", err)
	}
	rowsAffected, _ := res.RowsAffected()
	return rowsAffected, nil
}

func (s *pgStore) Close() error {
	return nil
}

// ---------------------------------------------------------------------------
// EventTx implementation
// ---------------------------------------------------------------------------

func (t *pgEventTx) Append(ctx context.Context, event *StoredEvent) error {
	if err := ensureSessionWritable(ctx, t.tx, event.SessionID, t.dialect.Rebind, true); err != nil {
		return err
	}
	expiresAt, err := contentExpiryForSession(
		ctx, t.tx, t.sql["lifecycle.get_content_policy"], t.retention,
		event.SessionID, event.CreatedAt, event.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("eventstore: resolve content expiry: %w", err)
	}
	_, err = t.tx.ExecContext(ctx, t.sql["insert"],
		event.SessionID, event.Seq, event.Type, event.Data, event.Direction, event.Source, event.CreatedAt, expiresAt)
	if err != nil {
		return fmt.Errorf("eventstore: tx append: %w", err)
	}
	if err := updateSessionContentDeadline(ctx, t.tx, t.sql["lifecycle.update_session_content_deadline"], event.SessionID, expiresAt); err != nil {
		return fmt.Errorf("eventstore: tx update session content deadline: %w", err)
	}
	return nil
}

// ExecContext runs a statement inside the transaction.
//
// It exists so a caller owning a wider invariant — for example committing a
// delivery effect together with its content snapshot — can share THIS
// transaction instead of opening a second one. Two transactions could commit
// independently, which is exactly the split this method is meant to prevent.
// The query is rebound to PostgreSQL placeholders by the caller's dialect.
func (t *pgEventTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.dialect.Rebind(query), args...)
}

// QueryRowContext reads a single row inside the transaction, rebinding the
// query to PostgreSQL placeholders.
func (t *pgEventTx) QueryRowContext(ctx context.Context, query string, args ...any) Row {
	return t.tx.QueryRowContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *pgEventTx) AppendTurn(ctx context.Context, turn *TurnWriteRequest) error {
	if err := ensureSessionWritable(ctx, t.tx, turn.SessionID, t.dialect.Rebind, true); err != nil {
		return err
	}
	var successVal any
	if turn.Success != nil {
		successVal = t.dialect.BoolValue(*turn.Success)
	}

	expiresAt, err := contentExpiryForSession(
		ctx, t.tx, t.sql["lifecycle.get_content_policy"], t.retention,
		turn.SessionID, turn.CreatedAt, turn.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("eventstore: resolve turn content expiry: %w", err)
	}

	var id int64
	err = t.tx.QueryRowContext(ctx, t.sql["turns.insert"],
		turn.SessionID, nullableClientMessageID(turn.ClientMessageID), turn.Generation, turn.TurnNum, turn.Seq, turn.Role, turn.Content,
		turn.Platform, turn.UserID, turn.Model, successVal, turn.Source, turn.ToolsJSON, turn.ToolCount,
		turn.TokensInput, turn.TokensCacheWrite, turn.TokensCacheRead, turn.TokensOut,
		turn.DurationMs, turn.CostUSD, turn.CreatedAt, expiresAt,
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("eventstore: tx append turn: %w", err)
	}
	if err := updateSessionContentDeadline(ctx, t.tx, t.sql["lifecycle.update_session_content_deadline"], turn.SessionID, expiresAt); err != nil {
		return fmt.Errorf("eventstore: tx update session content deadline: %w", err)
	}
	return nil
}

func (t *pgEventTx) Commit() error {
	return t.tx.Commit()
}

func (t *pgEventTx) Rollback() error {
	return t.tx.Rollback()
}

// ---------------------------------------------------------------------------
// TurnQuerier implementation
// ---------------------------------------------------------------------------

func (s *pgStore) QueryTurns(ctx context.Context, sessionID string, limit, offset int) ([]*TurnRecord, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	nowMS, legacyCutoffMS := s.retention.readWindow(time.Now())
	rows, err := s.db.QueryContext(ctx, s.sql["turns.query_with_gen"], sessionID, sessionID, nowMS, legacyCutoffMS, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanTurnsPG(rows)
}

func (s *pgStore) QueryTurnsBefore(ctx context.Context, sessionID string, beforeID int64, limit int) ([]*TurnRecord, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	nowMS, legacyCutoffMS := s.retention.readWindow(time.Now())
	rows, err := s.db.QueryContext(ctx, s.sql["turns.query_before"], sessionID, beforeID, nowMS, legacyCutoffMS, limit)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query turns before: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records, err := scanTurnsPG(rows)
	if err != nil {
		return nil, fmt.Errorf("eventstore: scan turns: %w", err)
	}
	// Reverse to ASC order (SQL returns DESC).
	slices.Reverse(records)
	return records, nil
}

func (s *pgStore) QueryLatestTurns(ctx context.Context, sessionID string, limit int) ([]*TurnRecord, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	nowMS, legacyCutoffMS := s.retention.readWindow(time.Now())
	rows, err := s.db.QueryContext(ctx, s.sql["turns.query_latest"], sessionID, sessionID, nowMS, legacyCutoffMS, limit)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query latest turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records, err := scanTurnsPG(rows)
	if err != nil {
		return nil, fmt.Errorf("eventstore: scan turns: %w", err)
	}
	// Reverse to ASC order (SQL returns DESC).
	slices.Reverse(records)
	return records, nil
}

func (s *pgStore) QueryTurnStats(ctx context.Context, sessionID string) (*TurnStats, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	nowMS, legacyCutoffMS := s.retention.readWindow(time.Now())
	rows, err := s.db.QueryContext(ctx, s.sql["turns.stats_with_gen"], sessionID, sessionID, nowMS, legacyCutoffMS)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query turn stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectTurnStats(rows, sessionID, func() successScanner { return &pgSuccessScanner{} }, s.log)
}

func (s *pgStore) LatestGeneration(ctx context.Context, sessionID string) (int64, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	var gen int64
	err := s.db.QueryRowContext(ctx, s.sql["turns.latest_generation"], sessionID).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("eventstore: latest generation: %w", err)
	}
	return gen, nil
}

func (s *pgStore) LatestTurnNum(ctx context.Context, sessionID string, generation int64) (int, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	var tn int
	err := s.db.QueryRowContext(ctx, s.sql["turns.latest_turn_num"], sessionID, generation).Scan(&tn)
	if err != nil {
		return 0, fmt.Errorf("eventstore: latest turn num: %w", err)
	}
	return tn, nil
}

// LatestSeq returns the maximum event seq for a session, or 0 if no events
// exist. Used to hydrate the in-memory SeqGen on reconnect (issue #879).
func (s *pgStore) LatestSeq(ctx context.Context, sessionID string) (int64, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	var seq int64
	err := s.db.QueryRowContext(ctx, s.sql["latest_seq"], sessionID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("eventstore: latest seq: %w", err)
	}
	return seq, nil
}

func (s *pgStore) DeleteExpiredTurns(ctx context.Context, legacyCutoff time.Time) (int64, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	res, err := s.db.ExecContext(ctx, s.sql["turns.delete_expired"], time.Now().UnixMilli(), legacyCutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("eventstore: delete expired turns: %w", err)
	}
	rowsAffected, _ := res.RowsAffected()
	return rowsAffected, nil
}

// ---------------------------------------------------------------------------
// PG-specific scanning helpers
// ---------------------------------------------------------------------------

// scanTurnsPG scans turn rows from PG, using sql.NullBool for the success
// column (PG BOOLEAN) instead of sql.NullInt64 (SQLite INTEGER).
func scanTurnsPG(rows *sql.Rows) ([]*TurnRecord, error) {
	var records []*TurnRecord
	for rows.Next() {
		var r TurnRecord
		var success sql.NullBool
		var clientMessageID sql.NullString
		var toolsJSON sql.NullString
		if err := rows.Scan(turnScanDest(&r, &success, &clientMessageID, &toolsJSON)...); err != nil {
			return nil, fmt.Errorf("eventstore: scan turn: %w", err)
		}
		r.ClientMessageID = clientMessageID.String
		if success.Valid {
			r.Success = &success.Bool
		}
		if toolsJSON.Valid && toolsJSON.String != "" {
			_ = json.Unmarshal([]byte(toolsJSON.String), &r.Tools) //nolint:errcheck // best-effort
		}
		records = append(records, &r)
	}
	if len(records) == 0 {
		return nil, ErrNotFound
	}
	return records, rows.Err()
}
