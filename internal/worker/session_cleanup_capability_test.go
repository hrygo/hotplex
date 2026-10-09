package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCleanupSessionCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		workerSessionID string
		registerCleaner bool
		cleanerError    error
		wantError       error
		wantErrorText   string
		wantCalls       int32
	}{
		{
			name:            "empty native session id is a no-op",
			workerSessionID: "",
		},
		{
			name:            "unregistered worker with native session id is unsupported",
			workerSessionID: "native-session-unsupported",
			wantError:       ErrSessionCleanupUnsupported,
		},
		{
			name:            "registered cleaner is invoked and its error is propagated",
			workerSessionID: "native-session-registered",
			registerCleaner: true,
			cleanerError:    errors.New("remote cleanup failed"),
			wantErrorText:   "remote cleanup failed",
			wantCalls:       1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workerType := WorkerType("cleanup-capability-test-" + uuid.NewString())
			var calls atomic.Int32
			if tt.registerCleaner {
				RegisterSessionCleanup(workerType, func(_ context.Context, sessionID string) error {
					calls.Add(1)
					require.Equal(t, tt.workerSessionID, sessionID)
					return tt.cleanerError
				})
			}

			err := CleanupSession(context.Background(), workerType, tt.workerSessionID)
			if tt.wantError != nil {
				require.ErrorIs(t, err, tt.wantError)
			} else if tt.wantErrorText != "" {
				require.EqualError(t, err, tt.wantErrorText)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.wantCalls, calls.Load())
		})
	}
}
