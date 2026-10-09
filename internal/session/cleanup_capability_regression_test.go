package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

func TestCleanupRunner_ReportsWorkerCleanupCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		registerCleaner      bool
		failFirstAttempt     bool
		repeatUnsupported    bool
		wantFirstItemStatus  PurgeItemState
		wantFirstJobStatus   PurgeJobState
		wantFirstAttempts    int
		wantFirstErrorCode   string
		wantFinalItemStatus  PurgeItemState
		wantFinalJobStatus   PurgeJobState
		wantFinalAttempts    int
		wantFinalErrorCode   string
		wantCleanupCalls     int32
		wantCleanupTaskCount int
	}{
		{
			name:                 "unregistered worker is unsupported and does not retry",
			repeatUnsupported:    true,
			wantFirstItemStatus:  PurgeItemUnsupported,
			wantFirstJobStatus:   PurgeJobBlocked,
			wantFirstAttempts:    1,
			wantFirstErrorCode:   "cleanup_unsupported",
			wantFinalItemStatus:  PurgeItemUnsupported,
			wantFinalJobStatus:   PurgeJobBlocked,
			wantFinalAttempts:    1,
			wantFinalErrorCode:   "cleanup_unsupported",
			wantCleanupCalls:     0,
			wantCleanupTaskCount: 0,
		},
		{
			name:                 "registered worker failure retries and later completes",
			registerCleaner:      true,
			failFirstAttempt:     true,
			wantFirstItemStatus:  PurgeItemRetrying,
			wantFirstJobStatus:   PurgeJobInProgress,
			wantFirstAttempts:    1,
			wantFirstErrorCode:   "cleanup_failed",
			wantFinalItemStatus:  PurgeItemComplete,
			wantFinalJobStatus:   PurgeJobInProgress,
			wantFinalAttempts:    2,
			wantCleanupCalls:     2,
			wantCleanupTaskCount: 0,
		},
		{
			name:                 "registered worker success completes once",
			registerCleaner:      true,
			wantFirstItemStatus:  PurgeItemComplete,
			wantFirstJobStatus:   PurgeJobInProgress,
			wantFirstAttempts:    1,
			wantFinalItemStatus:  PurgeItemComplete,
			wantFinalJobStatus:   PurgeJobInProgress,
			wantFinalAttempts:    1,
			wantCleanupCalls:     1,
			wantCleanupTaskCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store, _ := helperDB(t)
			now := time.Now()
			workerType := worker.WorkerType("cleanup-capability-regression-" + uuid.NewString())
			sessionID := "session-cleanup-capability-" + uuid.NewString()
			workerSessionID := "native-session-" + uuid.NewString()
			info := &SessionInfo{
				ID:              sessionID,
				UserID:          "cleanup-capability-user",
				WorkerType:      workerType,
				WorkerSessionID: workerSessionID,
				State:           events.StateTerminated,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			require.NoError(t, store.Upsert(ctx, info))
			info.State = events.StateDeleted
			require.NoError(t, store.MarkDeletedWithCleanup(ctx, info))

			var cleanupCalls atomic.Int32
			if tt.registerCleaner {
				worker.RegisterSessionCleanup(workerType, func(_ context.Context, gotSessionID string) error {
					require.Equal(t, workerSessionID, gotSessionID)
					if cleanupCalls.Add(1) == 1 && tt.failFirstAttempt {
						return errors.New("remote cleanup failure")
					}
					return nil
				})
			}

			runner := NewCleanupRunner(nil, store, worker.CleanupSession)
			runAt := now.Add(time.Second)
			runner.now = func() time.Time { return runAt }
			runner.RunOnce(ctx)

			status, err := store.GetPurgeStatus(ctx, sessionID, info.UserID)
			require.NoError(t, err)
			require.Equal(t, tt.wantFirstJobStatus, status.Status)
			requirePurgeItem(t, status, PurgeItemWorkerSession, tt.wantFirstItemStatus, tt.wantFirstAttempts, tt.wantFirstErrorCode)

			if tt.repeatUnsupported {
				runner.RunOnce(ctx)
				require.NoError(t, store.MarkDeletedWithCleanup(ctx, info))
				requireCleanupTaskCount(t, store, sessionID, 0,
					"repeating soft deletion must not recreate a terminal unsupported cleanup task")
				deleted, err := store.DeletePhysicalWithCleanup(ctx, sessionID)
				require.NoError(t, err)
				require.NotNil(t, deleted)
				requireCleanupTaskCount(t, store, sessionID, 0,
					"physical deletion must not recreate a terminal unsupported cleanup task")
			} else if tt.failFirstAttempt {
				runAt = runAt.Add(2 * time.Second)
				runner.RunOnce(ctx)
			}

			status, err = store.GetPurgeStatus(ctx, sessionID, info.UserID)
			require.NoError(t, err)
			require.Equal(t, tt.wantFinalJobStatus, status.Status)
			requirePurgeItem(t, status, PurgeItemWorkerSession, tt.wantFinalItemStatus, tt.wantFinalAttempts, tt.wantFinalErrorCode)
			require.Equal(t, tt.wantCleanupCalls, cleanupCalls.Load())

			requireCleanupTaskCount(t, store, sessionID, tt.wantCleanupTaskCount, "unexpected durable cleanup task count")
		})
	}
}

