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

func completeFor(claim *effect.Claim, now time.Time) effect.Completion {
	return effect.Completion{
		EffectID:     claim.Effect.EffectID,
		Attempt:      claim.Attempt,
		LeaseToken:   claim.LeaseToken,
		LeaseVersion: claim.LeaseVersion,
		Outcome:      effect.AttemptAccepted,
		ProviderRef:  "C123:1",
		Now:          now,
	}
}

func TestClaimSend_TakesExclusiveOwnership(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)
	now := time.Now()

	claim, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)
	require.Equal(t, effect.StatusStarted, claim.Effect.Status)
	require.Equal(t, "instance-a", claim.Effect.OwnerInstanceID)
	require.Equal(t, int64(1), claim.Effect.LeaseVersion, "claiming must fence the previous owner")
	require.Equal(t, int64(0), claim.Attempt)
	require.NotEmpty(t, claim.LeaseToken)
	require.NotNil(t, claim.Effect.LeaseUntilMs)

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

// TestCompleteSend_EffectStatusFollowsTheAttempt is the mapping that used to
// be left to callers: an outcome and a status could disagree, and that gap is
// where blind resends came from.
func TestCompleteSend_EffectStatusFollowsTheAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		completion    func(effect.Completion) effect.Completion
		wantStatus    effect.Status
		wantReattempt bool
	}{
		{
			name:       "accepted delivers",
			completion: func(c effect.Completion) effect.Completion { return c },
			wantStatus: effect.StatusDelivered,
		},
		{
			name: "a permanent rejection fails",
			completion: func(c effect.Completion) effect.Completion {
				c.Outcome = effect.AttemptRejected
				c.RejectionClass = "permanent"
				return c
			},
			wantStatus: effect.StatusFailed,
		},
		{
			name: "an unspecified rejection fails",
			completion: func(c effect.Completion) effect.Completion {
				c.Outcome = effect.AttemptRejected
				return c
			},
			wantStatus: effect.StatusFailed,
		},
		{
			name: "a safe rejection waits for a retry",
			completion: func(c effect.Completion) effect.Completion {
				c.Outcome = effect.AttemptRejected
				c.RejectionClass = "safe_retry"
				c.RetryAfter = 30 * time.Second
				return c
			},
			wantStatus:    effect.StatusStarted,
			wantReattempt: true,
		},
		{
			name: "an unprovable outcome ends as unknown",
			completion: func(c effect.Completion) effect.Completion {
				c.Outcome = effect.AttemptUnknown
				return c
			},
			wantStatus: effect.StatusUnknown,
		},
		{
			name: "a request that never left stays owed",
			completion: func(c effect.Completion) effect.Completion {
				c.Outcome = effect.AttemptNotSent
				return c
			},
			wantStatus: effect.StatusPlanned,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, ctx, planned := plannedEffect(t)
			now := time.Now()
			claim, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
			require.NoError(t, err)

			require.NoError(t, store.CompleteSend(ctx, tc.completion(completeFor(claim, now))))

			stored, err := store.GetByID(ctx, planned.EffectID)
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, stored.Status)
			require.Empty(t, stored.OwnerInstanceID, "a finished attempt holds no owner")
			require.Nil(t, stored.LeaseUntilMs, "a finished attempt holds no lease")

			attempts, err := store.ListAttempts(ctx, planned.EffectID)
			require.NoError(t, err)
			require.Len(t, attempts, 1)
			require.False(t, attempts[0].InFlight(), "the attempt must be closed")
		})
	}
}

// TestCompleteSend_ForgedTokenCannotWrite is what a guessed lease version
// cannot do: without the per-attempt token, a stale executor is powerless.
func TestCompleteSend_ForgedTokenCannotWrite(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)
	now := time.Now()
	claim, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)

	forged := completeFor(claim, now)
	forged.LeaseToken = "ltk_forged"
	forged.Outcome = effect.AttemptUnknown
	require.ErrorIs(t, store.CompleteSend(ctx, forged), effect.ErrLeaseLost)

	// The real owner still holds the effect.
	require.NoError(t, store.CompleteSend(ctx, completeFor(claim, now)))
	stored, err := store.GetByID(ctx, planned.EffectID)
	require.NoError(t, err)
	require.Equal(t, effect.StatusDelivered, stored.Status)
}

