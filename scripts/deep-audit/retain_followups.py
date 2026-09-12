"""Persist earlier source-only findings that are outside D01-D10 delivery."""
from pathlib import Path

BASE='fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3'
ROOT=Path('docs/issues/2026-09-runtime-audit')
SOURCE='internal/worker/codexcli/worker.go'
text=Path(SOURCE).read_text()
assert 'func (w *AppServerWorker) closeAndMarkDone()' in text
assert 'func (w *AppServerWorker) injectHistoryPrefix(' in text
assert 'func (w *AppServerWorker) Start(' in text
items=[
 ('F01-wait-after-start-failure.md','启动失败后 Wait 的完成信号',
  'closeAndMarkDone 关闭 doneCh 后又将它置 nil。Acquire 成功而 thread/start 失败时，稍后 Wait 可能仅剩仍开放的共享进程 crashSub 可监听。此观察基于上述基准，不能把 D02 引用释放或 D07 RPC 等待修复外推为这个等待者问题已解决。',
  '另一会话保持共享 manager 运行；令 thread/start 失败后再调用 Wait，确认无需结束共享进程即可返回。另覆盖失败前已等待、失败后新等待和新的生命周期。',
  '生命周期完成通道关闭后保持可读，建立新生命周期才替换；所有提前失败路径均通知等待者，重复清理不二次关闭。'),
 ('F02-history-consumed-before-delivery.md','历史上下文在确认投递前被消耗',
  'Input/InvokeSkill 先调用 injectHistoryPrefix，后调用 startTurn。拼接函数立即清空 pendingHistory 并设置 historyInjected，因此预取消、确定未开始写入的请求也可能消耗从未送达的历史。',
  '预置历史后用已取消 context 输入，确认零协议写入；再进行有效文本或 Skill 输入，检查历史仍应存在。覆盖 reset 交错和结果未知的边界。',
  '分离历史快照与消费提交；仅在确知未写出时恢复所属代际的历史，不在结果未知时盲目重复注入，不让旧请求恢复 reset 已清空的历史。'),
 ('F03-late-start-after-termination.md','迟到的启动成功与终止交错',
  'Start/startNewThread 在 RPC 期间释放 Worker 锁，响应后发布 conn/state。需要进一步确认 Terminate 已完成后，迟到的成功响应是否仍能将 Worker 改回 ready。D02 的引用持有修复不能替代完整生命周期尝试身份验证。',
  '用确定性屏障暂停 thread/start 响应；先 Terminate 再返回成功，验证不发布 ready、不遗留订阅、引用计数准确。对 Start/Resume/Reset 分别覆盖，不以随机睡眠判断结果。',
  '网络调用外执行、状态提交时校验所属启动尝试身份；终止令该尝试失效，迟到结果只清理自己的资源。'),
]
for filename,title,observation,reproduction,acceptance in items:
    path=ROOT/filename
    assert not path.exists(),path
    body=f'''---
title: "后续验证：{title}"
weight: 70
description: "保留此前源码发现；不计入本批十项已交付修复。"
---

# {title}

状态：Source / OPEN，尚未完成本批运行复现与修复；不计入 D01–D10 的十个已交付 PR。来源为此前会话中的暂定 HP 文档，单独保存以免被新的源码台账编号覆盖。

基准：`{BASE}`；[源码](https://github.com/hrygo/hotplex/blob/{BASE}/{SOURCE})。

## 观察及边界

{observation}

## 复现计划

{reproduction}

## 验收条件

{acceptance}

修复提交：无。实现 PR：无。测试通过声明：无。后续应先在将要修改的实际 HEAD 核查是否仍成立，再补失败回归和独立修复。
'''
    path.write_text(body)
with (ROOT/'README.md').open('a') as output:
    output.write('\n## 另存的待验证发现（不计入十项修复）\n\n')
    for filename,title,*_ in items:
        output.write(f'- [{title}]({filename})：Source，待验证、未修复。\n')
print('Preserved three earlier source findings without claiming fixes or extra PRs.')
