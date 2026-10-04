package effect

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// PGStore implements Store on PostgreSQL. The SQL is identical; only
// placeholder binding differs.
type PGStore struct {
	db      *dbutil.DB
	log     *slog.Logger
	planner *Planner
}

// NewPGStore creates a PostgreSQL-backed effect store.
func NewPGStore(db *dbutil.DB, log *slog.Logger) *PGStore {
	return &PGStore{
		db:      db,
		log:     log.With("component", "effect_store"),
		planner: NewPlanner(db.Dialect()),
	}
}

// PlanOnce owns the transaction so callers cannot accidentally commit the
// effect without its snapshot.
func (s *PGStore) PlanOnce(
	ctx context.Context, plan Plan, now time.Time,
) (*Effect, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("effect: begin: %w", err)
	}
	stored, created, err := s.planner.PlanWithPayload(ctx, dbTx{tx}, plan, now)
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("effect: commit: %w", err)
	}
	if !created {
		existing, err := s.GetByKey(ctx, plan.OccurrenceID, plan.DeliveryOrdinal, plan.TargetRevision)
		if err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	return stored, true, nil
}

// GetByKey returns the effect with the given business key.
func (s *PGStore) GetByKey(
	ctx context.Context, occurrenceID string, deliveryOrdinal int64, targetRevision string,
) (*Effect, error) {
	return scanEffect(s.db.QueryRowContext(ctx, s.db.Dialect().Rebind(`SELECT `+effectColumns+`
		FROM effects WHERE occurrence_id = ? AND delivery_ordinal = ? AND target_revision = ?`),
		occurrenceID, deliveryOrdinal, targetRevision))
}

// GetByID returns the effect by its surrogate ID.
func (s *PGStore) GetByID(ctx context.Context, effectID string) (*Effect, error) {
	return scanEffect(s.db.QueryRowContext(ctx, s.db.Dialect().Rebind(
		`SELECT `+effectColumns+` FROM effects WHERE effect_id = ?`), effectID))
}

// GetPayload returns a content snapshot by ID.
func (s *PGStore) GetPayload(ctx context.Context, payloadID string) (*Payload, error) {
	return scanPayload(s.db.QueryRowContext(ctx, s.db.Dialect().Rebind(
		`SELECT `+payloadColumns+` FROM effect_payloads WHERE payload_id = ?`), payloadID))
}

// GetPayloadForExecution returns the snapshot committed for one execution.
func (s *PGStore) GetPayloadForExecution(
	ctx context.Context, occurrenceID, executionID string,
) (*Payload, error) {
	return scanPayload(s.db.QueryRowContext(ctx, s.db.Dialect().Rebind(
		`SELECT `+payloadColumns+` FROM effect_payloads
		 WHERE occurrence_id = ? AND execution_id = ?`), occurrenceID, executionID))
}
