package opencodeserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

func TestDeepAudit992ResetOnlyAcknowledgesSupportedSuccess(t *testing.T) {
	t.Parallel()
	for _, status := range []int{200, 204, 404, 405, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "POST", r.Method)
				require.Equal(t, "/session/native-session/reset", r.URL.Path)
				w.WriteHeader(status)
			}))
			defer server.Close()
			c := &conn{sessionID: "native-session", systemPrompt: "preserved", recvCh: make(chan *events.Envelope, 8)}
			w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil), httpConn: c, httpAddr: server.URL, client: server.Client()}
			result, err := w.ResetContext(context.Background())
			require.False(t, result.ConnReplaced)
			if status == 200 || status == 204 {
				require.NoError(t, err)
				require.EqualValues(t, 1, w.LoadResetGeneration())
				require.Equal(t, events.KindInternalReset, (<-c.Recv()).Event.Type)
			} else {
				require.Error(t, err, "an unsupported endpoint must not acknowledge a native reset")
				require.Zero(t, w.LoadResetGeneration())
				require.Empty(t, c.recvCh, "failure must not emit a reset event")
				if status == 404 || status == 405 {
					require.ErrorIs(t, err, worker.ErrNotImplemented)
				}
			}
			require.Equal(t, "native-session", c.getSessionID())
			require.Equal(t, "preserved", c.systemPrompt)
			require.NoError(t, c.Close())
		})
	}
}

func TestDeepAudit992CancelledResetDoesNotAdvanceGeneration(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c := &conn{sessionID: "native-session", recvCh: make(chan *events.Envelope, 8)}
	defer c.Close()
	w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil), httpConn: c, httpAddr: server.URL, client: server.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.ResetContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, requests.Load())
	require.Zero(t, w.LoadResetGeneration())
	require.Empty(t, c.recvCh)
}
