package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/hrygo/hotplex/pkg/events"
)

// queryExecer abstracts the read-side query capability shared by *sql.DB
// (SQLite) and *dbutil.DB (Postgres, which embeds *sql.DB). It lets the
// cursor-pagination and stat-aggregation logic live in one place regardless of
// the storage backend.
type queryExecer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type commandExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func rebindEventSQL(query string) string { return query }

func ensureSessionWritable(ctx context.Context, tx *sql.Tx, sessionID string, rebind func(string) string, lockRow bool) error {
	stateQuery := `SELECT state FROM sessions WHERE id = ?`
	if lockRow {
		stateQuery += ` FOR UPDATE`
	}
	var state sql.NullString
	err := tx.QueryRowContext(ctx, rebind(stateQuery), sessionID).Scan(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("eventstore: check session write barrier: %w", err)
	}
	if state.Valid && state.String == string(events.StateDeleted) {
		return ErrSessionDeleted
	}
	if state.Valid {
		return nil
	}

	var hasPurgeJob bool
	err = tx.QueryRowContext(ctx, rebind(`SELECT EXISTS(
		SELECT 1 FROM session_purge_jobs WHERE session_id = ?
	)`), sessionID).Scan(&hasPurgeJob)
	if err != nil {
		return fmt.Errorf("eventstore: check session purge barrier: %w", err)
	}
	if hasPurgeJob {
		return ErrSessionDeleted
	}
	return nil
}

// queryBySession is the dialect-agnostic cursor pagination implementation
// shared by SQLiteStore and pgStore. The caller supplies its own query map
// (the package-level `queries` for SQLite, the rebound `s.sql` for Postgres).
func queryBySession(
	ctx context.Context,
	qe queryExecer,
	q map[string]string,
	sessionID string,
	cursor int64,
	dir CursorDirection,
	limit int,
	nowMS int64,
	legacyCutoffMS int64,
) (*EventPage, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}

	// Fetch one extra to detect has_more.
	fetchLimit := limit + 1

	var rows *sql.Rows
	var err error
	switch dir {
	case CursorAfter:
		rows, err = qe.QueryContext(ctx, q["query_after"], sessionID, cursor, nowMS, legacyCutoffMS, fetchLimit)
	case CursorBefore:
		rows, err = qe.QueryContext(ctx, q["query_before"], sessionID, cursor, nowMS, legacyCutoffMS, fetchLimit)
	default: // CursorLatest
		rows, err = qe.QueryContext(ctx, q["query_latest"], sessionID, nowMS, legacyCutoffMS, fetchLimit)
	}
	if err != nil {
		return nil, fmt.Errorf("eventstore: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events, err := scanEvents(rows)
	if err != nil {
		// scanEvents already wraps its driver error as "eventstore: scan: ...".
		return nil, err
	}

	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}

	// For DESC queries (CursorLatest, CursorBefore), reverse to ASC order.
	if dir == CursorLatest || dir == CursorBefore {
		slices.Reverse(events)
	}

	page := &EventPage{Events: events}
	if len(events) > 0 {
		page.OldestID = events[0].ID
		page.NewestID = events[len(events)-1].ID
		page.OldestSeq = events[0].Seq
		page.NewestSeq = events[len(events)-1].Seq
	}
	if len(events) > 0 {
		switch dir {
		case CursorLatest, CursorBefore:
			page.HasOlder = hasMore
		default:
			var exists int
			err := qe.QueryRowContext(ctx, q["has_older"], sessionID, page.OldestID, nowMS, legacyCutoffMS).Scan(&exists)
			page.HasOlder = err == nil && exists == 1
		}
	}

	return page, nil
}

func updateSessionContentDeadline(ctx context.Context, exec commandExecer, query, sessionID string, expiresAt int64) error {
	if expiresAt <= 0 {
		return nil
	}
	deadline := time.UnixMilli(expiresAt).UTC()
	_, err := exec.ExecContext(ctx, query, deadline, deadline, deadline, deadline, sessionID)
	return err
}

func contentExpiryForSession(
	ctx context.Context,
	tx *sql.Tx,
	policyQuery string,
	retention RetentionPolicy,
	sessionID string,
	createdAt int64,
	existingExpiry int64,
) (int64, error) {
	if existingExpiry > 0 {
		return existingExpiry, nil
	}
	if retention.Content <= 0 || createdAt <= 0 {
		return 0, nil
	}

	var policy string
	err := tx.QueryRowContext(ctx, policyQuery, sessionID).Scan(&policy)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if policy != "v2" {
		return 0, nil
	}
	return retention.expiry(createdAt, 0), nil
}

