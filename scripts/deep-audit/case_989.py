from common import write,replace,main


def tests():
    write('internal/worker/codexcli/deep_audit_989_test.go',r'''
package codexcli

import (
    "context"
    "sync"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

type deep989Context struct {
    context.Context
    entered chan struct{}
    proceed chan struct{}
    once sync.Once
}
func(c *deep989Context) Deadline()(time.Time,bool){
    c.once.Do(func(){close(c.entered);<-c.proceed})
    return time.Time{},false
}

func TestDeepAudit989QueuedFrameCannotMoveToReplacementTransport(t *testing.T){
    t.Parallel()
    m:=auditRPCManager()
    oldWriter,newWriter:=&auditRPCSink{},&auditRPCSink{}
    m.stdin=oldWriter
    ctx:=&deep989Context{Context:context.Background(),entered:make(chan struct{}),proceed:make(chan struct{})}
    result:=make(chan error,1)
    m.writeMu.Lock()
    go func(){result<-m.Notify(ctx,"old-generation/probe",map[string]string{"request":"old"})}()
    <-ctx.entered
    // The call has entered writeFrame but cannot encode. This transfer is
    // channel-ordered, so the test itself never races on the source fixture.
    m.stdin=newWriter
    close(ctx.proceed)
    m.writeMu.Unlock()
    require.NoError(t,<-result)
    require.Empty(t,newWriter.String(),"an old call migrated to a new process stdin")
    require.Contains(t,oldWriter.String(),"old-generation/probe")
}
''')


def fixes():
    path='internal/worker/codexcli/manager.go'
    replace(path,'\twriteMu sync.Mutex','\twriteMu sync.Mutex\n\tstdinMu sync.RWMutex // pointer ownership only; never held across Encode')
    replace(path,'\tm.stdin = stdin','\tm.setTransportWriter(stdin)')
    replace(path,'\t\tm.stdin = nil','\t\tm.setTransportWriter(nil)')
    replace(path,'\tm.stdin = nil','\tm.setTransportWriter(nil)')
    replace(path,'\t// 0: queued; 1: encoder owns frame; 2: caller abandoned before encoding.', '''    // Capture before queueing. Lifecycle replacement must never redirect a
    // previously admitted frame to a different process. The state lock is
    // distinct from m.mu because startup handshake already owns m.mu.
    writer := m.transportWriter()
    if writer == nil {return &writeNotStartedError{cause:io.ErrClosedPipe}}
    // 0: queued; 1: encoder owns frame; 2: caller abandoned before encoding.''')
    replace(path,'json.NewEncoder(m.stdin).Encode(v)','json.NewEncoder(writer).Encode(v)')
    replace(path,'// writeFrame serializes a JSON-RPC frame to stdin. Caller must not hold m.mu.','// writeFrame serializes a JSON-RPC frame to its captured transport. Startup may hold m.mu.')
    write('internal/worker/codexcli/transport_writer.go',r'''
package codexcli

import "io"

// setTransportWriter runs during process lifecycle changes. It does not wait
// for pipe I/O: a closing old pipe may still own writeMu until the OS wakes it.
func(m *CodexAppServerManager) setTransportWriter(writer io.WriteCloser){
    m.stdinMu.Lock()
    m.stdin=writer
    m.stdinMu.Unlock()
}

func(m *CodexAppServerManager) transportWriter() io.WriteCloser {
    m.stdinMu.RLock()
    defer m.stdinMu.RUnlock()
    return m.stdin
}
''')
    write('internal/worker/codexcli/transport_writer_test.go',r'''
package codexcli

import (
    "context"
    "io"
    "sync"
    "testing"

    "github.com/stretchr/testify/require"
)

type deep989Discard struct{}
func(deep989Discard) Write(p []byte)(int,error){return len(p),nil}
func(deep989Discard) Close()error{return nil}

func TestDeepAudit989NilAndConcurrentTransportSnapshots(t *testing.T){
    t.Parallel()
    m:=auditRPCManager()
    m.setTransportWriter(nil)
    require.ErrorIs(t,m.Notify(context.Background(),"probe",nil),io.ErrClosedPipe)
    var wg sync.WaitGroup
    wg.Add(2)
    go func(){defer wg.Done();for i:=0;i<1000;i++{m.setTransportWriter(deep989Discard{})}}()
    go func(){defer wg.Done();for i:=0;i<1000;i++{_ = m.Notify(context.Background(),"probe",nil)}}()
    wg.Wait()
}
''')


if __name__=='__main__':main(tests,fixes)
