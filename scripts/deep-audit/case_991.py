from common import write,replace,between,main


def tests():
    write('internal/worker/codexcli/deep_audit_991_test.go',r'''
package codexcli

import (
    "context"
    "log/slog"
    "sync"
    "testing"

    "github.com/stretchr/testify/require"
    "github.com/hrygo/hotplex/internal/worker/proc"
)

type deep991ExitObserver struct {
    manager *CodexAppServerManager
    observed chan managerState
    once sync.Once
}
func(h *deep991ExitObserver) Enabled(context.Context,slog.Level)bool{return true}
func(h *deep991ExitObserver) WithAttrs([]slog.Attr)slog.Handler{return h}
func(h *deep991ExitObserver) WithGroup(string)slog.Handler{return h}
func(h *deep991ExitObserver) Handle(_ context.Context,r slog.Record)error{
    if r.Message=="codex-app-server: process exited" || r.Message=="codex-app-server: process crashed" {
        // Both original and repaired monitor log this boundary while holding
        // manager.mu. Observe its invariant without introducing a test race.
        h.once.Do(func(){h.observed<-h.manager.state})
    }
    return nil
}

func TestDeepAudit991ExitCannotAdvertiseIdleBeforeOwnedCleanup(t *testing.T){
    t.Parallel()
    m:=auditRPCManager()
    m.proc=&proc.Manager{}
    m.refs=0
    observer:=&deep991ExitObserver{manager:m,observed:make(chan managerState,1)}
    m.log=slog.New(observer)
    m.Subscribe("old","old-session")
    m.getOrCreateConverter("old")
    // Block old converter cleanup. A process state of idle at this boundary
    // would permit the next Acquire before stale resources were retired.
    m.convMu.Lock()
    finished:=make(chan struct{})
    go func(){m.monitorProcess();close(finished)}()
    state:=<-observer.observed
    m.convMu.Unlock()
    <-finished
    require.NotEqual(t,stateIdle,state,"old generation exposed idle before cleanup")
    require.Equal(t,stateIdle,m.state)
    require.Empty(t,m.subscribers)
    require.Empty(t,m.converters)
}
''')


