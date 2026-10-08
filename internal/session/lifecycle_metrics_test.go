package session

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLifecycleExpirationLagUsesLatestRequiredDeadline(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	conversationDeadline := now.Add(-2 * time.Hour)
	contentDeadline := now.Add(-time.Hour)

	lag := lifecycleExpirationLag(&SessionInfo{
		ConversationExpiresAt: &conversationDeadline,
		LastContentExpiresAt:  &contentDeadline,
	}, now)

	require.Equal(t, time.Hour, lag)
}

func TestLifecycleGCStatusUpdatesBacklogAndRaisesMaxLagState(t *testing.T) {
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	statusStore := &staticLifecycleGCStatusStore{
		status: lifecycleGCStatus{
			eligible:             4,
			blocked:              2,
			unknownExecutionHold: 1,
			eligibleLag:          2 * time.Hour,
			blockedLag:           30 * time.Minute,
		},
	}
	manager := &Manager{
		store: statusStore,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	manager.refreshLifecycleGCStatus(context.Background(), now, time.Hour)
	require.True(t, manager.lifecycleGCLagExceeded.Load())
	require.EqualValues(t, 4, lifecycleGCGauges.eligibleBacklog.Load())
	require.EqualValues(t, 2, lifecycleGCGauges.blockedBacklog.Load())
	require.EqualValues(t, 1, lifecycleGCGauges.unknownExecutionHold.Load())
	require.Equal(t, int64(2*time.Hour), lifecycleGCGauges.eligibleLagNanos.Load())
	require.Equal(t, int64(30*time.Minute), lifecycleGCGauges.blockedLagNanos.Load())

	statusStore.status = lifecycleGCStatus{}
	manager.refreshLifecycleGCStatus(context.Background(), now, time.Hour)
	require.False(t, manager.lifecycleGCLagExceeded.Load())
	require.Zero(t, lifecycleGCGauges.eligibleBacklog.Load())
	require.Zero(t, lifecycleGCGauges.blockedBacklog.Load())
	require.Zero(t, lifecycleGCGauges.unknownExecutionHold.Load())
}

type staticLifecycleGCStatusStore struct {
	Store
	status lifecycleGCStatus
}

func (s *staticLifecycleGCStatusStore) GetLifecycleGCStatus(context.Context, time.Time) (lifecycleGCStatus, error) {
	return s.status, nil
}
