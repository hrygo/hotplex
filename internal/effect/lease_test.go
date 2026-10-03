package effect_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/effect"
)

func plannedEffect(t *testing.T) (*effect.SQLiteStore, context.Context, *effect.Effect) {
	t.Helper()
	store, _ := newStore(t)
	ctx := context.Background()
	planned, _, err := store.PlanOnce(ctx, validPlan(), time.Now())
	require.NoError(t, err)
	return store, ctx, planned
}

func claimFor(effectID string, attempt int64, now time.Time) effect.ClaimRequest {
	return effect.ClaimRequest{
		EffectID:        effectID,
		OwnerInstanceID: "instance-a",
		LeaseUntil:      now.Add(30 * time.Second),
		ExpectedAttempt: attempt,
		Now:             now,
	}
}

func TestClaimSend_TakesExclusiveOwnership(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)
	now := time.Now()

	claimed, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)
	require.Equal(t, effect.StatusStarted, claimed.Status)
	require.Equal(t, "instance-a", claimed.OwnerInstanceID)
	require.Equal(t, int64(1), claimed.LeaseVersion, "claiming must fence the previous owner")
	require.NotNil(t, claimed.LeaseUntilMs)

	// A second instance recovering the same occurrence must not be able to
	// start its own send: only one owner may hold the effect.
	_, err = store.ClaimSend(ctx, effect.ClaimRequest{
		EffectID:        planned.EffectID,
		OwnerInstanceID: "instance-b",
		LeaseUntil:      now.Add(30 * time.Second),
		ExpectedAttempt: 0,
		Now:             now,
	})
	require.ErrorIs(t, err, effect.ErrLeaseLost)
}

func TestClaimSend_RefusesUnusableRequests(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)
	now := time.Now()

	tests := []struct {
		name string
		req  effect.ClaimRequest
	}{
		{
			name: "no owner",
			req: effect.ClaimRequest{
				EffectID: planned.EffectID, LeaseUntil: now.Add(time.Second), Now: now,
			},
		},
		{
			name: "no effect",
			req: effect.ClaimRequest{
				OwnerInstanceID: "instance-a", LeaseUntil: now.Add(time.Second), Now: now,
			},
		},
		{
			name: "lease already expired",
			req: effect.ClaimRequest{
				EffectID: planned.EffectID, OwnerInstanceID: "instance-a",
				LeaseUntil: now, Now: now,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := store.ClaimSend(ctx, tc.req)
			require.ErrorIs(t, err, effect.ErrNotClaimable)
		})
	}
}

func TestCompleteSend_RecordsTypedOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		outcome  effect.Status
		wantRefs bool
	}{
		{name: "delivered", outcome: effect.StatusDelivered, wantRefs: true},
		{name: "failed", outcome: effect.StatusFailed},
		{name: "unknown", outcome: effect.StatusUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, ctx, planned := plannedEffect(t)
			now := time.Now()
			claimed, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
			require.NoError(t, err)

			completion := effect.Completion{
				EffectID:     planned.EffectID,
				Attempt:      claimed.Attempt,
				LeaseVersion: claimed.LeaseVersion,
				Outcome:      tc.outcome,
				Reason:       "typed outcome",
				Now:          now.Add(time.Second),
			}
			if tc.wantRefs {
				completion.ProviderRef = "C123:1717171717.000100"
				completion.EvidenceRef = "ev-1"
			}
			require.NoError(t, store.CompleteSend(ctx, completion))

			stored, err := store.GetByID(ctx, planned.EffectID)
			require.NoError(t, err)
			require.Equal(t, tc.outcome, stored.Status)
			require.Empty(t, stored.OwnerInstanceID, "a finished effect holds no owner")
			require.Nil(t, stored.LeaseUntilMs, "a finished effect holds no lease")
			if tc.wantRefs {
				require.Equal(t, "C123:1717171717.000100", stored.ProviderRef)
				require.Equal(t, "ev-1", stored.EvidenceRef)
			} else {
				require.Empty(t, stored.ProviderRef, "an unaccepted send has no message reference")
			}
		})
	}
}

// TestCompleteSend_FencedWriterCannotOverwrite is the guarantee that makes a
// late or superseded executor harmless: it cannot rewrite history.
func TestCompleteSend_FencedWriterCannotOverwrite(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)
	now := time.Now()
	claimed, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)

	require.NoError(t, store.CompleteSend(ctx, effect.Completion{
		EffectID:     planned.EffectID,
		Attempt:      claimed.Attempt,
		LeaseVersion: claimed.LeaseVersion,
		Outcome:      effect.StatusDelivered,
		ProviderRef:  "C123:1",
		Now:          now,
	}))

	// The same owner replaying an older outcome, or a different attempt,
	// must not be able to replace the recorded receipt.
	err = store.CompleteSend(ctx, effect.Completion{
		EffectID:     planned.EffectID,
		Attempt:      claimed.Attempt,
		LeaseVersion: claimed.LeaseVersion,
		Outcome:      effect.StatusUnknown,
		Reason:       "stale writer",
		Now:          now.Add(time.Minute),
	})
	require.ErrorIs(t, err, effect.ErrLeaseLost)

	stored, err := store.GetByID(ctx, planned.EffectID)
	require.NoError(t, err)
	require.Equal(t, effect.StatusDelivered, stored.Status)
	require.Equal(t, "C123:1", stored.ProviderRef)
}

func TestCompleteSend_RefusesNonOutcomesAndStaleAttempts(t *testing.T) {
	t.Parallel()

	t.Run("planned is not an outcome", func(t *testing.T) {
		t.Parallel()
		store, ctx, planned := plannedEffect(t)
		now := time.Now()
		claimed, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
		require.NoError(t, err)

		err = store.CompleteSend(ctx, effect.Completion{
			EffectID:     planned.EffectID,
			Attempt:      claimed.Attempt,
			LeaseVersion: claimed.LeaseVersion,
			Outcome:      effect.StatusPlanned,
			Now:          now,
		})
		require.ErrorIs(t, err, effect.ErrNotClaimable)
	})

	t.Run("stale attempt cannot write", func(t *testing.T) {
		t.Parallel()
		store, ctx, planned := plannedEffect(t)
		now := time.Now()
		claimed, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
		require.NoError(t, err)

		err = store.CompleteSend(ctx, effect.Completion{
			EffectID:     planned.EffectID,
			Attempt:      claimed.Attempt + 1,
			LeaseVersion: claimed.LeaseVersion,
			Outcome:      effect.StatusDelivered,
			Now:          now,
		})
		require.ErrorIs(t, err, effect.ErrLeaseLost)
	})
}
