package effect

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

// SQLiteStore implements Store on SQLite, sharing the gateway's database
// handle and write mutex rather than opening a second connection pool.
type SQLiteStore struct {
	db      *sql.DB
	log     *slog.Logger
	writeMu *sqlutil.WriteMu
	planner *Planner
}

// NewSQLiteStore creates a SQLite-backed effect store.
func NewSQLiteStore(db *sql.DB, log *slog.Logger, writeMu *sqlutil.WriteMu) *SQLiteStore {
	return &SQLiteStore{
		db:      db,
		log:     log.With("component", "effect_store"),
		writeMu: writeMu,
		planner: NewPlanner(dbutil.DialectSQLite),
	}
}

// PlanOnce owns the transaction so callers cannot accidentally commit the
// effect without its snapshot.
func (s *SQLiteStore) PlanOnce(
	ctx context.Context, plan Plan, now time.Time,
) (*Effect, bool, error) {
	var (
		stored  *Effect
		created bool
	)
	err := s.writeMu.WithLock(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("effect: begin: %w", err)
		}
		stored, created, err = s.planner.PlanWithPayload(ctx, dbTx{tx}, plan, now)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("effect: commit: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if !created {
		// Converge on what the ledger already holds rather than returning the
		// in-memory row: a previous process may have stored a different
		// payload ID or timestamps.
		existing, err := s.GetByKey(ctx, plan.OccurrenceID, plan.DeliveryOrdinal, plan.TargetRevision)
		if err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	return stored, true, nil
}

// GetByKey returns the effect with the given business key.
func (s *SQLiteStore) GetByKey(
	ctx context.Context, occurrenceID string, deliveryOrdinal int64, targetRevision string,
) (*Effect, error) {
	return scanEffect(s.db.QueryRowContext(ctx, `SELECT `+effectColumns+` FROM effects
		WHERE occurrence_id = ? AND delivery_ordinal = ? AND target_revision = ?`,
		occurrenceID, deliveryOrdinal, targetRevision))
}

// GetByID returns the effect by its surrogate ID.
func (s *SQLiteStore) GetByID(ctx context.Context, effectID string) (*Effect, error) {
	return scanEffect(s.db.QueryRowContext(ctx,
		`SELECT `+effectColumns+` FROM effects WHERE effect_id = ?`, effectID))
}

// GetPayload returns a content snapshot by ID.
func (s *SQLiteStore) GetPayload(ctx context.Context, payloadID string) (*Payload, error) {
	return scanPayload(s.db.QueryRowContext(ctx,
		`SELECT `+payloadColumns+` FROM effect_payloads WHERE payload_id = ?`, payloadID))
}

// GetPayloadForExecution returns the snapshot committed for one execution.
func (s *SQLiteStore) GetPayloadForExecution(
	ctx context.Context, occurrenceID, executionID string,
) (*Payload, error) {
	return scanPayload(s.db.QueryRowContext(ctx,
		`SELECT `+payloadColumns+` FROM effect_payloads
		 WHERE occurrence_id = ? AND execution_id = ?`, occurrenceID, executionID))
}
