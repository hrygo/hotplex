from common import write, replace, between, main


def tests():
    write('internal/messaging/deep_audit_1002_test.go',r'''
package messaging

import (
    "log/slog"
    "sync/atomic"
    "testing"
    "testing/synctest"
    "time"

    "github.com/stretchr/testify/require"
    "github.com/hrygo/hotplex/pkg/events"
)

func TestDeepAudit1002OldTimeoutCannotDenyReplacement(t *testing.T) {
    t.Parallel()
    for _,finish:=range []string{"complete","claimed","cancel"} {
        t.Run(finish,func(t *testing.T){
            t.Parallel()
            synctest.Test(t,func(t *testing.T){
                m:=NewInteractionManager(slog.Default())
                var sent atomic.Int32
                old:=&PendingInteraction{ID:"same",SessionID:"old-session",Type:events.PermissionRequest,Timeout:time.Second,SendResponse:func(map[string]any){sent.Add(1)}}
                m.Register(old)
                synctest.Wait()
                oldCancel:=old.cancelCh
                switch finish {
                case "complete":_,ok:=m.Complete(old.ID);require.True(t,ok)
                case "claimed":_,ok:=m.Claim(old.ID);require.True(t,ok);_,ok=m.CompleteClaimed(old.ID);require.True(t,ok)
                case "cancel":m.CancelAll(old.SessionID)
                }
                current:=&PendingInteraction{ID:"same",SessionID:"new-session",Type:events.QuestionRequest,Timeout:time.Hour,SendResponse:func(map[string]any){sent.Add(1)}}
                m.Register(current)
                synctest.Wait()
                <-time.After(2*time.Second) // virtual clock, not a scheduling sleep
                synctest.Wait()
                got,pending:=m.Get(current.ID)
                m.CancelAll(current.SessionID)
                require.True(t,pending,"the old deadline consumed a newer registration")
                require.Same(t,current,got)
                require.Zero(t,sent.Load())
                select {case <-oldCancel:default:t.Error("completion did not retire the old watcher")}
            })
        })
    }
}

func TestDeepAudit1002SameObjectCanBeRegisteredAfterCompletion(t *testing.T) {
    t.Parallel()
    synctest.Test(t,func(t *testing.T){
        m:=NewInteractionManager(slog.Default())
        var sent atomic.Int32
        pi:=&PendingInteraction{ID:"same",SessionID:"session",Type:events.PermissionRequest,Timeout:time.Second,SendResponse:func(map[string]any){sent.Add(1)}}
        // Do not wait for the first watcher to start. Reusing the struct must
        // not redirect its old goroutine to the new registration's channel.
        m.Register(pi)
        _,ok:=m.Claim(pi.ID);require.True(t,ok)
        _,ok=m.CompleteClaimed(pi.ID);require.True(t,ok)
        m.Register(pi)
        synctest.Wait()
        _,claimable:=m.Claim(pi.ID)
        m.CancelAll(pi.SessionID)
        require.True(t,claimable,"resolving state leaked across registration lifetimes")
        require.Zero(t,sent.Load())
    })
}

func TestDeepAudit1002CurrentTimeoutStillDeniesOnce(t *testing.T) {
    t.Parallel()
    synctest.Test(t,func(t *testing.T){
        m:=NewInteractionManager(slog.Default())
        var sent atomic.Int32
        pi:=&PendingInteraction{ID:"current",SessionID:"session",Type:events.PermissionRequest,Timeout:time.Second,SendResponse:func(map[string]any){sent.Add(1)}}
        m.Register(pi)
        synctest.Wait()
        <-time.After(2*time.Second)
        synctest.Wait()
        require.Zero(t,m.Len())
        require.EqualValues(t,1,sent.Load())
        m.CancelAll(pi.SessionID)
    })
}
''')