// TestCompleteSend_ReplayingAClosedAttemptFails stops a duplicate write from
// rewriting a receipt that is already recorded.
func TestCompleteSend_ReplayingAClosedAttemptFails(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)
	now := time.Now()
	claim, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)

	require.NoError(t, store.CompleteSend(ctx, completeFor(claim, now)))

	replay := completeFor(claim, now.Add(time.Minute))
	replay.Outcome = effect.AttemptUnknown
	require.ErrorIs(t, store.CompleteSend(ctx, replay), effect.ErrLeaseLost)

	stored, err := store.GetByID(ctx, planned.EffectID)
	require.NoError(t, err)
	require.Equal(t, effect.StatusDelivered, stored.Status)
	require.Equal(t, "C123:1", stored.ProviderRef)
}

func TestCompleteSend_RefusesUnknownOutcomes(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)
	now := time.Now()
	claim, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)

	bad := completeFor(claim, now)
	bad.Outcome = effect.AttemptOutcome("delivered")
	require.ErrorIs(t, store.CompleteSend(ctx, bad), effect.ErrNotClaimable)

	badClass := completeFor(claim, now)
	badClass.Outcome = effect.AttemptRejected
	badClass.RejectionClass = "maybe_later"
	require.ErrorIs(t, store.CompleteSend(ctx, badClass), effect.ErrNotClaimable)
}

// TestClaimRetry_OnlyAfterASafeRejection is the whole retry policy in one
// place: a new attempt requires an explicit safe refusal, an elapsed backoff
// and remaining budget.
func TestClaimRetry_OnlyAfterASafeRejection(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := context.Background()
	planned, _, err := store.PlanOnce(ctx, validPlan(), time.Now())
	require.NoError(t, err)

	now := time.Now()
	claim, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)

	// A first attempt that was merely claimed is not retryable.
	_, err = store.ClaimRetry(ctx, effect.RetryRequest{
		EffectID: planned.EffectID, OwnerInstanceID: "instance-a",
		LeaseUntil: now.Add(time.Minute), ExpectedAttempt: 0, MaxAttempts: 3, Now: now,
	})
	require.ErrorIs(t, err, effect.ErrLeaseLost)

	safeRetry := completeFor(claim, now)
	safeRetry.Outcome = effect.AttemptRejected
	safeRetry.RejectionClass = "safe_retry"
	safeRetry.RetryAfter = 30 * time.Second
	safeRetry.ProviderRef = ""
	require.NoError(t, store.CompleteSend(ctx, safeRetry))

	// The backoff has not elapsed yet.
	_, err = store.ClaimRetry(ctx, effect.RetryRequest{
		EffectID: planned.EffectID, OwnerInstanceID: "instance-a",
		LeaseUntil: now.Add(2 * time.Minute), ExpectedAttempt: 0, MaxAttempts: 3,
		Now: now.Add(10 * time.Second),
	})
	require.ErrorIs(t, err, effect.ErrLeaseLost)

	// Once it has, the next attempt opens on the same effect.
	retry, err := store.ClaimRetry(ctx, effect.RetryRequest{
		EffectID: planned.EffectID, OwnerInstanceID: "instance-a",
		LeaseUntil: now.Add(2 * time.Minute), ExpectedAttempt: 0, MaxAttempts: 3,
		Now: now.Add(31 * time.Second),
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), retry.Attempt, "a retry advances the attempt on the same effect")
	require.NotEqual(t, claim.LeaseToken, retry.LeaseToken, "each attempt gets its own token")

	attempts, err := store.ListAttempts(ctx, planned.EffectID)
	require.NoError(t, err)
	require.Len(t, attempts, 2, "one effect, two attempts — never two effects")
	require.Equal(t, effect.AttemptRejected, attempts[0].Outcome)
	require.Equal(t, "safe_retry", attempts[0].RejectionClass)
	require.True(t, attempts[1].InFlight())

	require.Equal(t, 1, countRows(t, db, "effects"))
}

