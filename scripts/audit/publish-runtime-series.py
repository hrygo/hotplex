#!/usr/bin/env python3
"""Bootstrap-only: red/green verification and hook-protected stacked commits."""
import hashlib
import json
import os
import re
import subprocess
from pathlib import Path

ROOT = Path.cwd()
EVIDENCE = Path(os.environ['RUNNER_TEMP']) / 'runtime-series-evidence'
EVIDENCE.mkdir(exist_ok=True)
DATA = json.loads((Path(os.environ['RUNNER_TEMP']) / 'runtime-patchset.json').read_text())
RUN_URL = f"https://github.com/{os.environ['GITHUB_REPOSITORY']}/actions/runs/{os.environ['GITHUB_RUN_ID']}"
MANIFEST = EVIDENCE / 'series.json'
RESULTS = []


def run(args, log_name=None, required=True):
    if log_name:
        with (EVIDENCE / log_name).open('w') as out:
            result = subprocess.run(args, stdout=out, stderr=subprocess.STDOUT, text=True)
    else:
        result = subprocess.run(args, text=True)
    if required and result.returncode:
        if log_name:
            print((EVIDENCE / log_name).read_text()[-12000:], flush=True)
        raise SystemExit(f"Command failed ({result.returncode}): {args}")
    return result.returncode


def output(args):
    return subprocess.check_output(args, text=True).strip()


def events(log):
    values = []
    for line in (EVIDENCE / log).read_text().splitlines():
        try:
            values.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    return values


def counts(values):
    return {action: sum(r.get('Action') == action and bool(r.get('Test')) for r in values)
            for action in ('pass', 'fail', 'skip')}


def verify_green(log, status, allow_skip=False):
    values = events(log)
    if status or any(r.get('Action') in ('fail', 'build-fail') for r in values):
        print((EVIDENCE / log).read_text()[-12000:], flush=True)
        raise SystemExit('Green verification failed: ' + log)
    result = counts(values)
    if not result['pass'] or (result['skip'] and not allow_skip):
        raise SystemExit('Missing or skipped green evidence: ' + log)
    return result, [r for r in values if r.get('Action') == 'skip' and r.get('Test')]


def apply(text, name):
    path = EVIDENCE / name
    path.write_text(text)
    run(['git', 'apply', '--check', str(path)])
    run(['git', 'apply', str(path)])


if output(['git', 'rev-parse', 'HEAD']) != DATA['baseline']:
    raise SystemExit('Unexpected source baseline')
if output(['git', 'config', '--get', 'core.hooksPath']) != 'scripts/git-hooks':
    raise SystemExit('Repository hooks are not installed')
if len(DATA['items']) != 10 or len({i['id'] for i in DATA['items']}) != 10:
    raise SystemExit('Ten independent records are required')

