package opencodeserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

func TestD05ResetRejects404WithoutSuccessSignal(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusOK, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			c := &conn{sessionID: "original", recvCh: make(chan *events.Envelope, 2)}
			w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil), httpConn: c, httpAddr: server.URL, client: server.Client()}
			before := w.LoadResetGeneration()
			_, err := w.ResetContext(context.Background())
			if status >= 400 {
				require.Error(t, err)
				require.Equal(t, before, w.LoadResetGeneration())
				require.Empty(t, c.recvCh)
				require.Equal(t, "original", c.getSessionID())
			} else {
				require.NoError(t, err)
				require.Equal(t, before+1, w.LoadResetGeneration())
				require.Equal(t, events.KindInternalReset, (<-c.recvCh).Event.Type)
			}
		})
	}
}
