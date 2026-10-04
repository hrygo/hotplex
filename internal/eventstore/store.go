package eventstore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/hrygo/hotplex/internal/sqlutil"
)

//go:embed sql/queries/*.sql
var sqlFS embed.FS

var queries = loadQueries()

func loadQueries() map[string]string {
	entries, err := fs.ReadDir(sqlFS, "sql/queries")
	if err != nil {
		panic("eventstore: read sql fs: " + err.Error())
	}
	m := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		data, err := fs.ReadFile(sqlFS, "sql/queries/"+name)
		if err != nil {
			panic("eventstore: read sql file " + name + ": " + err.Error())
		}
		key := strings.TrimSuffix(name, ".sql")
		// Strip "events." prefix from key: "events.insert.sql" → "insert"
		key = strings.TrimPrefix(key, "events.")
		text := strings.TrimSpace(stripComments(string(data)))
		if text != "" {
			m[key] = text
		}
	}
	return m
}

func stripComments(s string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			b.WriteString(line)
		}
	}
	return b.String()
}

// Source constants for event provenance tracking.
const (
	SourceNormal     = "normal"
	SourceCrash      = "crash"
	SourceTimeout    = "timeout"
	SourceFreshStart = "fresh_start"
)

// CursorDirection controls pagination direction relative to a persisted row ID.
type CursorDirection int

const (
	// CursorLatest fetches the most recent N events (no cursor needed).
	CursorLatest CursorDirection = iota
	// CursorAfter fetches events with id > cursor (newer, for incremental catch-up).
	CursorAfter
	// CursorBefore fetches events with id < cursor (older, for loading history).
	CursorBefore
)

var ErrNotFound = errors.New("eventstore: no events found")

const defaultTimeout = 5 * time.Second

// withDefaultTimeout wraps ctx with a 5s timeout if it has no deadline.
func withDefaultTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}

// TurnRecord represents a single conversational turn from the materialized turns table.
type TurnRecord struct {
	ID               int64          `json:"id"`
	SessionID        string         `json:"session_id"`
	ClientMessageID  string         `json:"client_message_id,omitempty"`
	Generation       int64          `json:"generation"`
	TurnNum          int            `json:"turn_num"`
	Seq              int64          `json:"seq"`
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	Platform         string         `json:"platform"`
	UserID           string         `json:"user_id"`
	Model            string         `json:"model"`
	Success          *bool          `json:"success"`
	Source           string         `json:"source"`
	Tools            map[string]int `json:"tools"`
	ToolCount        int            `json:"tool_call_count"`
	TokensIn         int64          `json:"tokens_in"` // computed: input + cache_write + cache_read
	TokensInput      int64          `json:"tokens_input"`
	TokensCacheWrite int64          `json:"tokens_cache_write"`
	TokensCacheRead  int64          `json:"tokens_cache_read"`
	TokensOut        int64          `json:"tokens_out"`
	DurationMs       int64          `json:"duration_ms"`
	CostUSD          float64        `json:"cost_usd"`
	CreatedAt        int64          `json:"created_at"`
}

// TurnStats holds aggregated statistics across all assistant turns of a session.
type TurnStats struct {
	SessionID          string         `json:"session_id"`
	Generation         int64          `json:"generation"`
	TotalTurns         int            `json:"total_turns"`
	SuccessTurns       int            `json:"success_turns"`
	FailedTurns        int            `json:"failed_turns"`
	TotalDurMs         int64          `json:"total_duration_ms"`
	TotalCostUSD       float64        `json:"total_cost_usd"`
	TotalTokIn         int64          `json:"total_tokens_in"` // computed sum
	TotalTokInput      int64          `json:"total_tokens_input"`
	TotalTokCacheWrite int64          `json:"total_tokens_cache_write"`
	TotalTokCacheRead  int64          `json:"total_tokens_cache_read"`
	TotalTokOut        int64          `json:"total_tokens_out"`
	Turns              []TurnStatItem `json:"turns"`
}

