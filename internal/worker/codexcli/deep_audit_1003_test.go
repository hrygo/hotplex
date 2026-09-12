package codexcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker/base"
)

// deep1003ResponseWriter is a protocol fake, not a production fallback. A
// notification never receives an ACK. Only a correctly identified request does.
type deep1003ResponseWriter struct {
	manager *CodexAppServerManager
	output  io.Writer
	reject  bool
	drop    bool
	frames  []JSONRPCRequest
}

func (s *deep1003ResponseWriter) Close() error { return nil }
func (s *deep1003ResponseWriter) Write(p []byte) (int, error) {
	if s.output != nil {
		if _, err := s.output.Write(p); err != nil {
			return 0, err
		}
	}
	var frame JSONRPCRequest
	if err := json.Unmarshal(p, &frame); err != nil {
		return 0, err
	}
	s.frames = append(s.frames, frame)
	if frame.Method == "turn/interrupt" && frame.ID != 0 && !s.drop {
		response := fmt.Sprintf(`{"id":%d,"result":{}}`, frame.ID)
		if s.reject {
			response = fmt.Sprintf(`{"id":%d,"error":{"code":-32602,"message":"no matching turn"}}`, frame.ID)
		}
		s.manager.dispatchFrame([]byte(response))
	}
	return len(p), nil
}

func TestDeepAudit1003StopRequiresAcknowledgedRequest(t *testing.T) {
	t.Parallel()
	for _, reject := range []bool{false, true} {
		name := "ack"
		if reject {
			name = "rejection"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := auditRPCManager()
			m.refs = 2
			sink := &deep1003ResponseWriter{manager: m, reject: reject}
			m.stdin = sink
			w := &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m, threadID: "thread", turnID: "turn"}
			err := w.StopCurrentTurn(context.Background())
			require.Len(t, sink.frames, 1)
			frame := sink.frames[0]
			require.NotZero(t, frame.ID, "turn/interrupt is a request, not a one-way notification")
			require.Equal(t, "turn/interrupt", frame.Method)
			require.JSONEq(t, `{"threadId":"thread","turnId":"turn"}`, string(frame.Params))
			if reject {
				require.Error(t, err)
				require.False(t, w.IsStopped())
			} else {
				require.NoError(t, err)
				require.True(t, w.IsStopped())
			}
			require.Equal(t, 2, m.refs)
			require.True(t, m.IsRunning())
			pending := 0
			m.pending.Range(func(_, _ any) bool { pending++; return true })
			require.Zero(t, pending)
		})
	}
}

func TestDeepAudit1003MissingStopAckHonorsCallerBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := auditRPCManager()
		sink := &deep1003ResponseWriter{manager: m, drop: true}
		m.stdin = sink
		w := &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m, threadID: "thread", turnID: "turn"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := w.StopCurrentTurn(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.False(t, w.IsStopped(), "unacknowledged stop must not claim success")
		require.True(t, m.IsRunning())
	})
}

func TestDeepAudit1003PreCancelledStopWritesNothing(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	sink := &deep1003ResponseWriter{manager: m}
	m.stdin = sink
	w := &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m, threadID: "thread", turnID: "turn"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, w.StopCurrentTurn(ctx), context.Canceled)
	require.Empty(t, sink.frames)
	require.False(t, w.IsStopped())
}
