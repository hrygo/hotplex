package effect_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/effect"
)

func TestRetentionSnapshotSurvivesShorterPolicy(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	store.SetRetentionPolicy(7*24*time.Hour, 7*24*time.Hour, "old-policy")
	oldPlan := validPlan()
	old, _, err := store.PlanOnce(ctx, oldPlan, base)
	require.NoError(t, err)
	claim, err := store.ClaimSend(ctx, claimFor(old.EffectID, 0, base.Add(time.Hour)))
	require.NoError(t, err)
	settledAt := base.Add(2 * time.Hour)
	require.NoError(t, store.CompleteSend(ctx, completeFor(claim, settledAt)))

	store.SetRetentionPolicy(24*time.Hour, 24*time.Hour, "new-policy")
	now := settledAt.Add(2 * 24 * time.Hour)
	n, err := store.DeleteExpiredPayloads(ctx, now, 10)
	require.NoError(t, err)
	require.Zero(t, n, "a shorter current policy must not shorten a stored delivery deadline")
	n, err = store.DeleteSettledAttempts(ctx, now, 10)
	require.NoError(t, err)
	require.Zero(t, n, "attempt evidence follows the policy captured by the effect")

	newPlan := validPlan()
	newPlan.OccurrenceID, newPlan.ExecutionID = "occ-new-policy", "exec-new-policy"
	newEffect, _, err := store.PlanOnce(ctx, newPlan, base)
	require.NoError(t, err)
	newClaim, err := store.ClaimSend(ctx, claimFor(newEffect.EffectID, 0, base.Add(time.Hour)))
	require.NoError(t, err)
	require.NoError(t, store.CompleteSend(ctx, completeFor(newClaim, settledAt)))
	n, err = store.DeleteExpiredPayloads(ctx, now, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "subsequent effects use the new shorter policy")
	_, err = store.GetPayload(ctx, old.PayloadID)
	require.NoError(t, err)
	_, err = store.GetPayload(ctx, newEffect.PayloadID)
	require.ErrorIs(t, err, effect.ErrPayloadNotFound)
	n, err = store.DeleteSettledAttempts(ctx, now, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	n, err = store.DeleteExpiredPayloads(ctx, settledAt.Add(7*24*time.Hour), 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	n, err = store.DeleteSettledAttempts(ctx, settledAt.Add(7*24*time.Hour), 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}
