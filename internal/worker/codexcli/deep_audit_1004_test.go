package codexcli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
)

type deep1004Sink struct {
	manager *CodexAppServerManager
	result  json.RawMessage
}

func (s *deep1004Sink) Close() error { return nil }
func (s *deep1004Sink) Write(p []byte) (int, error) {
	var frame JSONRPCRequest
	if err := json.Unmarshal(p, &frame); err != nil {
		return 0, err
	}
	if frame.ID != 0 {
		if pending, ok := s.manager.pending.Load(frame.ID); ok {
			pending.(chan *JSONRPCResponse) <- &JSONRPCResponse{Result: s.result}
		}
	}
	return len(p), nil
}

func TestDeepAudit1004InvalidThreadIdentityNeverPublishesConnection(t *testing.T) {
	t.Parallel()
	for name, payload := range map[string]string{"empty": "{}", "null": "null", "empty_id": `{"thread":{"id":""}}`, "blank_id": `{"thread":{"id":"  "}}`, "wrong_type": `{"thread":{"id":73}}`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := auditRPCManager()
			m.refs = 1
			m.stdin = &deep1004Sink{manager: m, result: json.RawMessage(payload)}
			w := &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m}
			err := w.Start(context.Background(), worker.SessionInfo{SessionID: "session", UserID: "user"})
			// Cleanup happens even when the unmodified implementation wrongly
			// accepts this response. Do not leak an idle timer into other tests.
			accepted := err == nil
			nativeID := w.threadID
			connection := w.Conn()
			refs := m.refs
			emptySubscription := m.subscribers[""] != nil || m.subscribers["  "] != nil
			if accepted {
				_ = w.Terminate(context.Background())
			}
			m.Shutdown(context.Background())
			require.Error(t, err, "missing native identity was treated as a ready worker")
			require.Empty(t, nativeID)
			require.Nil(t, connection)
			require.False(t, emptySubscription)
			require.Equal(t, 1, refs, "failed startup must release its acquired reference")
		})
	}
}

func TestDeepAudit1004MalformedTurnResponseDoesNotReusePreviousTurn(t *testing.T) {
	t.Parallel()
	for name, payload := range map[string]string{"empty": "{}", "null": "null", "empty_id": `{"turn":{"id":""}}`, "blank_id": `{"turn":{"id":"  "}}`, "wrong_type": `{"turn":{"id":73}}`, "malformed": "secret-response-not-json"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := auditRPCManager()
			m.stdin = &deep1004Sink{manager: m, result: json.RawMessage(payload)}
			w := &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m, threadID: "thread", turnID: "previous-turn"}
			w.MarkStopped()
			err := w.startTurn(context.Background(), []TurnInputItem{{Type: "text", Text: "probe"}})
			require.Error(t, err)
			var classified *worker.WorkerError
			require.ErrorAs(t, err, &classified)
			require.Equal(t, worker.ErrKindTimeout, classified.Kind, "an accepted request's outcome is unknown, not safe to replay")
			require.Empty(t, w.turnID, "a malformed response retained the previous native turn ID")
			require.NotContains(t, err.Error(), "secret-response-not-json")
			require.True(t, m.IsRunning(), "one malformed response must not stop all sibling sessions")
		})
	}
}

func TestDeepAudit1004ValidLifecycleResponseRetainsExtensions(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	sink := &deep1004Sink{manager: m, result: json.RawMessage(`{"thread":{"id":"thread-ok","newField":true}}`)}
	m.stdin = sink
	m.refs = 1
	w := &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m}
	require.NoError(t, w.Start(context.Background(), worker.SessionInfo{SessionID: "session", UserID: "user"}))
	require.Equal(t, "thread-ok", w.threadID)
	sink.result = json.RawMessage(`{"turn":{"id":"turn-ok","status":"inProgress"},"extension":true}`)
	require.NoError(t, w.startTurn(context.Background(), []TurnInputItem{{Type: "text", Text: "probe"}}))
	require.Equal(t, "turn-ok", w.turnID)
	require.NoError(t, w.Terminate(context.Background()))
	require.Equal(t, 1, m.refs)
	m.Shutdown(context.Background())
}