// TurnStatItem holds per-turn statistics.
type TurnStatItem struct {
	TurnNum          int     `json:"turn_num"`
	Seq              int64   `json:"seq"`
	Success          bool    `json:"success"`
	DurationMs       int64   `json:"duration_ms"`
	CostUSD          float64 `json:"cost_usd"`
	TokensIn         int64   `json:"tokens_in"` // computed: input + cache_write + cache_read
	TokensInput      int64   `json:"tokens_input"`
	TokensCacheWrite int64   `json:"tokens_cache_write"`
	TokensCacheRead  int64   `json:"tokens_cache_read"`
	TokensOut        int64   `json:"tokens_out"`
	Model            string  `json:"model"`
	Source           string  `json:"source"`
	CreatedAt        int64   `json:"created_at"`
}

// StoredEvent represents a single persisted AEP event.
type StoredEvent struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"session_id"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	Direction string          `json:"direction"`
	Source    string          `json:"source"`
	CreatedAt int64           `json:"created_at"`
}

// EventPage is a page of events with pagination metadata.
type EventPage struct {
	Events   []*StoredEvent `json:"events"`
	OldestID int64          `json:"oldest_id"`
	NewestID int64          `json:"newest_id"`
	// OldestSeq and NewestSeq are event metadata, not pagination cursors.
	OldestSeq int64 `json:"oldest_seq"`
	NewestSeq int64 `json:"newest_seq"`
	HasOlder  bool  `json:"has_older"`
}

// EventStore defines the interface for AEP event persistence.
type EventStore interface {
	// Append adds a single event (used internally by the collector's batch writer).
	Append(ctx context.Context, event *StoredEvent) error

	// BeginTx starts a transaction for batch writes.
	BeginTx(ctx context.Context) (EventTx, error)

	// QueryBySession fetches events with cursor-based bidirectional pagination.
	//   dir=CursorLatest, cursor=0  → latest N events (initial load)
	//   dir=CursorAfter,  cursor=X  → events with id > X (catch-up)
	//   dir=CursorBefore, cursor=X  → events with id < X (load older)
	// Returns events always in insertion-id ASC order.
	QueryBySession(ctx context.Context, sessionID string, cursor int64, dir CursorDirection, limit int) (*EventPage, error)

	// DeleteBySession removes all events for a session.
	DeleteBySession(ctx context.Context, sessionID string) error

	// DeleteExpired removes events older than the cutoff.
	DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error)

	// Close flushes pending writes and closes the database (if owned).
	Close() error
}

// EventTx is a transaction handle for batch event and turn writes.
// Callers MUST call either Commit or Rollback to release the underlying
// write lock — failure to do so will deadlock all subsequent writes.
type EventTx interface {
	Append(ctx context.Context, event *StoredEvent) error
	AppendTurn(ctx context.Context, turn *TurnWriteRequest) error
	// ExecContext runs a statement inside the transaction.
	//
	// It lets a caller owning a wider invariant — for example committing a
	// delivery effect together with its content snapshot — share THIS
	// transaction instead of opening a second one. Two transactions could
	// commit independently, which is exactly the split this method prevents.
	// On PostgreSQL the query is rebound to $N placeholders.
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	// QueryRowContext reads a single row inside the transaction, with the same
	// sharing rationale as ExecContext.
	QueryRowContext(ctx context.Context, query string, args ...any) Row
	Commit() error
	Rollback() error
}

// Row is the minimal result-row surface an EventTx exposes.
type Row interface {
	Scan(dest ...any) error
}

