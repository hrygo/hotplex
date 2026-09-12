#!/usr/bin/env python3
"""Apply test-split/dead-helper corrections, then run unchanged quality gates."""
from pathlib import Path
import os


def normalize_tests_and_helpers(issue):
    if issue == 'D01':
        path = Path('internal/worker/codexcli/documented_audit_test.go')
        text = path.read_text()
        old = '\ts := &documentedSink{m: m, reply: true, result: json.RawMessage(`{"thread":{"id":"documented-thread"},"turn":{"id":"documented-turn"}}`)}'
        new = '\ts := &documentedSink{m: m}\n\ts.setResult(true, false, `{"thread":{"id":"documented-thread"},"turn":{"id":"documented-turn"}}`)'
        assert text.count(old) == 1
        text = text.replace(old, new)
        old = '\treturn w, s\n'
        new = '\tframes := s.snapshot()\n\trequire.NotEmpty(t, frames, "fixture must traverse the real protocol encoder")\n\trequire.Equal(t, "thread/start", frames[0].Method)\n\treturn w, s\n'
        assert text.count(old) == 1
        path.write_text(text.replace(old, new))
    if issue == 'D07':
        path = Path('internal/worker/codexcli/manager.go')
        text = path.read_text()
        old = '// writeRequest marshals and writes a JSON-RPC request to stdin.\nfunc (m *CodexAppServerManager) writeRequest(ctx context.Context, req *JSONRPCRequest) error {\n\treturn m.writeFrame(ctx, req)\n}\n\n'
        assert text.count(old) == 1
        path.write_text(text.replace(old, ''))


if __name__ == '__main__':
    original = Path(os.environ['RUNNER_TEMP']) / 'publish-runtime-series.py'
    text = original.read_text()
    anchor = "    apply(item['fix_patch'], issue + '-fix.patch')\n"
    assert text.count(anchor) == 1
    text = text.replace(anchor, anchor + '    normalize_tests_and_helpers(issue)\n')
    exec(compile(text, str(original), 'exec'), {'__name__':'__main__', 'normalize_tests_and_helpers':normalize_tests_and_helpers})
