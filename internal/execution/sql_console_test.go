package execution

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/pkg/events"
)

// acceptN records n executions for one session, completing each before the
// next: the single-active index means only one may be pending at a time.
func acceptN(t *testing.T, store *SQLStore, sessionID string, n int) []*Record {
	t.Helper()
	ctx := context.Background()
	var out []*Record
	for i := 0; i < n; i++ {
		rec, dup, err := store.Accept(ctx, AcceptRequest{
			SessionID:       sessionID,
			ClientMessageID: fmt.Sprintf("cm-%03d", i),
			PayloadHash:     fmt.Sprintf("hash-%03d", i),
			OwnerInstanceID: testOwner,
			WorkerRunID:     testRun,
		})
		require.NoError(t, err)
		require.False(t, dup)
		require.NoError(t, store.FinishRuntime(ctx, rec.ExecutionID, testRun, RuntimeCompleted, ""))
		out = append(out, rec)
	}
	return out
}

func TestByID_ReturnsTheRecord(t *testing.T) {
	t.Parallel()
	store, _ := newTestSQLStore(t)
	created := acceptN(t, store, "session-1", 1)

	got, err := store.ByID(context.Background(), created[0].ExecutionID)

	require.NoError(t, err)
	require.Equal(t, created[0].ExecutionID, got.ExecutionID)
	require.Equal(t, "session-1", got.SessionID)
	require.Equal(t, RuntimeCompleted, got.RuntimeStatus)
}

// The detail view must be able to say "no such run" rather than returning an
// empty projection that reads like a run with no history.
func TestByID_UnknownIsNotFound(t *testing.T) {
	t.Parallel()
	store, _ := newTestSQLStore(t)

	_, err := store.ByID(context.Background(), "exec-does-not-exist")

	require.ErrorIs(t, err, ErrNotFound)
}

func TestByID_RequiresAnID(t *testing.T) {
	t.Parallel()
	store, _ := newTestSQLStore(t)

	_, err := store.ByID(context.Background(), "")

	require.Error(t, err)
}

func TestListRecent_NewestFirst(t *testing.T) {
	t.Parallel()
	store, _ := newTestSQLStore(t)
	acceptN(t, store, "session-1", 5)

	got, err := store.ListRecent(context.Background(), ListFilter{Limit: 10})

	require.NoError(t, err)
	require.Len(t, got, 5)
	for i := 0; i+1 < len(got); i++ {
		require.GreaterOrEqual(t, got[i].CreatedAt, got[i+1].CreatedAt,
			"records must come back newest-first")
	}
}

// An unbounded request must not be able to ask for the whole table.
func TestListRecent_BoundsAnUnboundedRequest(t *testing.T) {
	t.Parallel()
	store, _ := newTestSQLStore(t)
	acceptN(t, store, "session-1", 3)

	got, err := store.ListRecent(context.Background(), ListFilter{})

	require.NoError(t, err)
	require.LessOrEqual(t, len(got), defaultListLimit)

	capped, err := store.ListRecent(context.Background(), ListFilter{Limit: 100000})
	require.NoError(t, err)
	require.LessOrEqual(t, len(capped), maxListLimit)
}

func TestListRecent_Filters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, sessions := newTestSQLStore(t)
	acceptN(t, store, "session-1", 3)

	// The executions table references the session, so a second session has to
	// actually exist before its executions can be recorded.
	now := time.Now()
	require.NoError(t, sessions.Upsert(ctx, &session.SessionInfo{
		ID: "session-2", UserID: "user-1", WorkerType: "claude_code",
		State: events.StateRunning, CreatedAt: now, UpdatedAt: now,
	}))

	other, _, err := store.Accept(ctx, AcceptRequest{
		SessionID:       "session-2",
		ClientMessageID: "cm-other",
		PayloadHash:     "hash-other",
		OwnerInstanceID: testOwner,
		WorkerRunID:     testRun,
	})
	require.NoError(t, err)
	require.NoError(t, store.FinishRuntime(ctx, other.ExecutionID, testRun, RuntimeFailed, "boom"))

	bySession, err := store.ListRecent(ctx, ListFilter{SessionID: "session-2"})
	require.NoError(t, err)
	require.Len(t, bySession, 1)
	require.Equal(t, "session-2", bySession[0].SessionID)

	byRuntime, err := store.ListRecent(ctx, ListFilter{RuntimeStatus: RuntimeFailed})
	require.NoError(t, err)
	require.Len(t, byRuntime, 1)
	require.Equal(t, other.ExecutionID, byRuntime[0].ExecutionID)

	byDelivery, err := store.ListRecent(ctx, ListFilter{DeliveryStatus: StatusAccepted})
	require.NoError(t, err)
	require.Len(t, byDelivery, 4)

	none, err := store.ListRecent(ctx, ListFilter{SessionID: "session-nope"})
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestListRecent_TimeWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	created := acceptN(t, store, "session-1", 2)
	pivot := created[0].CreatedAt

	after, err := store.ListRecent(ctx, ListFilter{SinceMs: pivot})
	require.NoError(t, err)
	require.Len(t, after, 2)

	before, err := store.ListRecent(ctx, ListFilter{UntilMs: pivot})
	require.NoError(t, err)
	require.Empty(t, before, "UntilMs is exclusive, so nothing created at the pivot qualifies")

	window, err := store.ListRecent(ctx, ListFilter{SinceMs: pivot, UntilMs: pivot + 1})
	require.NoError(t, err)
	require.Len(t, window, 1)
}

// Offset pagination would shift under concurrent inserts and make a console
// page silently skip or repeat rows. The keyset cursor must return each record
// exactly once across pages.
func TestListRecent_KeysetCursorHasNoGapOrOverlap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _ := newTestSQLStore(t)
	const total = 12
	all := acceptN(t, store, "session-1", total)

	seen := map[string]int{}
	pageSize := 5
	cursorCreated, cursorID := int64(0), ""

	for pages := 0; pages < 10; pages++ {
		page, err := store.ListRecent(ctx, ListFilter{
			Limit:             pageSize,
			BeforeCreatedAt:   cursorCreated,
			BeforeExecutionID: cursorID,
		})
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, rec := range page {
			seen[rec.ExecutionID]++
		}
		last := page[len(page)-1]
		cursorCreated, cursorID = last.CreatedAt, last.ExecutionID
		if len(page) < pageSize {
			break
		}
	}

	require.Len(t, seen, total, "every record must be returned exactly once")
	for id, n := range seen {
		require.Equal(t, 1, n, "record %s appeared %d times", id, n)
	}
	for _, rec := range all {
		require.Contains(t, seen, rec.ExecutionID)
	}
}

func TestListRecent_RespectsLimit(t *testing.T) {
	t.Parallel()
	store, _ := newTestSQLStore(t)
	acceptN(t, store, "session-1", 6)

	got, err := store.ListRecent(context.Background(), ListFilter{Limit: 2})

	require.NoError(t, err)
	require.Len(t, got, 2)
}

// A console session id is not a clock: the store must not invent one.
func TestListRecent_DoesNotDependOnWallClock(t *testing.T) {
	t.Parallel()
	store, _ := newTestSQLStore(t)
	acceptN(t, store, "session-1", 2)

	first, err := store.ListRecent(context.Background(), ListFilter{Limit: 10})
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	second, err := store.ListRecent(context.Background(), ListFilter{Limit: 10})
	require.NoError(t, err)

	require.Equal(t, len(first), len(second))
}
