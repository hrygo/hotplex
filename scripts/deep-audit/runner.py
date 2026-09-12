"""One isolated Issue per runner. This file never enters a product branch."""
from pathlib import Path
import importlib
import json
import os
import subprocess
import sys

BASE='68b9f7f0cd11e2dbf603971c4a987fa2de682fb0'
ISSUE=int(os.environ['AUDIT_ISSUE'])
ROOT=Path(os.environ['RUNNER_TEMP'])/'evidence'
ROOT.mkdir(exist_ok=True,parents=True)
PACKAGES={988:['./internal/worker/codexcli','./internal/worker/base'],989:['./internal/worker/codexcli','./internal/worker/base'],990:['./internal/worker/codexcli','./internal/worker/base'],991:['./internal/worker/codexcli','./internal/worker/base'],992:['./internal/worker/opencodeserver','./internal/worker/base'],993:['./internal/messaging/feishu'],994:['./internal/worker/codexcli','./internal/worker/base'],995:['./internal/gateway'],996:['./pkg/events','./pkg/aep'],997:['./internal/gateway']}[ISSUE]


def run(name,args,allow_failure=False):
    print('RUN',name,flush=True)
    with (ROOT/name).open('w') as out:
        result=subprocess.run(args,stdout=out,stderr=subprocess.STDOUT)
    print('EXIT',name,result.returncode,flush=True)
    if result.returncode and not allow_failure:
        diagnose(ROOT/name)
        raise RuntimeError(f'{name} failed: {result.returncode}')
    return result.returncode


def output(args):return subprocess.check_output(args,text=True).strip()


def diagnose(path):
    text=path.read_text(errors='replace')
    records=[]
    for line in text.splitlines():
        try: records.append(json.loads(line))
        except (json.JSONDecodeError,ValueError): pass
    failed={(r.get('Package'),r.get('Test')) for r in records if isinstance(r,dict) and r.get('Action')=='fail' and r.get('Test')}
    if failed:
        print('FAILED TESTS',sorted(failed),flush=True)
        for record in records:
            if (record.get('Package'),record.get('Test')) in failed and record.get('Output'):
                print(record['Output'],end='')
    else:print('\n'.join(text.splitlines()[-100:]),flush=True)


def stats(name):
    records=[]
    for line in (ROOT/name).read_text().splitlines():
        try:records.append(json.loads(line))
        except json.JSONDecodeError:pass
    return {a:[{'package':r['Package'],'test':r['Test']} for r in records if r.get('Action')==a and r.get('Test')] for a in ('pass','fail','skip')}


def format_changes():
    run('intent-to-add.txt',['git','add','-N','internal','pkg'])
    changed=output(['git','diff','--name-only','--','*.go']).splitlines()
    assert changed
    run('format.txt',['golangci-lint','fmt',*changed])
    run('diff-check.txt',['git','diff','--check'])