func TestClaimRetry_RefusesAtTheAttemptCap(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)
	now := time.Now()
	claim, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)

	safeRetry := completeFor(claim, now)
	safeRetry.Outcome = effect.AttemptRejected
	safeRetry.RejectionClass = "safe_retry"
	safeRetry.RetryAfter = time.Second
	safeRetry.ProviderRef = ""
	require.NoError(t, store.CompleteSend(ctx, safeRetry))

	// A cap of one means the effect has already spent its only send.
	_, err = store.ClaimRetry(ctx, effect.RetryRequest{
		EffectID: planned.EffectID, OwnerInstanceID: "instance-a",
		LeaseUntil: now.Add(time.Minute), ExpectedAttempt: 0, MaxAttempts: 1,
		Now: now.Add(2 * time.Second),
	})
	require.ErrorIs(t, err, effect.ErrLeaseLost)
}

// TestExpireLeases_BecomeUnknownNotSendable is the safety property behind the
// lease: a lapsed lease proves nothing, so the effect must not become
// sendable again.
func TestExpireLeases_BecomeUnknownNotSendable(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)
	now := time.Now()
	_, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)

	// Before the lease elapses nothing is touched.
	expired, err := store.ExpireLeases(ctx, now.Add(10*time.Second), 10)
	require.NoError(t, err)
	require.Empty(t, expired)

	expired, err = store.ExpireLeases(ctx, now.Add(time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	require.Equal(t, planned.EffectID, expired[0].EffectID)

	stored, err := store.GetByID(ctx, planned.EffectID)
	require.NoError(t, err)
	require.Equal(t, effect.StatusUnknown, stored.Status)
	require.Equal(t, "lease_expired", stored.ErrorCode)

	recoverable, err := store.ListRecoverable(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, recoverable, "an uncertain effect must never be picked up as fresh work")

	attempts, err := store.ListAttempts(ctx, planned.EffectID)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	require.Equal(t, effect.AttemptUnknown, attempts[0].Outcome)
	require.False(t, attempts[0].InFlight())
}

func TestListRecoverable_ReturnsOnlyUnsentWork(t *testing.T) {
	t.Parallel()

	store, ctx, planned := plannedEffect(t)

	recoverable, err := store.ListRecoverable(ctx, 10)
	require.NoError(t, err)
	require.Len(t, recoverable, 1)
	require.Equal(t, planned.EffectID, recoverable[0].EffectID)

	now := time.Now()
	claim, err := store.ClaimSend(ctx, claimFor(planned.EffectID, 0, now))
	require.NoError(t, err)
	require.NoError(t, store.CompleteSend(ctx, completeFor(claim, now)))

	recoverable, err = store.ListRecoverable(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, recoverable, "a delivered effect is not recoverable work")
}

func TestRetryBudget_BackoffRespectsTheProviderHint(t *testing.T) {
	t.Parallel()

	budget := effect.DefaultRetryBudget()
	require.Equal(t, int64(3), budget.MaxAttempts)
	require.Equal(t, 30*time.Second, budget.Backoff(1, 0))
	require.Equal(t, time.Minute, budget.Backoff(2, 0))
	require.Equal(t, 2*time.Minute, budget.Backoff(3, 0))
	require.Equal(t, 5*time.Minute, budget.Backoff(9, 0), "backoff is capped")
	require.Equal(t, 90*time.Second, budget.Backoff(2, 90*time.Second),
		"a provider hint longer than our backoff wins")
}
