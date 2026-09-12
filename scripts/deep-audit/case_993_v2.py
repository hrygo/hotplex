from pathlib import Path
from common import replace,between,main
from case_993 import tests


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
    p=Path(path);source=p.read_text();start=source.index('func (q *ChatQueue) Close() {')
    assert source[start:].count('func ')==1 and 'q.wg.Wait() // wait for all workers to finish' in source[start:]
    p.write_text(source[:start]+r'''
func (q *ChatQueue) Close() {
    _ = q.CloseContext(context.Background())
}

// CloseContext drains within the caller budget, then cancels active tasks and
// prevents queued tasks from starting. Uncooperative tasks may outlive the call;
// one queue-owned waiter is retained, never a new waiter per caller.
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
'''.lstrip())
    path='internal/messaging/feishu/adapter.go'
    replace(path,'''\t// Close chat queue to drain all worker goroutines.
\tif a.chatQueue != nil {
\t\ta.chatQueue.Close()
\t}''','''    // Preserve the caller's deadline while draining accepted chat tasks.
    var queueErr error
    if a.chatQueue != nil {
        queueErr = a.chatQueue.CloseContext(ctx)
    }''')
    replace(path,'\treturn nil\n}\n\nfunc controlFeedbackMessageCN','    return queueErr\n}\n\nfunc controlFeedbackMessageCN')


if __name__=='__main__':main(tests,fixes)