// TurnQuerier provides read-only access to conversation turn records.
type TurnQuerier interface {
	QueryTurns(ctx context.Context, sessionID string, limit, offset int) ([]*TurnRecord, error)
	QueryTurnsBefore(ctx context.Context, sessionID string, beforeID int64, limit int) ([]*TurnRecord, error)
	QueryLatestTurns(ctx context.Context, sessionID string, limit int) ([]*TurnRecord, error)
	QueryTurnStats(ctx context.Context, sessionID string) (*TurnStats, error)
	LatestGeneration(ctx context.Context, sessionID string) (int64, error)
	LatestTurnNum(ctx context.Context, sessionID string, generation int64) (int, error)
	DeleteExpiredTurns(ctx context.Context, cutoff time.Time) (int64, error)
	// LatestSeq returns the maximum event seq persisted for a session, or 0 if
	// none. Used to hydrate the in-memory SeqGen on resume/reconnect so new
	// events do not collide with the prior seq range (issue #879).
	LatestSeq(ctx context.Context, sessionID string) (int64, error)
}

// SQLiteStore implements EventStore using a shared SQLite database connection.
type SQLiteStore struct {
	db      *sql.DB
	ownsDB  bool // true only when opened independently (tests); false when sharing session store DB.
	writeMu *sqlutil.WriteMu
}

var _ EventStore = (*SQLiteStore)(nil)

// NewSQLiteStore creates an event store using a shared *sql.DB.
// The schema is managed by the session store goose migrations (002_events_table.sql).
func NewSQLiteStore(db *sql.DB, writeMu *sqlutil.WriteMu) *SQLiteStore {
	return &SQLiteStore{db: db, ownsDB: false, writeMu: writeMu}
}

// NewIndependentStore opens its own DB for testing.
func NewIndependentStore(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open(sqlutil.DriverName, dbPath)
	if err != nil {
		return nil, fmt.Errorf("eventstore: open: %w", err)
	}
	// Apply same pragmas as production for test fidelity.
	_, _ = db.Exec("PRAGMA journal_mode=WAL")
	_, _ = db.Exec("PRAGMA busy_timeout=5000")
	return &SQLiteStore{db: db, ownsDB: true}, nil
}

func (s *SQLiteStore) Append(ctx context.Context, event *StoredEvent) error {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	return s.writeMu.WithLock(func() error {
		_, err := s.db.ExecContext(ctx, queries["insert"],
			event.SessionID, event.Seq, event.Type, event.Data, event.Direction, event.Source, event.CreatedAt)
		if err != nil {
			return fmt.Errorf("eventstore: append: %w", err)
		}
		return nil
	})
}

func (s *SQLiteStore) BeginTx(ctx context.Context) (EventTx, error) {
	if s.writeMu != nil {
		s.writeMu.Lock()
	}
	ctx, cancel := withDefaultTimeout(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		if s.writeMu != nil {
			s.writeMu.Unlock()
		}
		return nil, fmt.Errorf("eventstore: begin tx: %w", err)
	}
	// The cancel must NOT run here: this context drives the returned
	// transaction, so cancelling it on return would abort the transaction
	// before the caller's first statement. It is released with the tx instead.
	return &sqliteTx{tx: tx, writeMu: s.writeMu, cancel: cancel}, nil
}

type sqliteTx struct {
	tx       *sql.Tx
	writeMu  *sqlutil.WriteMu
	released bool
	cancel   context.CancelFunc
}

func (t *sqliteTx) release() {
	if t.released {
		return
	}
	t.released = true
	if t.cancel != nil {
		t.cancel()
	}
	if t.writeMu != nil {
		t.writeMu.Unlock()
	}
}

