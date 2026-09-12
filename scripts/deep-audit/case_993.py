from common import write, replace, between, main


def tests():
    write('internal/messaging/feishu/deep_audit_993_test.go',r'''
package feishu

import (
    "context"
    "sync/atomic"
    "testing"
    "testing/synctest"
    "time"

    "github.com/stretchr/testify/require"
)

func TestDeepAudit993AdapterShutdownHonorsCallerDeadline(t *testing.T){
    t.Parallel()
    synctest.Test(t,func(t *testing.T){
        q:=NewChatQueue(nil)
        a:=&Adapter{chatQueue:q}
        entered:=make(chan struct{})
        require.NoError(t,q.Enqueue("chat",func(ctx context.Context)error{close(entered);<-ctx.Done();return ctx.Err()}))
        <-entered
        var tail atomic.Int32
        require.NoError(t,q.Enqueue("chat",func(context.Context)error{tail.Add(1);return nil}))
        ctx,cancel:=context.WithTimeout(context.Background(),time.Second);defer cancel()
        start:=time.Now()
        err:=a.Close(ctx)
        elapsed:=time.Since(start)
        synctest.Wait()
        require.ErrorIs(t,err,context.DeadlineExceeded)
        require.LessOrEqual(t,elapsed,2*time.Second)
        require.Zero(t,tail.Load(),"expired shutdown must not begin queued side effects")
        require.ErrorIs(t,q.Enqueue("chat",func(context.Context)error{return nil}),ErrChatQueueClosed)
        q.Close()
    })
}

func TestDeepAudit993UncooperativeTaskDoesNotHoldAdapterClose(t *testing.T){
    t.Parallel()
    synctest.Test(t,func(t *testing.T){
        q:=NewChatQueue(nil)
        a:=&Adapter{chatQueue:q}
        entered,release:=make(chan struct{}),make(chan struct{})
        require.NoError(t,q.Enqueue("chat",func(context.Context)error{close(entered);<-release;return nil}))
        <-entered
        ctx,cancel:=context.WithTimeout(context.Background(),time.Second);defer cancel()
        result:=make(chan error,1)
        go func(){result<-a.Close(ctx)}()
        <-time.After(2*time.Second) // deterministic virtual time
        synctest.Wait()
        timely:=false
        var err error
        select{case err=<-result:timely=true;default:}
        // Always release the external task before reporting a baseline failure.
        close(release)
        if !timely{err=<-result}
        q.Close()
        require.True(t,timely,"Adapter.Close ignored its budget")
        require.ErrorIs(t,err,context.DeadlineExceeded)
    })
}

func TestDeepAudit993LegacyCloseStillDrainsAcceptedTasks(t *testing.T){
    t.Parallel()
    q:=NewChatQueue(nil)
    var completed atomic.Int32
    for i:=0;i<16;i++{require.NoError(t,q.Enqueue("chat",func(ctx context.Context)error{
        require.NoError(t,ctx.Err())
        completed.Add(1)
        return nil
    }))}
    q.Close()
    q.Close()
    require.EqualValues(t,16,completed.Load())
}
''')


def fixes():
    path='internal/messaging/feishu/chat_queue.go'
    between(path,'type ChatQueue struct {','type chatWorker struct {',r'''
type ChatQueue struct {
    log *slog.Logger
    mu sync.Mutex
    workers map[string]*chatWorker
    closed bool
    wg sync.WaitGroup
    lifecycle context.Context
    cancel context.CancelFunc
    closeDone chan struct{}
    closeWait sync.Once
}
''')
    replace(path,'\treturn &ChatQueue{','\tlifecycle,cancel:=context.WithCancel(context.Background())\n\treturn &ChatQueue{\n\t\tlifecycle:lifecycle, cancel:cancel, closeDone:make(chan struct{}),')
    replace(path,'\tctx, cancel := context.WithTimeout(context.Background(), chatTaskTimeout)', '''    if q.lifecycle.Err()!=nil {
        if q.log!=nil {q.log.Warn("feishu: queued task discarded after shutdown deadline","chat_id",chatID)}
        return
    }
    ctx, cancel := context.WithTimeout(q.lifecycle, chatTaskTimeout)''')
    # Close is the final function in this fixed source snapshot.
    replace(path,'''func (q *ChatQueue) Close() {
\tq.mu.Lock()
\tif !q.closed {
\t\tq.closed = true
\t\tfor _, w := range q.workers {
\t\t\tclose(w.tasks)
\t\t}
\t}
\tq.mu.Unlock()
\tq.wg.Wait()
}''',r'''
func (q *ChatQueue) Close() {
    _ = q.CloseContext(context.Background())
}

// CloseContext gracefully drains within the caller's budget. When that budget
// expires, active tasks are canceled and queued tasks are not started. A task
// ignoring cancellation may outlive this call; only one queue-owned waiter is
// retained, rather than spawning a new waiter for every shutdown caller.
func (q *ChatQueue) CloseContext(ctx context.Context) error {
    q.mu.Lock()
    if q.closeDone==nil {q.closeDone=make(chan struct{})}
    if q.cancel==nil {q.lifecycle,q.cancel=context.WithCancel(context.Background())}
    if !q.closed {
        q.closed=true
        for _,w:=range q.workers{close(w.tasks)}
    }
    done:=q.closeDone
    cancel:=q.cancel
    q.closeWait.Do(func(){go func(){q.wg.Wait();cancel();close(done)}()})
    q.mu.Unlock()
    select{case <-done:return nil;default:}
    select {
    case <-done:return nil
    case <-ctx.Done():cancel();return ctx.Err()
    }
}
'''.strip())
    path='internal/messaging/feishu/adapter.go'
    replace(path,'''\t// Close chat queue to drain all worker goroutines.
\tif a.chatQueue != nil {
\t\ta.chatQueue.Close()
\t}''','''    // Keep the adapter's shutdown budget while draining accepted tasks.
    var queueErr error
    if a.chatQueue != nil {
        queueErr = a.chatQueue.CloseContext(ctx)
    }''')
    replace(path,'''\treturn nil
}

func controlFeedbackMessageCN''','''    return queueErr
}

func controlFeedbackMessageCN''')


if __name__=='__main__':main(tests,fixes)
