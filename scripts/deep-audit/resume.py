"""Resume existing exact-source repair recipes; bootstrap-only, never shipped."""
from pathlib import Path
import importlib
import os
import runpy
import subprocess
import sys

HERE = Path(__file__).parent
BASE = 'fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3'
issue = int(os.environ['AUDIT_ISSUE'])

# Correct test harness signatures. This edits generated tests, never a product
# API and never a test assertion. The prior Shutdown() calls did not compile.
import common
original_write = common.write
original_replace = common.replace

def checked_write(path, text):
    if path.endswith('_test.go') and 'm.Shutdown()' in text:
        text = text.replace('m.Shutdown()', 'm.Shutdown(context.Background())')
        if '"context"' not in text:
            text = text.replace('import (', 'import (\n    "context"', 1)
    return original_write(path, text)

def checked_replace(path, old, new, count=1):
    if issue == 991 and old == '\tstateStopped\n':
        old = '\tstateStopped                      // gateway shutdown\n'
        new = old + '\tstateRetiring // reclamation owns the current process\n'
    return original_replace(path, old, new, count)

common.write = checked_write
common.replace = checked_replace
variant = HERE / f'case_{issue}_v2.py'
module = importlib.import_module(f'case_{issue}_v2' if variant.exists() else f'case_{issue}')
original_fixes = module.fixes

def repaired_fixes():
    original_fixes()
    if issue == 990:
        # This fixture manually establishes two already-acquired wrappers.
        # Its new explicit lease field must match the fixture's existing refs=2;
        # keep its sibling-isolation and refcount assertions unchanged.
        path = 'internal/worker/codexcli/worker_test.go'
        for name in ('workerA', 'workerB'):
            old = name + ' := &AppServerWorker{\n'
            original_replace(path, old, old + '\t\tmanagerRefHeld: true,\n')

module.fixes = repaired_fixes

runner = (HERE / 'runner.py').read_text()
runner = runner.replace("BASE='68b9f7f0cd11e2dbf603971c4a987fa2de682fb0'", f"BASE='{BASE}'")
runner = runner.replace("'-timeout=10m',*PACKAGES", "'-timeout=3m',*PACKAGES")
# Map published GitHub issues to the actual source-document IDs, not the
# temporary HP numbering from an earlier chat-only draft.
docs = {
    988: 'D01-codex-subscription-ownership.md',
    990: 'D02-codex-reference-release.md',
    991: 'D04-codex-process-retirement.md',
    993: 'D06-feishu-shutdown-deadline.md',
    994: 'D07-codex-pending-on-disconnect.md',
}
if issue not in docs:
    raise SystemExit(f'No documented issue mapping for {issue}')
issue_path = 'docs/issues/2026-09-runtime-audit/' + docs[issue]
assert Path(issue_path).is_file()
anchor = "    run('lint.txt',['golangci-lint','run','--timeout=10m'])"
assert runner.count(anchor) == 1
addition = f'''    issue_doc = Path({issue_path!r})
    original = issue_doc.read_text()
    original = original.replace('状态：已登记，待修复', '状态：修复及回归已完成，发布状态见运行结果')
    original = original.replace('证据：Source；本批运行证据尚未生成', '证据：Test；原实现失败、修复后通过；未执行Live联调')
    original += '\\n## 本批修复与验证\\n\\n'
    original += f'对应 GitHub Issue #{{ISSUE}}；实现基准 `{{BASE}}`。独立验证运行：{{url}}。\\n\\n'
    original += '| 范围 | 通过 | 失败 | 跳过 |\\n| --- | ---: | ---: | ---: |\\n' + rows + '\\n\\n'
    original += '计数包含重复轮次和父子用例。相关跳过项见同一提交的架构验证报告。完整lint、原始hooks和发布尚由后续步骤确认；不因文档存在就宣称远端已交付。\\n'
    issue_doc.write_text(original)
'''
runner = runner.replace(anchor, addition + anchor)
runner = runner.replace("['git','add','--','internal','pkg',str(doc)]", "['git','add','--','internal','pkg',str(doc),str(issue_doc)]")
# Print actual failed assertions and active tests even when a package times out.
old = "    else:print('\\n'.join(text.splitlines()[-100:]),flush=True)"
new = "    else:print('\\n'.join(text.splitlines()[-220:]),flush=True)"
assert old in runner
runner = runner.replace(old,new)
prepared = HERE / 'runner-resumed.py'
prepared.write_text(runner)
runpy.run_path(str(prepared), run_name='__main__')
