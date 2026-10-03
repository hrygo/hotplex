package cron

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// pgOccurrenceStore implements OccurrenceStore using PostgreSQL.
type pgOccurrenceStore struct {
	db      *dbutil.DB
	dialect dbutil.Dialect
	log     *slog.Logger
}

// NewPGOccurrenceStore creates a PostgreSQL-backed occurrence store.
func NewPGOccurrenceStore(db *dbutil.DB, log *slog.Logger) OccurrenceStore {
	return &pgOccurrenceStore{
		db:      db,
		dialect: db.Dialect(),
		log:     log.With("component", "pg_cron_occurrence_store"),
	}
}

func (s *pgOccurrenceStore) Claim(
	ctx context.Context, occ *Occurrence,
) (*Occurrence, bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	query := s.dialect.Rebind(`INSERT INTO cron_occurrences (` + occurrenceColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trigger_key, generation) DO NOTHING`)
	res, err := s.db.ExecContext(ctx, query,
		occ.OccurrenceID, occ.TriggerKey, occ.Generation, occ.JobID, occ.TriggerKind,
		occ.ScheduleRev, occ.ScheduledAtMs, occ.Nonce, occ.SourceID,
		occ.SessionID, occ.ExecutionID, occ.Status, occ.ErrorCode,
		occ.CreatedAtMs, occ.UpdatedAtMs, occ.StartedAtMs, occ.FinishedAtMs,
	)
	if err != nil {
		return nil, false, fmt.Errorf("cron occurrence: claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("cron occurrence: claim rows: %w", err)
	}
	stored, err := s.Get(ctx, occ.TriggerKey, occ.Generation)
	if err != nil {
		return nil, false, err
	}
	return stored, n > 0, nil
}

func (s *pgOccurrenceStore) Get(
	ctx context.Context, triggerKey string, generation int64,
) (*Occurrence, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT `+occurrenceColumns+` FROM cron_occurrences
		 WHERE trigger_key = ? AND generation = ?`,
		triggerKey, generation)
	return scanOccurrence(row)
}

func (s *pgOccurrenceStore) GetByID(
	ctx context.Context, occurrenceID string,
) (*Occurrence, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT `+occurrenceColumns+` FROM cron_occurrences WHERE occurrence_id = ?`,
		occurrenceID)
	return scanOccurrence(row)
}

func (s *pgOccurrenceStore) BindSession(
	ctx context.Context, occurrenceID, sessionID string,
) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	query := s.dialect.Rebind(
		`UPDATE cron_occurrences SET session_id = ?, updated_at = updated_at
		 WHERE occurrence_id = ?`)
	if _, err := s.db.ExecContext(ctx, query, sessionID, occurrenceID); err != nil {
		return fmt.Errorf("cron occurrence: bind session: %w", err)
	}
	return nil
}

func (s *pgOccurrenceStore) UpdateStatus(
	ctx context.Context,
	occurrenceID string,
	status OccurrenceStatus,
	errCode string,
	at time.Time,
) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	query := s.dialect.Rebind(`UPDATE cron_occurrences SET
		status = ?,
		error_code = ?,
		updated_at = ?,
		started_at = CASE WHEN ? = 'started' AND started_at IS NULL THEN ? ELSE started_at END,
		finished_at = CASE WHEN ? IN ('completed', 'failed', 'unknown') THEN ? ELSE finished_at END
		WHERE occurrence_id = ?`)
	_, err := s.db.ExecContext(ctx, query,
		status, errCode, at.UnixMilli(), status, at.UnixMilli(),
		status, at.UnixMilli(), occurrenceID)
	if err != nil {
		return fmt.Errorf("cron occurrence: update status: %w", err)
	}
	return nil
}

func (s *pgOccurrenceStore) ListByJob(
	ctx context.Context, jobID string, limit int,
) ([]*Occurrence, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+occurrenceColumns+` FROM cron_occurrences
		 WHERE job_id = ? ORDER BY created_at DESC, occurrence_id DESC LIMIT ?`,
		jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("cron occurrence: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Occurrence
	for rows.Next() {
		occ, err := scanOccurrence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, occ)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cron occurrence: list rows: %w", err)
	}
	return out, nil
}

// compile-time assertion that the PG store satisfies the interface.
var _ OccurrenceStore = (*pgOccurrenceStore)(nil)