def fixes():
    path='internal/worker/codexcli/manager.go'
    replace(path,'\tstateStopped\n','\tstateStopped\n\tstateRetiring // exit/reclamation owns the process; Acquire must not reuse it\n')
    replace(path,'go m.monitorProcess()','go m.monitorOwnedProcess(m.proc)')
    between(path,'// monitorProcess waits','func (m *CodexAppServerManager) buildEnv()',r'''
// monitorProcess preserves the internal compatibility entry point. Production
// startup passes its process directly to monitorOwnedProcess before spawning.
func (m *CodexAppServerManager) monitorProcess() {
    m.mu.Lock()
    owned:=m.proc
    m.mu.Unlock()
    m.monitorOwnedProcess(owned)
}

func (m *CodexAppServerManager) monitorOwnedProcess(owned *proc.Manager) {
    if owned==nil{return}
    code,_:=owned.Wait()
    m.finishOwnedProcess(owned,code)
}

func (m *CodexAppServerManager) finishOwnedProcess(owned *proc.Manager,code int) {
    m.mu.Lock()
    defer m.mu.Unlock()
    // A failed startup/replacement may already have installed another process.
    // Neither its subscribers nor its transport belong to this exit callback.
    if m.proc!=owned{return}
    wasRunning:=m.state==stateRunning || m.state==stateRetiring
    unexpected:=m.state==stateRunning && m.refs>0
    if m.state!=stateStopped{m.state=stateRetiring}
    if m.idleTimer!=nil{m.idleTimer.Stop();m.idleTimer=nil}
    if m.cancel!=nil{m.cancel();m.cancel=nil}
    if unexpected {
        m.log.Warn("codex-app-server: process crashed","exit_code",code,"refs",m.refs)
    } else {
        m.log.Info("codex-app-server: process exited","exit_code",code,"refs",m.refs)
    }
    // Keep the acquisition mutex until all old-generation metadata is retired.
    // No Wait/Kill or blocking OS operation runs while this lock is held.
    if wasRunning {
        m.clearConverters()
        m.clearAllServerRequests()
        m.subsClosed.Store(true)
        m.subMu.Lock()
        for id,ch:=range m.subscribers{close(ch);delete(m.subscribers,id)}
        m.subSessions=make(map[string]string)
        m.subMu.Unlock()
    }
    m.proc=nil
    m.pgid=0
    m.stdin=nil
    m.stdout=nil
    if unexpected {
        m.crashExitCode=code
        close(m.crashCh)
        m.crashCh=make(chan struct{})
    }
    if m.state!=stateStopped{m.state=stateIdle}
}

// claimIdleProcessLocked linearizes zero-reference reclamation against Acquire.
// The returned PGID belongs to owned; a later Acquire sees retiring, not running.
func(m *CodexAppServerManager) claimIdleProcessLocked(owned *proc.Manager)(int,bool){
    if m.proc!=owned || m.refs!=0 || m.state!=stateRunning || m.pgid<=0{return 0,false}
    m.state=stateRetiring
    if m.idleTimer!=nil{m.idleTimer.Stop();m.idleTimer=nil}
    return m.pgid,true
}

func (m *CodexAppServerManager) startIdleDrainLocked() {
    m.log.Info("codex-app-server: starting idle drain timer","period",m.cfg.IdleDrainPeriod)
    owned:=m.proc
    var timer *time.Timer
    timer=time.AfterFunc(m.cfg.IdleDrainPeriod,func(){
        m.mu.Lock()
        if m.idleTimer!=timer{m.mu.Unlock();return}
        pgid,claimed:=m.claimIdleProcessLocked(owned)
        m.mu.Unlock()
        if claimed {
            m.log.Info("codex-app-server: idle drain expired, killing process")
            _=proc.ForceKill(pgid)
            proc.ForceKillTree(pgid,m.log)
        }
    })
    m.idleTimer=timer
}

// KillIfIdle claims retirement under m.mu, then performs process I/O unlocked.
func (m *CodexAppServerManager) KillIfIdle() {
    m.mu.Lock()
    if m.idleTimer!=nil{m.idleTimer.Stop();m.idleTimer=nil}
    pgid,claimed:=m.claimIdleProcessLocked(m.proc)
    m.mu.Unlock()
    if claimed {
        m.log.Info("codex-app-server: killing idle process immediately","pgid",pgid)
        _=proc.ForceKill(pgid)
        proc.ForceKillTree(pgid,m.log)
    }
}
''')
    write('internal/worker/codexcli/process_retirement_test.go',r'''
package codexcli

import (
    "context"
    "testing"
    "github.com/stretchr/testify/require"
    "github.com/hrygo/hotplex/internal/worker/proc"
)

func TestDeepAudit991StaleExitCannotClearReplacement(t *testing.T){
    t.Parallel()
    m:=auditRPCManager()
    old,current:=&proc.Manager{},&proc.Manager{}
    m.proc=current
    m.refs=1
    channel:=m.Subscribe("new","new-session")
    mapper:=m.getOrCreateConverter("new")
    m.finishOwnedProcess(old,42)
    require.Same(t,current,m.proc)
    require.Equal(t,stateRunning,m.state)
    require.Equal(t,channel,m.subscribers["new"])
    require.Same(t,mapper,m.getConverter("new"))
    m.Unsubscribe("new")
}

func TestDeepAudit991IdleClaimRejectsNewAcquireAndWrongGeneration(t *testing.T){
    t.Parallel()
    m:=auditRPCManager()
    owned:=&proc.Manager{}
    m.proc=owned;m.pgid=123456;m.refs=0
    // These tests exercise the claim only, never signal a real process group.
    m.mu.Lock()
    _,claimed:=m.claimIdleProcessLocked(&proc.Manager{})
    require.False(t,claimed)
    pgid,claimed:=m.claimIdleProcessLocked(owned)
    m.mu.Unlock()
    require.True(t,claimed)
    require.Equal(t,123456,pgid)
    _,err:=m.Acquire(context.Background())
    require.Error(t,err)
    require.Zero(t,m.refs)
    require.Equal(t,stateRetiring,m.state)
    m.finishOwnedProcess(owned,0)
    require.Equal(t,stateIdle,m.state)
}
''')


if __name__=='__main__':main(tests,fixes)
