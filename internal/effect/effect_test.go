package effect_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

const effectDDL = `
CREATE TABLE effect_payloads (
    payload_id      TEXT PRIMARY KEY,
    occurrence_id   TEXT NOT NULL,
    execution_id    TEXT NOT NULL DEFAULT '',
    worker_run_id   TEXT NOT NULL DEFAULT '',
    content         TEXT NOT NULL,
    content_bytes   INTEGER NOT NULL,
    content_sha256  TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    UNIQUE(occurrence_id, execution_id)
);
CREATE TABLE effects (
    effect_id         TEXT PRIMARY KEY,
    occurrence_id     TEXT NOT NULL,
    delivery_ordinal  INTEGER NOT NULL,
    target_revision   TEXT NOT NULL,
    attempt           INTEGER NOT NULL DEFAULT 0,
    session_id        TEXT NOT NULL DEFAULT '',
    execution_id      TEXT NOT NULL DEFAULT '',
    worker_run_id     TEXT NOT NULL DEFAULT '',
    payload_id        TEXT NOT NULL DEFAULT '',
    payload_sha256    TEXT NOT NULL DEFAULT '',
    target_kind       TEXT NOT NULL,
    target_ref        TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL CHECK(status IN ('planned','started','delivered','failed','unknown','reconciled_succeeded','reconciled_failed','fenced')),
    next_attempt_at   INTEGER,
    error_code        TEXT NOT NULL DEFAULT '',
    reason            TEXT NOT NULL DEFAULT '',
    owner_instance_id TEXT NOT NULL DEFAULT '',
    lease_until       INTEGER,
    lease_version     INTEGER NOT NULL DEFAULT 0,
    provider_ref      TEXT NOT NULL DEFAULT '',
    evidence_ref      TEXT NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    settled_at        INTEGER,
    payload_retention_ms INTEGER,
    facts_retention_ms INTEGER,
    retention_policy_revision TEXT NOT NULL DEFAULT '',
    UNIQUE(occurrence_id, delivery_ordinal, target_revision)
);
CREATE TABLE effect_attempts (
    attempt_id        TEXT PRIMARY KEY,
    effect_id         TEXT NOT NULL,
    attempt           INTEGER NOT NULL,
    owner_instance_id TEXT NOT NULL,
    lease_version     INTEGER NOT NULL,
    lease_token       TEXT NOT NULL,
    started_at        INTEGER NOT NULL,
    finished_at       INTEGER,
    outcome           TEXT NOT NULL DEFAULT ''
                       CHECK(outcome IN ('','accepted','rejected','unknown','not_sent')),
    rejection_class   TEXT NOT NULL DEFAULT ''
                       CHECK(rejection_class IN ('','safe_retry','permanent','unspecified')),
    provider_ref      TEXT NOT NULL DEFAULT '',
    evidence_ref      TEXT NOT NULL DEFAULT '',
    error_code        TEXT NOT NULL DEFAULT '',
    reason            TEXT NOT NULL DEFAULT '',
    UNIQUE(effect_id, attempt)
);`

// newLedgerDB opens a SQLite database carrying the effect tables from goose
// migration 034 and an eventstore bound to the same connection, so tests
// exercise the real shared-transaction path rather than a stand-in.
func newLedgerDB(t *testing.T) (*sql.DB, *eventstore.SQLiteStore) {
	t.Helper()
	db, err := sql.Open(sqlutil.DriverName, filepath.Join(t.TempDir(), "effect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA busy_timeout=5000`)
	require.NoError(t, err)
	_, err = db.Exec(effectDDL)
	require.NoError(t, err)

	return db, eventstore.NewSQLiteStore(db, nil)
}

func validPlan() effect.Plan {
	return effect.Plan{
		OccurrenceID:    "occ-1",
		DeliveryOrdinal: 0,
		TargetRevision:  "rev-a",
		SessionID:       "s-1",
		ExecutionID:     "exec-1",
		WorkerRunID:     "run-1",
		Content:         "the final answer",
		TargetKind:      "slack",
		TargetRef:       "C123",
	}
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&n))
	return n
}

func TestPlanWithPayload_CommitsSnapshotAndEffectTogether(t *testing.T) {
	t.Parallel()

	db, es := newLedgerDB(t)
	planner := effect.NewPlanner(dbutil.DialectSQLite)

	tx, err := es.BeginTx(context.Background())
	require.NoError(t, err)

	eff, created, err := planner.PlanWithPayload(context.Background(), tx, validPlan(), time.Now())
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, tx.Commit())

	// Both rows are visible together after commit.
	require.Equal(t, 1, countRows(t, db, "effect_payloads"))
	require.Equal(t, 1, countRows(t, db, "effects"))
	require.Equal(t, effect.StatusPlanned, eff.Status)
	require.Equal(t, effect.ContentHash("the final answer"), eff.PayloadSHA256)

	var content, sha string
	require.NoError(t, db.QueryRow(
		`SELECT content, content_sha256 FROM effect_payloads WHERE payload_id = ?`,
		eff.PayloadID).Scan(&content, &sha))
	require.Equal(t, "the final answer", content)
	require.Equal(t, effect.ContentHash("the final answer"), sha)
}

func TestPlanWithPayload_RollbackLeavesNeitherRow(t *testing.T) {
	t.Parallel()

	db, es := newLedgerDB(t)
	planner := effect.NewPlanner(dbutil.DialectSQLite)

	tx, err := es.BeginTx(context.Background())
	require.NoError(t, err)
	_, _, err = planner.PlanWithPayload(context.Background(), tx, validPlan(), time.Now())
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	// A crash before commit must leave no half-written intent: an effect that
	// exists without its content would be a send with nothing to send.
	require.Equal(t, 0, countRows(t, db, "effect_payloads"))
	require.Equal(t, 0, countRows(t, db, "effects"))
}

