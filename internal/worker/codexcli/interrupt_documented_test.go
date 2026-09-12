package codexcli

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestD08InterruptRequiresCorrelatedAcknowledgement(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "accepted"
		if reject {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w, s := documentedWorker(t)
			defer w.Terminate(context.Background())
			w.turnID = "target-turn"
			s.setResult(true, reject, `{}`)
			err := w.StopCurrentTurn(context.Background())
			if reject {
				require.Error(t, err)
				require.False(t, w.IsStopped())
			} else {
				require.NoError(t, err)
				require.True(t, w.IsStopped())
			}
			frames := s.snapshot()
			frame := frames[len(frames)-1]
			require.Equal(t, "turn/interrupt", frame.Method)
			require.NotZero(t, frame.ID, "turn/interrupt is a request, not a notification")
			var params map[string]string
			require.NoError(t, json.Unmarshal(frame.Params, &params))
			require.Equal(t, "target-turn", params["turnId"])
			require.Equal(t, w.threadID, params["threadId"])
			require.True(t, w.manager.IsRunning())
		})
	}
}

func TestD08UnacknowledgedInterruptHonorsDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w, s := documentedWorker(t)
		defer w.Terminate(context.Background())
		w.turnID = "target-turn"
		s.setResult(false, false, "")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, w.StopCurrentTurn(ctx), context.DeadlineExceeded)
		require.False(t, w.IsStopped())
		require.True(t, w.manager.IsRunning())
	})
}

// Existing lifecycle fixtures must acknowledge request frames, while retaining
// their original output assertions. Notifications still have no response.
type documentedAckWriter struct {
	target  io.Writer
	manager *CodexAppServerManager
}

func (s documentedAckWriter) Close() error { return nil }
func (s documentedAckWriter) Write(p []byte) (int, error) {
	n, err := s.target.Write(p)
	if err != nil {
		return n, err
	}
	var frame JSONRPCRequest
	if json.Unmarshal(p, &frame) == nil && frame.ID != 0 {
		s.manager.dispatchResponse(&JSONRPCResponse{ID: frame.ID, Result: json.RawMessage(`{}`)})
	}
	return n, nil
}
