package effect_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/effect"
)

// #947: 晚到证据收敛同一 effect，不产生第二个 effect。
func TestReconcileUnknown_ConvergesSameEffect(t *testing.T) {
	t.Parallel()
	store, db := newStore(t)
	ctx := context.Background()

	planned, _, err := store.PlanOnce(ctx, validPlan(), time.Now())
	require.NoError(t, err)
	claim, err := store.ClaimSend(ctx, effect.ClaimRequest{
		EffectID: planned.EffectID, OwnerInstanceID: "i1",
		LeaseUntil: time.Now().Add(time.Minute), Now: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, store.CompleteSend(ctx, effect.Completion{
		EffectID: claim.Effect.EffectID, Attempt: claim.Attempt,
		LeaseToken: claim.LeaseToken, LeaseVersion: claim.LeaseVersion,
		Outcome: effect.AttemptUnknown, ErrorCode: "unproven",
		Reason: "response lost", Now: time.Now(),
	}))

	got, err := store.ReconcileUnknown(ctx, effect.ReconcileRequest{
		EffectID: planned.EffectID, Outcome: effect.ReconcileSucceeded,
		ExpectedStatus: effect.StatusUnknown,
		ProviderRef:    "msg-1", EvidenceRef: "lookup-1",
		Reason: "provider lookup confirmed", Now: time.Now(),
	})
	require.NoError(t, err)
	require.Equal(t, effect.StatusReconciledSucceeded, got.Status)
	require.Equal(t, "msg-1", got.ProviderRef)
	require.Equal(t, 1, countRows(t, db, "effects"), "reconcile must not create a second effect")

	// 终态不可再动：二次收敛、claim 都被拒绝。
	_, err = store.ReconcileUnknown(ctx, effect.ReconcileRequest{
		EffectID: planned.EffectID, Outcome: effect.ReconcileFailed,
		ExpectedStatus: effect.StatusUnknown, Reason: "late", Now: time.Now(),
	})
	require.ErrorIs(t, err, effect.ErrLeaseLost)
	_, err = store.ClaimSend(ctx, effect.ClaimRequest{
		EffectID: planned.EffectID, OwnerInstanceID: "i2",
		LeaseUntil: time.Now().Add(time.Minute), Now: time.Now(),
	})
	require.Error(t, err)
}

// #947: fence 后永不被 claim/retry/reconcile。
func TestFence_QuarantinesEffect(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()

	planned, _, err := store.PlanOnce(ctx, validPlan(), time.Now())
	require.NoError(t, err)
	claim, err := store.ClaimSend(ctx, effect.ClaimRequest{
		EffectID: planned.EffectID, OwnerInstanceID: "i1",
		LeaseUntil: time.Now().Add(time.Minute), Now: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, store.CompleteSend(ctx, effect.Completion{
		EffectID: claim.Effect.EffectID, Attempt: claim.Attempt,
		LeaseToken: claim.LeaseToken, LeaseVersion: claim.LeaseVersion,
		Outcome: effect.AttemptUnknown, Reason: "lost", Now: time.Now(),
	}))

	got, err := store.Fence(ctx, effect.FenceRequest{
		EffectID: planned.EffectID, ExpectedStatus: effect.StatusUnknown,
		Reason: "operator quarantine", Now: time.Now(),
	})
	require.NoError(t, err)
	require.Equal(t, effect.StatusFenced, got.Status)

	_, err = store.ReconcileUnknown(ctx, effect.ReconcileRequest{
		EffectID: planned.EffectID, Outcome: effect.ReconcileSucceeded,
		ExpectedStatus: effect.StatusUnknown, Reason: "too late", Now: time.Now(),
	})
	require.ErrorIs(t, err, effect.ErrLeaseLost)
}
