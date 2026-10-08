package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type purgeItemStoreStub struct {
	tasks       []PurgeItemTask
	claimErr    error
	completeErr error
	retryErr    error

	claimCalls    int
	completeCalls int
	retryCalls    int
}

func (s *purgeItemStoreStub) ClaimPurgeItems(context.Context, time.Time, time.Time, int) ([]PurgeItemTask, error) {
	s.claimCalls++
	return s.tasks, s.claimErr
}

func (s *purgeItemStoreStub) CompletePurgeItem(context.Context, string, string) error {
	s.completeCalls++
	return s.completeErr
}

func (s *purgeItemStoreStub) RetryPurgeItem(context.Context, string, string, time.Time, string) error {
	s.retryCalls++
	return s.retryErr
}

func TestPurgeItemRunner_RunOnceHandlesStoreFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		store             *purgeItemStoreStub
		execute           PurgeContentExecutor
		wantClaimCalls    int
		wantCompleteCalls int
		wantRetryCalls    int
	}{
		{
			name: "claim failure",
			store: &purgeItemStoreStub{
				claimErr: errors.New("database unavailable"),
			},
			execute:        func(context.Context, string) error { return nil },
			wantClaimCalls: 1,
		},
		{
			name: "completion failure",
			store: &purgeItemStoreStub{
				tasks:       []PurgeItemTask{{ID: "item-1", SessionID: "session-1", LeaseToken: "lease-1"}},
				completeErr: errors.New("database unavailable"),
			},
			execute:           func(context.Context, string) error { return nil },
			wantClaimCalls:    1,
			wantCompleteCalls: 1,
		},
		{
			name: "retry scheduling failure",
			store: &purgeItemStoreStub{
				tasks:    []PurgeItemTask{{ID: "item-1", SessionID: "session-1", LeaseToken: "lease-1", Attempts: 1}},
				retryErr: errors.New("database unavailable"),
			},
			execute:        func(context.Context, string) error { return errors.New("provider unavailable") },
			wantClaimCalls: 1,
			wantRetryCalls: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := NewPurgeItemRunner(nil, tt.store, tt.execute)
			runner.RunOnce(context.Background())

			require.Equal(t, tt.wantClaimCalls, tt.store.claimCalls)
			require.Equal(t, tt.wantCompleteCalls, tt.store.completeCalls)
			require.Equal(t, tt.wantRetryCalls, tt.store.retryCalls)
		})
	}
}

func TestPurgeItemRunner_RunReturnsOnCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var nilRunner *PurgeItemRunner
	nilRunner.Run(ctx)
	nilRunner.RunOnce(ctx)

	NewPurgeItemRunner(nil, nil, func(context.Context, string) error { return nil }).Run(ctx)
	NewPurgeItemRunner(nil, &purgeItemStoreStub{}, nil).Run(ctx)
	NewPurgeItemRunner(nil, &purgeItemStoreStub{}, nil).RunOnce(ctx)

	store, _ := helperDB(t)
	purgeExecuted := false
	NewPurgeItemRunner(nil, store, func(context.Context, string) error {
		purgeExecuted = true
		return nil
	}).Run(ctx)
	require.False(t, purgeExecuted, "a canceled runner must not execute purge work")
}

type purgeStatusUnavailableStore struct {
	Store
}

func TestManager_GetPurgeStatus(t *testing.T) {
	t.Parallel()

	store, _ := helperDB(t)
	manager := &Manager{store: store}
	_, err := manager.GetPurgeStatus(context.Background(), "missing-session", "user-1")
	require.ErrorIs(t, err, ErrPurgeNotFound)

	manager.store = purgeStatusUnavailableStore{Store: store}
	_, err = manager.GetPurgeStatus(context.Background(), "missing-session", "user-1")
	require.ErrorIs(t, err, ErrPurgeStatusNotReady)
}
