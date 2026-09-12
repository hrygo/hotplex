from common import replace, main
from case_988 import tests, fixes as original_fixes


def fixes():
    original_fixes()
    path='internal/worker/codexcli/worker_test.go'
    # These internal fixtures previously supplied a manager and an arbitrary
    # unregistered channel. Model an actual manager-owned subscription while
    # retaining capacities and every existing delivery/backpressure assertion.
    for capacity in (5,1):
        old=f'\tch := make(chan *events.Envelope, {capacity})\n\tconn := &appConn{{'
        new=f'\tch := make(chan *events.Envelope, {capacity})\n\tmgr.subscribers["fixture-thread"] = ch\n\tmgr.subSessions["fixture-thread"] = "sess-1"\n\tconn := &appConn{{'
        replace(path,old,new)
    replace(path,'\t// Fill channel\n\tconn.TrySend(events.NewEnvelope("id-1", "sess-1", 1, events.Done, events.DoneData{}))', '\t// Fill channel; prove the first event was admitted before testing saturation.\n\trequire.True(t, conn.TrySend(events.NewEnvelope("id-1", "sess-1", 1, events.Done, events.DoneData{})))\n\tt.Cleanup(func(){ _ = conn.Close() })')


if __name__=='__main__':main(tests,fixes)
