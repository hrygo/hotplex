from common import write,replace,between,main


def tests():
    write('internal/worker/codexcli/deep_audit_988_test.go',r'''
package codexcli

import (
    "testing"
    "github.com/stretchr/testify/require"
    "github.com/hrygo/hotplex/pkg/events"
)

func TestDeepAudit988ManagerThenConnectionClose(t *testing.T){
    t.Parallel()
    m:=auditRPCManager()
    c:=&appConn{manager:m,recvCh:m.Subscribe("thread","session")}
    m.Shutdown()
    require.NotPanics(t,func(){require.NoError(t,c.Close())})
    require.NotPanics(t,func(){m.Shutdown()})
}
func TestDeepAudit988ConnectionThenManagerClose(t *testing.T){
    t.Parallel()
    m:=auditRPCManager()
    c:=&appConn{manager:m,recvCh:m.Subscribe("thread","session")}
    require.NoError(t,c.Close())
    require.NotPanics(t,func(){m.Shutdown()})
}
func TestDeepAudit988ClosedConnectionRejectsSend(t *testing.T){
    t.Parallel()
    m:=auditRPCManager()
    c:=&appConn{manager:m,recvCh:m.Subscribe("thread","session")}
    require.NoError(t,c.Close())
    require.NotPanics(t,func(){require.False(t,c.TrySend(&events.Envelope{}))})
}
func TestDeepAudit988StaleConnectionCannotCloseReplacement(t *testing.T){
    t.Parallel()
    m:=auditRPCManager()
    old:=&appConn{manager:m,recvCh:m.Subscribe("thread","old")}
    m.Unsubscribe("thread")
    fresh:=&appConn{manager:m,recvCh:m.Subscribe("thread","new")}
    require.NoError(t,old.Close())
    require.True(t,fresh.TrySend(&events.Envelope{ID:"new"}))
    require.Equal(t,"new",(<-fresh.Recv()).ID)
    m.Unsubscribe("thread")
    require.NotPanics(t,func(){_ = fresh.Close()})
}
''')


