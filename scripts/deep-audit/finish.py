"""Run a documented repair against the pinned source tree with original hooks."""
from pathlib import Path
import os
import runpy

HERE=Path(__file__).parent
BASE='fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3'
ISSUE=int(os.environ['AUDIT_ISSUE'])
DOCS={988:'D01-codex-subscription-ownership.md',1002:'D10-interaction-timeout-generation.md',1003:'D08-codex-interrupt-rpc.md',1004:'D09-codex-lifecycle-response-validation.md'}
if ISSUE not in DOCS:raise SystemExit('No source issue document')
issue_path='docs/issues/2026-09-runtime-audit/'+DOCS[ISSUE]
assert Path(issue_path).is_file()
import common
original_write=common.write

def checked_write(path,text):
    if path.endswith('_test.go') and 'm.Shutdown()' in text:
        text=text.replace('m.Shutdown()','m.Shutdown(context.Background())')
        if '"context"' not in text:text=text.replace('import (','import (\n    "context"',1)
    return original_write(path,text)
common.write=checked_write

runner=(HERE/'runner.py').read_text()
runner=runner.replace("BASE='68b9f7f0cd11e2dbf603971c4a987fa2de682fb0'",f"BASE='{BASE}'")
lines=runner.splitlines()
package_line=next(line for line in lines if line.startswith('PACKAGES='))
packages=['./internal/messaging'] if ISSUE==1002 else ['./internal/worker/codexcli','./internal/worker/base']
runner=runner.replace(package_line,'PACKAGES='+repr(packages))
runner=runner.replace("'-timeout=10m',*PACKAGES","'-timeout=3m',*PACKAGES")
anchor="    run('lint.txt',['golangci-lint','run','--timeout=10m'])"
assert runner.count(anchor)==1
addition=f'''    issue_doc=Path({issue_path!r})
    text=issue_doc.read_text()
    text=text.replace('状态：已登记，待修复','状态：修复与回归已通过；发布结果见验证运行和关联PR')
    text=text.replace('证据：Source；本批运行证据尚未生成','证据：Test；同一回归修复前失败、修复后通过；未执行Live验收')
    text+='\\n## 本批独立修复证据\\n\\n'
    text+=f'GitHub Issue #{{ISSUE}}；基准 `{{BASE}}`；验证运行：{{url}}。\\n\\n'
    text+='| 范围 | 通过 | 失败 | 跳过 |\\n| --- | ---: | ---: | ---: |\\n'+rows+'\\n\\n'
    text+='计数包含父子测试和重复轮次。原有断言不削弱，不以编译失败冒充复现。相关跳过、lint、契约和原始提交/推送检查以架构报告与运行日志为准，未合并或部署。\\n'
    issue_doc.write_text(text)
'''
runner=runner.replace(anchor,addition+anchor)
runner=runner.replace("['git','add','--','internal','pkg',str(doc)]","['git','add','--','internal','pkg',str(doc),str(issue_doc)]")
prepared=HERE/'runner-finished.py'
prepared.write_text(runner)
runpy.run_path(str(prepared),run_name='__main__')
