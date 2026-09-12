package opencodeserver

import (
	"context"
	"log/slog"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

func TestAuditOCSCloseSynchronizesBlockedCriticalSend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := &conn{sessionID: "session", log: slog.Default(), recvCh: make(chan *events.Envelope)}
		w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil), httpConn: c}
		bus := make(chan *events.Envelope, 1)
		bus <- &events.Envelope{Event: events.Event{Type: events.Done}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go w.forwardBusEvents(ctx, "session", bus)
		synctest.Wait()
		require.NoError(t, c.Close())
		cancel()
		synctest.Wait()
		_, open := <-c.Recv()
		require.False(t, open)
	})
}