func TestPlanWithPayload_SameBusinessKeyConvergesInsteadOfDuplicating(t *testing.T) {
	t.Parallel()

	db, es := newLedgerDB(t)
	planner := effect.NewPlanner(dbutil.DialectSQLite)

	tx, err := es.BeginTx(context.Background())
	require.NoError(t, err)
	first, created, err := planner.PlanWithPayload(context.Background(), tx, validPlan(), time.Now())
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, tx.Commit())

	// Recovery re-plans the same occurrence/ordinal/target. It must converge on
	// the existing effect rather than producing a second one that would
	// double-deliver.
	tx2, err := es.BeginTx(context.Background())
	require.NoError(t, err)
	_, created2, err := planner.PlanWithPayload(context.Background(), tx2, validPlan(), time.Now())
	require.NoError(t, err)
	require.False(t, created2)
	require.NoError(t, tx2.Commit())
	require.Equal(t, 1, countRows(t, db, "effects"))
	require.Equal(t, 1, countRows(t, db, "effect_payloads"))
	require.NotEmpty(t, first.EffectID)
}

func TestPlanWithPayload_DifferentTargetRevisionIsANewEffect(t *testing.T) {
	t.Parallel()

	db, es := newLedgerDB(t)
	planner := effect.NewPlanner(dbutil.DialectSQLite)

	for _, revision := range []string{"rev-a", "rev-b"} {
		plan := validPlan()
		plan.TargetRevision = revision
		tx, err := es.BeginTx(context.Background())
		require.NoError(t, err)
		_, created, err := planner.PlanWithPayload(context.Background(), tx, plan, time.Now())
		require.NoError(t, err)
		require.True(t, created, "editing the target yields a new effect, not a mutation")
		require.NoError(t, tx.Commit())
	}

	require.Equal(t, 2, countRows(t, db, "effects"),
		"each target revision is a distinct effect")
	// The content snapshot is keyed by occurrence+execution, not by target:
	// re-targeting the same run reuses the one snapshot it already committed,
	// so the second plan converges on it instead of duplicating the text.
	require.Equal(t, 1, countRows(t, db, "effect_payloads"))
}

func TestPlanWithPayload_RefusesBeforeSending(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*effect.Plan)
		wantErr error
	}{
		{"empty content", func(p *effect.Plan) { p.Content = "" }, effect.ErrEmptyContent},
		{"oversized content", func(p *effect.Plan) {
			p.Content = strings.Repeat("x", effect.MaxPayloadBytes+1)
		}, effect.ErrPayloadTooLarge},
		{"missing target kind", func(p *effect.Plan) { p.TargetKind = "" }, effect.ErrMissingTarget},
		{"missing occurrence", func(p *effect.Plan) { p.OccurrenceID = "" }, effect.ErrMissingIdentity},
		{"missing target revision", func(p *effect.Plan) { p.TargetRevision = "" }, effect.ErrMissingIdentity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db, es := newLedgerDB(t)
			planner := effect.NewPlanner(dbutil.DialectSQLite)
			plan := validPlan()
			tt.mutate(&plan)

			tx, err := es.BeginTx(context.Background())
			require.NoError(t, err)
			_, _, err = planner.PlanWithPayload(context.Background(), tx, plan, time.Now())
			require.ErrorIs(t, err, tt.wantErr)
			require.NoError(t, tx.Rollback())

			require.Equal(t, 0, countRows(t, db, "effects"),
				"a refused plan must never reach the ledger, let alone a send")
		})
	}
}

func TestPlanWithPayload_RollbackThenRealCommitRecovers(t *testing.T) {
	t.Parallel()

	db, es := newLedgerDB(t)
	planner := effect.NewPlanner(dbutil.DialectSQLite)

	// An aborted attempt leaves nothing behind.
	tx, err := es.BeginTx(context.Background())
	require.NoError(t, err)
	_, _, err = planner.PlanWithPayload(context.Background(), tx, validPlan(), time.Now())
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	require.Equal(t, 0, countRows(t, db, "effects"))
	require.Equal(t, 0, countRows(t, db, "effect_payloads"))

	// The connection is still usable and a later real commit succeeds.
	tx2, err := es.BeginTx(context.Background())
	require.NoError(t, err)
	_, created, err := planner.PlanWithPayload(context.Background(), tx2, validPlan(), time.Now())
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, tx2.Commit())
	require.Equal(t, 1, countRows(t, db, "effects"))
}

func TestBusinessKeyExcludesAttempt(t *testing.T) {
	t.Parallel()

	// Two attempts of the same occurrence/ordinal/revision share one key by
	// construction: BusinessKey takes no attempt argument at all.
	key := effect.BusinessKey("occ-1", 0, "rev-a")
	require.Equal(t, key, effect.BusinessKey("occ-1", 0, "rev-a"))
	require.NotEqual(t, key, effect.BusinessKey("occ-1", 1, "rev-a"))
	require.NotEqual(t, key, effect.BusinessKey("occ-1", 0, "rev-b"))
	require.NotEqual(t, key, effect.BusinessKey("occ-2", 0, "rev-a"))
}

func TestStatusUnknownIsDistinctFromFailedAndDelivered(t *testing.T) {
	t.Parallel()

	// A lost response is unknown, never a retryable failure — that distinction
	// is what stops an unsafe resend.
	require.NotEqual(t, effect.StatusUnknown, effect.StatusFailed)
	require.NotEqual(t, effect.StatusUnknown, effect.StatusDelivered)
	require.NotEqual(t, effect.StatusUnknown, effect.StatusPlanned)
}