// turnScanDest returns the scan destination slice for a turn row. It is shared
// by scanTurns (SQLite) and scanTurnsPG (Postgres) so the 23-column projection
// order cannot drift between backends. successPtr is dialect-specific
// (*sql.NullInt64 for SQLite INTEGER, *sql.NullBool for Postgres BOOLEAN).
func turnScanDest(r *TurnRecord, successPtr, clientMessageID, toolsJSON any) []any {
	return []any{
		&r.ID, &r.SessionID, clientMessageID, &r.Generation, &r.TurnNum, &r.Seq, &r.Role, &r.Content,
		&r.Platform, &r.UserID, &r.Model, successPtr, &r.Source,
		toolsJSON, &r.ToolCount,
		&r.TokensInput, &r.TokensCacheWrite, &r.TokensCacheRead, &r.TokensIn,
		&r.TokensOut, &r.DurationMs, &r.CostUSD, &r.CreatedAt,
	}
}

// turnStatScanDest returns the scan destination slice for a turn-stat row.
// Shared by SQLiteStore and pgStore QueryTurnStats so the 16-column projection
// order cannot drift between backends. successPtr is dialect-specific.
func turnStatScanDest(gen *int64, ts *TurnStatItem, successPtr, toolsJSON, toolCount any) []any {
	return []any{
		gen, &ts.TurnNum, &ts.Seq, successPtr, &ts.Source,
		toolsJSON, toolCount,
		&ts.TokensInput, &ts.TokensCacheWrite, &ts.TokensCacheRead, &ts.TokensIn,
		&ts.TokensOut, &ts.DurationMs, &ts.CostUSD, &ts.Model, &ts.CreatedAt,
	}
}

// successScanner abstracts the dialect-specific success column (SQLite INTEGER
// via sql.NullInt64, Postgres BOOLEAN via sql.NullBool) for turn-stat scanning.
// A single instance is reused across all rows in a stat result — ScanPointer
// returns the same destination each row and Decode reads the latest scan — so
// there is no per-row allocation on the hot read path.
type successScanner interface {
	ScanPointer() any // scan destination, reused each row
	Decode() bool     // decodes the most recently scanned value
}

type sqliteSuccessScanner struct{ v sql.NullInt64 }

func (s *sqliteSuccessScanner) ScanPointer() any { return &s.v }
func (s *sqliteSuccessScanner) Decode() bool     { return s.v.Valid && s.v.Int64 == 1 }

type pgSuccessScanner struct{ v sql.NullBool }

func (s *pgSuccessScanner) ScanPointer() any { return &s.v }
func (s *pgSuccessScanner) Decode() bool     { return s.v.Valid && s.v.Bool }

// collectTurnStats iterates turn-stat rows and accumulates them into a TurnStats.
// It is shared by SQLiteStore and pgStore; the only dialect-specific concern is
// the success column (INTEGER vs BOOLEAN), supplied via newScanner which returns
// a reusable successScanner created once before the row loop.
func collectTurnStats(
	rows *sql.Rows,
	sessionID string,
	newScanner func() successScanner,
	log *slog.Logger,
) (*TurnStats, error) {
	scanner := newScanner()
	stats := &TurnStats{SessionID: sessionID}
	for rows.Next() {
		var ts TurnStatItem
		var gen int64
		var toolsJSON sql.NullString
		var toolCount sql.NullInt64
		if err := rows.Scan(turnStatScanDest(&gen, &ts, scanner.ScanPointer(), &toolsJSON, &toolCount)...); err != nil {
			log.Warn("eventstore: scan turn stats row", "session_id", sessionID, "err", err)
			continue
		}
		if stats.Generation == 0 {
			stats.Generation = gen
		}
		ts.Success = scanner.Decode()
		stats.TotalTurns++
		if ts.Success {
			stats.SuccessTurns++
		} else {
			stats.FailedTurns++
		}
		stats.TotalDurMs += ts.DurationMs
		stats.TotalCostUSD += ts.CostUSD
		stats.TotalTokIn += ts.TokensIn
		stats.TotalTokInput += ts.TokensInput
		stats.TotalTokCacheWrite += ts.TokensCacheWrite
		stats.TotalTokCacheRead += ts.TokensCacheRead
		stats.TotalTokOut += ts.TokensOut
		stats.Turns = append(stats.Turns, ts)
	}
	if stats.TotalTurns == 0 {
		return nil, ErrNotFound
	}
	return stats, rows.Err()
}
