package execution

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestQueuePayload_RoundTripsBothForms proves the store keeps the two kinds of
// queued input distinct. A Skill queued behind a busy turn must still be a
// Skill when it is finally dispatched; storing its slash text and replaying it
// as a prompt would silently change what the user asked for.
func TestQueuePayload_RoundTripsBothForms(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)

	_, textEntry, _, err := store.AcceptQueued(ctx, QueuedRequest{
		SessionID:       "session-1",
		ClientMessageID: "text",
		PayloadHash:     "hash_text",
		Payload:         QueuedPayload{Content: "run the tests"},
	}, QueueLimits{})
	require.NoError(t, err)

	text, err := store.QueuePayload(ctx, textEntry.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, "run the tests", text.Content)
	require.Nil(t, text.Invocation, "an ordinary input has no invocation")

	_, skillEntry, _, err := store.AcceptQueued(ctx, QueuedRequest{
		SessionID:       "session-1",
		ClientMessageID: "skill",
		PayloadHash:     "hash_skill",
		Payload: QueuedPayload{Invocation: &QueuedInvocation{
			Name: "review",
			Args: "--staged",
			Path: "/skills/review/SKILL.md",
			Mode: "skill",
		}},
	}, QueueLimits{})
	require.NoError(t, err)

	skill, err := store.QueuePayload(ctx, skillEntry.ExecutionID)
	require.NoError(t, err)
	require.Empty(t, skill.Content, "a native invocation carries no prompt text")
	require.Equal(t, &QueuedInvocation{
		Name: "review",
		Args: "--staged",
		Path: "/skills/review/SKILL.md",
		Mode: "skill",
	}, skill.Invocation)
}

// TestQueuePayload_DiesWithItsControlFact is the retention invariant. Content
// must be removable exactly when the promise to dispatch it is removed, in both
// directions: no orphaned content that outlives a settled input, and no queued
// row whose content vanished while it still claims to be recoverable.
func TestQueuePayload_DiesWithItsControlFact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		empty func(t *testing.T, store *SQLStore) []*QueueEntry
	}{
		{
			name: "cancel",
			empty: func(t *testing.T, store *SQLStore) []*QueueEntry {
				entries := fillQueue(t, store, "session-1", 1)
				_, err := store.CancelQueued(context.Background(), entries[0].ExecutionID, QueueReasonCancelled)
				require.NoError(t, err)
				return entries
			},
		},
		{
			name: "clear",
			empty: func(t *testing.T, store *SQLStore) []*QueueEntry {
				entries := fillQueue(t, store, "session-1", 3)
				_, err := store.ClearQueue(context.Background(), "session-1", QueueReasonCancelled)
				require.NoError(t, err)
				return entries
			},
		},
		{
			name: "claim",
			empty: func(t *testing.T, store *SQLStore) []*QueueEntry {
				entries := fillQueue(t, store, "session-1", 1)
				_, _, err := store.ClaimQueued(context.Background(), ClaimQueuedRequest{
					SessionID:       "session-1",
					OwnerInstanceID: testOwner,
					WorkerRunID:     testRun,
				})
				require.NoError(t, err)
				return entries
			},
		},
		{
			name: "expire",
			empty: func(t *testing.T, store *SQLStore) []*QueueEntry {
				_, entry, _, err := store.AcceptQueued(context.Background(),
					queuedReq("session-1", "short"), QueueLimits{TTL: time.Hour})
				require.NoError(t, err)
				_, err = store.ExpireQueued(context.Background(), time.Now().Add(2*time.Hour), 10)
				require.NoError(t, err)
				return []*QueueEntry{entry}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, _ := newTestSQLStore(t)

			settled := tc.empty(t, store)

			for _, entry := range settled {
				_, err := store.QueuePayload(ctx, entry.ExecutionID)
				require.ErrorIs(t, err, ErrNotFound,
					"content must not outlive the queue row that promised it")
			}
		})
	}
}

// TestQueuePayload_SessionDeleteRemovesContent covers the cascade path, which
// no application code runs.
func TestQueuePayload_SessionDeleteRemovesContent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, sessionStore := newTestSQLStore(t)
	entries := fillQueue(t, store, "session-1", 2)

	var remaining int
	require.NoError(t, sessionStore.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM execution_queue_payloads`).Scan(&remaining))
	require.Equal(t, 2, remaining)

	_, err := sessionStore.DB().ExecContext(ctx, `DELETE FROM sessions WHERE id = 'session-1'`)
	require.NoError(t, err)

	require.NoError(t, sessionStore.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM execution_queue_payloads`).Scan(&remaining))
	require.Zero(t, remaining, "deleting a session must not leave its queued prompts behind")

	for _, entry := range entries {
		_, err := store.QueuePayload(ctx, entry.ExecutionID)
		require.ErrorIs(t, err, ErrNotFound)
	}
}

// TestQueuePayload_ReportsMissingContentSeparately is the honesty case. A
// queued row whose content is gone must not read as "not queued": the input is
// still recorded as recoverable, and the correct response is to settle it
// rather than dispatch an empty turn.
func TestQueuePayload_ReportsMissingContentSeparately(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, sessionStore := newTestSQLStore(t)
	entries := fillQueue(t, store, "session-1", 1)

	_, err := sessionStore.DB().ExecContext(ctx,
		`DELETE FROM execution_queue_payloads WHERE execution_id = ?`, entries[0].ExecutionID)
	require.NoError(t, err)

	_, err = store.QueuePayload(ctx, entries[0].ExecutionID)
	require.ErrorIs(t, err, ErrPayloadContentUnavailable)

	_, err = store.QueuePayload(ctx, "exec_never_queued")
	require.ErrorIs(t, err, ErrNotFound, "an input that was never queued is simply absent")
}

// TestQueuePayload_InvocationCountsAgainstTheSizeBound proves a Skill cannot
// smuggle unbounded arguments past the per-item byte limit.
func TestQueuePayload_InvocationCountsAgainstTheSizeBound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestSQLStore(t)

	_, _, _, err := store.AcceptQueued(ctx, QueuedRequest{
		SessionID:       "session-1",
		ClientMessageID: "huge-skill",
		PayloadHash:     "hash_huge",
		Payload: QueuedPayload{Invocation: &QueuedInvocation{
			Name: "review",
			Args: string(make([]byte, 4096)),
		}},
	}, QueueLimits{MaxPayloadBytes: 512})
	require.ErrorIs(t, err, ErrQueuePayloadTooLarge,
		"the byte bound must cover command arguments, not just prompt text")
}
