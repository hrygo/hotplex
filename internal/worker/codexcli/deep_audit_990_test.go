package codexcli

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
)

type deep990Sink struct{ manager *CodexAppServerManager }

func (s *deep990Sink) Close() error { return nil }
func (s *deep990Sink) Write(p []byte) (int, error) {
	var frame JSONRPCRequest
	if err := json.Unmarshal(p, &frame); err != nil {
		return 0, err
	}
	if frame.ID != 0 {
		if pending, ok := s.manager.pending.Load(frame.ID); ok {
			pending.(chan *JSONRPCResponse) <- &JSONRPCResponse{Result: json.RawMessage(`{"thread":{"id":"owned-thread"}}`)}
		}
	}
	return len(p), nil
}
func deep990Worker() (*AppServerWorker, *CodexAppServerManager) {
	m := auditRPCManager()
	m.stdin = &deep990Sink{manager: m}
	m.refs = 1 // unrelated session, never owned by this worker
	return &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m}, m
}

func TestDeepAudit990ResetFailureReleasesOnlyItsReference(t *testing.T) {
	t.Parallel()
	w, m := deep990Worker()
	require.NoError(t, w.Start(context.Background(), worker.SessionInfo{SessionID: "session", UserID: "user"}))
	require.Equal(t, 2, m.refs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.ResetContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, w.Terminate(context.Background()))
	require.NoError(t, w.Terminate(context.Background()))
	require.Equal(t, 1, m.refs, "connection closed is not proof that the acquired reference was released")
}

func TestDeepAudit990UnacquiredAndFailedStartNeverReleaseOthers(t *testing.T) {
	t.Parallel()
	for _, attemptStart := range []bool{false, true} {
		w, m := deep990Worker()
		if attemptStart {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			require.ErrorIs(t, w.Start(ctx, worker.SessionInfo{SessionID: "session", UserID: "user"}), context.Canceled)
		}
		require.NoError(t, w.Terminate(context.Background()))
		require.NoError(t, w.Terminate(context.Background()))
		require.Equal(t, 1, m.refs)
	}
}

func TestDeepAudit990ConcurrentTerminateReleasesOnce(t *testing.T) {
	t.Parallel()
	w, m := deep990Worker()
	require.NoError(t, w.Start(context.Background(), worker.SessionInfo{SessionID: "session", UserID: "user"}))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = w.Terminate(context.Background()) }()
	}
	wg.Wait()
	require.Equal(t, 1, m.refs)
}
