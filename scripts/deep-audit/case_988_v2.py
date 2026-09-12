from pathlib import Path
from common import replace, main
from case_988 import tests, fixes as original_fixes


def fixes():
    original_fixes()
    path='internal/worker/codexcli/worker_test.go'
    text=Path(path).read_text()
    # Preserve each fixture's capacity and all existing assertions. Restrict
    # the edit to its named function; other capacity-one channels are unrelated.
    for name,capacity in [('TestAppConnSendRecvClose',5),('TestAppConnTrySendFull',1)]:
        marker=f'func {name}(t *testing.T) {{'
        assert text.count(marker)==1
        start=text.index(marker)
        end=text.index('\nfunc ',start+len(marker))
        section=text[start:end]
        old=f'\tch := make(chan *events.Envelope, {capacity})\n\tconn := &appConn{{'
        new=f'\tch := make(chan *events.Envelope, {capacity})\n\tmgr.subscribers["fixture-thread"] = ch\n\tmgr.subSessions["fixture-thread"] = "sess-1"\n\tconn := &appConn{{'
        assert section.count(old)==1
        section=section.replace(old,new)
        text=text[:start]+section+text[end:]
    Path(path).write_text(text)
    replace(path,'\t// Fill channel\n\tconn.TrySend(events.NewEnvelope("id-1", "sess-1", 1, events.Done, events.DoneData{}))', '\t// Prove admission before checking saturation.\n\trequire.True(t, conn.TrySend(events.NewEnvelope("id-1", "sess-1", 1, events.Done, events.DoneData{})))\n\tt.Cleanup(func(){ _ = conn.Close() })')


if __name__=='__main__':main(tests,fixes)
