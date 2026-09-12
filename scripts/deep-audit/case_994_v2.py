from common import write, replace, main
from case_994 import tests as original_tests, fixes as original_fixes


def tests():
    original_tests()
    write('internal/worker/codexcli/deep_audit_994_outcome_test.go',r'''
package codexcli

import (
    "context"
    "io"
    "testing"
    "testing/synctest"

    "github.com/stretchr/testify/require"
    "github.com/hrygo/hotplex/internal/worker"
    "github.com/hrygo/hotplex/internal/worker/base"
)

func TestDeepAudit994TurnDisconnectRetainsUnknownClassification(t *testing.T){
    t.Parallel()
    synctest.Test(t,func(t *testing.T){
        m:=auditRPCManager()
        w:=&AppServerWorker{BaseWorker:base.NewBaseWorker(nil,nil),manager:m,threadID:"thread"}
        ctx,cancel:=context.WithCancel(context.Background());defer cancel()
        result:=make(chan error,1)
        go func(){result<-w.startTurn(ctx,[]TurnInputItem{{Type:"text",Text:"probe"}})}()
        synctest.Wait()
        m.Shutdown(context.Background())
        synctest.Wait()
        var err error
        timely:=false
        select{case err=<-result:timely=true;default:cancel();<-result}
        require.True(t,timely,"a disconnected request kept waiting")
        require.ErrorIs(t,err,io.ErrUnexpectedEOF)
        var classified *worker.WorkerError
        require.ErrorAs(t,err,&classified)
        require.Equal(t,worker.ErrKindTimeout,classified.Kind,"unknown remote execution must not become a confirmed delivery failure")
    })
}
''')


def fixes():
    original_fixes()
    path='internal/worker/codexcli/manager.go'
    replace(path,'return nil,fmt.Errorf("codex-app-server: %s: response stream ended; execution outcome unknown: %w",method,io.ErrUnexpectedEOF)', 'return nil,&responseWaitError{cause:fmt.Errorf("codex-app-server: %s: response stream ended; execution outcome unknown: %w",method,io.ErrUnexpectedEOF)}')


if __name__=='__main__':main(tests,fixes)
