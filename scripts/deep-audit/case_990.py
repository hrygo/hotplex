from common import write, replace, between, main


def tests():
    write('internal/worker/codexcli/deep_audit_990_test.go',r'''
package codexcli

import (
    "context"
    "encoding/json"
    "sync"
    "testing"

    "github.com/stretchr/testify/require"

    "github.com/hrygo/hotplex/internal/worker"
    "github.com/hrygo/hotplex/internal/worker/base"
)

type deep990Sink struct { manager *CodexAppServerManager }
func (s *deep990Sink) Close() error {return nil}
func (s *deep990Sink) Write(p []byte)(int,error){
    var frame JSONRPCRequest
    if err:=json.Unmarshal(p,&frame);err!=nil{return 0,err}
    if frame.ID!=0 {
        if pending,ok:=s.manager.pending.Load(frame.ID);ok {
            pending.(chan *JSONRPCResponse)<-&JSONRPCResponse{Result:json.RawMessage(`{"thread":{"id":"owned-thread"}}`)}
        }
    }
    return len(p),nil
}
func deep990Worker()(*AppServerWorker,*CodexAppServerManager){
    m:=auditRPCManager()
    m.stdin=&deep990Sink{manager:m}
    m.refs=1 // unrelated session, never owned by this worker
    return &AppServerWorker{BaseWorker:base.NewBaseWorker(nil,nil),manager:m},m
}

func TestDeepAudit990ResetFailureReleasesOnlyItsReference(t *testing.T){
    t.Parallel()
    w,m:=deep990Worker()
    require.NoError(t,w.Start(context.Background(),worker.SessionInfo{SessionID:"session",UserID:"user"}))
    require.Equal(t,2,m.refs)
    ctx,cancel:=context.WithCancel(context.Background());cancel()
    _,err:=w.ResetContext(ctx)
    require.ErrorIs(t,err,context.Canceled)
    require.NoError(t,w.Terminate(context.Background()))
    require.NoError(t,w.Terminate(context.Background()))
    require.Equal(t,1,m.refs,"connection closed is not proof that the acquired reference was released")
}

func TestDeepAudit990UnacquiredAndFailedStartNeverReleaseOthers(t *testing.T){
    t.Parallel()
    for _,attemptStart:=range []bool{false,true}{
        w,m:=deep990Worker()
        if attemptStart {
            ctx,cancel:=context.WithCancel(context.Background());cancel()
            require.ErrorIs(t,w.Start(ctx,worker.SessionInfo{SessionID:"session",UserID:"user"}),context.Canceled)
        }
        require.NoError(t,w.Terminate(context.Background()))
        require.NoError(t,w.Terminate(context.Background()))
        require.Equal(t,1,m.refs)
    }
}

func TestDeepAudit990ConcurrentTerminateReleasesOnce(t *testing.T){
    t.Parallel()
    w,m:=deep990Worker()
    require.NoError(t,w.Start(context.Background(),worker.SessionInfo{SessionID:"session",UserID:"user"}))
    var wg sync.WaitGroup
    for i:=0;i<8;i++{wg.Add(1);go func(){defer wg.Done();_ = w.Terminate(context.Background())}()}
    wg.Wait()
    require.Equal(t,1,m.refs)
}
''')


def fixes():
    path='internal/worker/codexcli/worker.go'
    replace(path,'\treleased bool','\treleased bool\n\tmanagerRefHeld bool')
    replace(path,'''\tw.crashSub = crashCh
\tw.mu.Unlock()''','''    if w.released || w.closed {
        w.mu.Unlock()
        w.manager.Release()
        return fmt.Errorf("codexcli: worker closed while acquiring manager")
    }
    w.crashSub = crashCh
    w.managerRefHeld = true
    w.mu.Unlock()''')
    replace(path,'''\tif err := w.startNewThread(ctx, session, "start"); err != nil {
\t\tw.manager.Release()
\t\treturn err
\t}''','''    if err := w.startNewThread(ctx, session, "start"); err != nil {
        w.releaseManagerReference()
        return err
    }''')
    between(path,'func (w *AppServerWorker) release(ctx context.Context) error {','func (w *AppServerWorker) ResetContext(',r'''
// releaseManagerReference is independent of connection/done-channel state.
// Start failure and normal teardown race through the same ownership transfer.
func (w *AppServerWorker) releaseManagerReference() {
    w.mu.Lock()
    held,mgr:=w.managerRefHeld,w.manager
    w.managerRefHeld=false
    w.mu.Unlock()
    if held && mgr!=nil{mgr.Release()}
}

func (w *AppServerWorker) release(ctx context.Context) error {
    w.mu.Lock()
    if w.released {w.mu.Unlock();return nil}
    w.released=true
    wasClosed:=w.closed
    w.closed=true
    doneCh,tid,conn,mgr:=w.doneCh,w.threadID,w.conn,w.manager
    held:=w.managerRefHeld
    w.managerRefHeld=false
    w.mu.Unlock()

    if !wasClosed && doneCh!=nil {close(doneCh)}
    var unsubscribeErr error
    if mgr!=nil && tid!="" {
        unsubscribeErr=mgr.Notify(ctx,"thread/unsubscribe",ThreadUnsubscribeParams{ThreadID:tid})
        mgr.Unsubscribe(tid)
    }
    if conn!=nil {_ = conn.Close()}
    if held && mgr!=nil {mgr.Release()}
    return unsubscribeErr
}
''')


if __name__=='__main__':main(tests,fixes)
