package codexcli

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/pkg/events"
)

func TestDeepAudit988ConcurrentSendAndAllCloseOwners(t *testing.T) {
	t.Parallel()
	for i := 0; i < 32; i++ {
		m := auditRPCManager()
		c := &appConn{manager: m, recvCh: m.Subscribe("thread", "session")}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for actor := 0; actor < 3; actor++ {
			wg.Add(1)
			go func(actor int) {
				defer wg.Done()
				<-start
				switch actor {
				case 0:
					for j := 0; j < 128; j++ {
						c.TrySend(&events.Envelope{})
					}
				case 1:
					_ = c.Close()
				case 2:
					m.Shutdown(context.Background())
				}
			}(actor)
		}
		close(start)
		wg.Wait()
		require.Empty(t, m.subGates)
		require.Empty(t, m.subscribers)
	}
}
func TestDeepAudit988ShutdownWakesBlockedCriticalSend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := auditRPCManager()
		ch := m.Subscribe("thread", "session")
		c := &appConn{manager: m, recvCh: ch}
		for i := 0; i < cap(ch); i++ {
			require.True(t, c.TrySend(&events.Envelope{}))
		}
		sent := make(chan struct{})
		go func() { m.sendEnvelope(ch, &events.Envelope{Event: events.Event{Type: events.Done}}); close(sent) }()
		synctest.Wait()
		start := time.Now()
		m.Shutdown(context.Background())
		<-sent
		require.Less(t, time.Since(start), time.Second)
		require.NoError(t, c.Close())
	})
}
