#!/usr/bin/env python3
"""Generate the product documentation from completed audit results only."""
import json
import os
from pathlib import Path
from hardening import write, replace

root = Path(os.environ['RUNNER_TEMP']) / 'hotplex-audit'
summary = json.loads((root / 'summary.json').read_text())
run_url = f"https://github.com/{os.environ['GITHUB_REPOSITORY']}/actions/runs/{os.environ['GITHUB_RUN_ID']}"
base = (root / 'base.txt').read_text().strip()
toolchain = (root / 'toolchain.txt').read_text().strip()

for name in ('green', 'broad', 'client'):
    if summary[name]['fail']:
        raise SystemExit(f'Cannot document a passing run: {name} failed')
if not summary['green']['pass'] or summary['green']['skip']:
    raise SystemExit('No complete passing regression evidence')

rows = []
for name, title in [('red', '原实现 + 新回归'), ('green', '修复后新回归，重复 5 轮'), ('broad', 'Worker / Messaging / Gateway 相关测试集'), ('client', 'Go SDK 子模块测试')]:
    counts = summary[name]
    rows.append(f"| {title} | {len(counts['pass'])} | {len(counts['fail'])} | {len(counts['skip'])} |")

skips = []
for name in ('broad', 'client'):
    for item in summary[name]['skip']:
        skips.append(f"- `{item['Package']}` / `{item['Test']}`")

