package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCleanupSession(t *testing.T) {
	const testType WorkerType = "cleanup-test-worker"
	called := false
	RegisterSessionCleanup(testType, func(_ context.Context, id string) error {
		called = true
		require.Equal(t, "worker-session-1", id)
		return nil
	})

	require.NoError(t, CleanupSession(context.Background(), testType, "worker-session-1"))
	require.True(t, called)
	require.NoError(t, CleanupSession(context.Background(), testType, ""))
	require.ErrorIs(t, CleanupSession(context.Background(), "unregistered-worker", "worker-session-1"), ErrSessionCleanupUnsupported)
}
