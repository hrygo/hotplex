package codexcli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/pkg/events"
)

func TestDeepAudit988ManagerThenConnectionClose(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	c := &appConn{manager: m, recvCh: m.Subscribe("thread", "session")}
	m.Shutdown(context.Background())
	require.NotPanics(t, func() { require.NoError(t, c.Close()) })
	require.NotPanics(t, func() { m.Shutdown(context.Background()) })
}
func TestDeepAudit988ConnectionThenManagerClose(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	c := &appConn{manager: m, recvCh: m.Subscribe("thread", "session")}
	require.NoError(t, c.Close())
	require.NotPanics(t, func() { m.Shutdown(context.Background()) })
}
func TestDeepAudit988ClosedConnectionRejectsSend(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	c := &appConn{manager: m, recvCh: m.Subscribe("thread", "session")}
	require.NoError(t, c.Close())
	require.NotPanics(t, func() { require.False(t, c.TrySend(&events.Envelope{})) })
}
func TestDeepAudit988StaleConnectionCannotCloseReplacement(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	old := &appConn{manager: m, recvCh: m.Subscribe("thread", "old")}
	m.Unsubscribe("thread")
	fresh := &appConn{manager: m, recvCh: m.Subscribe("thread", "new")}
	require.NoError(t, old.Close())
	require.True(t, fresh.TrySend(&events.Envelope{ID: "new"}))
	require.Equal(t, "new", (<-fresh.Recv()).ID)
	m.Unsubscribe("thread")
	require.NotPanics(t, func() { _ = fresh.Close() })
}