def fixes():
    path='internal/worker/codexcli/manager.go'
    replace(path,'\tsubMu       sync.Mutex','\tsubMu       sync.Mutex\n\tsubGates map[chan *events.Envelope]*base.EventGate')
    replace(path,'\tch := make(chan *events.Envelope, 256)\n\tm.subscribers[threadID] = ch', '''    ch := make(chan *events.Envelope, 256)
    if m.subsClosed.Load(){close(ch);return ch}
    if m.subGates==nil{m.subGates=make(map[chan *events.Envelope]*base.EventGate)}
    m.subGates[ch]=&base.EventGate{}
    m.subscribers[threadID] = ch''')
    replace(path,'''\tif _, ok := m.subscribers[threadID]; ok {
\t\tdelete(m.subscribers, threadID)
\t\tdelete(m.subSessions, threadID)''','''    if ch, ok := m.subscribers[threadID]; ok {
        m.closeSubscriberLocked(ch)''')
    replace(path,'''// Precondition: the corresponding appConn must be closed before or after this
// call to ensure the channel is eventually cleaned up by monitorProcess.''','''// The manager owns channel teardown; a later appConn.Close is idempotent and
// cannot close a replacement subscription with the same thread ID.''')
    replace(path,'''\t\tfor id, ch := range m.subscribers {
\t\t\tclose(ch)
\t\t\tdelete(m.subscribers, id)
\t\t}''','''        m.closeAllSubscribersLocked()''',count=2)
    between(path,'func (m *CodexAppServerManager) sendEnvelope(', '// monitorProcess waits',r'''
func (m *CodexAppServerManager) sendEnvelope(ch chan *events.Envelope, env *events.Envelope) {
    gate:=m.subscriberGate(ch)
    if gate==nil{return}
    if env.Event.Type==events.MessageDelta || env.Event.Type==events.Reasoning {
        gate.TrySend(ch,env)
        return
    }
    if !gate.SendTimeout(ch,env,criticalEventSendTimeout) {
        m.log.Warn("codex-app-server: subscriber closed or critical send timed out","event_type",env.Event.Type)
    }
}
''')
    path='internal/worker/codexcli/worker.go'
    replace(path,'\tlastReplay worker.InputReplay','\tlastReplay worker.InputReplay\n\tstandaloneGate base.EventGate')
    between(path,'func (c *appConn) TrySend(', 'func (c *appConn) UserID()',r'''
func (c *appConn) TrySend(env *events.Envelope) bool {
    if c.manager!=nil {
        gate:=c.manager.subscriberGate(c.recvCh)
        return gate!=nil && gate.TrySend(c.recvCh,env)
    }
    return c.standaloneGate.TrySend(c.recvCh,env)
}
func (c *appConn) Close() error {
    c.mu.Lock()
    defer c.mu.Unlock()
    if c.closed{return nil}
    c.closed=true
    if c.manager!=nil {
        c.manager.closeSubscriber(c.recvCh)
    } else {
        c.standaloneGate.Close(c.recvCh)
    }
    return nil
}
''')
    write('internal/worker/codexcli/subscriber_gate.go',r'''
package codexcli

import (
    "github.com/hrygo/hotplex/internal/worker/base"
    "github.com/hrygo/hotplex/pkg/events"
)

// Gates exist only for active subscriptions. A caller retaining an old channel
// cannot resurrect it or close a replacement; the channel itself is its identity.
func(m *CodexAppServerManager) subscriberGate(ch chan *events.Envelope)*base.EventGate{
    m.subMu.Lock()
    defer m.subMu.Unlock()
    return m.subscriberGateLocked(ch)
}
func(m *CodexAppServerManager) subscriberGateLocked(ch chan *events.Envelope)*base.EventGate{
    if gate:=m.subGates[ch];gate!=nil{return gate}
    // Also support internal fixtures that install an active channel directly.
    for _,active:=range m.subscribers{
        if active==ch{
            if m.subGates==nil{m.subGates=make(map[chan *events.Envelope]*base.EventGate)}
            gate:=&base.EventGate{}
            m.subGates[ch]=gate
            return gate
        }
    }
    return nil
}
func(m *CodexAppServerManager) closeSubscriber(ch chan *events.Envelope){
    m.subMu.Lock()
    defer m.subMu.Unlock()
    m.closeSubscriberLocked(ch)
}
func(m *CodexAppServerManager) closeSubscriberLocked(ch chan *events.Envelope){
    gate:=m.subscriberGateLocked(ch)
    if gate==nil{return}
    // EventGate wakes blocked critical senders before closing recvCh. It does
    // not need subMu to complete, so this metadata lock cannot deadlock it.
    gate.Close(ch)
    for id,active:=range m.subscribers{
        if active==ch{delete(m.subscribers,id);delete(m.subSessions,id)}
    }
    delete(m.subGates,ch)
}
func(m *CodexAppServerManager) closeAllSubscribersLocked(){
    for _,ch:=range m.subscribers{m.closeSubscriberLocked(ch)}
}
''')
    write('internal/worker/codexcli/subscriber_gate_test.go',r'''
package codexcli

import (
    "sync"
    "testing"
    "testing/synctest"
    "time"
    "github.com/stretchr/testify/require"
    "github.com/hrygo/hotplex/pkg/events"
)

func TestDeepAudit988ConcurrentSendAndAllCloseOwners(t *testing.T){
    t.Parallel()
    for i:=0;i<32;i++{
        m:=auditRPCManager()
        c:=&appConn{manager:m,recvCh:m.Subscribe("thread","session")}
        var wg sync.WaitGroup
        start:=make(chan struct{})
        for actor:=0;actor<3;actor++{wg.Add(1);go func(actor int){
            defer wg.Done();<-start
            switch actor{case 0:for j:=0;j<128;j++{c.TrySend(&events.Envelope{})};case 1:_=c.Close();case 2:m.Shutdown()}
        }(actor)}
        close(start);wg.Wait()
        require.Empty(t,m.subGates)
        require.Empty(t,m.subscribers)
    }
}
func TestDeepAudit988ShutdownWakesBlockedCriticalSend(t *testing.T){
    t.Parallel()
    synctest.Test(t,func(t *testing.T){
        m:=auditRPCManager()
        ch:=m.Subscribe("thread","session")
        c:=&appConn{manager:m,recvCh:ch}
        for i:=0;i<cap(ch);i++{require.True(t,c.TrySend(&events.Envelope{}))}
        sent:=make(chan struct{})
        go func(){m.sendEnvelope(ch,&events.Envelope{Event:events.Event{Type:events.Done}});close(sent)}()
        synctest.Wait()
        start:=time.Now();m.Shutdown();<-sent
        require.Less(t,time.Since(start),time.Second)
        require.NoError(t,c.Close())
    })
}
''')


if __name__=='__main__':main(tests,fixes)
