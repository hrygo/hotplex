#!/usr/bin/env python3
"""Verify immutable audit evidence, consolidate only PR 986, and retain results.

Bootstrap-only: this program is never committed to the product branch. It does
not merge main, force-push, weaken hooks, change CI approvals, or create PRs.
"""
from pathlib import Path
import json
import os
import re
import subprocess
import sys

BASE = 'fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3'
TIP = '34f937028c3796eef35e87575618b134d39051cb'
BRANCH = 'fix/985-worker-channel-hardening'
ROOT = Path.cwd()
E = Path(os.environ['RUNNER_TEMP']) / 'pr986-consolidated'
OLD = E / 'original-series'
E.mkdir(exist_ok=True)
URL = f"https://github.com/{os.environ['GITHUB_REPOSITORY']}/actions/runs/{os.environ['GITHUB_RUN_ID']}"
DOCS = Path('docs/issues/2026-09-runtime-audit')
EXPECTED_IDS = {f'D{i:02}' for i in range(1, 11)}


def output(args):
    return subprocess.check_output(args, text=True).strip()


def run(args, name):
    with (E / name).open('w') as out:
        result = subprocess.run(args, stdout=out, stderr=subprocess.STDOUT, text=True)
    if result.returncode:
        print((E / name).read_text()[-16000:], flush=True)
        raise RuntimeError(f'{name}: command failed with status {result.returncode}')
    return result


def records(path):
    result = []
    for line in path.read_text().splitlines():
        try:
            entry = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(entry, dict):
            result.append(entry)
    return result


def summarize(path, allow_skip=False):
    data = records(path)
    if not data or any(e.get('Action') in ('fail', 'build-fail') for e in data):
        raise RuntimeError('Incomplete or failing evidence: ' + str(path))
    tests = [e for e in data if e.get('Test')]
    counts = {a: sum(e.get('Action') == a for e in tests) for a in ('pass', 'fail', 'skip')}
    if not counts['pass'] or (counts['skip'] and not allow_skip):
        raise RuntimeError('No complete passing tests: ' + str(path))
    return {
        'counts': counts,
        'passed_tests': sorted({e['Package'] + '/' + e['Test'] for e in tests if e.get('Action') == 'pass'}),
        'skipped_tests': sorted({e['Package'] + '/' + e['Test'] for e in tests if e.get('Action') == 'skip'}),
    }


