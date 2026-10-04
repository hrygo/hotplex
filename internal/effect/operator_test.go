package effect_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/effect"
)

// unknownEffect plans, claims and completes an effect into unknown.
func unknownEffect(t *testing.T) (*effect.SQLiteStore, context.Context, *effect.Effect) {
	t.Helper()
	store, ctx, planned := plannedEffect(t)
	now := time.Now()
	claim, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)
	require.NoError(t, store.CompleteSend(ctx, effect.Completion{
		EffectID:     claim.Effect.EffectID,
		Attempt:      claim.Attempt,
		LeaseToken:   claim.LeaseToken,
		LeaseVersion: claim.LeaseVersion,
		Outcome:      effect.AttemptUnknown,
		Reason:       "network failure",
		Now:          now,
	}))
	return store, ctx, planned
}

func TestOperatorAction_MovesAnUnknownEffect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		decision   effect.OperatorDecision
		wantStatus effect.Status
		wantCode   string
	}{
		{
			name:       "abandon fails it without inventing a provider outcome",
			decision:   effect.OperatorAbandon,
			wantStatus: effect.StatusFailed,
			wantCode:   "operator_abandoned",
		},
		{
			name:       "an operator can confirm a delivery they verified",
			decision:   effect.OperatorMarkDelivered,
			wantStatus: effect.StatusFailed,
			wantCode:   "operator_confirmed_delivered",
		},
		{
			name:       "requeue returns it to owed work",
			decision:   effect.OperatorRequeue,
			wantStatus: effect.StatusPlanned,
			wantCode:   "operator_requeued",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, ctx, planned := unknownEffect(t)
			updated, err := store.ApplyOperatorAction(ctx, effect.OperatorActionRequest{
				EffectID:       planned.EffectID,
				Decision:       tc.decision,
				ExpectedStatus: effect.StatusUnknown,
				Reason:         "checked the channel by hand",
				EvidenceRef:    "ticket-42",
				Now:            time.Now(),
			})
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, updated.Status)
			require.Equal(t, tc.wantCode, updated.ErrorCode)
			require.Contains(t, updated.Reason, "operator:",
				"an operator's words must never read as provider truth")
			require.Equal(t, "ticket-42", updated.EvidenceRef)
		})
	}
}

// TestOperatorAction_ConflictsWhenTheEffectMoved is what keeps an operator
// from overwriting a receipt that arrived while they were deciding.
func TestOperatorAction_ConflictsWhenTheEffectMoved(t *testing.T) {
	t.Parallel()

	store, ctx, planned := unknownEffect(t)

	// A stale view of the state is refused before any write.
	_, err := store.ApplyOperatorAction(ctx, effect.OperatorActionRequest{
		EffectID:       planned.EffectID,
		Decision:       effect.OperatorAbandon,
		ExpectedStatus: effect.StatusPlanned,
		Reason:         "stale view",
		Now:            time.Now(),
	})
	require.ErrorIs(t, err, effect.ErrNotClaimable)

	// Two operators racing on a genuinely unknown effect: the loser conflicts
	// instead of overwriting the winner.
	_, err = store.ApplyOperatorAction(ctx, effect.OperatorActionRequest{
		EffectID:       planned.EffectID,
		Decision:       effect.OperatorAbandon,
		ExpectedStatus: effect.StatusUnknown,
		Reason:         "first look",
		Now:            time.Now(),
	})
	require.NoError(t, err)

	_, err = store.ApplyOperatorAction(ctx, effect.OperatorActionRequest{
		EffectID:       planned.EffectID,
		Decision:       effect.OperatorRequeue,
		ExpectedStatus: effect.StatusUnknown,
		Reason:         "second look",
		Now:            time.Now(),
	})
	require.ErrorIs(t, err, effect.ErrLeaseLost)
}

func TestOperatorAction_RefusesNonUnknownAndMalformedRequests(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)

	tests := []struct {
		name string
		req  effect.OperatorActionRequest
	}{
		{
			name: "a planned effect is owned by the scheduler",
			req: effect.OperatorActionRequest{
				EffectID: planned.EffectID, Decision: effect.OperatorAbandon,
				ExpectedStatus: effect.StatusPlanned, Reason: "why",
			},
		},
		{
			name: "unknown decision",
			req: effect.OperatorActionRequest{
				EffectID: planned.EffectID, Decision: effect.OperatorDecision("obliterate"),
				ExpectedStatus: effect.StatusUnknown, Reason: "why",
			},
		},
		{
			name: "no reason",
			req: effect.OperatorActionRequest{
				EffectID: planned.EffectID, Decision: effect.OperatorAbandon,
				ExpectedStatus: effect.StatusUnknown,
			},
		},
		{
			name: "reason too long",
			req: effect.OperatorActionRequest{
				EffectID: planned.EffectID, Decision: effect.OperatorAbandon,
				ExpectedStatus: effect.StatusUnknown, Reason: string(make([]byte, 513)),
			},
		},
		{
			name: "evidence too long",
			req: effect.OperatorActionRequest{
				EffectID: planned.EffectID, Decision: effect.OperatorAbandon,
				ExpectedStatus: effect.StatusUnknown, Reason: "why",
				EvidenceRef: string(make([]byte, 257)),
			},
		},
		{
			name: "no effect id",
			req: effect.OperatorActionRequest{
				Decision: effect.OperatorAbandon, ExpectedStatus: effect.StatusUnknown,
				Reason: "why",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := store.ApplyOperatorAction(ctx, tc.req)
			require.ErrorIs(t, err, effect.ErrNotClaimable)
		})
	}
}

func TestListForOperator(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := context.Background()

	_, _, err := store.PlanOnce(ctx, validPlan(), time.Now())
	require.NoError(t, err)

	second := validPlan()
	second.TargetRevision = "rev-b"
	second.OccurrenceID = "occ-2"
	other, _, err := store.PlanOnce(ctx, second, time.Now())
	require.NoError(t, err)

	now := time.Now()
	claim, err := store.ClaimSend(ctx, claimFor(other.EffectID, 0, now))
	require.NoError(t, err)
	require.NoError(t, store.CompleteSend(ctx, effect.Completion{
		EffectID:     claim.Effect.EffectID,
		Attempt:      claim.Attempt,
		LeaseToken:   claim.LeaseToken,
		LeaseVersion: claim.LeaseVersion,
		Outcome:      effect.AttemptUnknown,
		Now:          now,
	}))

	all, err := store.ListForOperator(ctx, effect.OperatorListFilter{})
	require.NoError(t, err)
	require.Len(t, all, 2)

	unknownOnly, err := store.ListForOperator(ctx, effect.OperatorListFilter{
		Status: effect.StatusUnknown,
	})
	require.NoError(t, err)
	require.Len(t, unknownOnly, 1)
	require.Equal(t, other.EffectID, unknownOnly[0].EffectID)
	require.Equal(t, 2, countRows(t, db, "effects"))
}