// ExecContext runs a statement inside the transaction.
//
// It exists so a caller owning a wider invariant — for example committing a
// delivery effect together with its content snapshot — can share THIS
// transaction instead of opening a second one. Two transactions could commit
// independently, which is exactly the split this method is meant to prevent.
func (t *sqliteTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// QueryRowContext reads a single row inside the transaction.
func (t *sqliteTx) QueryRowContext(ctx context.Context, query string, args ...any) Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

func (t *sqliteTx) Append(ctx context.Context, event *StoredEvent) error {
	_, err := t.tx.ExecContext(ctx, queries["insert"],
		event.SessionID, event.Seq, event.Type, event.Data, event.Direction, event.Source, event.CreatedAt)
	if err != nil {
		return fmt.Errorf("eventstore: tx append: %w", err)
	}
	return nil
}

func (t *sqliteTx) AppendTurn(ctx context.Context, turn *TurnWriteRequest) error {
	var successVal any
	if turn.Success != nil {
		if *turn.Success {
			successVal = 1
		} else {
			successVal = 0
		}
	}
	_, err := t.tx.ExecContext(ctx, queries["turns.insert"],
		turn.SessionID, nullableClientMessageID(turn.ClientMessageID), turn.Generation, turn.TurnNum, turn.Seq, turn.Role, turn.Content,
		turn.Platform, turn.UserID, turn.Model, successVal, turn.Source, turn.ToolsJSON, turn.ToolCount,
		turn.TokensInput, turn.TokensCacheWrite, turn.TokensCacheRead, turn.TokensOut,
		turn.DurationMs, turn.CostUSD, turn.CreatedAt)
	if err != nil {
		return fmt.Errorf("eventstore: tx append turn: %w", err)
	}
	return nil
}

func (t *sqliteTx) Commit() error {
	err := t.tx.Commit()
	t.release()
	return err
}

func (t *sqliteTx) Rollback() error {
	err := t.tx.Rollback()
	t.release()
	return err
}

func (s *SQLiteStore) QueryBySession(ctx context.Context, sessionID string, cursor int64, dir CursorDirection, limit int) (*EventPage, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	return queryBySession(ctx, s.db, queries, sessionID, cursor, dir, limit)
}

func (s *SQLiteStore) DeleteBySession(ctx context.Context, sessionID string) error {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	return s.writeMu.WithLock(func() error {
		_, err := s.db.ExecContext(ctx, queries["delete_by_session"], sessionID)
		if err != nil {
			return fmt.Errorf("eventstore: delete by session: %w", err)
		}
		return nil
	})
}

func (s *SQLiteStore) DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	var rowsAffected int64
	err := s.writeMu.WithLock(func() error {
		res, err := s.db.ExecContext(ctx, queries["delete_expired"], cutoff.UnixMilli())
		if err != nil {
			return fmt.Errorf("eventstore: delete expired: %w", err)
		}
		rowsAffected, _ = res.RowsAffected()
		return nil
	})
	return rowsAffected, err
}

func (s *SQLiteStore) Close() error {
	if s.ownsDB {
		return s.db.Close()
	}
	return nil
}

// QueryTurns fetches conversation turns from the materialized turns table.
// Uses a CTE to resolve the latest generation and select in a single SQL round-trip.
func (s *SQLiteStore) QueryTurns(ctx context.Context, sessionID string, limit, offset int) ([]*TurnRecord, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, queries["turns.query_with_gen"], sessionID, sessionID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanTurns(rows)
}

// QueryTurnsBefore fetches turns with id < beforeID (cursor-based, DESC in SQL, reversed to ASC).
// Does not filter generation — supports cross-generation pagination.
func (s *SQLiteStore) QueryTurnsBefore(ctx context.Context, sessionID string, beforeID int64, limit int) ([]*TurnRecord, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, queries["turns.query_before"], sessionID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query turns before: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records, err := scanTurns(rows)
	if err != nil {
		return nil, err
	}
	// Reverse to ASC order (SQL returns DESC).
	slices.Reverse(records)
	return records, nil
}

