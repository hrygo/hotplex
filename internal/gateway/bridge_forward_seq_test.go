package gateway

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

// newCollectorBridgeForSeqTest creates a Bridge with a real Hub (hydrated to
// knownSeq) and a real eventstore Collector, so the collector-enabled seq path
// in processForwardedEvent is exercised.
func newCollectorBridgeForSeqTest(t *testing.T, sessionID string, knownSeq int64) (*Bridge, *eventstore.SQLiteStore) {
	t.Helper()

	hub := newTestHub(t)
	hub.seqGen.Init(sessionID, knownSeq)
	hub.seqGen.MarkHydrated(sessionID)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open(sqlutil.DriverName, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL,
		direction TEXT NOT NULL DEFAULT 'outbound',
		source TEXT NOT NULL DEFAULT 'normal'
			CHECK(source IN ('normal', 'crash', 'timeout', 'fresh_start')),
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL DEFAULT 0
	)`)
	require.NoError(t, err)
	createEventStoreSessionBarrierSchema(t, db)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS turns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		client_message_id TEXT,
		generation INTEGER NOT NULL DEFAULT 1,
		turn_num INTEGER NOT NULL,
		seq INTEGER NOT NULL DEFAULT 0,
		role TEXT NOT NULL,
		content TEXT NOT NULL DEFAULT '',
		platform TEXT NOT NULL DEFAULT '',
		user_id TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		success INTEGER,
		source TEXT NOT NULL DEFAULT 'normal',
		tools_json TEXT,
		tool_count INTEGER NOT NULL DEFAULT 0,
		tokens_input INTEGER NOT NULL DEFAULT 0,
		tokens_cache_write INTEGER NOT NULL DEFAULT 0,
		tokens_cache_read INTEGER NOT NULL DEFAULT 0,
		tokens_out INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0.0,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL DEFAULT 0
	)`)
	require.NoError(t, err)

	store := eventstore.NewSQLiteStore(db, nil)
	collector := eventstore.NewCollector(store, slog.Default())
	t.Cleanup(func() { _ = collector.Close() })

	b := NewBridge(BridgeDeps{Log: slog.Default(), Hub: hub})
	b.collector = collector
	b.shutdownCancel()

	return b, store
}

// TestProcessForwardedEvent_CollectorAlwaysAssignsHubSeq verifies that when
// eventstore (collector) is enabled, processForwardedEvent ALWAYS assigns the
// Hub SeqGen value, even when the Worker-provided envelope already has a
// non-zero seq.
//
// Regression for issue #879 (recurring): ACP/Codex/Claude Code mappers use a
// local atomic counter that restarts from 1 on every Worker launch. On resume,
// the bridge accepted the mapper's low seq directly (because env.Seq != 0),
// bypassing the hydrated Hub SeqGen. The low seq then collided with persisted
// events, causing UNIQUE constraint failures (2067) and lost events.
//
// Uses events.State (not Done) to isolate a single seq allocation — Done
// triggers maybeSendDoneFallback which allocates an additional seq for the
// fallback message, obscuring the primary assertion.
func TestProcessForwardedEvent_CollectorAlwaysAssignsHubSeq(t *testing.T) {
	t.Parallel()

	const sessionID = "seq-resume-test"
	const hydratedSeq = int64(295)

	b, _ := newCollectorBridgeForSeqTest(t, sessionID, hydratedSeq)

	// Simulate a Worker mapper that produced seq=1 (local counter restart).
	workerEnv := events.NewEnvelope(aep.NewID(), "", 1, events.State,
		events.StateData{State: events.StateRunning})
	require.Equal(t, int64(1), workerEnv.Seq, "precondition: worker set seq=1")

	// Verify Hub SeqGen starts at hydratedSeq.
	require.Equal(t, hydratedSeq, b.hub.NextSeqPeek(sessionID))

	fc := &forwardContext{
		sessionID:     sessionID,
		workerType:    worker.TypeACP,
		turnStartTime: time.Now(),
	}
	fw := &mockBridgeWorker{workerType: worker.TypeACP}

	b.processForwardedEvent(workerEnv, fw, forwardOpts{}, fc)

	// Hub SeqGen must have advanced exactly once — proving the bridge overrode
	// the worker's seq=1 with the Hub's hydrated value (296).
	require.Equal(t, hydratedSeq+1, b.hub.NextSeqPeek(sessionID),
		"bridge must assign Hub SeqGen when collector is enabled, not worker seq")
}

