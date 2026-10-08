package effect

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// PayloadGCStore exposes the narrowly scoped retention operation for delivery
// bodies without widening Store and every delivery adapter's test double.
type PayloadGCStore interface {
	DeleteExpiredPayloads(ctx context.Context, settledBefore time.Time, limit int) (int64, error)
}

// Payload bodies are blanked in place rather than deleting identity rows.
// Keeping the unique occurrence/execution row prevents a late duplicate trigger
// from recreating expired content. settledBefore is the latest settlement time
// still eligible for deletion (now minus the configured post-settlement TTL).
const deleteExpiredPayloadsSQL = `UPDATE effect_payloads
	SET content = '', content_bytes = 0
	WHERE content_bytes > 0
	  AND payload_id IN (
		SELECT p.payload_id
		FROM effect_payloads p
		WHERE p.content_bytes > 0
		  AND EXISTS (
			SELECT 1 FROM effects e WHERE e.payload_id = p.payload_id
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM effects e
			WHERE e.payload_id = p.payload_id
			  AND (e.status IN ('planned', 'started', 'unknown')
			       OR e.settled_at IS NULL
			       OR e.lease_until IS NOT NULL
			       OR e.next_attempt_at IS NOT NULL)
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM effects e
			WHERE e.payload_id = p.payload_id AND e.settled_at > ?
		  )
		ORDER BY p.created_at, p.payload_id
		LIMIT ?
	  )`

func (s *SQLiteStore) DeleteExpiredPayloads(
	ctx context.Context, settledBefore time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	var deleted int64
	err := s.writeMu.WithLock(func() error {
		return s.inTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, dbutil.DialectSQLite.Rebind(deleteExpiredPayloadsSQL),
				settledBefore.UnixMilli(), limit)
			if err != nil {
				return fmt.Errorf("effect: expire payloads: %w", err)
			}
			deleted, err = res.RowsAffected()
			if err != nil {
				return fmt.Errorf("effect: expired payload rows: %w", err)
			}
			return nil
		})
	})
	return deleted, err
}

func (s *PGStore) DeleteExpiredPayloads(
	ctx context.Context, settledBefore time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	var deleted int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Dialect().Rebind(deleteExpiredPayloadsSQL),
			settledBefore.UnixMilli(), limit)
		if err != nil {
			return fmt.Errorf("effect: expire payloads: %w", err)
		}
		deleted, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("effect: expired payload rows: %w", err)
		}
		return nil
	})
	return deleted, err
}
