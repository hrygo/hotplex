package session

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

func ocsTerminatedInfo(id, remoteID string, now time.Time) *SessionInfo {
	return &SessionInfo{
		ID:              id,
		UserID:          "user-1",
		WorkerType:      worker.TypeOpenCodeSrv,
		WorkerSessionID: remoteID,
		State:           events.StateTerminated,
		CreatedAt:       now.Add(-time.Hour),
		UpdatedAt:       now.Add(-time.Hour),
	}
}

func TestSQLiteStore_RetentionCleanupTombstoneBlocksStaleUpsert(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := ocsTerminatedInfo("sess-retention-race", "ocs-old", now)
	require.NoError(t, store.Upsert(ctx, info))

	deleted, err := store.DeleteTerminated(ctx, now, now)
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	require.Equal(t, "ocs-old", deleted[0].WorkerSessionID)

	_, err = store.Get(ctx, info.ID)
	require.ErrorIs(t, err, ErrSessionCleanupPending)

	stale := *info
	stale.State = events.StateRunning
	stale.UpdatedAt = now.Add(time.Second)
	require.ErrorIs(t, store.Upsert(ctx, &stale), ErrSessionCleanupPending,
		"a resume snapshot must not recreate a retention-deleted OCS session")

	claimNow := time.Now().Add(time.Second)
	tasks, err := store.ClaimCleanupTasks(ctx, claimNow, claimNow.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, info.ID, tasks[0].SessionID)
	require.Equal(t, "ocs-old", tasks[0].WorkerSessionID)
}

func TestSQLiteStore_MarkDeletedEnqueuesCleanupTask(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := ocsTerminatedInfo("sess-logical-delete", "ocs-logical-delete", now)
	info.State = events.StateRunning
	require.NoError(t, store.Upsert(ctx, info))

	snapshot := agentspec.SnapshotFromSpec(agentspec.AgentSpec{
		Worker: agentspec.WorkerSpec{Type: string(worker.TypeOpenCodeSrv)},
		Policy: agentspec.PolicySpec{
			PermissionMode: worker.PermissionModeWorkspace,
			AllowedTools:   []string{"Read"},
		},
	})
	require.NoError(t, store.UpdateSpecSnapshot(ctx, info.ID, &snapshot))

	deleted := *info
	deleted.State = events.StateDeleted
	deleted.UpdatedAt = now.Add(time.Second)
	require.NoError(t, store.MarkDeletedWithCleanup(ctx, &deleted))

	stored, err := store.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, events.StateDeleted, stored.State)
	require.Nil(t, stored.SpecSnapshot,
		"deleted-session context must be scrubbed after the cleanup task captures its required identifiers")

	claimNow := time.Now().Add(time.Second)
	tasks, err := store.ClaimCleanupTasks(ctx, claimNow, claimNow.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, "ocs-logical-delete", tasks[0].WorkerSessionID)
}