report = '''---
title: "Worker 与消息渠道可靠性审计（2026-09-12）"
weight: 65
description: "并发关闭、进程代际、流式合并、去重、任务隔离和 RPC 取消的根因、修复与验证边界。"
---

# Worker 与消息渠道可靠性审计

## 审计范围和结论

本次针对四类生产 Worker（Claude Code、Codex CLI、OpenCode Server、ACP）及 WebChat、Slack、飞书的共用链路开展源代码审计。重点不是增加宣传式能力声明，而是修复正常路径测试不容易触发的并发、取消、流式输出和失败隔离问题。

完整工作树基准为 `BASE_COMMIT`；源码获取时校验了 1,911 个受版本控制文件，Git tree 与远端一致。这是固定提交的完整源码快照，不是全历史克隆。本地运行环境在加载离线依赖后不可用，因此实际回归和质量验证在隔离的 GitHub Actions runner 上执行；未连接生产会话、真实平台账号或收费模型接口。

验证环境：`TOOLCHAIN`。本次 [验证运行](RUN_URL) 保留了修复前后的日志、补丁和检查结果。

## 已修复的根因

| 位置 | 原始缺陷及影响 | 修复策略 | 回归入口 |
| --- | --- | --- | --- |
| `worker/base/worker.go` | 旧进程 Wait / Terminate 返回时无条件清空 `Proc`，可能抹掉 reset 后的新进程引用 | 仅当当前引用仍等于操作快照时清理；保持进程代际所有权 | `TestAuditProcCompletionPreservesReplacement` |
| `worker/base/conn.go` | 关闭后 TrySend 可 panic；会话 ID 读写、stdin 指针快照缺少一致的锁保护 | 独立 EventGate 管理发送与关闭，元数据读取在锁内完成 | `TestAuditConnTrySendAfterClose`、`TestAuditConnSessionIDConcurrentAccess` |
| `worker/acp/conn.go` | 并发 send / close 依赖 recover，避免 panic 不等于消除 data race | 使用 EventGate；保留 critical 5 秒、注入 2 秒的预算并允许关闭立即唤醒阻塞发送 | `TestAuditACPConcurrentSendClose`、`TestAuditACPCloseUnblocksCriticalSend` |
| `worker/opencodeserver/worker.go` | SSE 转发到接收通道时与 Close 竞争，原先只 recover | 接收通道发送、注入与关闭使用同一 EventGate | `TestAuditOCSCloseSynchronizesBlockedCriticalSend` |
| OpenCode `conn.Send` | `prompt_async` 没有传递 workspace directory，和其他工作区请求不一致 | 保留非空 directory，并正确进行 URL query 编码 | `TestAuditPromptPreservesWorkspaceDirectory` |
| `gateway/platform_writer.go` | 首次定时刷新后 timer channel 被置 nil，后续 Reset 没有重新监听，短片段可能一直等到末尾才显示 | 每次重新启动 timer 同步恢复监听；循环退出停止 timer | `TestAuditPlatformTimerRearmsAfterFlush` |
| 同上 | 将“拥塞时允许丢弃”误当成“可转成文本合并”；reasoning 数据不能被文本提取器解析而被吞掉 | 拆开拥塞策略与内容变换策略；获准入队的 reasoning 保留类型与载荷 | `TestAuditPlatformReasoningIsNotTextCoalesced` |
| `messaging/dedup.go` | TTL 只在后台 sweep 时生效；重复启动替换关闭信号；先关闭后启动会复活清理循环 | 查询时判过期；固定关闭信号和 once-only 启动；旧 rollback 不删除新一代重试 | `TestAuditDedupExpiresBeforeSweep` 等 |
| `messaging/feishu/chat_queue.go` | 单个发送任务 panic 会结束整个串行 worker，使已接受的后续任务滞留 | 在单任务边界 recover、取消 context 并清理状态，继续串行处理队列 | `TestAuditChatQueuePanicPreservesAcceptedTail` |
| `worker/codexcli/manager.go` | RPC 写出后等待响应忽略 ctx，调用取消仍占用等待；超时错误不保留 DeadlineExceeded 身份 | 响应等待监听 ctx，保留 errors.Is；及时删除 pending；thread/start 继续传递调用方 ctx | `TestAuditCodexCallHonorsResponseCancellation`、`TestAuditCodexResponseTimeoutPreservesDeadlineIdentity` |
| Codex `worker.go` | 给响应等待补上取消后，若继续把所有 context 错误当作 stdin 阻塞，会错误触发共享进程不可用处理 | 区分已写出后的 responseWaitError 与写入失败；前者按 timeout 返回，不冒充 singleton unavailable | `TestAuditCodexResponseCancellationIsNotUnavailable` |

## 统一约束，而不是抹平 Worker 差异

### 1. 生命周期与资源所有权

进程退出清理由进程快照拥有者执行，不能因为旧操作成功就破坏新一代资源。接收通道关闭由通道自身的 EventGate 协调，不能借用 stdin 锁，也不能以 recover 代替同步。

EventGate 的零值可用，每个实例只拥有一个通道。关闭先广播 shutdown 信号，让阻塞的 critical send 退出，再取得排他锁关闭通道。已经接受的缓冲事件仍可以读取；Close 返回后不会再接受写入。此实现保持有界背压，不宣称网络或进程故障下绝不丢事件。

本次将该机制用于 BaseConn、ACP 和 OpenCode connection。Codex 的 subscriber 通道由 singleton manager 与 appConn 共同参与，不能只给 appConn 加锁就声称整个链路安全；其所有权归一仍列入下一轮审计。

### 2. 输出语义与拥塞策略分离

`droppable` 只表示容量不足时允许舍弃，不表示该事件可以转换为 `message.delta`。reasoning 应保持其事件类型和字段，最终是否展示仍由平台能力决定。文本合并保留现有 MessageDelta / Raw 行为，不改变 AEP wire schema。

### 3. 取消语义与共享进程隔离

写入成功后未观察到 RPC 响应，不代表共享 stdin 卡死，也不代表服务端一定没有执行。调用取消必须释放本地等待，但不能因此误杀其他会话共享的 Codex 进程。超时后的自动重试必须考虑副作用是否已经发生，本次不增加危险的“盲目重试”。

### 4. 渠道失败与重复事件

飞书单条发送任务失败不应让整个 chat 队列丢失已接受的尾部任务。Dedup 的过期与回滚必须有独立代际身份，后台清理只是回收优化，不应决定合法重试能否被接受。

## 可重现的验证证据

TEST_TABLE

计数来自 `go test -json` 的终态事件，包含父测试、子测试以及重复运行，不能解释为同样数量的独立缺陷或独立场景。原实现一列预期失败；发布流程要求指定回归逐个产生测试失败，不能用编译失败冒充复现。

现有平台 × Worker 契约门禁：**12 个组合、96 个核心场景、0 跳过、0 失败**。它是确定性契约测试，不是 12 个真实平台/模型端到端联调。

关键命令：

```bash
go test -race -count=5 -shuffle=on -run '^(TestAudit|TestEventGate)' \
  ./internal/worker/base ./internal/worker/acp ./internal/worker/opencodeserver \
  ./internal/worker/codexcli ./internal/messaging ./internal/messaging/feishu ./internal/gateway
go test -short -race -count=1 -shuffle=on -timeout=10m \
  ./internal/worker/... ./internal/messaging/... ./internal/gateway/...
make test-contract-matrix
(cd client && go test -short -race -count=1 -shuffle=on ./...)
```

源码未更改 AEP schema、公开 Worker 接口或 SDK wire contract。发布提交使用仓库 hooks，不跳过 pre-commit / pre-push；pre-push 还执行根模块格式、lint、vet、依赖校验、build 和短测。最终通过状态以运行日志为准。

### 相关测试集中实际跳过的用例

以下列表如实保留。需要额外环境的测试不能被计入“已验证”；具体 Skip 原因在同一运行的 JSON 日志中。

SKIP_LIST

## 尚未闭环的能力与下一步验收

本轮已修复的局部根因不等于所有渠道 × Worker 能力完全一致。以下是后续验收项，不把未经复现的风险写成已确认漏洞：

| 优先级 | 继续审查 / 完善方向 | 验收条件 |
| --- | --- | --- |
| P1 | Codex subscriber 的 manager / appConn 多方发送与关闭所有权 | 在满队列、unsubscribe、manager crash、重新订阅交错下，`-race` 为零且无旧事件进入新会话 |
| P1 | OpenCode reset 期间旧 SSE forwarder 的 connection 代际绑定和 bus EOF 收尾 | 旧 bus 不能写到新连接；EOF 只终结所属代际，不能误关新连接 |
| P1 | 发送成功但 ACK 丢失的副作用去重，与 effect ledger 关联 | 重试不重复执行外部副作用；无法判定执行结果时明确报告 unknown，不伪造 exactly-once |
| P1 | 三渠道四 Worker 的真实联调 | 实际验证 streaming、stop、reset、permission、question、多选、附件、重连及限流降级，并记录运行时版本 |
| P2 | 能力声明与实现一致 | unsupported 应在入口显式拒绝或提供已定义降级，不能返回成功但不执行 |
| P2 | Yuanxin 的专属契约覆盖 | 不把当前 WebChat / Slack / 飞书矩阵的成功外推为 Yuanxin 已验收 |
| P2 | 不遵守 context 的第三方发送任务 | 关闭不得无限等待，且不能通过无限创建 goroutine 规避取消 |

本次没有合并 main，没有升级运行服务，没有动生产数据，也没有用真实 API 凭据执行平台发消息或模型请求。
'''
report = report.replace('BASE_COMMIT', base).replace('TOOLCHAIN', toolchain).replace('RUN_URL', run_url)
report = report.replace('TEST_TABLE', '| 测试范围 | 通过 | 失败 | 跳过 |\n| --- | ---: | ---: | ---: |\n' + '\n'.join(rows))
report = report.replace('SKIP_LIST', '\n'.join(skips) if skips else '无跳过用例。')
path = 'docs/architecture/worker-channel-reliability-audit-20260912.md'
write(path, report)
replace('README.md', '![HotPlex Architecture](docs/assets/architecture.svg)', '![HotPlex Architecture](docs/assets/architecture.svg)\n\nSee the [worker and channel reliability audit](docs/architecture/worker-channel-reliability-audit-20260912.md) for lifecycle ownership, cancellation semantics, regression evidence, and explicit integration limits.')
replace('README_zh.md', '![HotPlex 架构](docs/assets/architecture.svg)', '![HotPlex 架构](docs/assets/architecture.svg)\n\n[Worker 与消息渠道可靠性审计](docs/architecture/worker-channel-reliability-audit-20260912.md)记录了生命周期所有权、取消语义、回归证据和仍需联调的能力边界。')
print(f'Wrote {path}')
