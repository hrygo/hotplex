---
title: "Worker 与消息渠道可靠性审计（2026-09-12）"
weight: 65
description: "并发关闭、进程代际、流式合并、去重、任务隔离和 RPC 取消的根因、修复与验证边界。"
---

# Worker 与消息渠道可靠性审计

## 审计范围和结论

本次针对四类生产 Worker（Claude Code、Codex CLI、OpenCode Server、ACP）及 WebChat、Slack、飞书的共用链路开展源代码审计。重点不是增加宣传式能力声明，而是修复正常路径测试不容易触发的并发、取消、流式输出和失败隔离问题。

完整工作树基准为 `29a23f02fa2874b9e856f2c0c1529f537f5f4ba4`；源码获取时校验了 1,911 个受版本控制文件，Git tree 与远端一致。这是固定提交的完整源码快照，不是全历史克隆。本地运行环境在加载离线依赖后不可用，因此实际回归和质量验证在隔离的 GitHub Actions runner 上执行；未连接生产会话、真实平台账号或收费模型接口。

验证环境：`go version go1.26.8 linux/amd64`。本次 [验证运行](https://github.com/hrygo/hotplex/actions/runs/34682960209) 保留了修复前后的日志、补丁和检查结果。

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

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 原实现 + 新回归 | 4 | 19 | 0 |
| 修复后新回归，重复 5 轮 | 125 | 0 | 0 |
| Worker / Messaging / Gateway 相关测试集 | 4726 | 0 | 24 |
| Go SDK 子模块测试 | 27 | 0 | 0 |

计数来自 `go test -json` 的终态事件，包含父测试、子测试以及重复运行，不能解释为同样数量的独立缺陷或独立场景。原实现一列预期失败；发布流程要求指定回归逐个产生测试失败，不能用编译失败冒充复现。

现有平台 × Worker 契约门禁：**12 个组合、96 个核心场景、0 跳过、0 失败**。它是确定性契约测试，不是 12 个真实平台/模型端到端联调。

关键命令：

```bash
go test -race -count=5 -shuffle=on -run '^(TestAudit|TestEventGate)'   ./internal/worker/base ./internal/worker/acp ./internal/worker/opencodeserver   ./internal/worker/codexcli ./internal/messaging ./internal/messaging/feishu ./internal/gateway
go test -short -race -count=1 -shuffle=on -timeout=10m   ./internal/worker/... ./internal/messaging/... ./internal/gateway/...
make test-contract-matrix
(cd client && go test -short -race -count=1 -shuffle=on ./...)
```

源码未更改 AEP schema、公开 Worker 接口或 SDK wire contract。发布提交使用仓库 hooks，不跳过 pre-commit / pre-push；pre-push 还执行根模块格式、lint、vet、依赖校验、build 和短测。最终通过状态以运行日志为准。

### 相关测试集中实际跳过的用例

以下列表如实保留。需要额外环境的测试不能被计入“已验证”；具体 Skip 原因在同一运行的 JSON 日志中。

- `github.com/hrygo/hotplex/internal/worker/claudecode` / `TestClaudeCodeWorker_Start_WithBinary`
- `github.com/hrygo/hotplex/internal/worker/claudecode` / `TestClaudeCodeWorker_DoubleStart`
- `github.com/hrygo/hotplex/internal/worker/claudecode` / `TestClaudeCodeWorker_Resume_WithBinary`
- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestIntegrationKillImmediatelyTerminatesIdleProcess`
- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestManager_PerThreadConverterIsolation/concurrent_dispatch_no_data_race`
- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestManagerAcquireStartsProcess`
- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestIntegrationStartSavesSessionAndResetRestarts`
- `github.com/hrygo/hotplex/internal/worker/opencodeserver` / `TestOpenCodeServerWorker_Start_WithBinary`
- `github.com/hrygo/hotplex/internal/worker/opencodeserver` / `TestSingletonProcessManager_IdleDrain_KillsWithoutProcMuDeadlock`
- `github.com/hrygo/hotplex/internal/worker/opencodeserver` / `TestOpenCodeServerWorker_Resume_WithBinary`
- `github.com/hrygo/hotplex/internal/worker/proc` / `TestManager_WaitOnce_TerminateThenWait`
- `github.com/hrygo/hotplex/internal/worker/proc` / `TestManager_Wait`
- `github.com/hrygo/hotplex/internal/worker/proc` / `TestManager_Kill_ReturnsPromptlyWhenProcessDies`
- `github.com/hrygo/hotplex/internal/worker/proc` / `TestManager_Start_AllowedTools`
- `github.com/hrygo/hotplex/internal/worker/proc` / `TestManager_Terminate_GracefulExit`
- `github.com/hrygo/hotplex/internal/worker/proc` / `TestManager_Start_RealProcess`
- `github.com/hrygo/hotplex/internal/worker/proc` / `TestManager_WaitOnce_KillThenWait`
- `github.com/hrygo/hotplex/internal/worker/proc` / `TestManager_ReadLine_MultiLine`
- `github.com/hrygo/hotplex/internal/worker/proc` / `TestManager_drainStderr/stderr_ownership_transferred_on_Start`
- `github.com/hrygo/hotplex/internal/worker/proc` / `TestCleanupOrphans_LiveOrphan`
- `github.com/hrygo/hotplex/internal/messaging/feishu` / `TestAudioToPCM_Success`
- `github.com/hrygo/hotplex/internal/messaging/stt` / `TestAudioToPCM_EmptyInput`
- `github.com/hrygo/hotplex/internal/gateway` / `TestWSPingPong`
- `github.com/hrygo/hotplex/internal/gateway` / `TestLogin_FirstLoginFlag`

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

## 发布验证补充

上文关联运行已完成红绿回归、相关测试集、契约矩阵与 Go SDK 验证；该运行最后因 7 个新测试文件的 goimports 分组格式失败，没有发布代码。后续 [发布验证运行](https://github.com/hrygo/hotplex/actions/runs/34683371222) 只修正导入分组，逐个校验生产 Go 文件 SHA-256 不变，并重新通过全部配置的 lint 和 5 轮回归（125 条通过，0 失败，0 跳过）。最终提交和推送继续使用仓库 hooks，最终状态以该运行日志为准。

## 最终提交验证

前一发布运行在全部 lint、重复回归和文档构建通过之后，因审计脚本误用了默认 hooks 路径及 shell heredoc 缩进而停止，未提交或推送代码。这不是产品测试失败。本次 [提交验证运行](https://github.com/hrygo/hotplex/actions/runs/34683744139) 使用 `make hooks` 配置的 `scripts/git-hooks`，由正常 `git commit` / `git push` 调用钩子；保留全部检查，未使用 `--no-verify`。


## 第二轮增量：取消传播与尚未开始的写入（Issue #987）

第二轮发现先保存在 [Issue #987](https://github.com/hrygo/hotplex/issues/987)，继续通过 PR #986 交付。该 Issue 创建时 A01–A08 均标注 Source，本节只更新本次实际验证的 A02 / A07，不关闭整张跟踪 Issue。

### 实现与边界

- **A02**：编码前取消返回独立的 `writeNotStartedError`；排队写入与开始编码用原子状态转换分界。等待写锁期间调用方放弃后，延迟执行的闭包不得再编码该请求。此类错误不再误分类为共享进程 unavailable，也不误报为已有孤儿管道写入。已经进入编码/管道写入后的取消仍保留原恢复分类与未知结果边界。
- **A07**：steer、compact、rewind、ServerCommander compact、MCP status / refresh / OAuth 贯穿调用方 context。manager 的六个原有无 context helper 保留兼容包装，Worker 和 ServerCommander 使用 Context 版本。正常 JSON-RPC method / params 不变。
- **验证**：`TestAudit987*` 校验七个入口的已取消零写入、六个请求的等待响应取消、七个正常协议载荷，以及 turn/start 取消阶段。已有单例真正阻塞写入的恢复分类单独防回归。
- **未外推**：这不是 A03 的完整 stdin/进程代际隔离，也不解决 A01、A04、A05、A06、A08；不改变公共 Worker / AEP 接口，不增加自动重放，不使用真实平台或模型凭据。

### 本地证据

完整恢复并校验 PR 基准 `1887b9b932be307120483a91cb2ce5f1e864ea97` 的 1,924 个受版本控制文件，Git tree 为 `dbe3d454b74f1dfb15dfc45b193b7c1aeb5260d3`。工具链为导出的离线 Go 1.26.8 / Linux amd64。RTK 不可用，使用原生 `make hooks`，未经 RTK 过滤或统计。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 原实现 + 最终新增测试 | 9 | 19 | 0 |
| 修复后新增测试，5 轮 race / shuffle | 140 | 0 | 0 |
| Codex CLI 与 Base 相关短测，race / shuffle | 335 | 0 | 4 |

以上为 go test -json 终态记录，含父子测试和重复轮次，不是独立缺陷数量。初版测试封装中的虚拟时间/互斥锁等待问题已修正，表中只统计修正后的实际用例结果，不能用测试超时或编译失败冒充缺陷复现。后续隔离 Actions 会再次验证本补丁、运行契约矩阵及仓库原始提交/推送门禁；实际运行链接与状态写回 Issue #987 和 PR #986。

### 本批隔离运行验证

[Issue #987 A02/A07 验证运行](https://github.com/hrygo/hotplex/actions/runs/34687204751) 校验了本地补丁 SHA-256，再执行原实现失败、修复后 5 轮 race 回归以及现有 12 组合 / 96 场景契约矩阵。 原实现：9 通过、19 失败、0 跳过。 修复后重复回归：140 通过、0 失败、0 跳过。 相关短测：335 通过、0 失败、4 跳过。 最终提交与推送使用仓库原始 hooks；是否完成发布以该运行最终状态及 PR 提交记录为准。