func TestSQLiteStore_MarkDeletedCreatesPurgeStatus(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := ocsTerminatedInfo("sess-purge-status", "ocs-purge-status", now)
	info.OwnerID = "owner-1"
	info.WorkspaceID = "workspace-1"
	info.LifecyclePolicy = "v2"
	info.Title = "private title"
	info.WorkDir = "/private/project"
	info.Context = map[string]any{"prompt": "private prompt"}
	info.PlatformKey = map[string]string{"channel": "private-channel"}
	require.NoError(t, store.Upsert(ctx, info))

	deleted := *info
	deleted.State = events.StateDeleted
	deleted.UpdatedAt = now.Add(time.Second)
	require.NoError(t, store.MarkDeletedWithCleanup(ctx, &deleted))

	status, err := store.GetPurgeStatus(ctx, info.ID, info.UserID)
	require.NoError(t, err)
	require.Equal(t, info.ID, status.SessionID)
	require.Equal(t, PurgeJobInProgress, status.Status)
	require.NotEmpty(t, status.JobID)
	require.Equal(t, "workspace-1", status.WorkspaceID)
	require.Len(t, status.Items, 3)
	require.NotContains(t, fmt.Sprintf("%+v", status), "private prompt")

	items := make(map[string]PurgeItemStatus, len(status.Items))
	for _, item := range status.Items {
		items[item.Kind] = item
	}
	require.Equal(t, PurgeItemComplete, items["session_metadata"].Status)
	require.Equal(t, PurgeItemPending, items["conversation_content"].Status)
	require.Equal(t, PurgeItemPending, items["worker_session"].Status)
	_, err = store.GetPurgeStatus(ctx, info.ID, "another-user")
	require.ErrorIs(t, err, ErrPurgeNotFound)

	claimNow := now.Add(2 * time.Second)
	tasks, err := store.ClaimCleanupTasks(ctx, claimNow, claimNow.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	status, err = store.GetPurgeStatus(ctx, info.ID, info.UserID)
	require.NoError(t, err)
	requirePurgeItem(t, status, PurgeItemWorkerSession, PurgeItemRunning, 1, "")

	retryAt := claimNow.Add(time.Minute)
	require.NoError(t, store.RetryCleanupTask(ctx, tasks[0].ID, tasks[0].LeaseToken, retryAt, "private provider error"))
	var lastError string
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT last_error FROM session_cleanup_tasks WHERE session_id = ?`, info.ID,
	).Scan(&lastError))
	require.Equal(t, "cleanup_failed", lastError)
	require.NotContains(t, lastError, "private provider error")
	status, err = store.GetPurgeStatus(ctx, info.ID, info.UserID)
	require.NoError(t, err)
	requirePurgeItem(t, status, PurgeItemWorkerSession, PurgeItemRetrying, 1, "cleanup_failed")
	require.NotContains(t, fmt.Sprintf("%+v", status), "private provider error")

	tasks, err = store.ClaimCleanupTasks(ctx, retryAt, retryAt.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.NoError(t, store.CompleteCleanupTask(ctx, tasks[0].ID, tasks[0].LeaseToken))
	status, err = store.GetPurgeStatus(ctx, info.ID, info.UserID)
	require.NoError(t, err)
	requirePurgeItem(t, status, PurgeItemWorkerSession, PurgeItemComplete, 2, "")
	require.Equal(t, PurgeJobInProgress, status.Status,
		"the root purge stays in progress while conversation content still needs cleanup")

	stored, err := store.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Empty(t, stored.Title)
	require.Empty(t, stored.WorkDir)
	require.Nil(t, stored.Context)
	require.Nil(t, stored.PlatformKey)
	require.Empty(t, stored.WorkerSessionID,
		"the remote identifier must live only in the durable worker cleanup task")
}

func requirePurgeItem(t *testing.T, status *PurgeStatus, kind string, state PurgeItemState, attempts int, errorCode string) {
	t.Helper()
	for _, item := range status.Items {
		if item.Kind == kind {
			require.Equal(t, state, item.Status)
			require.Equal(t, attempts, item.Attempts)
			require.Equal(t, errorCode, item.ErrorCode)
			return
		}
	}
	require.Failf(t, "purge item missing", "kind %q not found in %+v", kind, status.Items)
}

func TestManager_DeleteCacheHitAndMissPersistTombstones(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	hot := &SessionInfo{
		ID:         "sess-delete-hot",
		UserID:     "user-1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateTerminated,
		CreatedAt:  now.Add(-time.Hour),
		UpdatedAt:  now.Add(-time.Hour),
	}
	cold := &SessionInfo{
		ID:         "sess-delete-cold",
		UserID:     "user-1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateTerminated,
		CreatedAt:  now.Add(-time.Hour),
		UpdatedAt:  now.Add(-time.Hour),
	}
	require.NoError(t, store.Upsert(ctx, hot))
	require.NoError(t, store.Upsert(ctx, cold))

	manager, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer manager.Close()

	_, err = manager.Get(ctx, hot.ID)
	require.NoError(t, err, "the first session is loaded into the manager cache")
	require.NoError(t, manager.Delete(ctx, hot.ID))
	require.NoError(t, manager.Delete(ctx, cold.ID))

	for _, id := range []string{hot.ID, cold.ID} {
		stored, err := store.Get(ctx, id)
		require.NoError(t, err, "delete must preserve the lifecycle root as a tombstone")
		require.Equal(t, events.StateDeleted, stored.State)
		require.NotNil(t, stored.DeletedAt)

		stale := *stored
		stale.State = events.StateRunning
		stale.DeletedAt = nil
		stale.UpdatedAt = now.Add(time.Hour)
		require.ErrorIs(t, store.Upsert(ctx, &stale), ErrSessionCleanupPending,
			"stale worker snapshots must not revive a deleted session")

		storedAfterStaleWrite, err := store.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, events.StateDeleted, storedAfterStaleWrite.State)
		require.NotNil(t, storedAfterStaleWrite.DeletedAt)
	}
}

type blockingSessionGetStore struct {
	Store
	CleanupTaskStore
	getStarted chan struct{}
	unblockGet chan struct{}
}

func (s *blockingSessionGetStore) Get(ctx context.Context, id string) (*SessionInfo, error) {
	close(s.getStarted)
	<-s.unblockGet
	return s.Store.Get(ctx, id)
}

func TestManager_DeleteColdCacheFencesReadsAndSequenceWriters(t *testing.T) {
	t.Parallel()
	base, _ := helperDB(t)
	ctx := context.Background()
	info := &SessionInfo{
		ID: "sess-delete-cold-fence", UserID: "user-1", WorkerType: worker.TypeClaudeCode,
		State: events.StateTerminated, CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Hour),
	}
	require.NoError(t, base.Upsert(ctx, info))

	store := &blockingSessionGetStore{
		Store:            base,
		CleanupTaskStore: base,
		getStarted:       make(chan struct{}),
		unblockGet:       make(chan struct{}),
	}
	manager, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer manager.Close()

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- manager.Delete(ctx, info.ID) }()
	<-store.getStarted

	require.False(t, manager.IsSeqActive(ctx, info.ID))
	_, err = manager.Get(ctx, info.ID)
	require.ErrorIs(t, err, ErrSessionCleanupPending)

	close(store.unblockGet)
	require.NoError(t, <-deleteDone)
	stored, err := base.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, events.StateDeleted, stored.State)
	require.NotNil(t, stored.DeletedAt)
}

func TestSQLiteStore_StaleUpsertCannotReviveLegacyDeletedRow(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := &SessionInfo{
		ID: "sess-legacy-deleted", UserID: "user-1", WorkerType: worker.TypeClaudeCode,
		State: events.StateTerminated, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}
	require.NoError(t, store.Upsert(ctx, info))
	_, err := store.db.ExecContext(ctx, `UPDATE sessions SET state = ?, deleted_at = NULL WHERE id = ?`, string(events.StateDeleted), info.ID)
	require.NoError(t, err)

	stale := *info
	stale.State = events.StateRunning
	stale.UpdatedAt = now.Add(time.Hour)
	require.ErrorIs(t, store.Upsert(ctx, &stale), ErrSessionCleanupPending)

	stored, err := store.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, events.StateDeleted, stored.State)
	require.Nil(t, stored.DeletedAt, "the legacy row remains protected even without a tombstone timestamp")
}

func TestCleanupRunner_RunReturnsOnCancellation(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := NewCleanupRunner(nil, store, func(context.Context, worker.WorkerType, string) error { return nil })
	runner.Run(ctx)
	require.True(t, isCleanupPendingError(ErrSessionCleanupPending))
	require.True(t, containsCleanupPending("session cleanup pending"))
}

func TestSQLiteStore_DeletePhysicalWithCleanupNotFound(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	deleted, err := store.DeletePhysicalWithCleanup(context.Background(), "missing-session")
	require.NoError(t, err)
	require.Nil(t, deleted)
}

func TestCleanupRunner_RetriesAndCompletesDurably(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := ocsTerminatedInfo("sess-outbox-retry", "ocs-retry", now)
	require.NoError(t, store.Upsert(ctx, info))
	_, err := store.DeletePhysicalWithCleanup(ctx, info.ID)
	require.NoError(t, err)

	var calls atomic.Int32
	runner := NewCleanupRunner(nil, store, func(_ context.Context, workerType worker.WorkerType, remoteID string) error {
		require.Equal(t, worker.TypeOpenCodeSrv, workerType)
		require.Equal(t, "ocs-retry", remoteID)
		if calls.Add(1) == 1 {
			return errors.New("ocs temporarily unavailable")
		}
		return nil
	})
	base := time.Now().Add(time.Second)
	runner.now = func() time.Time { return base }
	runner.RunOnce(ctx)

	var lastError string
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT last_error FROM session_cleanup_tasks WHERE session_id = ?`, info.ID,
	).Scan(&lastError))
	require.Equal(t, "cleanup_failed", lastError)
	require.NotContains(t, lastError, "ocs temporarily unavailable")

	tasks, err := store.ClaimCleanupTasks(ctx, base, base.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Empty(t, tasks, "failure must back off instead of losing or immediately re-leasing the task")

	runner.now = func() time.Time { return base.Add(2 * time.Second) }
	runner.RunOnce(ctx)
	require.EqualValues(t, 2, calls.Load())

	_, err = store.Get(ctx, info.ID)
	require.ErrorIs(t, err, ErrSessionNotFound)
	var count int
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_cleanup_tasks WHERE session_id = ?`, info.ID).Scan(&count))
	require.Zero(t, count)
}

func TestCleanupRunner_ReclaimsExpiredLeaseAfterRestart(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := ocsTerminatedInfo("sess-outbox-lease", "ocs-lease", now)
	require.NoError(t, store.Upsert(ctx, info))
	_, err := store.DeletePhysicalWithCleanup(ctx, info.ID)
	require.NoError(t, err)

	claimNow := time.Now().Add(time.Second)
	leaseUntil := claimNow.Add(time.Minute)
	leased, err := store.ClaimCleanupTasks(ctx, claimNow, leaseUntil, 1)
	require.NoError(t, err)
	require.Len(t, leased, 1)

	var calls atomic.Int32
	runner := NewCleanupRunner(nil, store, func(context.Context, worker.WorkerType, string) error {
		calls.Add(1)
		return nil
	})
	runner.now = func() time.Time { return leaseUntil.Add(time.Second) }
	runner.RunOnce(ctx)
	require.EqualValues(t, 1, calls.Load())
}

func TestPurgeItemRunner_RetriesAndCompletesConversationDurably(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := ocsTerminatedInfo("sess-content-purge", "", now)
	require.NoError(t, store.Upsert(ctx, info))
	info.State = events.StateDeleted
	require.NoError(t, store.MarkDeletedWithCleanup(ctx, info))

	var calls atomic.Int32
	runner := NewPurgeItemRunner(nil, store, func(_ context.Context, sessionID string) error {
		require.Equal(t, info.ID, sessionID)
		if calls.Add(1) == 1 {
			return errors.New("private content-store failure")
		}
		return nil
	})
	base := now.Add(time.Second)
	runner.now = func() time.Time { return base }
	runner.RunOnce(ctx)

	status, err := store.GetPurgeStatus(ctx, info.ID, info.UserID)
	require.NoError(t, err)
	require.Equal(t, PurgeJobInProgress, status.Status)
	requirePurgeItem(t, status, PurgeItemConversationContent, PurgeItemRetrying, 1, "cleanup_failed")
	require.NotContains(t, fmt.Sprintf("%+v", status), "private content-store failure")

	runner.now = func() time.Time { return base.Add(2 * time.Second) }
	runner.RunOnce(ctx)
	require.EqualValues(t, 2, calls.Load())

	status, err = store.GetPurgeStatus(ctx, info.ID, info.UserID)
	require.NoError(t, err)
	require.Equal(t, PurgeJobComplete, status.Status)
	requirePurgeItem(t, status, PurgeItemConversationContent, PurgeItemComplete, 2, "")
}

func TestPurgeItemLease_ReclaimsAfterRestartAndFencesStaleCompletion(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := ocsTerminatedInfo("sess-content-purge-lease", "", now)
	require.NoError(t, store.Upsert(ctx, info))
	info.State = events.StateDeleted
	require.NoError(t, store.MarkDeletedWithCleanup(ctx, info))

	firstNow := time.Now().Add(time.Second)
	firstLeaseUntil := firstNow.Add(time.Minute)
	first, err := store.ClaimPurgeItems(ctx, firstNow, firstLeaseUntil, 1)
	require.NoError(t, err)
	require.Len(t, first, 1)

	tooEarly, err := store.ClaimPurgeItems(ctx, firstLeaseUntil.Add(-time.Second), firstLeaseUntil.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Empty(t, tooEarly)

	reclaimedAt := firstLeaseUntil.Add(time.Second)
	second, err := store.ClaimPurgeItems(ctx, reclaimedAt, reclaimedAt.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.NotEqual(t, first[0].LeaseToken, second[0].LeaseToken)
	require.Equal(t, 2, second[0].Attempts)

	require.ErrorIs(t, store.CompletePurgeItem(ctx, first[0].ID, first[0].LeaseToken), ErrCleanupLeaseLost)
	require.ErrorIs(t, store.RetryPurgeItem(ctx, first[0].ID, first[0].LeaseToken, reclaimedAt.Add(time.Minute), "private error"), ErrCleanupLeaseLost)
	require.NoError(t, store.CompletePurgeItem(ctx, second[0].ID, second[0].LeaseToken))

	status, err := store.GetPurgeStatus(ctx, info.ID, info.UserID)
	require.NoError(t, err)
	require.Equal(t, PurgeJobComplete, status.Status)
	requirePurgeItem(t, status, PurgeItemConversationContent, PurgeItemComplete, 2, "")
}

func TestCleanupTask_ExpiredLeaseCannotCompleteOrRetryNewLease(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	now := time.Now()
	info := ocsTerminatedInfo("sess-outbox-fence", "ocs-fence", now)
	require.NoError(t, store.Upsert(ctx, info))
	_, err := store.DeletePhysicalWithCleanup(ctx, info.ID)
	require.NoError(t, err)

	firstNow := time.Now().Add(time.Second)
	firstLeaseUntil := firstNow.Add(time.Minute)
	first, err := store.ClaimCleanupTasks(ctx, firstNow, firstLeaseUntil, 1)
	require.NoError(t, err)
	require.Len(t, first, 1)

	secondNow := firstLeaseUntil.Add(time.Second)
	second, err := store.ClaimCleanupTasks(ctx, secondNow, secondNow.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.NotEqual(t, first[0].LeaseToken, second[0].LeaseToken)

	require.ErrorIs(t, store.CompleteCleanupTask(ctx, first[0].ID, first[0].LeaseToken), ErrCleanupLeaseLost)
	require.ErrorIs(t, store.RetryCleanupTask(ctx, first[0].ID, first[0].LeaseToken, secondNow.Add(time.Minute), "late failure"), ErrCleanupLeaseLost)
	require.NoError(t, store.CompleteCleanupTask(ctx, second[0].ID, second[0].LeaseToken))
}
