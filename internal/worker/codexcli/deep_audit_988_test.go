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

func auditManagedConn(m *CodexAppServerManager, threadID, sessionID string) (*appConn, *codexSubscription) {
	sub := m.subscribe(threadID, sessionID)
	return &appConn{manager: m, recvCh: sub.ch, recvGate: &sub.gate}, sub
}

func TestDeepAudit988ManagerThenConnectionClose(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	c, _ := auditManagedConn(m, "thread", "session")
	m.Shutdown(context.Background())
	require.NotPanics(t, func() { require.NoError(t, c.Close()) })
	require.NotPanics(t, func() { m.Shutdown(context.Background()) })
}
func TestDeepAudit988ConnectionThenManagerClose(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	c, _ := auditManagedConn(m, "thread", "session")
	require.NoError(t, c.Close())
	require.NotPanics(t, func() { m.Shutdown(context.Background()) })
}
func TestDeepAudit988ClosedConnectionRejectsSend(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	c, _ := auditManagedConn(m, "thread", "session")
	require.NoError(t, c.Close())
	require.NotPanics(t, func() { require.False(t, c.TrySend(&events.Envelope{})) })
}
func TestDeepAudit988StaleConnectionCannotCloseReplacement(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	old, _ := auditManagedConn(m, "thread", "old")
	m.Unsubscribe("thread")
	fresh, _ := auditManagedConn(m, "thread", "new")
	require.NoError(t, old.Close())
	require.True(t, fresh.TrySend(&events.Envelope{ID: "new"}))
	require.Equal(t, "new", (<-fresh.Recv()).ID)
	m.Unsubscribe("thread")
	require.NotPanics(t, func() { _ = fresh.Close() })
}

func TestDeepAudit988ConcurrentSendAndAllCloseOwners(t *testing.T) {
	t.Parallel()
	for i := 0; i < 32; i++ {
		m := auditRPCManager()
		c, _ := auditManagedConn(m, "thread", "session")
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
		require.Empty(t, m.subscribers)
	}
}

func TestDeepAudit988ShutdownWakesBlockedCriticalSend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := auditRPCManager()
		_, sub := auditManagedConn(m, "thread", "session")
		for i := 0; i < cap(sub.ch); i++ {
			require.True(t, sub.gate.TrySend(sub.ch, &events.Envelope{}))
		}
		sent := make(chan struct{})
		go func() {
			m.sendEnvelope(sub, &events.Envelope{Event: events.Event{Type: events.Done}})
			close(sent)
		}()
		synctest.Wait()
		start := time.Now()
		m.Shutdown(context.Background())
		<-sent
		require.Less(t, time.Since(start), time.Second)
	})
}
