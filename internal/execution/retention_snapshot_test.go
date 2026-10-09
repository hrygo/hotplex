package execution

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFactsRetentionSnapshotSurvivesShorterPolicy(t *testing.T) {
	t.Parallel()
	store, sessions := newTestSQLStore(t)
	ctx := context.Background()
	store.SetRetentionPolicy(7*24*time.Hour, "old-policy")
	settledAt := time.Now().Add(-2 * 24 * time.Hour).Truncate(time.Millisecond)
	settle := func(messageID string) *Record {
		t.Helper()
		r, _, err := store.Accept(ctx, testAcceptReq("session-1", messageID, messageID+"-hash"))
		require.NoError(t, err)
		require.NoError(t, store.MarkRunning(ctx, r.ExecutionID, testOwner, testRun))
		require.NoError(t, store.SetDelivery(ctx, r.ExecutionID, testOwner, StatusDelivered, ""))
		require.NoError(t, store.FinishRuntime(ctx, r.ExecutionID, testRun, RuntimeCompleted, ""))
		_, err = sessions.DB().ExecContext(ctx, `UPDATE execution_inputs SET
			finished_at = ?, runtime_error_code = 'retained-detail' WHERE execution_id = ?`,
			settledAt.UnixMilli(), r.ExecutionID)
		require.NoError(t, err)
		return r
	}
	old := settle("old-policy-message")
	store.SetRetentionPolicy(24*time.Hour, "new-policy")
	n, err := store.CompactSettledFacts(ctx, time.Now(), 10)
	require.NoError(t, err)
	require.Zero(t, n, "a restart with shorter retention must protect old execution details")
	newRecord := settle("new-policy-message")
	n, err = store.CompactSettledFacts(ctx, time.Now(), 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	kept, err := store.getByID(ctx, old.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, "retained-detail", kept.RuntimeErrorCode)
	compacted, err := store.getByID(ctx, newRecord.ExecutionID)
	require.NoError(t, err)
	require.Empty(t, compacted.RuntimeErrorCode)
	n, err = store.CompactSettledFacts(ctx, settledAt.Add(7*24*time.Hour), 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}
