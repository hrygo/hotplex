"""Persist verified remote delivery metadata; no merge or deployment actions."""
from pathlib import Path
import base64
import json
import os
import re
import subprocess
import urllib.parse
import urllib.request

REPO='hrygo/hotplex'
BASE='fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3'
TARGET='fix/985-worker-channel-hardening'
ROOT=Path('docs/issues/2026-09-runtime-audit')
CASES=[
 ('D01',988,'fix/988-deep-reliability','D01-codex-subscription-ownership.md'),
 ('D02',990,'fix/990-deep-reliability','D02-codex-reference-release.md'),
 ('D03',989,'fix/989-deep-reliability','D03-codex-transport-generation.md'),
 ('D04',991,'fix/991-deep-reliability','D04-codex-process-retirement.md'),
 ('D05',992,'fix/992-deep-reliability','D05-opencode-reset-404.md'),
 ('D06',993,'fix/993-deep-reliability','D06-feishu-shutdown-deadline.md'),
 ('D07',994,'fix/994-rpc-disconnect-outcome','D07-codex-pending-on-disconnect.md'),
 ('D08',1003,'fix/1003-deep-reliability','D08-codex-interrupt-rpc.md'),
 ('D09',1004,'fix/1004-deep-reliability','D09-codex-lifecycle-response-validation.md'),
 ('D10',1002,'fix/1002-deep-reliability','D10-interaction-timeout-generation.md'),
]


def get(path):
    request=urllib.request.Request('https://api.github.com/repos/'+REPO+'/'+path,
        headers={'Authorization':'Bearer '+os.environ['GH_TOKEN'],'Accept':'application/vnd.github+json','User-Agent':'hotplex-audit-manifest'})
    with urllib.request.urlopen(request,timeout=30) as response:
        return json.load(response)


def command(*args):
    return subprocess.check_output(args,text=True).strip()


assert command('git','rev-parse','HEAD')==BASE
assert get('pulls/986')['head']['sha']==BASE
rows=[]
for document,issue,branch,filename in CASES:
    assert (ROOT/filename).is_file(),filename
    prs=get('pulls?state=open&head='+urllib.parse.quote('hrygo:'+branch,safe='')+'&per_page=10')
    assert len(prs)==1,(document,'expected one open implementation PR')
    pr=get('pulls/'+str(prs[0]['number']))
    assert pr['base']['ref']==TARGET and not pr['merged'] and not pr['draft'],document
    assert pr['head']['repo']['full_name']==REPO and pr['head']['ref']==branch
    sha=pr['head']['sha']
    files=get('pulls/'+str(pr['number'])+'/files?per_page=100')
    assert len(files)<100,'unexpected large PR; inspect pagination manually'
    paths=[entry['filename'] for entry in files]
    assert any(p.endswith('_test.go') for p in paths),document
    assert any(p.endswith('.go') and not p.endswith('_test.go') for p in paths),document
    assert not any(p.startswith('.github/workflows/') or p.startswith('scripts/deep-audit/') for p in paths),document
    report='docs/architecture/worker-channel-issue-'+str(issue)+'.md'
    value=get('contents/'+report+'?ref='+sha)
    text=base64.b64decode(value['content']).decode('utf-8')
    run_id=int(re.search(r'actions/runs/(\d+)',text).group(1))
    stats={name:{'pass':int(passed),'fail':int(failed),'skip':int(skipped)} for name,passed,failed,skipped in re.findall(r'\|\s*(red|green|related)\s*\|\s*(\d+)\s*\|\s*(\d+)\s*\|\s*(\d+)\s*\|',text)}
    assert stats['red']['fail']>0 and stats['green']['pass']>0,document
    assert stats['green']['fail']==0 and stats['green']['skip']==0 and stats['related']['fail']==0,document
    jobs=get(f'actions/runs/{run_id}/jobs?filter=latest&per_page=100')['jobs']
    matching=[j for j in jobs if re.search(r'\('+str(issue)+r'\)',j['name'])]
    assert matching and all(j['conclusion']=='success' for j in matching),(document,'no successful independent repair job')
    ci=get('actions/runs?event=pull_request&head_sha='+sha+'&per_page=100')['workflow_runs']
    ci=[{'name':r['name'],'status':r['status'],'conclusion':r['conclusion'],'url':r['html_url']} for r in ci if any(p['number']==pr['number'] for p in r.get('pull_requests',[]))]
    rows.append({'document':document,'source_document':filename,'issue':issue,'pull_request':pr['number'],'branch':branch,'head_sha':sha,'pr_url':pr['html_url'],'report':report,'verification_run_id':run_id,'verified_job_ids':[j['id'] for j in matching],'tests':stats,'regular_pr_workflows':ci})
    print(document,'issue',issue,'PR',pr['number'],sha,'verified',run_id,flush=True)

assert len(rows)==10 and len({r['pull_request'] for r in rows})==10
manifest={'schema_version':1,'repository':REPO,'source_baseline':BASE,'implementation_base_pr':986,'status':'ten independent repairs submitted; unmerged; live testing not performed','items':rows}
(ROOT/'delivery-manifest.json').write_text(json.dumps(manifest,ensure_ascii=False,indent=2)+'\n')
readme=ROOT/'README.md'
text=readme.read_text()
heading='## 已提交修复与验证索引'
assert heading not in text,'delivery manifest already appended; review rather than overwrite'
text+='\n'+heading+'\n\n以下十项已分别提交修复 PR，**尚未合并或部署**。本节由远端 PR、提交、验证报告及成功的独立 Actions job 交叉核对生成；上表保留最初登记状态。\n\n'
text+='| 文档 | Issue | 独立 PR | 修复提交 | 验证运行 |\n| --- | --- | --- | --- | --- |\n'
for r in rows:
    root='https://github.com/'+REPO
    text+=f"| [{r['document']}]({r['source_document']}) | [#{r['issue']}]({root}/issues/{r['issue']}) | [#{r['pull_request']}]({r['pr_url']}) | `{r['head_sha'][:12]}` | [运行 {r['verification_run_id']}]({root}/actions/runs/{r['verification_run_id']}) |\n"
text+='\n### 证据与合并边界\n\n每项均有原实现失败、修复后五轮 race/shuffle 通过、相关测试、96 个核心契约场景及原始提交/推送 hooks 的独立验证。测试计数包含父子用例和重复轮次，不能相加后解释为独立场景或缺陷总数。矩阵中其他任务失败不影响某个已成功修复 job 的证据，但也不允许用总体声明掩盖单项失败。\n\n'
text+='这些 PR 以 #986 为依赖基线，而非直接合并 main。源码相邻位置可能产生兄弟分支冲突，独立通过不等于组合通过；合并时必须复核冲突并执行联合回归。没有执行真实平台/收费模型联调。各 PR 的常规工作流状态与独立验证分开保存在 `delivery-manifest.json`；没有工作流或等待审批不能写为通过。\n\n'
text+='当前交付以本目录 D01–D10 的已落库编号为准，不把聊天中早期暂定 HP 编号当作同一映射；例如完成通道置空、历史注入消费时机和并发启动/终止的进一步风险不属于本批十项已完成声明。既有额外 PR #998/#999 也不计入这十项。\n'
readme.write_text(text)
print('Wrote verified delivery manifest for ten independent PRs; no branches merged.')
