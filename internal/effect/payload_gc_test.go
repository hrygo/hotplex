package effect_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/effect"
)

func TestDeleteExpiredPayloadsWaitsForEveryDeliveryToSettle(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	firstPlan := validPlan()
	first, created, err := store.PlanOnce(ctx, firstPlan, base)
	require.NoError(t, err)
	require.True(t, created)

	secondPlan := firstPlan
	secondPlan.TargetRevision = "rev-b"
	second, created, err := store.PlanOnce(ctx, secondPlan, base.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, first.PayloadID, second.PayloadID)

	firstClaimAt := base.Add(time.Hour)
	firstClaim, err := store.ClaimSend(ctx, effect.ClaimRequest{
		EffectID: first.EffectID, OwnerInstanceID: "instance-a",
		LeaseUntil: firstClaimAt.Add(time.Hour), ExpectedAttempt: 0, Now: firstClaimAt,
	})
	require.NoError(t, err)
	firstSettledAt := base.Add(24 * time.Hour)
	require.NoError(t, store.CompleteSend(ctx, completeFor(firstClaim, firstSettledAt)))

	// The first target is complete, but the second still has a delivery
	// obligation. The shared snapshot must remain available for it.
	n, err := store.DeleteExpiredPayloads(ctx, base.Add(100*24*time.Hour), 10)
	require.NoError(t, err)
	require.Zero(t, n)
	_, err = store.GetPayload(ctx, first.PayloadID)
	require.NoError(t, err)

	secondClaimAt := base.Add(48 * time.Hour)
	secondClaim, err := store.ClaimSend(ctx, effect.ClaimRequest{
		EffectID: second.EffectID, OwnerInstanceID: "instance-b",
		LeaseUntil: secondClaimAt.Add(time.Hour), ExpectedAttempt: 0, Now: secondClaimAt,
	})
	require.NoError(t, err)
	secondSettledAt := base.Add(72 * time.Hour)
	require.NoError(t, store.CompleteSend(ctx, completeFor(secondClaim, secondSettledAt)))

	// Expiry starts at the latest settlement among all effects that share the
	// payload. Before that deadline, the snapshot is still recoverable.
	n, err = store.DeleteExpiredPayloads(ctx, secondSettledAt.Add(-time.Nanosecond), 10)
	require.NoError(t, err)
	require.Zero(t, n)

	// At the deadline, remove the body while retaining the idempotency row.
	n, err = store.DeleteExpiredPayloads(ctx, secondSettledAt, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = store.GetPayload(ctx, first.PayloadID)
	require.ErrorIs(t, err, effect.ErrPayloadNotFound)

	// A replay after expiry must not recreate the old body or produce a new
	// sendable effect under another target revision.
	replay := firstPlan
	replay.TargetRevision = "rev-c"
	_, _, err = store.PlanOnce(ctx, replay, base.Add(100*24*time.Hour))
	require.ErrorIs(t, err, effect.ErrPayloadNotFound)
}

func TestDeleteExpiredPayloadsPreservesUnknownAndLegacyTerminalEffects(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	planned, _, err := store.PlanOnce(ctx, validPlan(), base)
	require.NoError(t, err)

	claimAt := base.Add(time.Hour)
	_, err = store.ClaimSend(ctx, effect.ClaimRequest{
		EffectID: planned.EffectID, OwnerInstanceID: "instance-a",
		LeaseUntil: claimAt.Add(time.Minute), ExpectedAttempt: 0, Now: claimAt,
	})
	require.NoError(t, err)
	_, err = store.ExpireLeases(ctx, claimAt.Add(time.Hour), 10)
	require.NoError(t, err)

	// unknown is unresolved evidence, so age alone cannot delete its recovery
	// content.
	n, err := store.DeleteExpiredPayloads(ctx, base.Add(365*24*time.Hour), 10)
	require.NoError(t, err)
	require.Zero(t, n)
	_, err = store.GetPayload(ctx, planned.PayloadID)
	require.NoError(t, err)

	settledAt := base.Add(400 * 24 * time.Hour)
	_, err = store.ReconcileUnknown(ctx, effect.ReconcileRequest{
		EffectID: planned.EffectID, Outcome: effect.ReconcileFailed,
		ExpectedStatus: effect.StatusUnknown, Reason: "provider evidence confirmed no delivery",
		Now: settledAt,
	})
	require.NoError(t, err)

	// Terminal rows created before settled_at was introduced have no reliable
	// settlement clock. Keep their body until a separate migration preview
	// establishes a safe deadline.
	_, err = db.Exec(`UPDATE effects SET settled_at = NULL WHERE effect_id = ?`, planned.EffectID)
	require.NoError(t, err)
	n, err = store.DeleteExpiredPayloads(ctx, settledAt.Add(365*24*time.Hour), 10)
	require.NoError(t, err)
	require.Zero(t, n)
}
