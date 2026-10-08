package effect_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDeleteSettledAttemptsKeepsUnresolvedEvidenceAndIdempotency(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	settledPlan := validPlan()
	settledPlan.OccurrenceID = "occ-settled"
	settledPlan.ExecutionID = "exec-settled"
	settled, _, err := store.PlanOnce(ctx, settledPlan, base)
	require.NoError(t, err)
	settledAt := base.Add(2 * time.Hour)
	claim, err := store.ClaimSend(ctx, claimFor(settled.EffectID, 0, base.Add(time.Hour)))
	require.NoError(t, err)
	require.NoError(t, store.CompleteSend(ctx, completeFor(claim, settledAt)))

	unknownPlan := validPlan()
	unknownPlan.OccurrenceID = "occ-unknown"
	unknownPlan.ExecutionID = "exec-unknown"
	unknown, _, err := store.PlanOnce(ctx, unknownPlan, base)
	require.NoError(t, err)
	unknownClaimAt := base.Add(3 * time.Hour)
	_, err = store.ClaimSend(ctx, claimFor(unknown.EffectID, 0, unknownClaimAt))
	require.NoError(t, err)
	expired, err := store.ExpireLeases(ctx, unknownClaimAt.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)

	activePlan := validPlan()
	activePlan.OccurrenceID = "occ-active"
	activePlan.ExecutionID = "exec-active"
	active, _, err := store.PlanOnce(ctx, activePlan, base)
	require.NoError(t, err)
	_, err = store.ClaimSend(ctx, claimFor(active.EffectID, 0, base.Add(5*time.Hour)))
	require.NoError(t, err)

	legacyPlan := validPlan()
	legacyPlan.OccurrenceID = "occ-legacy"
	legacyPlan.ExecutionID = "exec-legacy"
	legacy, _, err := store.PlanOnce(ctx, legacyPlan, base)
	require.NoError(t, err)
	legacyClaim, err := store.ClaimSend(ctx, claimFor(legacy.EffectID, 0, base.Add(6*time.Hour)))
	require.NoError(t, err)
	require.NoError(t, store.CompleteSend(ctx, completeFor(legacyClaim, base.Add(7*time.Hour))))
	_, err = db.Exec(`UPDATE effects SET settled_at = NULL WHERE effect_id = ?`, legacy.EffectID)
	require.NoError(t, err)

	openAttemptPlan := validPlan()
	openAttemptPlan.OccurrenceID = "occ-open-attempt"
	openAttemptPlan.ExecutionID = "exec-open-attempt"
	openAttempt, _, err := store.PlanOnce(ctx, openAttemptPlan, base)
	require.NoError(t, err)
	_, err = store.ClaimSend(ctx, claimFor(openAttempt.EffectID, 0, base.Add(8*time.Hour)))
	require.NoError(t, err)
	// Guard against inconsistent or legacy terminal rows that still have an
	// attempt open: the detailed evidence must not be partially discarded.
	_, err = db.Exec(`UPDATE effects
		SET status = 'delivered', settled_at = ?, owner_instance_id = '', lease_until = NULL
		WHERE effect_id = ?`, settledAt.UnixMilli(), openAttempt.EffectID)
	require.NoError(t, err)

	n, err := store.DeleteSettledAttempts(ctx, settledAt.Add(-time.Nanosecond), 10)
	require.NoError(t, err)
	require.Zero(t, n, "attempt evidence must remain until the full post-settlement retention elapses")

	cutoff := base.Add(400 * 24 * time.Hour)
	n, err = store.DeleteSettledAttempts(ctx, cutoff, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	attempts, err := store.ListAttempts(ctx, settled.EffectID)
	require.NoError(t, err)
	require.Empty(t, attempts)
	_, err = store.GetByKey(ctx, settledPlan.OccurrenceID, settledPlan.DeliveryOrdinal, settledPlan.TargetRevision)
	require.NoError(t, err, "the effect identity remains to prevent duplicate delivery")

	for _, effectID := range []string{
		unknown.EffectID, active.EffectID, legacy.EffectID, openAttempt.EffectID,
	} {
		attempts, err := store.ListAttempts(ctx, effectID)
		require.NoError(t, err)
		require.Len(t, attempts, 1, "unresolved, active, or legacy evidence without a settlement clock is protected")
	}
}
