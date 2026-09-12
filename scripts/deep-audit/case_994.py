from common import write,replace,main


def tests():
    write('internal/worker/codexcli/deep_audit_994_test.go',r'''
package codexcli

import (
    "context"
    "io"
    "strings"
    "testing"
    "testing/synctest"

    "github.com/stretchr/testify/require"
)

func TestDeepAudit994PendingCallsFinishWhenTransportEnds(t *testing.T){
    t.Parallel()
    for _,source:=range []string{"shutdown","stdout_eof"}{
        t.Run(source,func(t *testing.T){
            t.Parallel()
            synctest.Test(t,func(t *testing.T){
                m:=auditRPCManager()
                ctx,cancel:=context.WithCancel(context.Background());defer cancel()
                result:=make(chan error,1)
                go func(){_,err:=m.Call(ctx,"pending/probe",nil);result<-err}()
                synctest.Wait()
                if source=="shutdown"{m.Shutdown()}else{m.readNotifications(context.Background(),strings.NewReader(""))}
                synctest.Wait()
                timely:=false
                var err error
                select{case err=<-result:timely=true;default:}
                if !timely{cancel();<-result}
                require.True(t,timely,"known response-stream termination left Call waiting for its timeout")
                require.ErrorIs(t,err,io.ErrUnexpectedEOF)
                count:=0;m.pending.Range(func(_, _ any)bool{count++;return true})
                require.Zero(t,count)
            })
        })
    }
}
''')


def fixes():
    path='internal/worker/codexcli/manager.go'
    replace(path,'\tpending sync.Map // map[int64]chan *JSONRPCResponse','\tpending sync.Map // map[int64]chan *JSONRPCResponse\n\trpcMu sync.Mutex\n\trpcRun *rpcGeneration')
    replace(path,'func (m *CodexAppServerManager) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {', '''func (m *CodexAppServerManager) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
    generation:=m.currentRPCGeneration()
    select {
    case <-generation.done:
        return nil, fmt.Errorf("codex-app-server: response stream closed before request: %w",io.ErrUnexpectedEOF)
    default:
    }''')
    replace(path,'\tcase <-ctx.Done():\n\t\treturn nil, &responseWaitError', '''    case <-generation.done:
        // A response already accepted before EOF wins over stream termination.
        select {
        case resp:=<-respCh:
            if resp.Error!=nil{return nil,fmt.Errorf("codex-app-server: %s: %s (code %d)",method,resp.Error.Message,resp.Error.Code)}
            return resp.Result,nil
        default:
            // The frame was written. Do not imply that retrying is safe.
            return nil,fmt.Errorf("codex-app-server: %s: response stream ended; execution outcome unknown: %w",method,io.ErrUnexpectedEOF)
        }
    case <-ctx.Done():
        return nil, &responseWaitError''')
    replace(path,'\tm.state = stateStopped','\tm.state = stateStopped\n\tm.currentRPCGeneration().finish()')
    replace(path,'\tm.state = stateStarting','\tm.state = stateStarting\n\tgeneration:=m.beginRPCGeneration()')
    replace(path,'go m.readNotifications(bgCtx, stdout)','go m.readNotificationsForGeneration(bgCtx, stdout, generation)')
    replace(path,'func (m *CodexAppServerManager) readNotifications(ctx context.Context, reader io.Reader) {', '''func (m *CodexAppServerManager) readNotifications(ctx context.Context, reader io.Reader) {
    m.readNotificationsForGeneration(ctx,reader,m.currentRPCGeneration())
}

func (m *CodexAppServerManager) readNotificationsForGeneration(ctx context.Context, reader io.Reader, generation *rpcGeneration) {
    defer generation.finish()''')
    write('internal/worker/codexcli/rpc_generation.go',r'''
package codexcli

import "sync"

// rpcGeneration owns completion of one process's response stream. Old reader
// cleanup only closes its captured generation, never a replacement's calls.
type rpcGeneration struct {
    once sync.Once
    done chan struct{}
}
func(g *rpcGeneration) finish(){g.once.Do(func(){close(g.done)})}
func(m *CodexAppServerManager) currentRPCGeneration()*rpcGeneration{
    m.rpcMu.Lock()
    defer m.rpcMu.Unlock()
    if m.rpcRun==nil{m.rpcRun=&rpcGeneration{done:make(chan struct{})}}
    return m.rpcRun
}
func(m *CodexAppServerManager) beginRPCGeneration()*rpcGeneration{
    m.rpcMu.Lock()
    defer m.rpcMu.Unlock()
    if m.rpcRun!=nil{m.rpcRun.finish()}
    m.rpcRun=&rpcGeneration{done:make(chan struct{})}
    return m.rpcRun
}
''')
    write('internal/worker/codexcli/rpc_generation_test.go',r'''
package codexcli

import (
    "context"
    "encoding/json"
    "strings"
    "testing"
    "testing/synctest"
    "github.com/stretchr/testify/require"
)

func TestDeepAudit994OldReaderCannotCancelNewGeneration(t *testing.T){
    t.Parallel()
    synctest.Test(t,func(t *testing.T){
        m:=auditRPCManager()
        old:=m.currentRPCGeneration()
        current:=m.beginRPCGeneration()
        ctx,cancel:=context.WithCancel(context.Background());defer cancel()
        result:=make(chan error,1)
        go func(){_,err:=m.Call(ctx,"new/probe",nil);result<-err}()
        synctest.Wait()
        m.readNotificationsForGeneration(context.Background(),strings.NewReader(""),old)
        select{case <-current.done:t.Fatal("old reader closed the new response lifetime");default:}
        m.pending.Range(func(_,value any)bool{value.(chan *JSONRPCResponse)<-&JSONRPCResponse{Result:json.RawMessage(`{"ok":true}`)};return true})
        require.NoError(t,<-result)
        m.Shutdown()
    })
}
''')


if __name__=='__main__':main(tests,fixes)
