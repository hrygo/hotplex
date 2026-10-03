package effect_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/effect"
)

// The console's timeline links a run to its deliveries without loading every
// effect ever recorded, which is what ExecutionID filtering is for.
func TestListForOperator_FiltersByExecution(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()

	_, _, err := store.PlanOnce(ctx, validPlan(), time.Now())
	require.NoError(t, err)

	other := validPlan()
	other.OccurrenceID = "occ-2"
	other.ExecutionID = "exec-2"
	_, _, err = store.PlanOnce(ctx, other, time.Now())
	require.NoError(t, err)

	mine, err := store.ListForOperator(ctx, effect.OperatorListFilter{ExecutionID: "exec-1"})
	require.NoError(t, err)
	require.Len(t, mine, 1)
	require.Equal(t, "exec-1", mine[0].ExecutionID)

	theirs, err := store.ListForOperator(ctx, effect.OperatorListFilter{ExecutionID: "exec-2"})
	require.NoError(t, err)
	require.Len(t, theirs, 1)
	require.Equal(t, "exec-2", theirs[0].ExecutionID)

	none, err := store.ListForOperator(ctx, effect.OperatorListFilter{ExecutionID: "exec-nope"})
	require.NoError(t, err)
	require.Empty(t, none)
}

// Status and ExecutionID must compose rather than overwrite each other: the
// assembled query has to bind one placeholder per condition.
func TestListForOperator_ExecutionAndStatusFiltersCompose(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	now := time.Now()

	planned, _, err := store.PlanOnce(ctx, validPlan(), now)
	require.NoError(t, err)

	sending := validPlan()
	sending.OccurrenceID = "occ-2"
	started, _, err := store.PlanOnce(ctx, sending, now)
	require.NoError(t, err)

	claim, err := store.ClaimSend(ctx, claimFor(started.EffectID, 0, now))
	require.NoError(t, err)
	require.NoError(t, store.CompleteSend(ctx, effect.Completion{
		EffectID:     claim.Effect.EffectID,
		Attempt:      claim.Attempt,
		LeaseToken:   claim.LeaseToken,
		LeaseVersion: claim.LeaseVersion,
		Outcome:      effect.AttemptUnknown,
		Now:          now,
	}))

	unknown, err := store.ListForOperator(ctx, effect.OperatorListFilter{
		ExecutionID: "exec-1",
		Status:      effect.StatusUnknown,
	})
	require.NoError(t, err)
	require.Len(t, unknown, 1)
	require.Equal(t, started.EffectID, unknown[0].EffectID)

	plannedOnly, err := store.ListForOperator(ctx, effect.OperatorListFilter{
		ExecutionID: "exec-1",
		Status:      effect.StatusPlanned,
	})
	require.NoError(t, err)
	require.Len(t, plannedOnly, 1)
	require.Equal(t, planned.EffectID, plannedOnly[0].EffectID)
}