def fixes():
    path='internal/messaging/interaction.go'
    replace(path,'\t// cancelCh is closed by CancelAll to abort the watchTimeout goroutine.', '\t// cancelCh identifies one registration and is closed on completion or cancellation.')
    replace(path,'''\tpi.cancelCh = make(chan struct{})
\tm.pending[pi.ID] = pi
\tm.mu.Unlock()

\t// Start timeout goroutine
\tgo m.watchTimeout(pi)''', '''    pi.cancelCh = make(chan struct{})
    pi.resolving = false
    cancelCh, timeout := pi.cancelCh, pi.Timeout
    m.pending[pi.ID] = pi
    m.mu.Unlock()

    // Capture the registration signal before another caller can complete and
    // re-register the same object. A watcher never follows a replacement.
    go m.watchRegistrationTimeout(pi, cancelCh, timeout)''')
    replace(path,'''\tif ok {
\t\tdelete(m.pending, requestID)
\t}
\treturn pi, ok''', '''    if ok {
        delete(m.pending, requestID)
        if pi.cancelCh != nil { close(pi.cancelCh) }
    }
    return pi, ok''')
    replace(path,'''\tdelete(m.pending, requestID)
\treturn pi, true''', '''    delete(m.pending, requestID)
    if pi.cancelCh != nil { close(pi.cancelCh) }
    return pi, true''')
    replace(path,'''func (m *InteractionManager) watchTimeout(pi *PendingInteraction) {
\ttimer := time.NewTimer(pi.Timeout)''', '''func (m *InteractionManager) watchTimeout(pi *PendingInteraction) {
    m.mu.RLock()
    cancelCh, timeout := pi.cancelCh, pi.Timeout
    m.mu.RUnlock()
    m.watchRegistrationTimeout(pi, cancelCh, timeout)
}

func (m *InteractionManager) watchRegistrationTimeout(pi *PendingInteraction, cancelCh chan struct{}, timeout time.Duration) {
    timer := time.NewTimer(timeout)''')
    replace(path,'\tcase <-pi.cancelCh:\n\t\t// Cancelled by CancelAll — interaction already removed from map.', '\tcase <-cancelCh:\n\t\t// This exact registration was completed or cancelled.')
    replace(path,'''\tclaimed, ok := m.Claim(pi.ID)
\tif !ok {
\t\treturn
\t}
\tif _, ok := m.CompleteClaimed(pi.ID); !ok {
\t\treturn
\t}''', '''    claimed, ok := m.completeTimedOutRegistration(pi, cancelCh)
    if !ok { return }''')
    marker='// CancelAll removes all pending interactions for a given session.'
    helper='''// completeTimedOutRegistration performs identity comparison, claim and removal
// in one critical section. A timer already firing during Complete/CancelAll
// cannot claim a newer request that happens to reuse the worker request ID.
func (m *InteractionManager) completeTimedOutRegistration(pi *PendingInteraction, cancelCh chan struct{}) (*PendingInteraction, bool) {
    m.mu.Lock()
    defer m.mu.Unlock()
    current, ok := m.pending[pi.ID]
    if !ok || current != pi || current.cancelCh != cancelCh || current.resolving {
        return nil, false
    }
    current.resolving = true
    delete(m.pending, pi.ID)
    if cancelCh != nil { close(cancelCh) }
    // The response runs outside the lock. Copy the registration fields so a
    // later registration of the same input object cannot change its kind.
    snapshot := *current
    return &snapshot, true
}

'''
    replace(path,marker,helper+marker)
    # All fields used after claiming belong to the captured registration.
    p=__import__('pathlib').Path(path)
    text=p.read_text();a=text.index('\tm.log.Info("interaction: timeout, auto-denying"');b=text.index('// completeTimedOutRegistration performs',a)
    section=text[a:b].replace('pi.ID','claimed.ID').replace('pi.Type','claimed.Type').replace('pi.SessionID','claimed.SessionID')
    p.write_text(text[:a]+section+text[b:])


if __name__=='__main__':main(tests,fixes)