def work():
    assert output(['git','rev-parse','HEAD'])==BASE
    assert not Path('scripts/deep-audit/common.py').exists()
    (ROOT/'base.txt').write_text(BASE+'\n')
    (ROOT/'toolchain.txt').write_text(output(['go','version'])+'\n')
    run('hooks.txt',['make','hooks'])
    assert output(['git','config','--get','core.hooksPath'])=='scripts/git-hooks'
    variant=f'case_{ISSUE}_v2'
    module=importlib.import_module(variant if Path(__file__).with_name(variant+'.py').exists() else f'case_{ISSUE}')
    module.tests()
    format_changes()
    test=['go','test','-json','-race','-shuffle=on','-timeout=3m','-run',f'^TestDeepAudit{ISSUE}']
    red_exit=run('red.jsonl',[*test,'-count=1',*PACKAGES],allow_failure=True)
    red=stats('red.jsonl')
    assert red_exit and red['fail'], 'compile errors/no-tests are not per-test defect evidence'
    if ISSUE==996:
        run('benchmark-before.txt',['go','test','-run','^$','-bench','BenchmarkDeepAudit996CloneDelta','-benchmem','-count=3','./pkg/events'])
    module.fixes()
    format_changes()
    run('green.jsonl',[*test,'-count=5',*PACKAGES])
    green=stats('green.jsonl')
    assert green['pass'] and not green['fail'] and not green['skip']
    run('related.jsonl',['go','test','-json','-short','-race','-shuffle=on','-count=1','-timeout=10m',*PACKAGES])
    related=stats('related.jsonl')
    assert related['pass'] and not related['fail']
    run('contracts.txt',['make','test-contract-matrix'])
    assert '12 combinations, 96 core scenarios, 0 skipped, 0 failed' in (ROOT/'contracts.txt').read_text()
    if ISSUE==996:
        run('benchmark-after.txt',['go','test','-run','^$','-bench','BenchmarkDeepAudit996CloneDelta','-benchmem','-count=3','./pkg/events'])
        run('client.txt',['bash','-lc','cd client && go test -short -race -count=1 ./...'])
    summary={'red':red,'green':green,'related':related}
    (ROOT/'summary.json').write_text(json.dumps(summary,indent=2))
    counts={n:{a:len(v) for a,v in group.items()} for n,group in summary.items()}
    print('TEST SUMMARY',json.dumps(counts),flush=True)
    url=f"https://github.com/{os.environ['GITHUB_REPOSITORY']}/actions/runs/{os.environ['GITHUB_RUN_ID']}"
    rows='\n'.join(f"| {n} | {g['pass']} | {g['fail']} | {g['skip']} |" for n,g in counts.items())
    fails='\n'.join('- `'+r['test']+'`' for r in red['fail'])
    skips='\n'.join('- `'+r['package']+'` / `'+r['test']+'`' for r in related['skip']) or '无。'
    doc=Path(f'docs/architecture/worker-channel-issue-{ISSUE}.md')
    doc.write_text(f'''---
title: "Issue #{ISSUE}：独立可靠性修复"
weight: 66
description: "根因跟踪、实际红绿回归证据和交付边界。"
---

# Issue #{ISSUE} 可靠性修复

[Issue #{ISSUE}](https://github.com/hrygo/hotplex/issues/{ISSUE}) 已先保存根因、触发条件和验收标准。依赖 PR #986 的固定提交 `{BASE}`，本分支只包含本问题修复，未计入其他兄弟分支。

## 实际验证

[本次隔离 Actions 运行]({url})，工具链 `{(ROOT/'toolchain.txt').read_text().strip()}`。同一回归先在原实现产生测试失败，再在修复后进行五轮 race / shuffle。编译失败不被当作复现。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
{rows}

计数为 go test -json 的终态记录，含父子测试及重复轮次，不是独立 bug 数。red 为预期失败。既有 12 组合 × 8 场景契约矩阵共 96 场景全部通过，无跳过；这是确定性协议 fake，不是生产平台/收费模型联调。

### 原实现失败入口

{fails}

### 相关测试实际跳过

{skips}

## 边界与合并

本变更不修改 AEP wire schema、权限默认值或生产数据，不部署、不合并 main。完整 lint、文档构建、原始 pre-commit / pre-push 是后续发布门禁，最终成功状态以运行与 PR 提交记录为准，不因报告存在就宣称推送成功。独立 PR 依赖 #986；同文件兄弟分支合并后必须复核冲突并运行集成回归。完整 JSON 日志、补丁和命令输出保存在该运行 artifacts。
''')
    run('lint.txt',['golangci-lint','run','--timeout=10m'])
    run('docs.txt',['go','run','./cmd/build-docs'])
    run('diff-check.txt',['git','diff','--check'])
    assert output(['git','ls-remote','origin','refs/heads/fix/985-worker-channel-hardening']).split()[0]==BASE
    branch=f'fix/{ISSUE}-deep-reliability'
    assert not output(['git','ls-remote','origin','refs/heads/'+branch]),'ref already exists; refusing overwrite'
    run('branch.txt',['git','checkout','-b',branch])
    subprocess.run(['git','config','user.name','HotPlex Audit'],check=True)
    subprocess.run(['git','config','user.email','41898282+github-actions[bot]@users.noreply.github.com'],check=True)
    run('stage.txt',['git','add','--','internal','pkg',str(doc)])
    run('commit.txt',['git','commit','-m',f'fix(runtime): resolve deep reliability issue #{ISSUE}','-m',f'Refs #{ISSUE}; depends on PR #986.'])
    run('push.txt',['git','push','origin',f'HEAD:refs/heads/{branch}'])
    commit=output(['git','rev-parse','HEAD'])
    (ROOT/'product-commit.txt').write_text(commit+'\n')
    run('clean.txt',['git','diff','--exit-code'])
    print('PUBLISHED',ISSUE,branch,commit,flush=True)


try:
    work()
finally:
    subprocess.run(['git','add','-N','internal','pkg'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    with (ROOT/'product.patch').open('w') as f:subprocess.run(['git','diff','--binary',BASE],stdout=f)
    with (ROOT/'stat.txt').open('w') as f:subprocess.run(['git','diff','--stat',BASE],stdout=f)
    print((ROOT/'stat.txt').read_text(),flush=True)