// TestProcessForwardedEvent_NoCollectorPreservesWorkerSeq verifies the inverse:
// when collector is nil (eventstore disabled), the bridge respects the
// worker-provided seq if non-zero (non-durable deployment behavior).
func TestProcessForwardedEvent_NoCollectorPreservesWorkerSeq(t *testing.T) {
	t.Parallel()

	const sessionID = "seq-nocollector-test"

	hub := newTestHub(t)
	b := NewBridge(BridgeDeps{Log: slog.Default(), Hub: hub})
	b.shutdownCancel()
	// b.collector is nil — eventstore disabled.

	hub.seqGen.Init(sessionID, 0)

	// Worker provides a non-zero seq.
	workerEnv := events.NewEnvelope(aep.NewID(), "", 42, events.State,
		events.StateData{State: events.StateRunning})

	fc := &forwardContext{
		sessionID:     sessionID,
		workerType:    worker.TypeACP,
		turnStartTime: time.Now(),
	}
	fw := &mockBridgeWorker{workerType: worker.TypeACP}

	b.processForwardedEvent(workerEnv, fw, forwardOpts{}, fc)

	// Hub SeqGen should NOT have been used (collector is nil, worker seq != 0).
	require.Equal(t, int64(0), hub.NextSeqPeek(sessionID),
		"Hub SeqGen must not advance when collector is disabled and worker provided a non-zero seq")
}

func TestFinishTurnTimeoutPersistsFailureAndAllocatesOrderedEvents(t *testing.T) {
	t.Parallel()

	const sessionID = "turn-timeout-terminal"
	b, eventStore := newCollectorBridgeForSeqTest(t, sessionID, 0)
	execStore := &fakeExecutionStore{
		openRecord: &execution.Record{ExecutionID: "execution-1"},
	}
	b.executionStore = execStore
	b.turnTimeout = 30 * time.Minute
	fc := &forwardContext{
		sessionID:   sessionID,
		workerRunID: "worker-run-1",
		sessOwner:   "owner-1",
	}

	b.finishTurnTimeout(sessionID, fc, syntheticTurnParams{
		SessionID: sessionID,
		Reason:    "turn_timeout",
		Message:   "Turn exceeded 30m time limit",
		Source:    eventstore.SourceTimeout,
		Owner:     "owner-1",
	})

	require.Equal(t, execution.RuntimeFailed, execStore.finishStatus)
	require.Equal(t, "worker-run-1", execStore.finishRunID)
	require.Equal(t, int64(3), b.hub.NextSeqPeek(sessionID),
		"timeout Error, persisted synthetic Done, and runtime failure fact must allocate in order")
	require.NoError(t, b.collector.Flush())
	page, err := eventStore.QueryBySession(t.Context(), sessionID, 0, eventstore.CursorLatest, 10)
	require.NoError(t, err)
	require.Len(t, page.Events, 2)
	require.Equal(t, []string{string(events.Error), string(events.Done)}, []string{
		page.Events[0].Type,
		page.Events[1].Type,
	})
	require.Equal(t, []int64{1, 2}, []int64{
		page.Events[0].Seq,
		page.Events[1].Seq,
	})
}

// TestProcessForwardedEvent_DoneArrivesAfterTheRuntimeFactItOvertakes pins the
// client-visible order AND seq invariant for the terminal pair: the Done
// arrives first and the terminal runtime fact follows it, with seq increasing
// in arrival order. This is the order docs/reference/aep-protocol.md:385-386
// specifies (completed after done); the pre-D01 gateway emitted them
// reversed, which this test now rejects. The seq invariant is what clients
// depend on — a Done arriving with a LOWER seq than an event the client has
// already seen is dropped by monotonic-seq clients and the turn never ends
// (found by a live run against a real Worker).
func TestProcessForwardedEvent_DoneArrivesAfterTheRuntimeFactItOvertakes(t *testing.T) {
	t.Parallel()

	const sessionID = "done-vs-runtime-seq"
	b, _ := newCollectorBridgeForSeqTest(t, sessionID, 0)

	// A joined WebSocket conn makes the client-visible order observable: these
	// are the exact frames a browser or SDK receives, in the exact order.
	conn, server := newTestWSConnPair(t)
	t.Cleanup(func() { _ = conn.Close(); _ = server.Close() })
	b.hub.JoinSession(sessionID, newConn(b.hub, conn, sessionID, nil))
	b.executionStore = &fakeExecutionStore{
		openRecord: testExecutionRecord(execution.StatusDelivered),
	}

	fc := &forwardContext{sessionID: sessionID, workerType: worker.TypeClaudeCode, firstEvent: true}
	fc.turnText.WriteString("reply")
	fw := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		conn:       &fakeWorkerConn{ch: make(chan *events.Envelope)},
	}
	done := events.NewEnvelope(aep.NewID(), sessionID, 0, events.Done, events.DoneData{Success: true})

	b.processForwardedEvent(done, fw, forwardOpts{}, fc)

	first := tryReadEnvelope(t, server)
	require.NotNil(t, first, "the Done must reach the client first")
	require.Equal(t, events.Done, first.Event.Type,
		"AEP requires done before runtime.execution.completed")

	second := tryReadEnvelope(t, server)
	require.NotNil(t, second, "the terminal runtime fact must follow the Done")
	require.Equal(t, events.RuntimeExecutionCompleted, second.Event.Type)

	// The invariant every seq-enforcing client depends on: arrival order and
	// seq order agree.
	require.Greater(t, second.Seq, first.Seq,
		"client-visible seq must increase with arrival order")
}