func requireCleanupTaskCount(t *testing.T, store *SQLiteStore, sessionID string, want int, message string) {
	t.Helper()
	var taskCount int
	require.NoError(t, store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM session_cleanup_tasks WHERE session_id = ?`, sessionID,
	).Scan(&taskCount))
	require.Equal(t, want, taskCount, message)
}

func TestUnsupportedCleanupTask_StaleLeaseIsFenced(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := helperDB(t)
	now := time.Now()
	info := &SessionInfo{
		ID:              "session-cleanup-stale-lease-" + uuid.NewString(),
		UserID:          "cleanup-capability-user",
		WorkerType:      worker.WorkerType("unregistered-cleanup-" + uuid.NewString()),
		WorkerSessionID: "native-session-" + uuid.NewString(),
		State:           events.StateTerminated,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	require.NoError(t, store.Upsert(ctx, info))
	info.State = events.StateDeleted
	require.NoError(t, store.MarkDeletedWithCleanup(ctx, info))

	firstNow := now.Add(time.Second)
	firstLeaseUntil := firstNow.Add(time.Minute)
	first, err := store.ClaimCleanupTasks(ctx, firstNow, firstLeaseUntil, 1)
	require.NoError(t, err)
	require.Len(t, first, 1)

	reclaimedAt := firstLeaseUntil.Add(time.Second)
	second, err := store.ClaimCleanupTasks(ctx, reclaimedAt, reclaimedAt.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.NotEqual(t, first[0].LeaseToken, second[0].LeaseToken)

	require.ErrorIs(t, store.FailUnsupportedCleanupTask(ctx, first[0].ID, first[0].LeaseToken), ErrCleanupLeaseLost)
	status, err := store.GetPurgeStatus(ctx, info.ID, info.UserID)
	require.NoError(t, err)
	require.Equal(t, PurgeJobInProgress, status.Status)
	requirePurgeItem(t, status, PurgeItemWorkerSession, PurgeItemRunning, 2, "")
	var taskCount int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_cleanup_tasks WHERE session_id = ?`, info.ID,
	).Scan(&taskCount))
	require.Equal(t, 1, taskCount, "a stale lease must not delete the task owned by the current lease")

	require.NoError(t, store.FailUnsupportedCleanupTask(ctx, second[0].ID, second[0].LeaseToken))
	require.ErrorIs(t, store.FailUnsupportedCleanupTask(ctx, first[0].ID, first[0].LeaseToken), ErrCleanupLeaseLost)
	require.ErrorIs(t, store.CompleteCleanupTask(ctx, first[0].ID, first[0].LeaseToken), ErrCleanupLeaseLost)
	require.ErrorIs(t, store.RetryCleanupTask(ctx, first[0].ID, first[0].LeaseToken, reclaimedAt.Add(time.Minute), "stale failure"), ErrCleanupLeaseLost)

	status, err = store.GetPurgeStatus(ctx, info.ID, info.UserID)
	require.NoError(t, err)
	require.Equal(t, PurgeJobBlocked, status.Status)
	requirePurgeItem(t, status, PurgeItemWorkerSession, PurgeItemUnsupported, 2, "cleanup_unsupported")
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_cleanup_tasks WHERE session_id = ?`, info.ID,
	).Scan(&taskCount))
	require.Zero(t, taskCount, "stale cleanup results must not recreate the terminal task")
}
