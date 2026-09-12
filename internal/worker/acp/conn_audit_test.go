package acp

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/pkg/events"
)

func TestAuditACPConcurrentSendClose(t *testing.T) {
	t.Parallel()
	for i := 0; i < 32; i++ {
		c := newACPConn("user", "session", slog.Default())
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 64; j++ {
				c.TrySend(&events.Envelope{Event: events.Event{Type: events.MessageDelta}})
			}
		}()
		go func() { defer wg.Done(); <-start; _ = c.Close() }()
		close(start)
		wg.Wait()
	}
}

func TestAuditACPCloseUnblocksCriticalSend(t *testing.T) {
	t.Parallel()
	c := newACPConn("user", "session", slog.Default())
	for i := 0; i < cap(c.recvCh); i++ {
		require.True(t, c.TrySend(&events.Envelope{Event: events.Event{Type: events.MessageDelta}}))
	}
	result := make(chan bool, 1)
	go func() { result <- c.TrySend(&events.Envelope{Event: events.Event{Type: events.Done}}) }()
	require.NoError(t, c.Close())
	select {
	case sent := <-result:
		require.False(t, sent)
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock a critical sender")
	}
}