// QueryLatestTurns fetches the newest N turns of the latest generation.
// SQL returns DESC (newest first); the slice is reversed to ASC for display.
//
// Use this instead of QueryTurns when the caller wants the most recent turns:
// QueryTurns uses ORDER BY id ASC + LIMIT, which returns the OLDEST N once a
// session exceeds `limit` rows — silently dropping recent history (refresh bug).
func (s *SQLiteStore) QueryLatestTurns(ctx context.Context, sessionID string, limit int) ([]*TurnRecord, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, queries["turns.query_latest"], sessionID, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query latest turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records, err := scanTurns(rows)
	if err != nil {
		return nil, err
	}
	// Reverse to ASC order (SQL returns DESC).
	slices.Reverse(records)
	return records, nil
}

// QueryTurnStats returns aggregated turn statistics for a session's latest generation.
// Uses a CTE to resolve the latest generation and select in a single SQL round-trip.
func (s *SQLiteStore) QueryTurnStats(ctx context.Context, sessionID string) (*TurnStats, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, queries["turns.stats_with_gen"], sessionID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query turn stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectTurnStats(rows, sessionID, func() successScanner { return &sqliteSuccessScanner{} }, slog.Default())
}

// LatestGeneration returns the maximum generation for a session, or 0 if no turns exist.
func (s *SQLiteStore) LatestGeneration(ctx context.Context, sessionID string) (int64, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	var gen int64
	err := s.db.QueryRowContext(ctx, queries["turns.latest_generation"], sessionID).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("eventstore: latest generation: %w", err)
	}
	return gen, nil
}

// LatestTurnNum returns the maximum turn_num for a session+generation, or 0 if none exist.
func (s *SQLiteStore) LatestTurnNum(ctx context.Context, sessionID string, generation int64) (int, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	var tn int
	err := s.db.QueryRowContext(ctx, queries["turns.latest_turn_num"], sessionID, generation).Scan(&tn)
	if err != nil {
		return 0, fmt.Errorf("eventstore: latest turn num: %w", err)
	}
	return tn, nil
}

// LatestSeq returns the maximum event seq for a session, or 0 if no events
// exist. Used to hydrate the in-memory SeqGen on reconnect so seq continues
// monotonically instead of restarting from 1 (issue #879).
func (s *SQLiteStore) LatestSeq(ctx context.Context, sessionID string) (int64, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	var seq int64
	err := s.db.QueryRowContext(ctx, queries["latest_seq"], sessionID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("eventstore: latest seq: %w", err)
	}
	return seq, nil
}

// DeleteExpiredTurns removes turns older than the cutoff by created_at.
func (s *SQLiteStore) DeleteExpiredTurns(ctx context.Context, cutoff time.Time) (int64, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	var rowsAffected int64
	err := s.writeMu.WithLock(func() error {
		res, err := s.db.ExecContext(ctx, queries["turns.delete_expired"], cutoff.UnixMilli())
		if err != nil {
			return fmt.Errorf("eventstore: delete expired turns: %w", err)
		}
		rowsAffected, _ = res.RowsAffected()
		return nil
	})
	return rowsAffected, err
}

func scanEvents(rows *sql.Rows) ([]*StoredEvent, error) {
	var events []*StoredEvent
	for rows.Next() {
		var e StoredEvent
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Seq, &e.Type, &e.Data, &e.Direction, &e.Source, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("eventstore: scan: %w", err)
		}
		events = append(events, &e)
	}
	if len(events) == 0 {
		return nil, ErrNotFound
	}
	return events, rows.Err()
}

func scanTurns(rows *sql.Rows) ([]*TurnRecord, error) {
	var records []*TurnRecord
	for rows.Next() {
		var r TurnRecord
		var success sql.NullInt64
		var clientMessageID sql.NullString
		var toolsJSON sql.NullString
		if err := rows.Scan(turnScanDest(&r, &success, &clientMessageID, &toolsJSON)...); err != nil {
			return nil, fmt.Errorf("eventstore: scan turn: %w", err)
		}
		r.ClientMessageID = clientMessageID.String
		if success.Valid {
			s := success.Int64 == 1
			r.Success = &s
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