def main():
    assert output(['git', 'rev-parse', 'HEAD']) == TIP
    assert output(['git', 'config', '--get', 'core.hooksPath']) == 'scripts/git-hooks'
    assert output(['git', 'merge-base', BASE, TIP]) == BASE
    assert output(['git', 'rev-list', '--count', BASE + '..' + TIP]) == '10'
    assert output(['git', 'ls-remote', 'origin', 'refs/heads/' + BRANCH]).split()[0] == BASE, 'PR HEAD moved; do not overwrite it'
    rows = json.loads((OLD / 'series.json').read_text())
    assert len(rows) == 10 and {row['id'] for row in rows} == EXPECTED_IDS
    previous = BASE
    for row in rows:
        assert row['parent'] == previous and row['published']
        assert row['red']['fail'] > 0 and row['green']['pass'] > 0
        assert row['green']['fail'] == row['green']['skip'] == row['related']['fail'] == 0
        assert output(['git', 'rev-parse', row['commit'] + '^']) == previous
        assert output(['git', 'show', '-s', '--format=%s', row['commit']]).startswith('fix(runtime): ' + row['id'])
        summarize(OLD / (row['id'] + '-green.jsonl'))
        red = records(OLD / (row['id'] + '-red.jsonl'))
        assert any(e.get('Action') == 'fail' and e.get('Test') for e in red)
        assert not any(e.get('Action') == 'build-fail' for e in red)
        previous = row['commit']
    assert previous == TIP
    assert (OLD / 'final-lint.log').read_text().strip() == '0 issues.'
    assert '12 combinations, 96 core scenarios, 0 skipped, 0 failed' in (OLD / 'contract-matrix.log').read_text()
    assert '6 passed, 0 failed' in (OLD / 'push.log').read_text()
    assert not output(['git', 'diff', BASE, TIP, '--', 'scripts/git-hooks', '.github', 'go.mod', 'go.sum', 'pkg/events'])
    subprocess.run(['git', 'checkout', '-b', BRANCH], check=True)
    print('Confirmed ten verified commits, unchanged hooks and AEP; PR head is the exact ancestor.', flush=True)

    run(['go', 'test', '-json', '-race', '-count=5', '-shuffle=on', '-timeout=3m', '-run', '^TestD(0[1-9]|10)',
         './internal/worker/codexcli', './internal/worker/opencodeserver', './internal/messaging', './internal/messaging/feishu'], 'consolidated-targeted.jsonl')
    targeted = summarize(E / 'consolidated-targeted.jsonl')
    exercised = {re.match(r'Test(D\d{2})', e['Test']).group(1) for e in records(E / 'consolidated-targeted.jsonl') if e.get('Action') == 'pass' and e.get('Test') and re.match(r'TestD\d{2}', e['Test'])}
    assert exercised == EXPECTED_IDS, exercised
    run(['go', 'test', '-json', '-short', '-race', '-count=1', '-shuffle=on', '-timeout=10m',
         './internal/worker/...', './internal/messaging/...', './internal/gateway/...'], 'consolidated-related.jsonl')
    related = summarize(E / 'consolidated-related.jsonl', True)
    run(['make', 'test-contract-matrix'], 'consolidated-contract.log')
    assert '12 combinations, 96 core scenarios, 0 skipped, 0 failed' in (E / 'consolidated-contract.log').read_text()
    run(['go', 'test', '-C', 'client', '-json', '-short', '-race', '-count=1', '-shuffle=on', '-timeout=5m', './...'], 'consolidated-client.jsonl')
    client = summarize(E / 'consolidated-client.jsonl', True)
    evidence = {'baseline': BASE, 'verified_code_commit': TIP, 'pull_request': 986, 'run_url': URL,
                'series_run_url': rows[0]['run'], 'go_version': output(['go', 'version']), 'targeted': targeted,
                'related': related, 'client': client, 'contract_matrix': {'combinations':12,'scenarios':96,'failed':0,'skipped':0},
                'items': rows, 'evidence_level':'Test (deterministic peers); not Live', 'publication':'pending original git hooks'}
    (E / 'summary.json').write_text(json.dumps(evidence, ensure_ascii=False, indent=2) + '\n')
    # Compact, persistent evidence remains in Git when downloadable raw logs expire.
    (DOCS / 'verification-20260912.json').write_text(json.dumps(evidence, ensure_ascii=False, indent=2) + '\n')
    ordered = sorted(rows, key=lambda r:r['id'])
    lines = ['---', 'title: "Worker 与消息渠道十项缺陷验收"', 'weight: 66',
             'description: "D01–D10 的修复提交、复现与统一 PR 验证证据。"', '---', '', '# 十项独立缺陷台账', '',
             'D01–D10 统一通过 [PR #986](https://github.com/hrygo/hotplex/pull/986) 交付，关联 #985、#987；不再要求十个独立 PR。',
             '以下十项均已有指定失败回归、修复提交及通过回归；状态为“已修复并验证、待合并”，不等同于生产验收或所有缺陷均已消除。', '',
             '| ID | 问题 | 修复提交 | 状态 |', '| --- | --- | --- | --- |']
    for row in ordered:
        document = Path(row['document'])
        assert document.is_file() and document.parent == DOCS
        text = document.read_text()
        matches = [line for line in text.splitlines() if line.startswith('- 实施PR：')]
        assert len(matches) == 1
        text = text.replace(matches[0], '- 实施PR：[统一 PR #986](https://github.com/hrygo/hotplex/pull/986)；关联 Issue #987')
        text += '\n## 统一交付复核\n\n修复提交：`' + row['commit'] + '`。十项连续提交的完整代码树：`' + TIP + '`。\n\n'
        text += '[统一验证运行](' + URL + ') 再次执行所有 D01–D10 定向测试（5 轮 race/shuffle）、相关模块、96 场景契约矩阵及 Go SDK。\n'
        text += '结果与实际跳过名单见同目录 `verification-20260912.json`；原始日志保存在运行 artifact。提交/推送结果以该运行最终状态与 PR HEAD 为准。未合并 main，未连接真实平台或收费模型。\n'
        document.write_text(text)
        lines.append(f"| {row['id']} | [{row['title']}]({document.name}) | `{row['commit'][:8]}` | 已修复、Test 通过；PR #986 |")
    lines += ['', '## 验证结果', '', '| 范围 | 通过 | 失败 | 跳过 |', '| --- | ---: | ---: | ---: |']
    for name, val in [('十项定向回归，5 轮',targeted),('Worker / Messaging / Gateway',related),('Go SDK',client)]:
        c = val['counts']; lines.append(f"| {name} | {c['pass']} | {c['fail']} | {c['skip']} |")
    lines += ['', '现有三渠道四 Worker 核心契约：12 组合、96 场景、0 失败、0 跳过。',
              '计数包含父子测试和重复运行，不能当作独立缺陷数量；与先前系列逐项测试的计数不相加。',
              f'[逐项红绿回归及原始推送门禁]({rows[0]["run"]}) · [统一复核与交付]({URL})', '',
              '## 明确边界', '',
              '- D05 修正 404 虚假成功，不虚构上游原地 reset 支持，也不宣称新增会话替换已经实现。',
              '- D06 保留兼容 graceful drain；有期限关闭不会等待不合作任务到无限期，但不能强杀 Go goroutine。',
              '- D08 原生 ACK 只代表接受停止请求，最终结束仍由完成事件确认；ACK 丢失不意味着可以安全自动重试。',
              '- Source / Test / Live 分开；真实平台、生产部署及手工验收不计入本批完成声明。',
              '- PR 常规 CI 与独立验证分开记录；需要批准的工作流不冒充已通过，不更改审批规则。', '',
              '## 未跳过质量检查', '',
              '原始 make hooks、pre-commit、pre-push 保留不变。RTK 不可用时使用原生 make。主分支保持不变。', '']
    (DOCS / 'README.md').write_text('\n'.join(lines))
    agents = Path('AGENTS.md')
    text = agents.read_text()
    old = '此批目标10个独立修复PR，保留既有PR #986作为基线，不将原已完成问题重复计数。'
    assert text.count(old) == 1
    agents.write_text(text.replace(old, '用户已确认此批十项独立问题统一通过PR #986增量交付；逐项保留修复和验证记录，不再要求十个PR，不将原已完成问题重复计数。'))
    arch = Path('docs/architecture/worker-channel-reliability-audit-20260912.md')
    with arch.open('a') as stream:
        stream.write('\n\n## D01–D10 统一交付\n\n十项修复的独立提交和验证证据已统一整理到 [缺陷台账](../issues/2026-09-runtime-audit/README.md)，由 PR #986 承载。涉及订阅所有权、引用释放、传输代际、进程回收、reset 失败语义、shutdown 期限、断线等待、停止 RPC、生命周期响应校验及交互超时代际。\n\n完整验证代码提交：`' + TIP + '`；[本轮复核与提交运行](' + URL + ')。源码内 JSON 记录实际测试与跳过项；最终 push 和 PR 常规 CI 状态必须分别核对，不以定向测试替代真实平台验收。\n')
    run(['golangci-lint', 'run', '--timeout=10m'], 'consolidated-lint.log')
    run(['go', 'run', './cmd/build-docs'], 'consolidated-docs-build.log')
    run(['git', 'diff', '--check'], 'diff-check.log')
    assert not output(['git', 'diff', TIP, '--', 'internal', 'scripts/git-hooks', '.github', 'go.mod', 'go.sum', 'pkg/events']), 'Publication normalization changed code'
    subprocess.run(['git', 'config', 'user.name', 'HotPlex Audit'], check=True)
    subprocess.run(['git', 'config', 'user.email', '41898282+github-actions[bot]@users.noreply.github.com'], check=True)
    subprocess.run(['git', 'add', '--', 'AGENTS.md', str(DOCS), str(arch)], check=True)
    run(['git', 'commit', '-m', 'docs(audit): consolidate D01-D10 verification and delivery in PR 986', '-m', 'Refs #987. All ten independently verified fixes are retained without rewriting history.'], 'consolidated-commit.log')
    assert output(['git', 'ls-remote', 'origin', 'refs/heads/' + BRANCH]).split()[0] == BASE, 'PR HEAD changed during verification'
    assert not output(['git', 'status', '--porcelain'])
    run(['git', 'push', 'origin', 'HEAD:refs/heads/' + BRANCH], 'consolidated-push.log')
    final = output(['git', 'rev-parse', 'HEAD'])
    assert output(['git', 'ls-remote', 'origin', 'refs/heads/' + BRANCH]).split()[0] == final
    evidence['publication'] = 'success; original pre-commit and pre-push passed'
    evidence['published_head'] = final
    (E / 'summary.json').write_text(json.dumps(evidence, ensure_ascii=False, indent=2) + '\n')
    (E / 'PUBLISHED_HEAD').write_text(final + '\n')
    print('PUBLISHED PR #986:', final, flush=True)
    print('TEST TOTALS:', json.dumps({n:v['counts'] for n,v in [('targeted',targeted),('related',related),('client',client)]}), flush=True)
    print((E / 'consolidated-push.log').read_text()[-8000:], flush=True)


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print(type(exc).__name__ + ': ' + str(exc), flush=True)
        sys.exit(1)
