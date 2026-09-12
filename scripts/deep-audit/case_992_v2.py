from pathlib import Path
from common import main
from case_992 import tests


def fixes():
    path=Path('internal/worker/opencodeserver/worker.go')
    source=path.read_text()
    start=source.index('func (w *Worker) ResetContext(')
    end=source.index('func (w *Worker) SendControlRequest(',start)
    body=source[start:end]
    old='\tif resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {'
    new='''    // A missing endpoint is not evidence that native history was cleared.
    // Preserve local generation and connection state on unsupported reset.
    if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
        return worker.ResetResult{}, fmt.Errorf("opencodeserver: reset unsupported (status %d): %w", resp.StatusCode, worker.ErrNotImplemented)
    }
    if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {'''
    assert body.count(old)==1
    path.write_text(source[:start]+body.replace(old,new)+source[end:])


if __name__=='__main__':main(tests,fixes)
