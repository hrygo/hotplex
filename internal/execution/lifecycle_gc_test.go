package execution

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCompactSettledFactsPreservesIdempotencyAndProtectsUnresolvedRows(t *testing.T) {
	t.Parallel()

	store, sessionStore := newTestSQLStore(t)
	ctx := context.Background()
	oldFinishedAt := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)

	settled, _, err := store.Accept(ctx, testAcceptReq("session-1", "message-settled", "payload-hash"))
	require.NoError(t, err)
	require.NoError(t, store.MarkRunning(ctx, settled.ExecutionID, testOwner, testRun))
	require.NoError(t, store.SetDelivery(ctx, settled.ExecutionID, testOwner, StatusDelivered, ""))
	require.NoError(t, store.FinishRuntime(ctx, settled.ExecutionID, testRun, RuntimeCompleted, ""))
	_, err = sessionStore.DB().ExecContext(ctx, `UPDATE execution_inputs SET
		created_at = ?, updated_at = ?, finished_at = ?, delivered_at = ?,
		error_code = 'old-delivery-error', owner_instance_id = 'old-owner',
		worker_run_id = 'old-run', lease_until = 123, runtime_error_code = 'old-runtime-error',
		started_at = ?, fence_created_at = ?, turn_started_at = ?, turn_deadline_at = ?,
		turn_policy_revision = 'old-policy'
		WHERE execution_id = ?`,
		oldFinishedAt.UnixMilli(), oldFinishedAt.UnixMilli(), oldFinishedAt.UnixMilli(),
		oldFinishedAt.UnixMilli(), oldFinishedAt.UnixMilli(), oldFinishedAt.UnixMilli(),
		oldFinishedAt.UnixMilli(), oldFinishedAt.Add(time.Hour).UnixMilli(), settled.ExecutionID)
	require.NoError(t, err)

	ensureSession(t, sessionStore, "session-unknown")
	unknown, _, err := store.Accept(ctx, testAcceptReq("session-unknown", "message-unknown", "unknown-hash"))
	require.NoError(t, err)
	require.NoError(t, store.MarkRunning(ctx, unknown.ExecutionID, testOwner, testRun))
	require.NoError(t, store.FinishRuntime(ctx, unknown.ExecutionID, testRun, RuntimeUnknown, ""))
	_, err = sessionStore.DB().ExecContext(ctx,
		`UPDATE execution_inputs SET finished_at = ? WHERE execution_id = ?`,
		oldFinishedAt.UnixMilli(), unknown.ExecutionID)
	require.NoError(t, err)

	ensureSession(t, sessionStore, "session-active")
	active, _, err := store.Accept(ctx, testAcceptReq("session-active", "message-active", "active-hash"))
	require.NoError(t, err)
	_, err = sessionStore.DB().ExecContext(ctx,
		`UPDATE execution_inputs SET created_at = ?, updated_at = ? WHERE execution_id = ?`,
		oldFinishedAt.UnixMilli(), oldFinishedAt.UnixMilli(), active.ExecutionID)
	require.NoError(t, err)

	ensureSession(t, sessionStore, "session-legacy")
	legacy, _, err := store.Accept(ctx, testAcceptReq("session-legacy", "message-legacy", "legacy-hash"))
	require.NoError(t, err)
	require.NoError(t, store.MarkRunning(ctx, legacy.ExecutionID, testOwner, testRun))
	require.NoError(t, store.SetDelivery(ctx, legacy.ExecutionID, testOwner, StatusDelivered, ""))
	require.NoError(t, store.FinishRuntime(ctx, legacy.ExecutionID, testRun, RuntimeCompleted, ""))
	_, err = sessionStore.DB().ExecContext(ctx, `UPDATE execution_inputs SET
		created_at = ?, updated_at = ?, finished_at = NULL, owner_instance_id = 'legacy-owner',
		worker_run_id = 'legacy-run' WHERE execution_id = ?`,
		oldFinishedAt.UnixMilli(), oldFinishedAt.UnixMilli(), legacy.ExecutionID)
	require.NoError(t, err)

	ensureSession(t, sessionStore, "session-late-convergence")
	lateConvergence, _, err := store.Accept(ctx,
		testAcceptReq("session-late-convergence", "message-late-convergence", "late-hash"))
	require.NoError(t, err)
	require.NoError(t, store.MarkRunning(ctx, lateConvergence.ExecutionID, testOwner, testRun))
	require.NoError(t, store.SetDelivery(ctx, lateConvergence.ExecutionID, testOwner, StatusFailed, "late-ack"))
	require.NoError(t,
		store.FinishRuntime(ctx, lateConvergence.ExecutionID, testRun, RuntimeCompleted, ""))
	_, err = sessionStore.DB().ExecContext(ctx, `UPDATE execution_inputs SET
		created_at = ?, updated_at = ?, finished_at = ?
		WHERE execution_id = ?`,
		oldFinishedAt.UnixMilli(), oldFinishedAt.UnixMilli(), oldFinishedAt.UnixMilli(),
		lateConvergence.ExecutionID)
	require.NoError(t, err)

	compacted, err := store.CompactSettledFacts(ctx, oldFinishedAt, 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, compacted, "the batch size bounds work")

	stored, err := store.getByClientMessage(ctx, "session-1", "message-settled")
	require.NoError(t, err)
	require.Equal(t, settled.ExecutionID, stored.ExecutionID)
	require.Equal(t, "payload-hash", stored.PayloadHash)
	require.Equal(t, StatusDelivered, stored.Status)
	require.Equal(t, RuntimeCompleted, stored.RuntimeStatus)
	require.Empty(t, stored.ErrorCode)
	require.Empty(t, stored.OwnerInstanceID)
	require.Equal(t, "old-run", stored.WorkerRunID,
		"the worker run ID remains to recognize late terminal events for this execution")
	require.Empty(t, stored.RuntimeErrorCode)
	require.Nil(t, stored.StartedAt)
	require.Nil(t, stored.TurnStartedAt)
	require.Nil(t, stored.TurnDeadlineAt)
	require.NotNil(t, stored.FinishedAt, "settlement clock remains available")

	duplicate, wasDuplicate, err := store.Accept(ctx, testAcceptReq("session-1", "message-settled", "payload-hash"))
	require.NoError(t, err)
	require.True(t, wasDuplicate, "compaction must not reopen the client idempotency key")
	require.Equal(t, settled.ExecutionID, duplicate.ExecutionID)

	for _, input := range []struct {
		sessionID string
		messageID string
	}{
		{sessionID: "session-unknown", messageID: "message-unknown"},
		{sessionID: "session-active", messageID: "message-active"},
		{sessionID: "session-legacy", messageID: "message-legacy"},
		{sessionID: "session-late-convergence", messageID: "message-late-convergence"},
	} {
		record, err := store.getByClientMessage(ctx, input.sessionID, input.messageID)
		require.NoError(t, err)
		require.NotEmpty(t, record.OwnerInstanceID, "unresolved, active, or no-clock rows remain unchanged")
		if input.messageID == "message-late-convergence" {
			require.Equal(t, StatusFailed, record.Status)
			require.Equal(t, RuntimeCompleted, record.RuntimeStatus)
		}
	}
}
