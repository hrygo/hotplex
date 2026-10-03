package effect_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

func newStore(t *testing.T) (*effect.SQLiteStore, *sql.DB) {
	t.Helper()
	db, _ := newLedgerDB(t)
	writeMu := sqlutil.NewWriteMu(string(dbutil.DialectSQLite))
	return effect.NewSQLiteStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)), writeMu), db
}

func TestStorePlanOnce_ConvergesOnTheSameEffect(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := context.Background()

	first, created, err := store.PlanOnce(ctx, validPlan(), time.Now())
	require.NoError(t, err)
	require.True(t, created)

	// A crash between commit and acknowledgement, or a duplicated trigger,
	// replans the same business key. It must resolve to the committed effect
	// rather than creating a second one that would deliver twice.
	second, created2, err := store.PlanOnce(ctx, validPlan(), time.Now())
	require.NoError(t, err)
	require.False(t, created2)
	require.Equal(t, first.EffectID, second.EffectID)
	require.Equal(t, first.PayloadID, second.PayloadID)
	require.Equal(t, 1, countRows(t, db, "effects"))
	require.Equal(t, 1, countRows(t, db, "effect_payloads"))
}

func TestStorePlanOnce_RejectsBeforeWritingAnything(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*effect.Plan)
		wantIs error
	}{
		{name: "empty content", mutate: func(p *effect.Plan) { p.Content = "" }, wantIs: effect.ErrEmptyContent},
		{name: "missing target", mutate: func(p *effect.Plan) { p.TargetKind = "" }, wantIs: effect.ErrMissingTarget},
		{name: "missing occurrence", mutate: func(p *effect.Plan) { p.OccurrenceID = "" }, wantIs: effect.ErrMissingIdentity},
		{name: "missing revision", mutate: func(p *effect.Plan) { p.TargetRevision = "" }, wantIs: effect.ErrMissingIdentity},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, db := newStore(t)
			plan := validPlan()
			tc.mutate(&plan)

			_, _, err := store.PlanOnce(context.Background(), plan, time.Now())
			require.ErrorIs(t, err, tc.wantIs)
			require.Equal(t, 0, countRows(t, db, "effects"),
				"a refused plan must leave no delivery intent behind")
			require.Equal(t, 0, countRows(t, db, "effect_payloads"))
		})
	}
}

func TestStorePlanOnce_RefusesContentThatDisagreesWithTheLedger(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := context.Background()

	_, _, err := store.PlanOnce(ctx, validPlan(), time.Now())
	require.NoError(t, err)

	// Same occurrence+execution, different final text. The ledger already
	// committed the first answer; sending the second would deliver something
	// the ledger cannot account for.
	divergent := validPlan()
	divergent.TargetRevision = "rev-b"
	divergent.Content = "a different answer"

	_, _, err = store.PlanOnce(ctx, divergent, time.Now())
	require.ErrorIs(t, err, effect.ErrPayloadEffectMismatch)
	require.Equal(t, 1, countRows(t, db, "effects"))
}

func TestStoreLookups(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()

	planned, _, err := store.PlanOnce(ctx, validPlan(), time.Now())
	require.NoError(t, err)

	byKey, err := store.GetByKey(ctx, "occ-1", 0, "rev-a")
	require.NoError(t, err)
	require.Equal(t, planned.EffectID, byKey.EffectID)
	require.Equal(t, effect.StatusPlanned, byKey.Status)
	require.Nil(t, byKey.LeaseUntilMs, "a planned effect holds no lease")

	byID, err := store.GetByID(ctx, planned.EffectID)
	require.NoError(t, err)
	require.Equal(t, planned.OccurrenceID, byID.OccurrenceID)

	payload, err := store.GetPayload(ctx, planned.PayloadID)
	require.NoError(t, err)
	require.Equal(t, "the final answer", payload.Content)
	require.Equal(t, effect.ContentHash("the final answer"), payload.ContentSHA)

	byExecution, err := store.GetPayloadForExecution(ctx, "occ-1", "exec-1")
	require.NoError(t, err)
	require.Equal(t, planned.PayloadID, byExecution.PayloadID)
}

func TestStoreLookups_ReportAbsenceDistinctly(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()

	_, err := store.GetByKey(ctx, "missing", 0, "rev-a")
	require.ErrorIs(t, err, effect.ErrEffectNotFound)

	_, err = store.GetByID(ctx, "eff-missing")
	require.ErrorIs(t, err, effect.ErrEffectNotFound)

	_, err = store.GetPayload(ctx, "pay-missing")
	require.ErrorIs(t, err, effect.ErrPayloadNotFound)

	_, err = store.GetPayloadForExecution(ctx, "occ-1", "exec-1")
	require.ErrorIs(t, err, effect.ErrPayloadNotFound)
}
