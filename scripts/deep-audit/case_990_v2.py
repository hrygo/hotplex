from common import replace,between,main
from case_990 import tests


def fixes():
    path='internal/worker/codexcli/worker.go'
    replace(path,'\treleased  bool','\treleased  bool\n\tmanagerRefHeld bool')
    replace(path,'\tw.crashSub = crashCh\n\tw.mu.Unlock()', '''    if w.released || w.closed {
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
// releaseManagerReference tracks the lease independently of connection state.
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