run(['git', 'config', 'user.name', 'HotPlex Audit'])
run(['git', 'config', 'user.email', '41898282+github-actions[bot]@users.noreply.github.com'])
parent_branch = DATA['base_branch']
for item in DATA['items']:
    issue = item['id']
    parent = output(['git', 'rev-parse', 'HEAD'])
    combined = item['test_patch'] + item['fix_patch']
    parts = [p for p in re.split(r'(?=^diff --git )', combined, flags=re.M) if p]
    parts.sort(key=lambda p: p.splitlines()[0].split(' b/', 1)[1])
    if hashlib.sha256(''.join(parts).encode()).hexdigest() != item['patch_sha256']:
        raise SystemExit('Patch digest mismatch: ' + issue)
    doc = Path('docs/issues/2026-09-runtime-audit') / (issue + '-' + item['slug'] + '.md')
    if not doc.is_file():
        raise SystemExit('Issue document must exist before its fix: ' + str(doc))
    run(['git', 'checkout', '-b', item['branch']])
    apply(item['test_patch'], issue + '-tests.patch')
    red_log = issue + '-red.jsonl'
    red_status = run(['go', 'test', '-json', '-race', '-count=1', '-shuffle=on', '-timeout=2m',
                      '-run', item['red_pattern'], item['package']], red_log, False)
    red = events(red_log)
    failures = {r['Test'].split('/')[0] for r in red if r.get('Action') == 'fail' and r.get('Test')}
    if not red_status or not set(item['required']).issubset(failures) or any(r.get('Action') == 'build-fail' for r in red):
        print((EVIDENCE / red_log).read_text()[-12000:], flush=True)
        raise SystemExit('Missing per-root baseline failure evidence: ' + issue)
    apply(item['fix_patch'], issue + '-fix.patch')
    run(['git', 'add', '-N', 'internal'])
    changed_go = output(['git', 'diff', '--name-only', '--', '*.go']).splitlines()
    run(['golangci-lint', 'fmt', *changed_go])
    run(['git', 'diff', '--check'])
    green_log = issue + '-green.jsonl'
    status = run(['go', 'test', '-json', '-race', '-count=5', '-shuffle=on', '-timeout=3m',
                  '-run', item['green_pattern'], item['package']], green_log, False)
    green_counts, _ = verify_green(green_log, status)
    broad_log = issue + '-related.jsonl'
    status = run(['go', 'test', '-json', '-short', '-race', '-count=1', '-shuffle=on', '-timeout=5m', item['package']], broad_log, False)
    broad_counts, skips = verify_green(broad_log, status, True)
    table = '\n'.join([
        '| 测试范围 | 通过 | 失败 | 跳过 |', '| --- | ---: | ---: | ---: |',
        '| 修复前定向回归 | {pass} | {fail} | {skip} |'.format(**counts(red)),
        '| 修复后定向回归，5轮race/shuffle | {pass} | {fail} | {skip} |'.format(**green_counts),
        '| 相关包短测，race/shuffle | {pass} | {fail} | {skip} |'.format(**broad_counts)])
    text = doc.read_text().replace('- 状态：已登记，待修复', '- 状态：修复已通过定向与相关测试，未合并')
    text = text.replace('- 证据：Source；本批运行证据尚未生成', '- 证据：Test；源码分析与运行复现已相互印证，非Live')
    text = text.replace('- 实施PR：待创建', '- 实施PR：对应分支 `' + item['branch'] + '`；目标分支 `' + parent_branch + '`（堆叠依赖）')
    text += '\n\n## 本次验证\n\n父提交：`' + parent + '`。验证运行：' + RUN_URL + '\n\n' + table
    text += '\n\n计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。\n'
    if issue == 'D03':
        text += '\n基线仅执行排队写入代际用例；缺失stdin安全性用例在修复后执行，避免旧版异步panic终止整包。\n'
    if issue == 'D05':
        text += '\n本次明确将404改为失败并保持generation不变，保留200/204兼容扩展；不宣称已实现上游没有承诺的原地reset或新会话替换。\n'
    if issue == 'D06':
        text += '\n旧Close仍无限期graceful drain；Adapter使用CloseContext遵守预算。取消后未执行任务计入DiscardedTasks。不合作的既有任务仍可能存活，但不会让本次CloseContext越过预算，也不会按关闭调用次数新建无限等待goroutine。\n'
    if issue == 'D04':
        text += '\n回收交错测试创建并终止本测试专用子进程，没有向任意宿主PID发信号。旧monitor替代进程隔离另有修复后定向断言。\n'
    if issue == 'D08':
        text += '\n空result仅确认interrupt被接受，最终turn结束仍由原生完成事件决定。ACK超时不证明远端未执行，不引入盲目重试。既有写入型fixture补上关联ACK，保留原有输出与会话隔离断言。\n'
    if skips:
        text += '\n实际跳过：\n' + '\n'.join('- `' + r['Test'] + '`' for r in skips) + '\n'
    else:
        text += '\n相关包无跳过。\n'
    doc.write_text(text)
    index = doc.parent / 'README.md'
    old = next(line for line in index.read_text().splitlines() if line.startswith('| ' + issue + ' |'))
    index.write_text(index.read_text().replace(old, old.replace('Source，已登记', 'Test，修复通过；`' + item['branch'] + '`')))
    run(['git', 'add', '--', 'internal', str(doc), str(index)])
    run(['git', 'commit', '-m', 'fix(runtime): ' + issue + ' ' + item['slug'],
         '-m', 'Refs #987. Source issue: ' + str(doc) + '.\n\nVerified: ' + RUN_URL], issue + '-commit.log')
    sha = output(['git', 'rev-parse', 'HEAD'])
    record = dict(id=issue, title=item['title'], branch=item['branch'], base=parent_branch, parent=parent,
                  commit=sha, red=counts(red), green=green_counts, related=broad_counts,
                  skipped=[r['Test'] for r in skips], run=RUN_URL, document=str(doc), published=False)
    RESULTS.append(record)
    MANIFEST.write_text(json.dumps(RESULTS, ensure_ascii=False, indent=2))
    print(json.dumps(record, ensure_ascii=False), flush=True)
    parent_branch = item['branch']

# The full series is checked once at HEAD. Each intermediate commit above had its
# own exact red/green and related package suite, plus the original pre-commit lint.
run(['golangci-lint', 'run', '--timeout=10m'], 'final-lint.log')
run(['go', 'run', './cmd/build-docs'], 'docs-build.log')
run(['make', 'test-contract-matrix'], 'contract-matrix.log')
run(['go', 'test', '-json', '-short', '-race', '-count=1', '-shuffle=on', '-timeout=10m',
     './internal/worker/...', './internal/messaging/...', './internal/gateway/...'], 'final-related.jsonl')
run(['git', 'diff', '--check'])
if output(['git', 'status', '--porcelain']):
    raise SystemExit('Uncommitted state before publication')
# One real Git push, without --no-verify, validates the final full tree through
# unchanged pre-push fmt, lint, vet, mod verify, build, and root test-short gates.
refs = [r['branch'] + ':refs/heads/' + r['branch'] for r in RESULTS]
run(['git', 'push', '--atomic', 'origin', *refs], 'push.log')
for record in RESULTS:
    remote = output(['git', 'ls-remote', 'origin', 'refs/heads/' + record['branch']]).split()[0]
    if remote != record['commit']:
        raise SystemExit('Remote head mismatch: ' + record['id'])
    record['published'] = True
MANIFEST.write_text(json.dumps(RESULTS, ensure_ascii=False, indent=2))
print('PUBLISHED ten verified branches. No PR created or merged by this script.', flush=True)
