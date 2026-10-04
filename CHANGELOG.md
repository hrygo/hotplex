# Changelog

## [1.51.0] - 2026-10-04

### Summary

v1.51.0 是一次 minor 版本更新，核心主题是 **可信运行与交付**。本版本把「输入是否被受理、实际生效的是哪份配置、是否仍在运行、结果是否真的送达、失败后能否安全继续」这五件事从推断变成可核验的事实，并把一批运行期审计发现一并修复。发布前在真实 PostgreSQL 与真实 Claude Code Worker 上完成了端到端运行验证。

### Added

- **输入生命周期**：Gateway 先持久化受理再向 Worker 投递，由 owner 租约与 active gate 保证同一轮次只有一次有效执行。`accepted` 只能前进到 `delivered` / `unknown` / `failed`；租约过期置为 `unknown` 并围栏，晚到的完成事件只能收敛状态；`unknown` 永不自动重投。`execution` 只保存 SHA-256 payload 指纹，不保存 prompt、metadata 值或凭证。
- **权威运行时计划**：每个会话启动都记录带指纹的 launch plan，配置差异以结构化字段暴露，事后可回答「实际生效的是哪份配置」。
- **执行与效果分离**：执行记录与投递效果各自持久化，投递回执独立于 Agent 的 `done`；仅 Slack 适配器实现了 provider receipt，其余渠道的效果只能作为尝试记录。
- **执行台**：Admin 控制台基于同一份运行时存储提供执行列表、时间线与队列操作（取消 / 清空），并暴露 keyset 分页游标。
- **Cron 与 Webhook 身份**：Cron 具备 occurrence 与投递模式记录；Webhook 以经校验的身份去重，缺少可验证 ID 的来源退化为逐请求 occurrence，不会静默合并。
- **数据库迁移**：SQLite 与 PostgreSQL 成对新增 033–038（cron_occurrences、effect_ledger、occurrence_delivery_mode、effect_attempts、execution_queue、execution_queue_payloads），启动时自动应用。

### Fixed

- **终态事件丢失**：Gateway 处理 `done` 时会先发出 `runtime.execution.completed`，而 `done` 的 seq 在更早处分配，导致终态 `done` 携带比客户端已见事件更小的 seq；依赖 seq 单调递增的客户端会丢弃它，回合永不结束。`done` 的 seq 现改为入队前分配，与到达顺序一致。该缺陷在修复前的 `main` 上同样复现。
- **Go SDK 多回合不可观测**：`recvPump` 在收到第一个 `done` 后即退出，但 `done` 是回合终态而非会话终态；补入输入会触发真正的第二轮并产生第二个 `done`，此前客户端对第二轮完全不可见。
- **Go SDK 订阅竞争**：`Unsubscribe` 在锁内关闭事件通道，而投递在锁外发送，交错即向已关闭通道发送（进程 panic）。监听者改为携带 done 信号，取消订阅只停止投递。
- **运行期审计十项**：Codex 订阅通道多方关闭与发送竞态、Codex reference 生命周期释放、Codex 传输代际冻结、Codex 进程回收、OpenCode reset 404、飞书关闭期限、Codex pending 在断连时的结算、Codex 中断 RPC、Codex 生命周期响应校验、交互超时代际隔离。
- **消息与事件身份**（#997/#998/#999）：delta 合并保留消息身份与 metadata；终态写回执只完成一次；克隆 envelope 时隔离可变类型载荷。

### Changed

- **AEP 契约**：`input.ack` 新增 `input_mode`、`durability`、`parent_execution_id` 三个可选字段，全部 `omitempty`，向后兼容；`pkg/events` 未新增或修改 Kind，`docs/reference/{aep-protocol,events}.md` 与四语言 SDK、双向协议测试同步更新。
- **Go SDK 订阅语义**：`Unsubscribe` 不再关闭事件通道（关闭会与正在投递的事件竞争），通道改由 `Close()` 在接收泵停止后统一关闭，`for range` 的终止语义不变。`Events()` 应只调用一次并持续排空：只注册不排空的通道填满后，SDK 为保证 `done` / `error` / `state` 不丢而阻塞投递，接收泵随之停滞。
- **PR 分支规则**：CI 的 pr-checks 接受 Codex 桌面端创建的 `codex/` 前缀，原有 `<type>/[<id>-]<desc>` 形态与可选数字 ID 要求不变。

### Known Issues

- `runtime.execution.completed` 仍先于 `done` 送达，与 `docs/reference/aep-protocol.md` 规定的顺序不符；seq 症状已修复，顺序本身待调整（#1022）。
- PostgreSQL 下会话 GC 每 60 秒失败一次（`session cleanup: enqueue: driver: bad connection`），会导致空闲会话占满额度（#1022）。
- Go SDK 在默认 `allowed_origins` 配置下无法连接网关：网关要求 Origin 匹配，而 gorilla 不发送 Origin（#1022）。

## [1.50.2] - 2026-09-05

### Summary

v1.50.2 是一次 patch 版本更新，核心主题是 **跨平台交互与会话恢复对齐**。本版本修复恢复前序号水化失败仍可能继续创建 Worker 的风险，补齐 ACP 问答/表单交互到 AEP 的映射，并让 Slack、飞书、OpenCode、WebChat 在权限、媒体、问答、停止和失败场景下保持明确且可重试的用户语义。

### Fixed

- **Session Recovery**: 会话恢复现在必须先完成持久化事件序号水化；事件存储不可用时保留原会话生命周期，不终止旧 Worker，也不静默降级为新会话。ACP 和 OpenCode 在原生上下文不可恢复时会显式提示上下文丢失，并尽可能注入已有文本历史。
- **Interactive Requests**: ACP 的 question/elicitation 请求现在归一化为 AEP 交互事件，校验非法参数并保留原始 JSON-RPC ID；Slack 文本响应保留完整问题文本、要求精确且无歧义的请求 ID，并拒绝过期或错误目标。
- **Platform Boundaries**: 飞书在访问控制通过前不再下载媒体附件；OpenCode 多题回答保留缺失题目的位置；不支持的 Codex 模型控制返回明确的 `ErrNotImplemented` 语义。
- **WebChat Terminal State**: 终态事件会关闭仍待处理的交互卡片，修复同一浏览器任务内 delta/done 竞态导致的回复丢失，并保留终态 turn summary；停止后的旧 assistant-ui 快照不会再覆盖已完成内容。

### Changed

- **Worker Capability Catalog**: 命令目录按 Worker 的真实能力隐藏不可执行的固定控制项，避免用户看到无法工作的命令或将同名 Skill 误判为 Gateway 控制命令。

## [1.50.1] - 2026-08-31

### Fixed

- **Terminal Failure Feedback**: Gateway 现在会在判断失败 `Done`、真实 Worker 错误和自动重试结果后再发送用户可见终态；失败或停止的轮次不再被误报为“本轮未收到可展示的 Agent 回复”。完整 `MessageData` 也会正确计入真实回复，飞书流式卡片会分别呈现停止和失败状态（#982）。

## [1.50.0] - 2026-08-25

### Summary

v1.50.0 是一次 minor 版本更新，核心主题是 **Worker Run 停止静默与跨通道失败反馈**。本版本让 Gateway 只有在旧 Worker run、连接和事件转发器全部完成静默后才确认停止，并收紧 OpenCode 重试、配额失败与 WebChat 错误展示，避免“已停止”或“成功完成”掩盖真实运行状态。

### Added

- **Gateway Lifecycle**: 新增 session dispatch gate、run 级事件写屏障和 provider acceptance 边界；`control.stop` 仅在旧 run 完成 teardown、forwarder 退出且事件链静默后发送 `done(reason="stopped_by_user")`。
- **Worker Reliability**: ACP、Claude Code、Codex CLI 和 OpenCode Server 适配器补充停止、释放、共享单例隔离与跨 Worker 契约回归测试，下一轮输入不会复用旧本地 run。

### Changed

- **OpenCode Retry**: 区分重试期间的短暂 `idle` 与真正终止，重试耗尽会发出明确的错误终态；配额、限流和 HTTP 429 统一映射为 rate-limit 错误。
- **WebChat UX**: 统一错误码映射，向用户展示可执行的重试、配额和凭据检查建议，而不是把上游原始错误当作成功或空响应。
- **Release Validation**: 发布说明校验增加目标版本标题和非空约束，避免自动生成空 Release body。

### Fixed

- **Stop Correctness**: 修复停止与输入投递、旧连接替换、forwarder 退出及 runtime 终态之间的竞态；旧 run 的晚到事件不再污染 Hub、event store 或下一轮会话。
- **Lifecycle Safety**: 限制停止 admission 等待并隔离 Worker 生命周期 panic，避免停止操作卡住 UI 或触发错误的 crash fallback。
- **Error Truthfulness**: 修复 OpenCode 重试后 `idle` 被误判为成功完成，以及配额失败只显示“本轮未收到可展示的 Agent 回复”的问题。

## [1.43.0] - 2026-08-25

### Summary

v1.43.0 是一次 minor 版本更新，核心主题是 **Gateway 受控重启与生命周期闭环**。本版本为 CLI、Admin API 和飞书运维入口统一了安全的重启控制面，加入全局 fenced lease、独立 helper、持久化 receipt 与 Worker run quiescence；同时向 Slack、飞书和元信会话发送可追踪的停止/启动通知。

### Added

- **Gateway Operations**: 新增 `hotplex gateway restart`、`POST /admin/restart` 与飞书 `/gateway restart` 入口；三者共享 fenced restart lease 和独立 helper，拒绝重叠事务并保留请求回执。
- **Feishu Operations**: 新增 `gateway_restart_allow_from` 及对应环境变量，仅允许显式 OpenID 白名单执行 Gateway 重启；原请求会话可收到带 request ID 的完成/失败回执。
- **Lifecycle**: 受控停止或重启时等待当前 Worker run 静默退出，并向已连接的 Slack、飞书和元信会话广播停止与启动通知；通知失败可重试且不阻塞生命周期。

### Changed

- **Restart Coordination**: CLI、Admin API、飞书和服务生命周期统一经过 coordinator，跨平台复用 lease、receipt、PID 探测和 detached helper 流程；`--detached` 保留为兼容参数。
- **Configuration**: Gateway restart 白名单支持平台级与 Bot 级配置，并遵循现有配置热更新边界。

### Fixed

- **Reliability**: 修复重启准备/提交/启动就绪阶段的并发竞争、旧 Gateway 仍存活时的清理时序、生命周期回执丢失，以及配置检查测试中的并发不确定性。

### Security

- **Gateway Operations**: 非法或扩展的 `/gateway` 命令保留在 Gateway 控制面，不会回退给 Worker；restart lease、receipt 有大小与状态校验，PID 探测异常默认拒绝执行。

## [1.42.1] - 2026-08-24

### Added

- **Messaging/Gateway**: 受控停止或重启时，向当前已连接的 Slack、飞书和元信会话广播服务停止、启动通知；启动通知复用停止前的去重会话快照，发送失败不会阻塞服务生命周期。
- **Built-in Skills/Onboarding**: 恢复 HotPlex 初始化工作流；内置 `hotplex-operator` 新增初始化参考，覆盖配置、诊断、服务安装/启动/状态验收与 Skill 同步边界，并更新 `onboard` 引导。

## [1.42.0] - 2026-08-23

### Summary

v1.42.0 是一次 minor 版本更新，核心主题是 **Turn 完整性、Agent 自描述与内置 Skill 生命周期管理**。本版本将不确定投递、重连恢复和 Worker 能力展示收敛到更明确的运行时契约，并把 canonical 内置 Skill 包、生成式 CLI 参考和安全对账能力纳入正式交付面。

### Added

- **Gateway Core**: Turn identity and delivery hardening — 持久化 client message identity、补充 reliability capabilities 协商，并在不确定 Worker 投递后保留可恢复的 turn 记录，避免输入与运行结果脱节。
- **Gateway Core**: Runtime facts and native skill provenance — Session 向 Worker 注入有边界的运行事实，按 Worker 类型声明 native catalog owner，并在 `/skills` 与命令分发中区分 callable、discoverable 和 unavailable 能力。
- **Agent Config**: Canonical tools and metacognition slots — `TOOLS.md`、runtime facts 与 built-in skill provenance 形成稳定的配置/身份边界；Doctor、Admin 和 onboard 流程可识别 legacy 配置并给出迁移方向。
- **Built-in Skills**: Canonical embedded packages — 内置 `hotplex-cli` 与 `hotplex-operator` 以生成式 manifest 和参考资料随二进制交付，支持来源查询、生命周期状态、同步、safe native-root reconciliation、receipt 校验与 rollback。
- **CLI/WebChat**: Skill lifecycle and capability-aware UX — 新增 built-in skill 生命周期命令、CLI surface 生成与 WebChat 不可调用 Skill 的明确禁用/错误提示，避免把不支持的命令误投递给 Worker。

### Changed

- **Reliability**: WebSocket 初始化、Session 解析、sequence hydration 和事件发布顺序统一收紧；初始化重试有上限，旧连接或不完整状态不会静默吞掉错误或制造乱序事件。
- **Skills**: Skill source 统一到 canonical repository portfolio；legacy flat/manual publisher 与兼容性断言不再作为运行时来源。现有自定义 Skill 应迁移到 `.agents/skills/<name>/SKILL.md`，并通过受支持的 lifecycle 命令管理。
- **Worker UX**: 不支持的 native command 与用户可修正的 command error 现在以稳定 capability/error 分类返回，调用方可以区分“不可调用”和“执行失败”。

### Fixed

- **Gateway Core**: 修复 WebSocket 初始化清理阶段丢失原始错误、Session 尚未完成水化时提前放行新 turn、事件终态/序列顺序不稳定，以及投递结果在 uncertain 状态下无法追踪的问题。
- **Worker/Skill Dispatch**: 修复 native catalog 来源漂移、unsupported capability 分类不一致，以及 WebChat 将不可调用 Skill 作为可调用命令展示的问题。
- **Built-in Skills**: 修复 reconciliation 失败时 receipt 或状态丢失、恢复后 bytes 未验证、所有权 receipt 不一致和生命周期报告不完整等问题。

## [1.41.0] - 2026-08-13

### Summary

v1.41.0 是一次 minor 版本更新，核心主题是 **可配置工作区根目录（issue #964）**——一个 `HOTPLEX_HOME` 环境变量统一控制全部状态：默认配置文件路径、WebChat 工作区沙箱、skills/phrases 目录、Worker 默认工作目录全部跟随解析，替代硬编码 `~/.hotplex`；未设置时完整回退旧行为，设置后整个 workspace（配置 + 数据 + 日志 + PID + 沙箱）整体迁移。同时修复了投递收敛泄漏（晚到 `Done` 收敛 `failed` 投递）与一批稳定性问题（workspace 更新误 403、worker stderr ANSI 污染、codex MCP 日志洪泛）。

### Added

- **Configuration**: `HOTPLEX_HOME` workspace root — the default config path (`DefaultConfigPath()`), WebChat workspace sandbox root, skills/phrases directories, and worker default work dir all resolve from `HOTPLEX_HOME`, replacing hardcoded `~/.hotplex`; unset falls back to legacy behavior. One env var relocates the entire workspace (config + data + logs + PID + sandbox) with injective four-identity-space sandbox segments. (#964)
- **WebChat UI**: Sandbox-aware workspace paths — a `workspace-path` utility builds server-consistent sandbox paths, so workspace creation and general-tab saves keep working after `HOTPLEX_HOME` is set.

### Fixed

- **Gateway Core**: Late `Done` events converge `failed` deliveries — a worker run completing after its delivery was marked failed no longer leaves the execution stranded in a stale state.
- **Gateway Core**: Unchanged `work_dir` is a no-op in workspace Update — saving name/overrides of a legacy UUID-root workspace no longer 403s from spurious sandbox re-validation.
- **Worker**: ANSI escape sequences are stripped from worker stderr logs, keeping JSON logs clean of raw terminal control codes.
- **Worker**: codexcli repetitive mcpServer/startupStatus updates are deduplicated, suppressing log flooding on status transitions.

## [1.40.0] - 2026-08-07

### Summary

v1.40.0 是一次 minor 版本更新，核心主题是 **Native Skill Dispatch（issue #957）**——让已知 skills 走各 Worker 的原生协议直接分发，而非退回普通文本。四大主线：① **原生命令模型**——claude_code / opencode_server / codex_cli / ACP 四类 Worker 统一 command descriptor，协议级 invoke 取代文本回退；② **Session 级命令目录**——/skills 条目标注 per-worker 可调用性（callable/discoverable/unavailable）、/worker 固定命令分发与 replay 语义收紧；③ **全通道覆盖**——webchat 与消息平台统一消费命令目录，command menu 交互完善（技能可调用性、尾随空格、幽灵条目清理）；④ **可靠性修复**——ACP stop 降级、codex MCP status 空 params、事件 seq 串行化等。WebChat 用户现可直接从命令菜单触发技能分发。

### Added

- **Worker**: Native command dispatch — known skills route through each worker's native protocol（claude_code stream-json user frame、opencode_server session command、codex_cli structured invocation、ACP available_commands）instead of plain-text fallback, with a shared command descriptor model and compat adapters for all four workers. (#957)
- **Gateway Core**: Session-scoped command catalog — merged /skills entries annotated with per-worker invokability（callable/discoverable/unavailable），/worker dispatch with fixed-command rejection（gateway-handled commands never bypass the busy gate），catalog state pruned on session release.
- **Messaging**: Platform surfaces（Feishu/Slack/webchat）consume the unified command catalog — bot replies reflect worker capability annotations.
- **WebChat UI**: Command menu surfaces skill invokability — unavailable skills render disabled, selection inserts a trailing space, and gateway-handled command turns complete a full turn loop with an "executed" confirmation.

### Fixed

- **Worker**: ACP stop degrades to process-level kill when the agent lacks the protocol-mandated session/cancel（JSON-RPC -32601）— the turn really halts instead of surfacing a perpetual INTERNAL_ERROR while the turn keeps running.
- **Worker**: codex mcpServerStatus/list sends an empty params object — eliminates "missing field params (code -32600)" MCP status errors in webchat.
- **Gateway Core**: Serialize gateway event sequence publication — concurrent producers can no longer reorder seq and violate the AEP strict-monotonic contract.
- **WebChat UI**: Dedupe gateway fixed commands（/gc、/reset、/skills）from command menu and skill chips；remove the phantom /status entry；preserve native skill dispatch during replay.

## [1.39.0] - 2026-08-06

### Summary

v1.39.0 是一次 minor 版本更新，核心主题是 **运维闭环与可靠性契约**。三个主线：① **Runtime 运维迭代**（#877/#946）——执行 fence 条件化决策（双实例竞态安全）、`hotplex runtime fences` 命令族、EffectiveRuntimePlan 期望状态解析与 `/admin/sessions/{id}/runtime-plan` 只读投影；② **审计链根因修复**（#949/#950）——DELETE 触发器保护、全断点校验、`audit verify`/`audit rebase` 命令与零丢失日志管道；③ **平台 × Worker 可靠性契约矩阵**（#955）——12 组合 96 场景契约测试 CI 门禁，顺带修复了 seq 严格递增、终态写入异步化、Feishu/OCS/Slack 端到端消息链与媒体摄入边界等一批可靠性问题。WebChat 增加 Codex 认证失效的可操作重登录提示，README 面向新用户全面改版。

### Added

- **Runtime Ops**: `hotplex runtime fences list/resolve/abandon` — operator fence CLI over the Admin API (never opens the DB), conditional on `--fence-version`, `--confirm` required, 409 FENCE_CONFLICT exits non-zero with re-inspection guidance. (#877)
- **Runtime Ops**: Execution fence decisions — `fence_version` conditional update (migration 031, SQLite + PostgreSQL) so concurrent gateways cannot double-migrate a fenced execution; `resolve` keeps runtime unknown, `abandon` fails it with `OPERATOR_ABANDONED`; late events converge but never regress a terminal state.
- **Runtime Ops**: Admin fence endpoints — `GET /admin/executions/fences` (runtime:read) and `POST /admin/executions/{id}/fence-action` (runtime:write) with audit (`runtime.fence.action`) and metrics (`hotplex.runtime.fence_actions`, `hotplex.runtime.fence_conflicts`).
- **Runtime Ops**: EffectiveRuntimePlan desired-state resolver (#946) — shadow-mode plan identity with canonical hash, fail-closed `ErrPlanBlocked` for unknown worker/sandbox/permission combos, `Redacted()` choke point for Admin/doctor surfaces; `GET /admin/sessions/{id}/runtime-plan` redacted projection.
- **Runtime Ops**: `hotplex runtime` command family and doctor checkers — `runtime.fenced_executions` (read-only SQLite probe), `runtime.effective_plan`, `execution_fences`.
- **Audit**: `hotplex audit verify` — read-only on-demand chain verification CLI (no migrations applied, safe against read-only copies), reports every break (up to 50) with per-reason non-PII diagnostics and remediation advice, exits non-zero on a broken chain for CI/cron gating.
- **Audit**: `hotplex audit rebase --next-id N --confirm` — operator repair CLI that re-anchors a broken hash chain at a surviving row (the only legitimate repair since migrations 023/030 reject row UPDATE and un-anchored DELETE).
- **Audit**: `trg_ua_no_delete` (migration 030, SQLite + PostgreSQL) — DELETE rejected unless covered by a checkpoint anchor; `DeleteBefore` rewritten as checkpoint-anchored prefix prune.
- **Reliability**: Platform × Worker contract matrix (#955) — gateway contract harness, shared scenario runner, real worker probe mappers, and 12 platform-worker combinations (slack/feishu/webchat/opencode-server × 4 workers) gated in CI via `scripts/test-contract-matrix.sh`.
- **WebChat UI**: Codex auth-expiry error mapping — token refresh / logout / 401 signatures now show an actionable re-login prompt instead of raw CLI errors. (#956)
- **CLI**: `worker.claude_bypass_mode` doctor checker — warns when Claude Code runs in bypass permission mode.

### Changed

- **Gateway Core**: Per-session `SeqOrderLock` — seq allocation order now equals broadcast enqueue order, fixing seq inversion under concurrent producers that violated the AEP strictly-increasing contract.
- **Gateway Core**: Terminal writes decoupled from the routing goroutine — pcEntry enqueues and returns, per-conn result + `aggregateTerminalWrites` collect outcomes, bounded lifecycle with no goroutine leak.
- **Gateway Core**: Exec-scoped stop fence keyed by `(run, execution)` — a new turn's execution admits its own stop, duplicate stops stay rejected, pending LLM auto-retry is cancelled on stop.
- **Audit**: `admin_audit` resolves the logger at call time (`slog.Default()`), so records flow through the configured JSON/lumberjack pipeline; failed regular batch flush re-spills in-flight events (zero-loss).
- **Audit**: Break-alert state machine — first WARN carries `broken_at`/hash/advice diagnostics (PII excluded), repeats downgrade to DEBUG with `first_seen`/`occurrences`, INFO `resolved` after repair.
- **Messaging**: Feishu/Slack hardening — bounded media reads and temp-file restrictions, retry-safe queue admission and dedup, abort commands routed through control stop, terminal delivery failures surfaced.
- **Messaging**: Slack streaming writer teardown fence — rate-limited final flush records unflushed tail instead of dropping; body-presented terminal errors no longer detach the platform conn.
- **Configuration**: `HOTPLEX_EVENTS_RETENTION` env now actually binds (documented but previously ignored); unified `worker.ResolvePermissionMode` shared by doctor and claudecode.
- **SDK/Docs**: README product-first overhaul for new users (#938); v2 roadmap finalized with qm-based runtime research; per-module AGENTS.md hierarchy established.

### Fixed

- **Gateway Core**: Concurrent-producer seq inversion — `processForwardedEvent` allocated seq upfront but enqueued at the end; stop-synthesized `done` could broadcast before earlier events, violating the AEP strictly-increasing contract (fixed via `SeqOrderLock`).
- **WebChat UI**: Codex auth-expiry errors (token refresh / logout / 401) previously rendered raw English CLI output — now mapped to actionable re-login guidance with a source prefix. (#956)
- **Messaging**: Feishu Claude Code end-to-end chain — terminal write errors now propagate from gateway to platform, terminal write budgets bounded, terminal cleanup detached from routing. (#943)
- **Worker**: OCS stop contract — abort retains SSE subscription, post-abort converter terminal suppressed while the user-stop marker is set (exactly one terminal per stopped turn); failed stop attempts clear the marker so the legitimate terminal still flows.
- **Messaging**: Slack terminal delivery accounting — `CloseContext` bounded by caller deadline, pending flush tracked so content in-flight at expiry is never reported presented; rollback of rejected message dedup.
- **Gateway Core**: pcEntry close drain — writeLoop's closeCh race could flush an empty buffer and drop the coalesce window; close now consumes the channel remainder before flushing.


## [1.38.1] - 2026-07-27

### Summary

v1.38.1 是一次 patch 版本更新，核心交付 **SESSION_BUSY mid-turn passthrough** 与 **AEP canonical schema + cross-SDK conformance**。Gateway 现在能智能处理忙碌中的用户补充输入：支持 mid-turn 注入的 Worker（Claude Code、Codex CLI）透明透传至当前 turn，其余 Worker（ACP、OCS）暂存并在当前 turn 结束后重投。同时引入 AEP v1 规范 schema（aep-v1.json）与 38 个 golden envelope 夹具，配合 Go/TS/Python/Java 四端一致性测试，杜绝 SDK 协议漂移。平台 session resume 的 SeqGen 水化和 ACP resume 空 sessionId 问题得到修复。

### Added

- **Gateway Core**: SESSION_BUSY mid-turn passthrough — user supplements injected transparently into running turn for MidTurnInjector workers (Claude Code, Codex CLI), or buffered and replayed after Done for fallback workers (ACP, OCS). (#934)
- **AEP Schema**: Canonical AEP v1 schema with golden envelope corpus and cross-SDK conformance suite (Go/TypeScript/Python/Java). (#929)
- **SDK**: Backfill missing AEP types — InputAck, InternalReset, RuntimeExecution* in TypeScript, Python, and Java SDKs.
- **Observability**: Mid-turn and supplement counters — `mid_turn_injected`, `supplement_buffered`.

### Fixed

- **WebChat UI**: Admin cookie auth — add `credentials:'include'` for cross-port support; gateway URL rewrite for standalone dev server.
- **Gateway Core**: CC TOCTOU race — re-check active gate before mid-turn inject to prevent ghost turns after Done releases the gate.
- **Gateway Core**: Harden busy supplement lifecycle — idempotent and bounded, fence replay across reset/shutdown, replay after durable runtime repair.
- **Gateway Core**: Hydrate SeqGen on platform session resume — fixes recurring UNIQUE constraint failures on Feishu/Slack/Yuanxin resume.
- **ACP**: Session/load empty sessionId and seq collision on resume — fixes WebChat HISTORY_LOSS and UNIQUE constraint violations (#879).

## [1.38.0] - 2026-07-23

### Summary

v1.38.0 是一次 minor 版本更新，交付了 **Wave 1 first-cut** 的五个运行时模型与可观测性 slice（#847/#848/#866/#852/#850），以及**完整的 webchat 后台管理控制台**。后端方面，AgentSpec 归一化运行时模型、AgentIdentity 绑定、EffectiveAgentSpec 快照持久化、RuntimeContext 只读 facade、语义键单一真相源共同构成 v2 依赖链的根；安全 review 中发现的 2 个无界 limit HIGH 问题已在发布前修复。前端方面，dashboard、sessions、bots、cron、users、api-keys 六个 /admin 子页面完成统一设计语言重构，cron 管理实现完整闭环 CRUD 操作（创建/编辑/启用/禁用/删除/执行历史）。cron 空平台字段现归一化为 "cron" 以保证 session audit 行可追溯。

### Added

- **Gateway Core**: AgentSpec 归一化运行时模型（#847）—— `WorkerSpec`+`PolicySpec`+`SandboxSpec`+`BudgetSpec`+`IdentityRefs` 纯 Resolver、idempotent mappers、shadow 模式接入 webchat 入口。
- **Gateway Core**: AgentIdentity UUIDv5 派生绑定到 runtime session（#848）——折叠进 `context_json` 列（无迁移），三处统一 `agent_id` 传播。
- **Gateway Core**: EffectiveAgentSpec 快照持久化（#866）——版本化 + SHA-256 内容哈希；resume/restart 重建运行时契约、`AllowedTools` 从快照恢复。
- **RuntimeContext**: 只读 `Load` facade（#852）——四源聚合（eventstore/materialized turns/worker config/workspace），适配器边界隔离，`Save` 留后续 slice。
- **Observability**: 语义键单一真相源（`keys.go`）+ 三层基数契约 + `span_id` 注入（`Hub.SendToSession`）+ 散落字面量迁移（#850）。
- **WebChat UI**: 后台管理控制台完整覆盖——dashboard 实时指标流、sessions 详情 + inspector drawer、bots 主列表与子页面归一化、cron 完整闭环 CRUD（创建/编辑/启用/禁用/删除/执行历史）、users 管理控制台（含密码重置）、api-keys 管理页面，统一设计语言。
- **Cron**: 新增 `max_runs` 和 `expires_at` 生命周期控制——modal 输入 + 后端默认值（1000 runs / 12 months）。
- **Admin**: 用户管理后端端点——用户列表、密码重置、唯一约束错误处理。

### Changed

- **Cron**: 空平台字段归一化为 "cron"——纯定时任务 session.create audit 行有合法 platform 值；有投递目标的任务（slack/feishu）保留原值。
- **WebChat UI**: Modal 配色与字体对齐系统设计标准。
- **Admin**: 用户管理控制台对齐共享设计系统。

### Fixed

- **RuntimeContext**: Load 无界 limit 安全修复——`MaxAllowedTurns`/`MaxAllowedEvents` cap 防止无界扫描（security review, HIGH）。
- **Agentspec**: 加固 identity 和 policy 持久化。
- **Cron**: 执行历史 turn 统计聚合——跨所有衍生 cron 会话汇总。
- **Cron**: `isRecentlyRun` JSX 布尔值渲染——修复 literal 0 显示问题。
- **Cron**: 创建时 payload kind 修正为 `isolated_session`。
- **Cron**: 默认 `owner_id` 填充与 target bot 下拉增强。
- **Admin**: 唯一约束错误处理 + 空响应体 JSON 解析异常修复。

### Security

- **AgentSpec/Wave 1**: 安全 review 发现的 2 个 HIGH（无界 limit）已在发布前修复（`MaxAllowedTurns`/`MaxAllowedEvents` cap）；2 个 MEDIUM 评估为不适用（低基数关联键非凭证，store 错误无密钥）。

## [1.37.2] - 2026-07-21

### Summary

v1.37.2 是一次 patch 版本更新，聚焦于 **Gateway Core 事件序列号竞态** 与 **WebChat 限流体验** 两项可靠性修复。最重要的修复解决了 session seq 计数器在 racing `NextSeq` 下从近 0 重启、与持久化历史碰撞反复触发 `events` UNIQUE 约束失败的问题——这是 issue #879 在 #900 写锁围栏之后仍然复现的回归根因。WebChat 现对上游 429 限流返回友好提示。管理端 Skills 页面顺带补齐了完整 CRUD 与 UI/UX 完善。

### Fixed

- **Gateway Core**: session seq 从 eventstore seed，修复 racing `NextSeq` 回归 — `EnsureSeqHydrated` 的 `Initialized()` 守卫在早 `init_ack` / forwarder resume 触发的竞态 `NextSeq` 已通过 `LoadOrStore` 建立低值计数器时短路了 DB hydration，致计数器从近 0 重启并与持久化历史碰撞，`events(session_id, seq_guard_id, seq)` 反复 UNIQUE 失败（#879 在 #900 写锁围栏后仍存活）。引入独立 `hydrated` flag 标记 DB 发起的 hydration——裸 `NextSeq` 不置位，`EnsureSeqHydrated` 仍能从 DB 抬高水位；并补充 `prior_peek` 日志与 collector append 告警的 seq/type 字段便于诊断。(0231fc74)
- **WebChat UI**: 上游 429 限流友好提示 — runtime adapter 在错误处理 default 分支检测 HTTP 429 rate-limit，返回友好错误信息替代原始错误展示。(26facd83)

### Added

- **Skills**: 管理端 Skills 完整 CRUD 与 UI/UX 完善 — 新增 create-text / list-paged / update / delete API handlers；修复 GBK zip entry 名解码与内联 modal 错误告警处理；`/admin/skills` 升级为响应式流式 modal、金色光标交互、搜索与分页，`/settings` skills tab 对齐视觉标准与 i18n。(2d836d8a)

## [1.37.1] - 2026-07-21

### Summary

v1.37.1 是一次 patch 版本更新，重点修复 **Permission Ceiling 强制执行** 的完整性缺陷，确保会话级权限上限在 Gateway 命令、Worker 原生控制路径、重置（reset）、恢复（resume）及 Worker 替换全生命周期中一致生效。新增 workspace-only skill 列表端点补全 SKill CRUD 闭环。同时修复交互事件（权限/提问/采集）的审计记录结构化程度不足的问题。

### Added

- **Skills**: Workspace-only skill 列表端点 `GET /api/workspaces/{wid}/skills` — 仅返回该 workspace 安装的受管 skill（不含全局/.claude 只读/其他 workspace）；webchat Skills tab 改用该端点并接上 skill 详情对话框（SKILL.md 正文 + 文件清单），凑齐 workspace 完整 CRUD 闭环。(#919)

### Fixed

- **Worker**: 不可变的 Permission Ceiling — 每个 session 持久化 set-once 权限上限，在 Gateway 命令（/plan、/auto-edit）、Worker 原生控制、reset、resume 及 Worker 替换全生命周期中强制执行；Claude Code allowed-tools 硬化、OCS 权限 replay 原子化、Codex sandbox policy 保守映射、SQLite/PostgreSQL 全覆盖。(#920)
- **Audit**: 交互事件（权限/提问/采集）的结构化审计记录 — 新增 `permission.response` / `question.response` / `elicitation.response` action 类型，将 response ID、决策（allowed/denied）、原因、回答解析为结构化 DetailJSON 写入 `user_activity`，取代原有的通用 `message.inbound` 空内容审计。(#99f8c206)
- **WebChat**: 屏蔽预期的 SKILL_* 校验错误（如 `SKILL_FILE_TYPE_BLOCKED`）在浏览器控制台中输出 — 这些是用户输入错误，已有 UI 内联提示，不再产生 console.error 干扰。(#0970f435)

### Security

- **Worker**: Permission Ceiling 加固 — plan 模式下拒绝 `allowed_tools` caller-supplied 规则覆盖；权限应用失败不再静默（`initSessionConn` 返回 error）；set-once 设计确保即使 Worker 重启或替换也无法降级已确定的权限上限。(#920)

### Summary

v1.37.0 是一次 minor 版本更新，核心是 **Skill 管理功能** 的落地与 **OCS read-only 权限绕过** 的安全修复。新增页面级 Skill 管理 HTTP API（用户全局 / workspace / admin 三层共 8 端点）与 webchat 管理界面，支持 zip 上传并经 zip-slip / 解压炸弹 / 越权五重安全管线校验。同时修复了 OpenCode Server Worker 在 read-only（plan）模式下未拦截文件写入的严重权限绕过缺陷，收紧 workspace 边界与审批链路。

### Added

- **Skills Management**: 完整的 Skill 管理 HTTP API 与 webchat UI — 用户全局、workspace、admin 三层共 8 个端点（查询 / 安装 / 读取 / 删除）；支持 zip 包上传安装，经 zip-slip、解压炸弹（≤20MB zip / ≤50MB 解压 / ≤5MB 单文件 / ≤500 entry / >100× 压缩率拒绝）、恶意 entry（symlink/device 过滤）、越权（非 owner → 403）、name 注入五重安全管线；scanner 区分 managed（`.agents`）与 external（`.claude` / `.hotplex`）skill，支持 global + 多 workspace 合并查询与同名遮蔽告警。(#915)
- **WebChat UI**: admin 全局 Skill 管理页与 workspace Skill 管理页，含上传组件、只读外部 skill 标注、admin-nav 入口，中英文 i18n 同步。(#915)

### Fixed

- **Worker (OCS)**: read-only（plan）模式未拦截文件写入 — 根因是 OCS 权限系统为 opt-in allowlist，未匹配规则时默认 ALLOW；原代码仅注入 `read: allow` 致使 `edit`/`write`/`bash`/`shell`/`patch` 全部直接放行。修复方案：plan 模式改为 `*:ask` 通配基线 + 选择性 allow 只读工具，写入操作变为审批请求而非静默放行；新增 workspace 工作区边界（`external_directory` 在 workspace 层 ask、auto-edit 层 allow）；plan 模式忽略 `allowed_tools` 防止 caller-supplied allow 规则重开写入；首启即应用 workspace 权限上限（同步 `si.WorkspaceID`）；权限应用失败不再静默（`initSessionConn` 返 error）；命令失败不误发 delivered ack。(#913)
- **WebChat UI**: 权限审批卡结构化渲染 — 解析权限工具参数（JSON string / object / array 三种形态），分区块展示 command / action / target / reason / 允许目录 / 允许模式，取代原始 JSON dump。(#913)

## [1.36.2] - 2026-07-17

### Summary

v1.36.2 是一次 patch 版本更新，修复了 Gateway Core 的两个执行流可靠性问题。`control.stop` 现在会正确关闭 execution runtime 并通知客户端，避免 runtime 泄露和状态不一致。WebSocket ReadPump 增加了 `Input` 和 `WorkerCmd` 事件的异步处理，防止长时间运行的 Worker 任务阻塞事件循环。

### Fixed

- **Gateway Core**: `control.stop` 现在正确关闭 execution runtime — 新增 `finishRuntimeOnStop` 在停止 Worker turn 时立即结束 runtime 记录，发送 `RuntimeExecutionFailed` 事件到客户端，并在 store 写入失败时通过 repairer 异步修复。(commands.go, handler.go)
- **Gateway Core**: WebSocket ReadPump 对 `Input` 和 `WorkerCmd` 事件增加异步 goroutine 处理，防止长时间 Worker 任务阻塞事件循环导致后续消息延迟或连接超时。(conn.go)

## [1.36.1] - 2026-07-17

### Summary

v1.36.1 是一次 patch 版本更新，聚焦于 **OCS singleton 崩溃可观测性** 与 **WebChat 连接稳定性**。修复了 Windows 下 OCS 崩溃丢失原始退出码、race 与 nil deref 的 P0 缺陷，并为 Bridge 增加原始退出码 hex 日志、指标与 stderr 摘要。WebChat 修复了断连重连时因死 owner 阻塞的 session orphaned 问题，并增强了客户端重连重试逻辑。管理后台新增 API Keys 搜索筛选功能。

### Fixed

- **Worker**: 保留 OCS singleton 崩溃的原始退出码（十进制 + `0x%08X` hex），兼容 Windows NT status 正/负两种 int 表示；ringBuffer 保留 64 行 stderr，启动失败附加有界摘要；`RawExitCoder` 可选接口透传原始退出码到 Bridge 日志/指标/用户消息，不破坏 `Wait()` 归一化契约。(#911)
- **Worker**: 修复 OCS singleton 的 P0 panic（`discover_port` 阶段 `s.proc` nil deref）、`stderrRing` 数据 race、跨生命周期 `lastExitCode` 误归因、POSIX `-1`（信号终止）丢失、UTF-8 mid-rune 截断。(#911)
- **Gateway Core**: `TryAcquireWebChatOwner` 允许已关闭的 owner 连接被替换，修复浏览器 WS 断连后重连的 session orphaned 问题。(#911)

### Added

- **WebChat UI**: 管理后台 API Keys 页面新增搜索筛选（按 user_id / description / api_key 实时过滤），支持搜索无结果提示与清除搜索按钮；中英文双语 i18n。
- **CLI**: doctor 新增 `opencode_server_resolve` checker，检测命令解析、native vs wrapper(`.cmd`/`.bat`/`.ps1`/`.sh`)、`--version` 探测与平台现场采集提示。(#911)

### Changed

- **WebChat UI**: TurnSummaryCard 以 SVG 图标替换 emoji 文本并添加 i18n tooltip；flex-wrap 布局替代 `join('·')`。
- **WebChat UI**: `SessionAlreadyConnected` 重试逻辑增强——以计数器替代一次性 guard，允许无 handoff 时也重试一次，延迟自适应。（handoff 1500ms / 普通冲突 500ms）
- **Docs**: 新增 `troubleshooting-ocs-windows.md` 运维 runbook（`0xC0000142`、Event Viewer、ProcMon、服务账户、EDR 验证）。(#911)
- **Infrastructure**: 优化 4 个 skill（arch-analyzer/release/update/issue-manager）共精简 689 行，提取 troubleshooting 到独立文件。(#912)

## [1.36.0] - 2026-07-17

### Summary

v1.36.0 是一次 minor 版本更新，核心是 **WebChat 可编辑后续消息队列** 的落地。新增页面级、按会话隔离的 FIFO 队列，支持对排队输入进行编辑、删除、重试与立即发送，并在 ACK、重连与终态事件的竞态下完成对账，不持久化 prompt 内容。同时修复了 OpenCode Server 异常退出未恢复与 Turn Summary token 丢失的稳定性问题（#904），并统一了权限模式标签文案（"Auto Edit"→"Auto Mode"、bypass→"最高权限"）。

### Added

- **WebChat UI**: 可编辑后续消息队列 — 页面级、按会话隔离的 FIFO 队列，支持编辑、删除、重试与立即发送；跨 ACK/重连/终态事件竞态完成对账，不持久化 prompt 内容。(#905)

### Changed

- **WebChat UI**: 统一权限模式标签文案 — "Auto Edit" 改为 "Auto Mode"，bypass 档位中文改为"最高权限"，跨 UI 组件与 en/zh-CN i18n 同步。
- **WebChat UI**: SessionPanel 管理入口由原生 `<a>` 改为 Next.js `<Link>`，走客户端路由避免整页刷新。

### Fixed

- **Worker**: Recover OpenCode Server 异常退出与 Turn Summary token 丢失 — 避免 OCS 进程退出后会话卡死，并补齐 Turn Summary 的 token 字段解析。(#904)

## [1.35.1] - 2026-07-16

### Summary

v1.35.1 是一次 patch 版本更新，主要优化了 **智能体多轮会话（Turn）指标统计与 WebChat 重连 UI 体验**。解决了在部分环境下因 Claude Code 和 OpenCode Server 返回字段采用 camelCase 导致 Token 指标解析失效的兼容性问题（#901）。此外，修复了网络重连遮罩盖住输入框导致无法操作的布局问题，重构了用户头像样式并支持了发送消息时展示呼吸微动效的“活着（living）”状态指示器，同时优化了 TTFT 指标在重试周期的生命周期追踪。

### Added

- **WebChat UI**: Beautiful custom user SVG gradient avatar and send "living" pulse ripple animation. (#894)
- **WebChat UI**: Add staged feedback support and visual TTFT (Time to First Token) metrics reporting.

### Changed

- **Gateway Core**: Support camelCase key fallbacks in session accumulator turn stats (`inputTokens`, `outputTokens` etc.) to handle diverse worker schema variants. (#901)
- **Worker**: Support camelCase fallbacks for OpenCode Server `tokens` path (`cacheRead`, `cacheWrite`) to make gateway statistics fully robust. (#901)

### Fixed

- **WebChat UI**: Fix reconnecting banner z-index overlap layout defect to prevent covering control buttons and inputs during network reconnection. (#894)
- **Gateway Core**: Retain TTFT tracking state and timestamp across LLM retry cycles.
- **Events**: Support string parsing and all numeric types in `ToInt64` to prevent token stats extraction failure.

## [1.35.0] - 2026-07-15

### Summary

v1.35.0 是一次 minor 版本更新，聚焦于 **消息投递（ingress）与轮次完整性（turn integrity）的可靠性闭环**，以及 **用户主动中断（Stop Current Turn）等交互控制的落地**。Stop Current Turn 能力让用户可随时取消正在运行的 Worker 轮次而不终止会话，消除了"只能等或强杀"的痛点。EventStore 持久化在重连后不再因 seq 重置损坏事件历史（#879），不可变 Worker-Run 绑定 + 冻结 Forwarder 的设计从根源杜绝了旧事件投递到新连接上的分裂问题（#892/#890）。Admin 后台新增用户显示名称解析与 Activity 面板完整国际化，WebChat 获得推理块自动折叠与连接稳定性的多重修复。

### Added

- **Gateway Core**: Stop Current Turn capability — send `control.stop` to interrupt a running worker turn without terminating the session; the worker detaches, session transitions to Idle, and the user can immediately send a new input. (#893)
- **Gateway Core**: Durable ingress reliability closure — execution owner lease with expiry recovery and repairer, immutable worker-run binding (sync.Map with CAS-guarded lifecycle), fenced session detection with auto-clear on next input, and `StartFreshWorker` path for stale-worker recovery. (#890)
- **Gateway Core**: Enforce single WebSocket per session — session ownership tied to one WS connection; duplicate connections are rejected with a conflict dialog. (#896)
- **WebChat UI**: Auto-collapse reasoning thought block when reasoning completes — no more expanded empty grey box after reasoning finishes.
- **WebChat UI**: Input lifecycle centralized into `_settlePending()` — prevents permanent send lockout after disconnect/reconnect across all error scenarios. (input lifecycle fixes)
- **Admin**: Resolve user display names in sessions and activity views — real usernames replacing raw user IDs throughout the admin UI.

### Changed

- **Session**: EventStore persistence resilience — SeqGen hydrates from DB on reconnect (CAS raise-only, never regresses), unique `(session_id, seq)` index prevents silent seq collision corruption, runWriter goroutine auto-restarts on panic. (#886)
- **Gateway Core**: Turn integrity closure across all platforms — frozen forwarder binding (`launchForwarderLocked`) prevents stale forwarders from splitting event streams; turn-scoped assistant message dedup with bounded `sentTexts`; empty-success terminal classification for webchat/Feishu; per-turn duration clock that excludes inter-turn idle time; OTel spans on `forwardEvents` with trace_id propagation through 16 helper sites. (#892)
- **Gateway Core**: Idempotent input acknowledgements now reflect the intended terminal status (`delivered`/`unknown`/`failed`) independent of DB write success — the client never blocks permanently on a DB transient error.
- **Messaging**: Verbose Debug logs gated behind `Enabled` checks across feishu/slack/yuanxin adapters, session GC, and gateway API — reduces argument allocation on high-frequency paths.
- **Gateway Core**: Ping/pong demoted from debug to trace log level — reduces protocol chatter in debug output.
- **Worker**: Cross-worker `/reset` single-consumer contract — `ConnReplaced` path now launches exactly one new forwarder with frozen binding, eliminating duplicate event consumption. (#892)

### Fixed

- **EventStore**: Session event persistence break on reconnect — SeqGen in-memory reset on WS disconnect caused seq collisions that buried new events under `ORDER BY seq DESC`; hydrated from DB with CAS raise-only counter to restore history visibility. (#886)
- **Gateway Core**: `GetHistory hasMore` truncation kept the oldest turns, discarding the latest exchange — user saw their last input but no response after session refresh. (#892)
- **WebChat UI**: Workspace-handshake error loop — repeated failed handshakes no longer drive a feedback loop of session remount → reconnect → fail again (5s debounce window). (#886)
- **WebChat UI**: Restore expand/collapse toggle for all tool components — regression in tool rendering that broke per-card collapse interaction.
- **Admin**: Complete i18n for activity detail drawer — all hardcoded English labels extracted into zh-CN/en locale files (identity/context/network KV fields, copy button, MSG badge, worker prefix, rows suffix).
- **Audit**: auth.* events now fire at credential boundary only — no more flooded `auth.login` on every webchat cookie-authed request; added `auth.logout` constant. (#888)

## [1.34.0] - 2026-07-13

### Summary

v1.34.0 是一次 minor 版本更新，聚焦于 **跨平台交互投递的端到端可靠性** 与 **飞书交互卡片的 UX 重设计**。引入 Claim/CompleteClaimed/Release 状态机与 WebChat ACK 协议，覆盖 Feishu/Slack/WebChat × 4 个 Worker × permission/question/elicitation 三类交互，消除"用户点击与超时竞态""投递失败静默丢失""按钮点击后消失"等长期问题。同时新增用户拒绝后抑制重复权限卡（permission deny dedup）、Claude Code 平台 session 的 operator permission_mode 默认值，以及 OpenCode Server session 跨 worker 重启持久化。

### Added

- **Messaging**: Deliver interactions reliably across Feishu, Slack, and WebChat for permission, question, and elicitation flows via a Claim/CompleteClaimed/Release state machine — user clicks and timeouts no longer race, and failed deliveries stay retryable instead of being silently dropped. (#863)
- **Messaging**: Redesign Feishu interactive cards — collapse raw command/arguments behind collapsible panels, render multi-question and multi-select prompts as single-callback forms, and adopt semantic button copy with explanatory cards for non-owner clicks. (#863)
- **Gateway Core**: Suppress repeated permission cards after a user denial via a session-scoped, owner-scoped denial cache (`worker.permission_deny_dedup`, default 60s) so agent retries of the same tool don't re-prompt; `/reset` clears the cache. (#863)
- **Worker**: Add operator-configured default `permission_mode` for Claude Code platform sessions (`HOTPLEX_WORKER_CLAUDE_CODE_PERMISSION_MODE`), with the session tier clamped to the operator ceiling; hot-reloadable without restart. (#863)

### Changed

- **WebChat UI**: Gateway now echoes the same-type response event as an authoritative ACK after the worker accepts it; the runtime adapter transitions submitting → resolved/rejected only on ACK, making delivery failures visible. (#863)
- **Worker**: Drain OpenCode Server stderr into the gateway log instead of discarding it, improving operator diagnostics. (#863)
- **Configuration**: Slim AGENTS.md by ~86 lines — move Slack/Cron CLI command examples into an on-demand `hotplex-cli` skill, saving ~512 tokens per session load. (#873)

### Fixed

- **Worker**: Preserve OpenCode Server sessions across worker restarts via a durable cleanup outbox table (`session_cleanup_tasks`, migration 025) with lease-based concurrency and retry tracking. (#873)
- **Worker**: Ensure singleton workers (OpenCode Server, Codex CLI) create the working directory before binding a session — fixes Windows HTTP 500 "Unexpected server error" and Codex thread failures on non-existent paths. (#863)
- **Worker**: Resolve empty `request_id` for Codex CLI `item/commandExecution/requestApproval` via an `approvalId → itemId → requestId` priority chain — fixes Feishu callback error 200672 and "no pending server request" failures. (#863)
- **Worker**: Support legacy Codex CLI approval methods (`execCommandApproval`/`applyPatchApproval`) and route server-requests for id=0 and string IDs. (#863)

## [1.33.2] - 2026-07-09

### Summary

v1.33.2 是一次 patch 版本更新，主要修复了 **OpenCode Server (OCS) 工作区绑定对历史版本（< 1.17）的向下兼容性**。通过在会话初始化请求中同时保留新版的 `directory` 查询参数和旧版的 `project_dir` 消息体载荷，保证了在使用旧版 OCS 服务端时能够正确识别并绑定工作区路径，防止运行中抛出 OCS 实例加载失败的 500 报错。

### Fixed

- **Worker**: Restore `project_dir` field in OCS session creation and clear request payloads to ensure backward compatibility with older OCS versions (<1.17).

## [1.33.1] - 2026-07-09

### Summary

v1.33.1 是一次 patch 版本更新，主要优化了 **交互式问答与授权响应的处理性能与跨平台体验**。通过将 WebSocket 连接事件处理解耦为异步执行，消除了客户端的阻塞 hang 起风险；在前端 WebChat UI、飞书端与 Slack 端，全面对齐并支持了交互卡片的自定义理由及文本直接回复（Write-in Inputs），并自动清理和回显决断内容。此外，优化了 OpenCode Server (OCS) 的 `seenToolCalls` 状态过滤，保证了流式工具状态的高效去重。

### Added

- **WebChat UI**: Add custom text write-in inputs and feedback fields for question, permission, and elicitation cards.
- **Messaging**: Support CardKit input fields in Feishu interaction cards and Block Kit input blocks in Slack interaction cards, extracting and routing user-submitted comments or reasons back to the worker.

### Changed

- **Gateway Core**: Execute interaction responses asynchronously in background goroutines within the WebSocket read pump, resolving UI blocking and socket hangs.
- **Worker**: Initialize `seenToolCalls` on state creation and turn boundaries inside OpenCode Server converter to improve tool invocation dedup reliability.
- **WebChat UI**: Decouple default tool card expansion from collapse interactions, allowing the last tool card of a message to be collapsed.
- **WebChat UI**: Auto-wrap inline code blocks containing tree-drawing characters or newlines into preformatted scrollable blocks for directory trees.

### Fixed

- **Worker**: Add project directory configuration check to ensure the directory query parameter is correctly cleared and forwarded.

## [1.33.0] - 2026-07-08

### Summary

v1.33.0 是一次 minor 版本更新，聚焦于 **跨平台交互式体验（Cross-Platform Interactive UX Loops, #859）** 的完整支持，以及 OpenCodeServer (OCS) 1.17+ 的增强。引入了多平台（Feishu/Slack）交互卡片模板与事件处理机制，WebChat UI 新增表单收集与动态权限审批卡片，并在 Gateway 层面实现了完善的交互超时控制。此外，允许 localhost 开发环境跨域，支持更灵活的 worker 默认权限模式。

### Added

- **Gateway Core**: Support cross-platform interactive UX loops by handling elicitation forms, permission approvals, and question-response flows. (#859)
- **WebChat UI**: Add interactive UX components including `ElicitationFormCard`, `PermissionApprovalCard`, and `QuestionResponseCard` with full i18n support.
- **WebChat UI**: Implement countdown and timeout control with `useInteractionTimeout` hook for action deadlines.
- **Messaging**: Support interactive cards in Feishu and Slack, updating templates and action routing to handle user responses.
- **Worker**: Support OCS 1.17 part metadata parsing to route streaming part deltas.
- **Configuration**: Add `default_permission_mode` under `worker` section in `config.yaml` to configure agent execution permissions.

### Changed

- **Configuration**: Update default `allowed_origins` to allow localhost next dev (port 3000) and embedded webchat (port 8888) by default.

### Fixed

- **WebChat UI**: Support `multi_select` and multiple fields in `QuestionResponseCard`, and fallback to question text if `q.id` is undefined.
- **WebChat UI**: Route admin cookie fetch requests to correct `adminUrl` instead of gateway BASE URL.
- **Gateway Core**: Support multi-select aliases in Question JSON unmarshaling.

## [1.32.3] - 2026-07-08

### Summary

v1.32.3 是一次 patch 版本更新，修复了 API Key 持有者（owner）规范化问题，确保鉴权、审计和跨模块用户标识的准确性与一致性。

### Fixed

- **Security**: Fix API key owner normalization — ensure consistent owner identification across auth, audit, and spill append paths, fixing edge cases where partial key owner metadata was incorrectly appended. (#868)

## [1.32.2] - 2026-07-08

### Summary

v1.32.2 是一次 patch 版本更新，支持 Bearer API 密钥鉴权并同步 WebChat 运行时状态，同时完成了 OAuth SSO 提供商集成的修复，并在开发脚本中优化了后台进程的启动。

### Added

- **Security**: Accept Bearer API keys in Gateway and sync webchat runtime status. (#858)

### Fixed

- **Security**: Complete OAuth SSO provider integration. (#857)
- **CLI**: Use `nohup` and redirect standard input/output for background gateway and webchat processes in development scripts to prevent job suspension.

## [1.32.1] - 2026-07-07

### Summary

v1.32.1 是一次 patch 版本更新，完成用户行为审计子系统（#833）的 P2 阶段交付。核心变更包括：admin activity 时间线 UI（分页、跨通道身份关联、平台标识）、tool-call 审计埋点（敏感工具全量记录 + PII 脱敏）、admin 写操作双写审计、WebhookSink 外部告警扩展、GC 并发安全修复（原子 prune 事务消除 hash chain 断裂风险）、以及 CI release workflow 健壮性修复。

### Added

- **WebChat UI**: Admin activity 时间线页面 — 分页表格、跨通道身份关联（principal_user_id）、平台标识（Feishu/Slack/WebChat/Cron）、结构化详情抽屉（KV 对齐 + JSON 查看器 + PlatformBadge）、i18n 完整支持（中/英）。(#854)
- **Audit**: Tool-call 审计埋点（spec §5.2）— 自动记录所有 worker tool 调用，敏感工具（Bash/Write/Edit/WebFetch 等）存储全量 detail_json，非敏感工具记录 input_sha256 + 200 rune 预览；PII/凭证自动脱敏（API key / Bearer / password 等前缀替换）。
- **Audit**: Admin 写操作双写审计（spec §7）— 所有 admin 写操作同步写入 slog admin_audit + tamper-evident user_activity 表，action 命名空间 `admin.*`（如 `admin.bot.create`）。
- **Audit**: 外部 RegisterSink 扩展点 + WebhookSink 参考（spec §5.6）— 公开 `sinks.Register()` 扩展契约，内置 HMAC-SHA256 签名 WebhookSink（bounded queue 1024、3 次指数退避、drop-on-overflow）。
- **Audit**: `audit.full_content_retention` 配置（spec §5.3）— 控制事件 drill-down 引用的有效窗口（默认 90d），延长 event_ref 在事件过期后仍可回溯的时长。
- **Audit**: 审计配置变更 meta-audit（spec §5.2）— 运行时 audit config 变更（enabled/retention/collector/sinks 等 7 字段）自动写入审计表，支持变更前后 diff 追溯。
- **Audit**: 跨通道身份关联 — SQLite/PostgreSQL 迁移 `024_audit_identity_links`，支持同一用户跨平台的身份链接查询。

### Changed

- **Audit**: GC prune 重写为单事务原子操作 — 消除 GC tick 与 flushBatch 之间的 hash chain 断裂风险（原先 find→checkpoint→delete→checkpoint 为 5 次独立 store 调用，并发 flush 可在中间插入行导致链断裂）。PG 通过 advisory lock 保障多 pod 安全。(#845)
- **Audit**: Collector sink 生命周期加固 — `Close()` 等待所有 in-flight sink goroutine 完成后再关闭 spill file，消除 goroutine 泄漏风险；最终 flush 使用 gateway shutdown ctx 而非 `context.Background()`。
- **Audit**: 导出失败同样记录审计事件 — 导出成功/失败均写入 `system.audit_export` 审计行，失败以 Outcome=failure 记录；detail 改用 `json.Marshal` 替代 `fmt.Sprintf('%q')`（JSON 安全）。
- **Security**: WebChat 用户身份标签修正 — cookie 认证用户正确标记为 `PlatformWebChat` + `UserIDTypeRegistered`，不再被错误归类为 opaque platform handle。(#845)

### Fixed

- **CI**: OpenCode 版本解析健壮性 — release workflow 中 sed JSON 解析在 GitHub API 限流时静默返回空串，导致 Pack 离线 bundle 失败。改用 jq + `curl -sf` + 空值检查。(#854)

## [1.32.0] - 2026-07-06

### Summary

v1.32.0 是一次 minor 版本更新，核心主题是**用户行为审计子系统（#833）的完整交付**。新增 `internal/audit/` 模块——涵盖 SHA-256 hash chain 不可变存储、zero-loss WAL spill collector、双数据库（SQLite/PostgreSQL）支持、retention GC + chain verifier、AlertSink 可扩展告警接口、Admin 活动查询 API（按用户查询 + PII 脱敏 + JSON/CSV 导出），以及 auth/session/gateway 三个维度的审计埋点。Worker 层面修复了 OCS 1.17 V1 协议 cursor-spin bug（#6562ac35）。

### Added

- **Audit**: 完整用户行为审计子系统（#833）— `internal/audit/` 模块，包括：
  - **类型与 Hash Chain**: 16 列 `user_activity` schema + SHA-256 hash chain（prev → self 链式校验）+ checkpoint rebase 支持
  - **双数据库存储**: SQLite（WriteMu 序列化）+ PostgreSQL（advisory lock 819207），NewStore 工厂自动选择
  - **Zero-loss Collector**: 3 级 Enqueue 回退（channel → WAL spill O_SYNC → bounded block 5s），fan-out 到 AlertSink
  - **WAL Spill**: 崩溃安全的磁盘溢出（4-byte BE length + JSON payload，自动跳过截断记录）
  - **Retention GC**: 基于时间窗口的保留裁剪 + checkpoint 保存，支持 full-prune edge case
  - **Chain Verifier**: 流式分页验证（O(batchSize) 内存，不 OOM），检测 hash chain 断裂
  - **AlertSink 接口**: Noop/Log 内置实现 + 类型安全注册表，P2 可扩展 Webhook/Slack/SIEM
  - **数据库迁移**: `023_user_activity.sql`（SQLite + PG），含 BEFORE UPDATE 不可变触发器 + 3 个查询索引
- **Admin API**: 活动查询端点 — `GET /admin/users/{id}/activity`、`GET /admin/activity`，支持 PII 脱敏（IPv4 末段置零、IPv6 后 4 组置零、UA 截断）+ JSON/CSV 导出（CSV formula injection 防护，OWASP CWE-1236）
- **Gateway Core**: 审计埋点 — auth（6 条路径）、conn init（3 条）、deliverToWorker（7 条）、session lifecycle（5 条）全覆盖
- **Configuration**: AuditConfig 热重载支持 — `audit.retention` / `batch_interval` / `batch_size` 运行时更新，`audit.enabled` 需重启
- **Observability**: 5 个审计专属 Prometheus 指标 — events / chain_breaks / spill / write_failures / sink_failures

### Fixed

- **Audit**: SQLITE_BUSY 竞态 — GC tick 与 collector flush 并发写 SQLite 时缺少序列化，导致 SQLITE_BUSY 错误和 zero-loss re-spill。修复：BeginTx 获取 writeMu，Commit/Rollback 释放（releaseOnce 防重入 + panic-safe）
- **Audit**: Atomic spill drain — Drain() 拆分为 ReadAll + Truncate 两步操作，中间 Enqueue 写入被 Truncate 丢弃（zero-loss 违反）。修复：单次 mu 锁内原子读取+截断
- **Audit**: EventID 碰撞 — `fmt.Sprintf("ev_%d_%x", ts, hash)` 在同一毫秒内产生相同 ID，破坏 sink 去重契约。修复：UUIDv7（RFC 9562）原生实现（48-bit unix ms + 12-bit atomic counter + 62-bit crypto/rand）
- **Audit**: CSV formula injection — user_id / user_agent 字段可被攻击者控制，CSV 导出未做防护。修复：`= + - @ Tab/CR` 前缀加单引号
- **Audit**: GC retention 热重载未生效 — watcher 接受 `audit.retention` 变更但未传播到 GC 实例。修复：mutex-guarded `UpdateRetention` 回调
- **Worker (OCS)**: OCS 1.17+ V1 协议 cursor-spin — `session.status(idle)` + `session.idle` 标记 turn 结束但不 emit `session.next.step.ended`（usage 唯一来源），旧 Done 门控依赖 stats 非空导致 turn terminator 被静默吞掉，WebChat cursor 永远不停止。修复：Done 去耦于 stats，`consumeDone()` 首次调用无条件发射

## [1.31.2] - 2026-07-05

### Summary

v1.31.2 是一次 patch 版本更新，核心主题是**流式 delta 传输链路的端到端修复与稳定性加固**。WebChat 前端修复了流式消息 delta 在 turn 终结点和背压场景下的丢失问题（#842），引入历史记录 reconcile 兜底机制；Gateway Core 新增 droppable 事件的非阻塞广播与 reasoning 背压语义统一；飞书适配器修复 streaming card 因长 delta 间隔导致 TTL 过期的静默关闭问题（#839）；Worker 层面修复了 proc.Kill() 在孙子进程持有管道时 cmd.Wait() 死锁（#838）、stdin 写入无 ctx 保护导致会话挂死、以及 CodexCLI mapper snapshot 前缀漂移等多个深层竞态和死锁问题。附带一篇架构文档（Streaming-Delta-Pipeline）。

### Added

- **Gateway Core**: 流式 delta 链路架构文档（`docs/architecture/Streaming-Delta-Pipeline.md`）— 四段九跳全链路 ASCII 图解（Worker 4 适配器 / Bridge 12 步 / Hub / Conn / WebChat），三层背压不对称设计说明，设计准则三条。
- **WebChat UI**: vitest 测试框架引入 + merge-parts 纯函数单测（15 个）— 抽取 `appendTextDelta` / `appendReasoningDelta` / `concatTextParts` 纯函数，独立验证 delta 合并逻辑。
- **Worker**: `base.WriteWithCtx` 共享 helper（`write_ctx.go`）— 为所有 stdin 写入提供 goroutine + ctx + grace period 保护，消除 worker 进程停止读 stdin 时 gateway 会话挂死。
- **Docs**: 飞书 streaming card TTL rotation spec（`docs/specs/Feishu-Streaming-Card-TTL-Rotation-Spec.md`）、CodexCLI delta integrity fix spec。

### Changed

- **Gateway Core**: Hub.sendBroadcast 拆出 trySendBroadcast（default 非阻塞），droppable 事件（delta/reasoning/raw）满时立即丢弃，消除单慢 session 阻塞跨 session 级联丢失；writeCh cap 64→256。Reasoning 加入 isDroppable 统一背压语义。
- **Gateway Core**: 删除 dropped 标记端到端协议（sessionDropped map / MarkDropped / GetAndClearDropped），前端 reconcile 改为无条件触发（内部已有前缀+长度+cancelled 三重保护）。
- **WebChat UI**: AssistantMessage memo 改为内容浅比较（替换失效的引用比较），修复 streaming cursor 不消失的 regression。

### Fixed

- **WebChat UI**: 流式 delta 在多个环节丢失（#842）— RAF 批处理在 done/error/unmount 时不 flush 残留 pendingDelta；trySendData 丢弃 delta 时不标记 done.dropped；reconcile 事件顺序错配（ASC vs DESC）导致 100% 死路径；memo 引用比较实质失效致每次 delta 重渲染所有消息。
- **WebChat UI**: 多 turn dropped 标记永不重置 — Conn.droppedMarked 跨 turn 长生命周期，Swap(true) 永久置位后 done.dropped 永不上报。改为 session 级去重（RLock fast path）。
- **WebChat UI**: reconcile 从 events API 改用 history API 的 TurnRecord — 消除 4KB size-flush 截断风险和 turn 边界歧义；加 collector 未落库重试窗口（2s）。
- **WebChat UI**: reasoning-content 的 `white-space:pre-wrap` 与 ReactMarkdown 渲染矛盾导致冗余换行。
- **Messaging (Feishu)**: streaming card 因长 delta 间隔（工具思考/长计算）错过 ~100s TTL rotation 窗口，被飞书 10min 硬限制静默关闭（#839）— 新增 proactive `time.AfterFunc(StreamTTL)` 独立于 delta 到达触发轮转。
- **Messaging (Feishu)**: 纯工具型 turn 空回复 — 全程只调工具不产出 message 时，飞书卡片/Slack 收到空回复；bridge 在 done 时合成含工具调用摘要的兜底 Message。
- **Worker (proc)**: proc.Manager.Kill() 持 m.mu 等待 cmd.Wait() 死锁（#838）— 孙子进程持有继承的 stdout 管道时 cmd.Wait() 永久阻塞，毒化所有 Manager 方法。修复：cmd.Wait() 移至后台 goroutine，Kill() 用 killWaitTimeout (5s) 限时返回。
- **Worker (OCS)**: idle drain 持 s.mu 调 proc.Kill() 死锁（#836）— 改用 proc.ForceKill(pgid) 不触碰 proc.mu；补充 TOCTOU 防护与 monitorProcess 状态守卫。
- **Worker**: stdin 写入无 ctx 保护致会话挂死 — 3 个适配器共 11 个未保护的写入点（claudecode 6 / codexcli 3 / acp 2），修复后全部经 base.WriteWithCtx 保护；orphan goroutine mutex 泄漏致并发写竞争也一并修复。
- **Worker (CodexCLI)**: mapper getDeltaText 在后端 re-sample/truncation 时前缀不变量被打破，静默丢弃或错位 delta — 新增 prefix 校验与 corrective delta 机制，drift 计数暴露至 DoneData.Stats。
- **Worker (ClaudeCode)**: Input/Compact stdin 写入 stall 不触发 worker recovery — DeadlineExceeded 归为 ErrKindUnavailable，gateway handler 识别后清理崩溃 worker。
- **Worker (CodexCLI)**: singleton manager 无 deadline 的 Notify wrapper 无超时保护 — writeFrame 检测 ctx 无 deadline 时自动包装 30s 超时。

## [1.31.1] - 2026-07-03

### Summary

v1.31.1 是一次 patch 版本更新，是 v1.31.0 中英双语 i18n 框架的**补全与 WebChat UI/UX 收尾**（#830）。集中消除核心聊天组件残留的英文硬编码、补齐 icon-only 按钮的无障碍标签（aria-label / focus trap / 焦点转发），并新增断线重连横幅、失败消息视觉区分、移动端 overlay drawer 等交互改进；同时修复登录页中文翻译缺失、OAuth 提供商 CORS 预检、dev 跨站 cookie 三个互相独立的根因。无新对外 API、无破坏性变更。

### Added

- **WebChat UI**: 主聊天面 i18n 补全（#830）— 消除 thread / ReasoningBlock / PreAssistantIndicator / MarkdownText / AssistantMessage / ToolLoadingSkeleton 等 8+ 处英文硬编码，zh-CN/en locale 同步对齐；`connLabel` 由模块级字符串常量重构为 i18n key 映射。
- **WebChat UI**: icon-only 按钮无障碍标签（#830）— 为 composer Send/Cancel、顶栏 sidebar/docs/settings、ThemeToggle、LanguageSwitcher 补 i18n `aria-label`；新增 `chat.json` aria 命名空间。
- **WebChat UI**: 断线重连横幅与搜索空状态引导（#830）— `disconnected` 时在 composer 上方显示醒目横幅（原仅角落小圆点）；SessionPanel 搜索无结果升级为放大镜图标 + 居中文案。
- **WebChat UI**: 失败消息视觉区分与重发引导（#830）— AssistantMessage 检测结构化 `status==="error"`，加 coral 左边框并追加 i18n 重发提示。
- **WebChat UI**: 移动端会话侧栏 overlay drawer（#830）— 移动端改为 fixed overlay + 半透明遮罩，桌面端保持 inline 折叠；附 ESC 关闭、body scroll lock、focus trap、`role=dialog` / `aria-modal`。
- **Docs**: 用户行为审计系统设计 spec（`docs/specs/User-Behavior-Audit-Spec.md`）— 独立 `user_activity` 审计表 + hash chain 不可变 + checkpoint rebase 裁剪 + AlertSink 可扩展接口 + 3 年保留；经自审与独立 agent 审计修正全部 P0/P1。

### Changed

- **WebChat UI**: workspace 下拉 hover-close（#830）— 由 click-toggle 改为 panel `onMouseLeave` 延迟 300ms 收起，click-catcher 仅保留 click-outside 关闭；经两轮 code review 修复“弹出后立即消失”与 `mouseleave` 失效问题。
- **WebChat UI**: global-error 恒暗配色去除 magic number — 由硬编码 hex 改为 import `globals.css` + `var()` 对齐暗色 token。
- **WebChat UI**: `globals.css` 删除逐字重复的动画定义（quantumBloom / rotateOrbit / quantumWobble / electricalGlow 及 `.skeleton-text` 第二份重复），消除零行为死代码。
- **WebChat UI**: 抽取 `lib/hooks/useTimeout` 与 `components/icons SearchIcon` 复用，消除手写 timer 模式与重复图标定义。

### Fixed

- **Gateway Core**: `/api/auth/oauth/providers` 漏注册 OPTIONS 预检路由 — ServeMux 返回 405 且不经 `corsMw`，浏览器以 CORS 错误阻断 `getOAuthProviders()`；补 OPTIONS 路由与 `/api/auth/*` 一致。
- **WebChat UI**: 登录页中文翻译缺失 — `zh-CN/auth.json` 大量 key（tab/label/placeholder/button）误填英文，中文用户看到满屏英文；翻译为中文（en 已正确，不动）。
- **Configuration**: dev 跨站 cookie 不携带致登录态丢失 — webchat（:3000）与 gateway（:8888）loopback 不同主机被浏览器判为跨站，`SameSite=Lax` 阻断 cookie；`config-dev.yaml` 设 `cookie_same_site=none`（loopback 属 secure context，Secure cookie 可用，CSRF 已由 `csrfMw` 防护）。
- **WebChat UI**: 工具组件硬编码英文 label → i18n — SearchTool / ListTool / TodoTool / FileDiffTool 加载文案改走 `chat:tool.*.loading`；顺手删除预先存在的死 import。
- **WebChat UI**: 断线状态语义化 — 引入 `reconnecting` / `reconnect_failed` 事件与状态，`disconnected` 改终态文案、`reconnecting` 显示重连中，消除“重连耗尽后仍承诺正在重连”的误导。
- **WebChat UI**: 错误消息检测由 ID 前缀嗅探改为结构化 `status` 字段；移动端首屏 `sidebarOpen` 不再盖住聊天区（mount 时 `matchMedia` 检测）；SessionPanel 搜索框补 `aria-label`；删除零引用死 key `chat:aria.scroll_to_latest`。

## [1.31.0] - 2026-07-03

### Summary

v1.31.0 是一次 minor 版本更新，核心主题是 **WebChat 中英双语国际化（i18n）**。基于 react-i18next + i18next 为登录、引导、设置、工作区、聊天与全部管理端页面接入完整中英双语资源，默认 `zh-CN`，附重新设计的语言切换器（匹配主题切换风格、点击直接循环切换、状态持久化于 LocalStorage）。附带一项安全加固（cookie `SameSite` 属性可配置以支持内网 HTTP 部署）以及一组稳定性修复：Feishu dispatcher 消除 `os.Stdout` 劫持导致的 data race、Worker reasoning/text delta diff 与 UTF-8 边界截断修复、CLI 启动 banner 优化（Truecolor + loopback URL）。

### Added

- **WebChat UI**: 中英双语国际化（#818）— 基于 `i18next` + `react-i18next` 搭建 i18n 基础设施，为 login / onboarding / settings / workspaces / chat 及全部 admin 页面（sessions / workspaces / API keys / cron / bots）接入完整双语资源（`webchat/locales/{zh-CN,en}/*.json`），`zh-CN` 为默认语言。语言切换器重新设计以匹配主题切换风格、点击直接循环切换，状态持久化于 LocalStorage `hotplex.locale`；error / global-error 边界页通过 i18n 实例直连安全本地化（脱离 React context）。完整 TypeScript 类型安全。
- **Configuration**: 双语 i18n 前端编码规范写入 `AGENTS.md` — 禁止硬编码中英文 UI 文案、zh-CN/en 资源属性键必须同步对齐、按业务域划分 Namespace（common / chat / admin / auth / errors）、`useTranslation('namespace')` 与 `{{param}}` 插值调用规范。
- **Security**: cookie `SameSite` 属性可配置，支持内网 HTTP 部署 — 新增 `cookie_same_site` 配置项（默认 `lax`），HTTP 部署时启动打印安全告警日志。`README` / `configs/README.md` / `env.example` 同步文档化安全权衡。

### Changed

- **CLI**: 启动 banner 优化 — 24-bit RGB Truecolor 渲染颜色与分隔线；绑定地址统一规范化为 loopback（`0.0.0.0` → `127.0.0.1`、`[::]` → `[::1]`）使 URL 可点击；bot 适配器状态表列对齐；Feishu SDK 默认 logger 经 `SlogLogger` 注入而非劫持 `os.Stdout`，消除 SDK 启动噪音。
- **WebChat UI**: 全局交互与设计令牌规范化 — cursor 与微交互统一，工具卡片 / Markdown 代码块 / 终端 / FileDiff 组件的 border-radius 收敛至设计系统变量（`radius-md`）；Markdown 代码块启用自动换行 + 主题感知折叠渐变 + `shadow-sm`；聊天输入框 focus 动画精简为仅外环 focus-ring。

### Fixed

- **Messaging (Feishu)**: dispatcher 注入 `SlogLogger` 替代劫持 `os.Stdout`（#828 P1）— `NewEventDispatcher` 构造窗口内将进程级 `os.Stdout` 重定向到 `/dev/null` 以消音 SDK 默认 logger，与并发 goroutine（slog stdout handler / 其他 adapter）竞态丢日志且构成 data race。改为直接注入 `d.Config.Logger = SlogLogger{}`，与 ws client 的 `WithLogger` 同一路径，彻底移除 `os.Stdout` 劫持。附带修复：`SlogLogger` 补 nil 防御 fallback `slog.Default()`；新增 `sdkWarnFilter` 按 level 过滤 SDK 日志（Debug/Info 仍静默 ping-pong 与重连，Warn/Error 不再被误静默，恢复"故障仍经 Warn/Error 上报"语义）。
- **Worker**: codexcli / claudecode 实现 reasoning / text delta diff 计算 — 此前 turn summary 聚合依赖完整文本覆盖，delta 缺失导致思考过程与文本更新错位。改用 rune count 替代 byte len 进行长度追踪，防止 UTF-8 多字节边界截断导致思考内容丢失；terminal 工具对齐 raw split bytes 支持可折叠输出。
- **CLI**: banner `formatBannerURL` 处理 `0.0.0.0` 无端口形式与 IPv6 `[::]` / `[::]:port` wildcard — 此前仅匹配带冒号的 `0.0.0.0:` 与 IPv4，无端口地址与 IPv6 wildcard 落入回退分支打印不可访问地址。补充对应分支 + table-driven 测试覆盖。
- **CLI**: `doctor --help` 列全 10 个 checker category（补 `tts` / `agent_config` / `worker`）— flag help 与 Long 描述遗漏 3 个类别，与 `worker.claude_auto_mode`（issue #789，第 26 个 checker / 第 10 个 category）对齐；`checkers/AGENTS.md` 计数同步 25→26 / 9→10。
- **WebChat UI**: thought process 流式自动展开、多空行压缩与段落间距收紧、Terminal / FileDiff 浅色模式文本可见性修复、terminal collapse 按钮配色对齐终端主题（accent-emerald）。

## [1.30.3] - 2026-07-01

### Summary

v1.30.3 是一次 patch 版本更新，聚焦于 **兼容性修复与构建链解封**，并附带一项改善 Worker 选择体验的轻量功能。① 恢复 v1.29.0 的 REST CreateSession 兼容性——v1.30 引入的 workspace_id 硬必填导致企业级 REST 集成升级后 400 "workspace_id is required"；② 对齐 admin create 与 gateway start 的 --config 默认值，避免 admin 写库与网关读库不一致；③ 解封 pnpm 11 构建链（trustLockfile），消除新依赖发布后 24h 的 supply-chain 验证阻塞。此外新增 `GET /api/workers` 端点，WebChat 创建会话时只列出实际安装的 Worker 引擎，避免误选未安装引擎。

### Added

- **Gateway Core**: `GET /api/workers` 端点 — 返回所有注册 worker 类型（claude_code / opencode_server / codex_cli / acp）及其二进制在主机 PATH 的安装状态（`installed` + `path`）。WebChat `NewSessionModal` 据此动态只列出已安装引擎，默认选第一个可用 worker，避免选未安装 worker 导致启动失败。

- **CLI**: doctor 的 `worker_binary` checker 升级 — 从硬编码仅查 claude / opencode 两项，改为按 config 遍历全部 4 种 worker 命令检查安装状态，doctor 诊断覆盖面与实际支持的 worker 对齐。

### Fixed

- **Gateway Core**: REST CreateSession `workspace_id` 放宽为可选，恢复 v1.29.0 兼容（#825）— v1.30 引入的硬必填（`44f461ff`，PR #746）导致企业级 REST 集成升级后 400 "workspace_id is required"。现与 WS init（`conn.go:334`）、ListSessions（`api.go:89`）对齐为可选语义：传则走多租户归属校验，不传则恢复 legacy 行为（work_dir 取 query / config 默认值，session key workspaceID 维度为空）。零客户端改动恢复兼容。

- **CLI**: `admin create --config` 默认值与 `gateway start` 对齐（#826）— 此前 admin 命令 `--config` 默认为空字符串，不传时加载内置默认配置而非 `~/.hotplex/config.yaml`，导致 admin 写入的数据库与网关读取的数据库不一致。改为统一 `config.DefaultConfigPath`。

- **WebChat UI**: 解封 pnpm 11 构建链（#822）— pnpm 11 默认启用 `minimumReleaseAge=1440`（24h），在 `--frozen-lockfile` 下锁定精确版本，新发布的传递依赖（如 typescript-eslint@8.62.1）无法回退到旧版本，触发 `ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION`，阻塞 `make build` 约 24h。通过 `trustLockfile=true`（跳过对已审查 lockfile 的二次校验）+ 固定 `pnpm@11.4.0`（CI 与本地一致）修复。

### Changed

- **Configuration**: 补齐 `opencode_server.context_window` 配置项文档（`docs/reference/configuration.md`）— turn summary parity（#776/#777/#778）为 OCS worker 引入的 YAML 配置（默认 200000）+ `HOTPLEX_WORKER_OPENCODE_SERVER_CONTEXT_WINDOW` env fallback 此前遗漏，现与代码同步。

## [1.30.2] - 2026-06-30

### Summary

v1.30.2 是一次 patch 版本更新，聚焦于 **codexcli 并发安全与 admin 面板可用性修复**。修复共享 app-server 模式下并发 codex session 互相覆盖 model / context window / lastUsage 的严重并发数据竞争（#813），并修复 v1.30.1 引入的三处 admin 面板回归：SPA 路由被主 mux 拦截导致面板不可访问、single-bot 模式 bot 详情页落到 "No bot name specified"、cookie-admin 通道暴露无意义的 Connection 设置入口。

### Fixed

- **Worker (codexcli)**: 并发 codex session 共享 app-server 时互相覆盖 model / context window / lastUsage（#813）— 用 threadID-keyed map 替换进程级单一 converter，`LastContextUsage` 与 `SetCurrentModel` 现按 threadID 隔离；Unsubscribe 与 monitorProcess 清理 per-thread mapper，避免跨 subscribe 周期与进程崩溃后的状态泄漏。含跨线程隔离、Unsubscribe 删除状态、monitorProcess 全量清理、未知 thread 返回空、10-goroutine `-race` 回归测试。

- **Admin**: SPA admin 面板通过 gateway 端口（8888）无法访问（v1.30.1 `c97f0c98` 回归）— 撤销在主 gateway mux 末尾挂载 `/admin/` 的改动，`/admin/*` 重新 fall through 到 webchat SPA fallback；admin API 仍由独立 admin server（9999）提供。

- **Admin**: single-bot 模式（env-only 平台级 `BOT_TOKEN` / `APP_ID`，无 YAML `bots` 列表）下 bot 卡片链接与详情页落到 "No bot name specified"，导致该模式 bot 在 admin 面板完全不可访问 — `normalizeSlackBots` / `normalizeFeishuBots` 为 single-bot 设 `Name = 平台名`（slack / feishu，保证 `GetByName` 全局唯一）+ `IsSingleBot = true`；`BotEntry` 透传 `IsSingleBot`；新增 `agentConfigBotName()` 对 single-bot 传空 botName 给 agentconfig，保留 `dir/<platform>/` 平台级目录语义与向后兼容。

### Changed

- **Admin**: cookie-admin（内嵌 webchat，经 chat session 鉴权）隐藏 Admin Connection 设置入口 — 该通道无独立 admin token 可配置，`/admin/settings` 对其无意义。AdminShell 按鉴权通道传入 `showConnectionSettings`：cookie-admin 隐藏，admin-token（standalone）保留；AdminNav 相应过滤 `/admin/settings` 导航条目。

## [1.30.1] - 2026-06-30

### Summary

v1.30.1 是一次 patch 版本更新，聚焦于 **worker 统一化与安全加固**。新增 workspace 权限模式（4 档 x 4 worker 统一映射），补齐 codex/acp/ocs 的 turn summary parity（模型名 + 上下文窗口），以及 admin 后台收口（cookie 通道 / CSRF / 审计 / workspaces 管理）。WebChat 获得亮暗主题系统和 workspace 切换修复。

### Added

- **Worker**: workspace 权限模式（#789）— admin 可为每个 workspace 指定统一 4 档权限（`read-only` / `workspace` / `auto-edit` / `bypass`），网关在 Worker 启动时映射到各 Worker（CC / Codex CLI / OCS / ACP）的原生权限参数，收紧默认 blast radius。含 migration 022（SQLite + PG nullable 列）、`worker.PermissionMode*` 常量 + `ValidatePermissionMode`、bridge `resolveWorkspacePermissionMode`（仿 `resolveWorkspaceOverrides`）、admin UI 权限分段控件、doctor `claude_auto_mode` 能力检测。

- **Worker**: Turn summary parity for codexcli、ACP、opencodeserver（#776、#777、#778）— workers now report model name + context window in done events, matching claudecode behavior. Messaging session idle transition fixes prevent >30min zombie scanner reaping for Slack/Feishu sessions（#815、#816、#817）.

- **Admin**: Workspaces management console — global workspace list with inline permission_mode editing at `/admin/workspaces`（#807）. Platform-base agent config editing for webchat channels（#806）. Backend restructuring — cookie channel for admin auth, CSRF same-origin enforcement, audit logging for all write operations（#788）.

- **WebChat UI**: Light/dark theme system with localStorage persistence + system preference detection. ThemeToggle component with animated sun/moon icon. i18n bilingual foundation spec（#818）.

### Changed

- **Worker**: `SessionInfo.PermissionMode` 语义重定义——从旧的 CC 私有 3 档（`default`/`plan`/`auto-accept`）改为统一 4 档（`read-only`/`workspace`/`auto-edit`/`bypass`）。CC `auto-edit` 档用原生 `--permission-mode auto`（强制升级，不回退 `acceptEdits`）。`SkipPermissions` 降级为 `bypass` 的 legacy 别名（生产无赋值点）。

- **Configuration**: permission_mode 收归 admin-only + 默认 workspace（r3）（#805）— `config.worker.default_permission_mode` 从 no-op 转为 bridge 真正消费，缺省 bypass->workspace。非 admin 传 permission_mode 字段 -> 403 fail-closed。

- **Gateway Core**: Messaging session idle transition — worker 完成 turn 后自动转入 IDLE 状态，避免 >30min 僵尸回收误杀飞书/Slack 会话（#815）。Worker LastIO 在每事件刷新防止长时间 turn 被误杀（#815 Fix C）。Codexcli history recovery for zombie-reclaimed sessions（#815 Fix B）。Idle timeout dead-config warning（#817）。

- **WebChat UI**: ChatContainer / Thread / SessionPanel 重构。Next.js 16 flat-config eslint 迁移。CSS 变量主题系统（light/dark tokens）。

### Fixed

- **WebChat UI**: Workspace switching now defaults to most recent session instead of always creating new sessions. Fixed `savedId` blocking auto-create on new workspace（#795）.

### Security

- **Worker**: bridge 不再无条件注入 `bypass`（#789 review P1 回归修复）。仅当 admin 为 workspace **显式**设置 `permission_mode` 时才注入；否则注入 `""`（"worker default"），让每个 Worker 应用自身默认/操作员配置（CC/OCS->bypass；Codex->`cfg.Sandbox`；ACP->`cfg.AutoApprove`）。fetch 失败同样降级为 `""`，避免静默把受限 operator 配置提权到 bypass。`config.worker.default_permission_mode` 当前为 no-op（已接受 + 热重载，但 bridge 不注入），保留以备将来按 worker-type 细化。

## [1.30.0] - 2026-06-25

### Summary

v1.30.0 是一次 minor 版本更新，主题是 **WebChat 多租户化**（spec ①-⑦ 全套落地）。引入 workspace 作为跨通道租户锚，覆盖企业 SSO（OIDC 统一认证）、per-workspace agent-configs 两层继承、workspace 级 worker 选择、配额热重载与聚合指标，以及完整的多租户前端（登录 / workspace 切换 / 设置页）。附带 admin 模块完整重构与 work_dir 沙箱化。

> **WebSocket Gateway 客户端 SDK 集成：完全向后兼容，现有集成无需改动。**
> - AEP 事件协议（`pkg/events`、`pkg/aep`）**未变更**（仅新增文档）
> - Session key 公式**向后兼容**：新增的 `workspaceID` 为可选参数，为空时保持旧 4 字段 hash，现有 session key 不变
> - 客户端 SDK（Go / TypeScript / Python / Java）**零改动**
> - WS init 握手**纯增量**：新增可选 `workspace_id` 字段，未设置时整块跳过
>
> **唯一行为边缘变更**：WS init 的 `session_id`（及 REST 的 `client_session_id`）若包含 `|` 字符，现在会被拒绝（`protocol_violation` / `invalid_message`）。此前是静默 alias 到错误的 session key —— 用 `|` 的客户端本就已损坏 —— 现在从静默 bug 变为硬错误。

### Added

- **Gateway Core**: WebChat 多租户地基（spec ①）— workspace 作为跨通道租户锚，`DeriveSessionKey` 扩展为 5 字段（ownerID, workerType, clientKey, workspaceID, workDir），`workspaceID` 为空时保持旧 hash；含 DB migrations 017-021（multitenancy tables / api_key_users / user_identities / 索引）。(#746)
- **Gateway Core**: workspace 跨通道租户接入（spec ⑦ Phase 1）— WS init envelope 新增可选 `workspace_id` 字段绑定 session 到 workspace，含 owner 校验、disabled 校验、anonymous dev-mode carve-out。(#773)
- **Configuration**: per-workspace agent-configs 两层继承（spec ②）— workspace 级配置覆盖全局。(#748)
- **Configuration**: workspace 级 worker 选择（spec ③）— 每个 workspace 可独立指定 worker_type。(#753)
- **Session**: 配额增强（spec ⑤）— per-workspace 配额热重载 + 聚合指标。(#755)
- **Security**: 企业 SSO（OIDC 统一认证）（spec ④）— 新增 `oauth_handlers.go` + `user_identities` 表。(#757)
- **WebChat UI**: 多租户前端一等公民化（spec ⑥）— 登录 / workspace 切换 / 设置页 + 后端 A1-A5 接线。(#762)
- **WebChat UI**: Settings Modal + 顶部 workspace bar（spec ⑦ Phase 3）。(#779)
- **WebChat UI**: workspace work_dir 可变 + Settings 页面 UI 重构 + workspace 创建入口 + Default 落沙箱。
- **WebChat UI**: session card 重新设计，优先展示 title。
- **Security**: workspace work_dir 沙箱前缀强制 — 防止 workspace work_dir 逃逸到约定目录之外。

### Changed

- **Admin**: admin 模块完整重构 — 前端去重 + 后端契约修复 + 文档补齐。(#764)
- **Admin**: Phase 2 错误信封统一 + AdminShell 预检。(#774)
- **WebChat UI**: Phase 3 契约修复 + UpdateWorkspace 乐观并发（`version` 字段 ETag）。(#775)
- **WebChat UI**: Settings General 去重 + workDir 沙箱可视化 + Tab 布局统一。
- **WebChat UI**: 移除死 admin 页面与未用模块。

### Fixed

- **Security**: session key aliasing — client-controlled `session_id` / `client_session_id` 含 `|` 会与 `DeriveSessionKey` 字段分隔符碰撞，alias 到错误的 (clientKey, workspaceID) 对。新增 `ValidateClientKey` 在所有入口预检。(#746 review P3)
- **Gateway Core**: REST/WS identity split — 已登录用户持续 "workspace access denied"。根因：跨源模式下浏览器无法在 WS upgrade 上附带 `X-API-Key`，WS 走 cookie 鉴权而 REST 走 api-key，后端 identity 解析 api-key-first 导致两侧用户不一致。改为 logged-in 时优先 cookie。
- **Gateway Core**: `markInitDone` 自死锁 — 持 `c.mu` 调 `Close()`（sync.Mutex 不可重入），改为锁外调 Close。
- **Gateway Core**: workspace work_dir Update 硬化（PR #785 review P1/P2）。
- **WebChat UI**: workspace 切换 session mismatch + 新 workspace 预创建 anchor session。
- **WebChat UI**: AI Config 保存后切 tab 内容丢失。
- **WebChat UI**: settings header 跨 tab 抖动。
- **WebChat UI**: workspace handshake 韧性 + drop session workdir。

## [1.29.1] - 2026-06-16

### Summary

v1.29.1 是一次 patch 版本更新，聚焦于 **API Key 安全约束加固**。新增 user_id 与 API Key 的 1:1 唯一映射约束（数据库层 UNIQUE INDEX + 应用层预检 + 409 Conflict），防止同一用户绑定多个 API Key 带来的权限边界模糊风险。同时修正 API Key 查询的错误码映射（此前将数据库瞬时故障误报为 404 Not Found，掩盖真实错误）。附带 config 包职责拆分（1659 行 god file → 5 文件）与 EventStore/gate/checkers 的内部去重重构（零行为变更，对最终用户无影响）。

### Security

- **Security**: Enforce 1:1 user_id ↔ API Key mapping — migration 016 adds a `UNIQUE INDEX` on `api_key_users(user_id)` for both SQLite and PostgreSQL, backed by an application-layer `requireUniqueUserID` pre-check that returns 409 Conflict on duplicate attempts. The DB constraint serves as defense-in-depth. Includes standalone dedup scripts (SQLite + PG) with fail-closed guards for resolving pre-existing duplicates before migration. (#741)

### Fixed

- **Security**: API Key lookup error-code accuracy — `HandleAPIKeyUserGet/Update/Delete` previously returned a blanket 404 on any `get()` failure, masking transient DB/connection errors as not-found. Now maps `sql.ErrNoRows` → 404 while other DB errors → 500; `update()`/`delete()` not-found (concurrent-delete window) also aligned to 404 via `%w sql.ErrNoRows` wrapping. Swagger updated with matching 500 responses. (#741)

## [1.29.0] - 2026-06-13

### Summary

v1.29.0 是一次 minor 版本更新，聚焦于 **发布产物归档化** 和 **会话存储可靠性**。GitHub Release 资产从裸二进制迁移至 tar.gz/zip 归档（压缩率 ~31%，71MB→22MB），自更新和安装脚本均已完成适配并保持向后兼容。EventStore 合并 resolveGeneration 为 CTE 单次查询（2→1 DB round-trip），超长 turn 截断改为换行边界保留而非整条丢弃。ACP Worker 新增 sessionId 空值防御，防止历史遗留 session 数据导致 "session not found" 错误。

### Added

- **CLI**: Release archive format — GitHub Release assets now publish as `hotplex-{os}-{arch}.tar.gz` (unix) / `.zip` (windows) with ~31% compression ratio. Updater and install scripts download, verify checksum, then extract. Legacy raw binary fallback for pre-archive releases. (#729)

### Changed

- **Gateway Core**: EventStore CTE merge — combine `resolveGeneration` + `SELECT` into single CTE for `QueryTurns` and `QueryTurnStats`, reducing DB round-trips from 2 to 1 (both SQLite and PostgreSQL). (#716, #727)
- **Gateway Core**: Oversized turn truncation now cuts at newline boundary instead of discarding entire turn, preserving partial content for the newest turn. (#715, #727)

### Fixed

- **Worker**: ACP sessionId empty guard — reject empty sessionId from agent in `callSessionMethod` and `Prompt`, preventing "session not found" errors when resuming sessions with missing worker_session_id in DB. (#732)

## [1.28.0] - 2026-06-12

### Summary

v1.28.0 是一次 minor 版本更新，聚焦于 **会话历史压缩** 和 **跨平台消息质量**。Gateway 新增基于 Brain LLM 的异步历史压缩（替代硬截断），Slack 和飞书共享统一的段落分割器提升移动端阅读体验，Codex Worker 支持 agent-configs 系统提示词注入。同时修复 CodexCLI idle drain 死锁导致 manager 完全不可用的关键缺陷。

### Added

- **Gateway Core**: Async history compression with Brain LLM — TruncateHistory fast path (sub-ms) injects truncated history immediately, async Brain compression produces cached summary for next resume. (#714)
- **Messaging**: Shared `ParagraphBreaker` for Slack + Feishu — unified threshold (150 chars) and sentence-end detection, extracted into `textutil` package. Slack gains paragraph breaks in streaming deltas. (#719)
- **Worker**: Codex agent-configs injection — `SystemPrompt` (merged B/C channel prompt) now forwarded to codex app-server via `thread/start` `baseInstructions` parameter. Previously agent-configs had no effect on Codex Worker. (#722)

### Changed

- **Messaging**: Paragraph break threshold reduced from 200 → 150 chars based on mobile readability research (~8–10 lines produces better reading rhythm).

### Fixed

- **Worker**: CodexCLI idle drain deadlock — timer callback held `CodexAppServerManager.mu` while calling `proc.Kill()`, which blocked acquiring `proc.mu` already held by `monitorProcess`. Fix: snapshot pgid under lock, unlock, then SIGKILL directly via `proc.ForceKill(pgid)` without touching proc mutex. (#725)
- **Gateway Core**: Async history compression edge cases — empty slice replaces nil to prevent meaningless async compression, extracted `resolveCachedHistory` with full test coverage. (#720)

## [1.27.0] - 2026-06-11

### Summary

v1.27.0 是一次 minor 版本更新，聚焦于 **Worker 会话恢复** 和 **消息质量**。CodexCLI 新增基于 turns 表的会话历史恢复 + Brain LLM 智能压缩（替代硬截断），飞书适配器引入智能段落分割（累计字符阈值 + 句末触发）。同时修复 ACP WorkerSessionID 持久化竞态条件、CodexCLI 崩溃恢复、ACP 通知丢包等多项可靠性问题。

### Added

- **Worker**: CodexCLI conversation history recovery — query persistent turns table on session resume, inject structured history prefix into new thread via crypto/rand boundary ID. (#704)
- **Worker**: Smart history compression via Brain LLM — new `HistoryCompressor` module replaces hard truncation with intelligent summarization when history exceeds budget (60k chars), with graceful truncation fallback when Brain is unavailable. (#704)
- **Gateway Core**: `pull_request` webhook trigger for fork PR support — dedup'd dual-path design (check_suite preferred, pull_request fallback) with 5min cooldown. (#704)
- **Messaging**: Feishu smart paragraph break — cumulative char threshold (200 chars) + sentence-end trigger replaces naive single-newline append, producing proper double-newline paragraph separation. (#707)
- **Observability**: `hotplex.session.transition.guard_repersist_overwrites` and `hotplex.session.transition.concurrent_overwrite` Prometheus counters for WorkerSessionID persist monitoring. (#710)

### Changed

- **Worker**: CodexCLI YOLO mode — default sandbox changed to `danger-full-access` for unrestricted network and filesystem access with `never` approval. (#704)
- **Worker**: CodexCLI `shutdown()` no longer calls `KillIfIdle` — singleton process lifecycle managed by idle drain or explicit `ShutdownSingleton`, aligned with OCS pattern. (#704)
- **Configuration**: ACP `auto_approve` default changed from `false` to `true` — sandboxed agents don't need manual approval.

### Fixed

- **Worker**: ACP WorkerSessionID persistence race condition — three-layer defense: targeted SQL UPDATE in `createAndLaunchWorker`, `transitionState` guard preserving concurrent updates, `forwardEvents` safety-net with forced persist. (#710)
- **Worker**: ACP readLoop burst drain increased from 16→128 to prevent notification drops under high throughput (1402 observed in 2min). (#702)
- **Worker**: ACP fatal JSONRPC errors now classified as `ErrKindUnavailable` to correctly trigger Bridge crash recovery; business errors (rate limit) continue returning nil. (#702)
- **Worker**: CodexCLI crash recovery extended to fresh sessions (previously only resumed), using `!doneReceived` instead of `exitCode` as trigger for robust crash detection. (#702)
- **Worker**: ACP rawInput `path` key normalized to `file_path` for toolfmt — file operations now show descriptive status instead of generic placeholder.
- **Gateway Core**: ForwardEvents turn count restore uses `bgCtx()` (shutdown-scoped) instead of request `ctx`, preventing DB query failures on user disconnect. (#702)
- **Messaging**: Feishu `id_convert` retries 3x with exponential backoff (100ms→200ms→400ms) for transient API failures, with permanent error skip (auth/404). (#702)

## [1.26.3] - 2026-06-09

### Summary

v1.26.3 是一次 patch 版本更新，聚焦于 **CodexCLI 和 OCS Worker 稳定性**。修复 CodexCLI `Wait()` 永久阻塞导致 goroutine 泄漏、elicitation 请求被静默丢弃、terminated session 无效 resume 三个缺陷；修复 OCS Worker SSE 读取器静默死亡导致会话挂起、关键事件被丢弃等六项并发安全缺陷。同时新增 `CanResumeTerminated()` 能力接口，统一了 Worker 类型级别的 terminated session 恢复策略。

### Changed

- **Worker**: Add `CanResumeTerminated()` capability to Worker interface — CodexCLI returns `false` (singleton killed on release), all others return `true`. Bridge uses this instead of hard-coded type switch to skip resume for terminated sessions. (#692)
- **Worker**: Register-time capability cache (`capCache`) in registry — `CanResumeTerminated()` queries cached value instead of creating temporary worker instances.
- **Infrastructure**: DRY extractions across messaging STT/TTS PID tracking, worker base lock pattern, session store Upsert JSON helpers, and admin API key store CRUD. (#695)

### Fixed

- **Worker**: CodexCLI `Wait()` permanent block — `release()` niled `doneCh`, causing `Wait()` to receive from nil channel forever. Fix: atomic capture under mutex, `release()` closes but does not nil. (#691)
- **Worker**: CodexCLI `mcpServer/elicitation/request` silently dropped — mapper had no case for this notification type, causing Codex agent permission prompts to hang indefinitely. (#698)
- **Worker**: CodexCLI terminated session dead resume path — resume always failed (singleton killed), producing WARN spam and double config load before falling back to fresh start. (#699)
- **Worker**: OCS SSE reader silent death — fatal errors now close all subscriber channels via `closeAllSubscribers()`, unblocking `forwardEvents` goroutines. (#697)
- **Worker**: OCS critical events silently dropped — two-hop pipeline (singleton → worker) now classifies events as droppable/critical; critical events use blocking send with 5s timeout. (#697)
- **Worker**: OCS `crashSub` false positive — `Wait()` checks `IsRunning()` when crashSub fires; returns 0 if singleton recovered. (#697)
- **Worker**: OCS channel panic race — `forwardBusEvents` checks `conn.closed` under mutex before writing to recvCh. (#697)
- **Worker**: OCS duplicate Done events — `handleSessionIdle` returns nil when stats already cleared by prior error handler. (#697)
- **Worker**: OCS `sync.Once` reentrant deadlock — `Wait()` called `release()` via `releaseOnce.Do`, but `release()` itself called `releaseOnce.Do`. Fixed by calling `release()` directly (idempotent). (#697)

## [1.26.2] - 2026-06-08

### Summary

v1.26.2 是一次 patch 版本更新，聚焦于 **Cron 投递可靠性** 和 **OpenCode Server 生命周期加固**。新增 Cron 投递重试机制（指数退避，最多 3 次），OCS Worker 实现 SystemPromptUpdater 接口支持动态刷新系统提示词。同时包含大量并发安全修复（OCS sessionID 数据竞争、platform_writer 通道防护、bridge 会话泄漏防护）和 Slack 适配器错误处理改进。

### Added

- **Cron**: Delivery retry with exponential backoff — in-memory retry queue (max 100 entries) for transient failures (429, timeout, 5xx), up to 3 retries with 30s→1m→2m backoff, new `hotplex.cron.delivery.result` metric. (#577)
- **Worker**: OCS `SystemPromptUpdater` interface — `UpdateSystemPrompt` method updates conn.systemPrompt under mutex, enables bridge to push refreshed system prompt after `/reset` without session recreation. (#664)
- **Observability**: `hotplex.cron.delivery.result` metric with `{status,platform}` labels — records all delivery outcomes (success, exhausted, permanent, transient).

### Fixed

- **Worker**: OCS sessionID data race — 6 read sites accessed `conn.sessionID` without mutex; unified via `getSessionID()` helper, symmetric with write-side lock discipline. (#664)
- **Gateway Core**: Bridge lifecycle hardening — delete orphaned session on transition-to-running failure, rollback resume attach-failure to TERMINATED, 5s timeout on rollback context. (#577)
- **Gateway Core**: Platform writer send-on-closed-channel — `recover()` guard eliminates TOCTOU window, `atomic.Bool` closed flag prevents writes after disconnect. (#577)
- **Messaging**: Slack adapter clears Thinking status on error and shows generic error feedback; fallback message for empty error events. (#577)
- **Worker**: OCS singleton goroutine leak — close stdout on `discoverPort` timeout; server-side session DELETE in `release()` for resource cleanup; non-blocking Wait() crash check eliminates 2s goroutine leak. (#664)

## [1.26.1] - 2026-06-08

### Summary

v1.26.1 是一次 patch 版本更新，修复 ACP Worker 启动后首轮 prompt 返回空文本的关键 bug，以及 `make dev-reset` 的日志清理竞态问题。

### Fixed

- **Worker**: ACP readLoop goroutine exited prematurely after initial handshake notifications — burst drain loop used `return` (exits entire function) instead of labeled `break`, killing the notification consumer before any prompt text arrived.
- **Infrastructure**: `make dev-reset` log cleanup raced with running gateway — restructured as three-phase stop→clean→start sequence with confirmed process termination before file deletion.

## [1.26.0] - 2026-06-07

### Summary

v1.26.0 是一次 minor 版本更新，聚焦于 **多 Bot 配置体验** 和 **Worker 架构现代化**。核心变更将 Agent 配置路径从平台运行时 ID（`ou_xxx`/`U04ABC`）迁移为 YAML 配置名（`my-bot`），使路径可读且不受 Bot 重命名影响。Gateway 层会话状态编排下沉至 Worker 层，消除 3 个 gateway 接口和 7 处类型断言。安全方面新增可配置 CORS origins 和 RFC 9116 security.txt 端点。ACP Worker 新增通知排空机制解决输出丢失问题。

### Added

- **Configuration**: Agent config path resolution uses YAML `bots[].name` instead of platform runtime IDs — readable paths (`feishu/my-bot/SOUL.md`), stable across Bot renames, with `ValidateBotName` path-traversal guard. (#678, #679)
- **Configuration**: `GATEWAY_BOT_NAME` environment variable injected into Worker processes; Cron `--bot-name` flag for multi-Bot agent-config isolation.
- **Worker**: `SystemPromptUpdater` interface — workers reload agent config on `/reset` without session recreation. (#659)
- **Worker**: `ForceKillTree` — kill orphaned child processes that escape PGID (e.g. MCP servers spawned by Codex). Platform-native `/proc` (Linux) and `sysctl` (macOS) child discovery. (#659)
- **Security**: Configurable CORS origins (`security.allowed_origins`), RFC 9116 `security.txt` endpoint (`security.contact`), tightened docs CSP `connect-src` to `'self'`. (#663)
- **Worker**: `SessionStartParams` struct replacing 11–13 positional parameters across `StartSession`/`StartPlatformSession` interfaces. (#679)

### Changed

- **Gateway Core**: Session state orchestration pushed from Bridge into Worker layer — workers return `ResetResult{ConnReplaced}`, emit `internal_reset` events. `ResetGenerationer` atomic guard prevents stale goroutine interference. (#659)
- **Worker**: CodexCLI exec mode removed — app-server singleton is the sole implementation. `use_app_server` config deprecated and auto-normalized. (#659)
- **Infrastructure**: Temp file management unified — `os.TempDir()` with `hotplex/worker/` subdirectory, gateway startup cleanup for orphaned files (>2h worker, >24h media). (#675)
- **CLI**: Setup skill rewritten for v1.25.0+ production alignment — 4-phase decision tree, 48% shorter. (#677)

### Fixed

- **Session**: GC deadlock — `worker.Terminate()` moved outside session mutex, preventing blocking all concurrent reads during graceful shutdown. (#656)
- **Worker**: ACP notification drain — channel handshake ensures `MessageDelta` reaches bridge before `Done`, fixing empty text output for all ACP turns. (#685)
- **Gateway Core**: TERMINATED session now attempts resume with fallback to fresh start — idle_timeout/gc no longer lose conversation context. (#683)
- **Events**: `ToInt64`/`ToFloat64` handle `int32` type — atomic counter values from `sessionAccumulator` no longer cause turn summary loss and feishu card instability. (#688)
- **Cron**: Timeout errors now logged with `error_type` + state confirmation in executor. (#667)
- **CLI**: `doctor config.required` checker now recognizes multi-bot YAML configurations. (#671)
- **Configuration**: Config watcher `~` expansion — `fsnotify` failed with "no such file" when home directory was specified with tilde. (#673, #674)
- **Docs**: API Console localized Scalar JS — CDN unreachable from Chinese servers caused infinite spinner. Layout redesigned to fit header + Scalar in one viewport.

## [1.25.0] - 2026-06-04

### Summary

v1.25.0 是一次 minor 版本更新，聚焦于 **可观测性现代化** 和 **API 文档基础设施**。新增 OTel 原生可观测性包（统一替换 split metrics/tracing，~55 个指标，AEP trace_id 注入），以及 swaggo 注解 + Scalar 控制台的 API 文档混合生成。Brain 模块完成大规模瘦身（删除 ~5700 行死代码），文档中心获得显著性能优化（消除 3.2MB 浪费 + gzip + 缓存）。

### Added

- **Infrastructure**: OTel-native observability package — unified `internal/observability/` replacing split `internal/metrics/` + `internal/tracing/`, ~55 metrics via OTel Meter API, trace_id injection in AEP metadata, W3C TraceContext + Baggage propagation, zero direct prometheus/client_golang in application code. (#642)
- **Docs**: API documentation with swaggo annotations + Scalar console — auto-generated `swagger.json` from Go source, interactive API explorer at `/docs/api`. (#632)
- **Infrastructure**: OTel Collector service in docker-compose (monitoring profile), aligned alert rules and SLO definitions with actual metric names. (#642)

### Changed

- **Brain**: Remove dead subsystems (IntentRouter, SafetyGuard, ContextCompressor) — ~5,700 lines deleted, collapse 4 Brain interfaces to single `Brain: Chat + ChatWithOptions`. (#638)
- **Brain**: Integrate CircuitBreaker into Chat/ChatWithOptions via `Execute()`, extract `RedactSensitive()` to `security/sanitize.go`. (#638)
- **Docs**: Performance optimization — conditional mermaid.js loading (1/56 pages), gzip compression (~70% size reduction), cache headers, Google Fonts localization at build time. (#644)
- **WebChat UI**: Redesign worker icons (Claude Code sparkle, Codex terminal, ACP lightning) with unified `WorkerIcon` component. (#637)

### Fixed

- **Cron**: Webhook trigger ignores TARGET_PR — inject explicit prefix into worker prompt directing review to specified PR. (#647)
- **Gateway Core**: Session resume fails after GC (TERMINATED→RUNNING) — move state transition before `AttachWorker` so session is active during check. (#642)
- **Gateway Core**: Webhook test race condition — `triggered.Add(1)` moved inside mutex critical section. (#642)
- **WebChat UI**: Connection lifecycle fixes — zombie WebSocket after disconnect, SESSION_TERMINATED graceful handling, assistant message cross-turn dedup. (#642)
- **Brain**: Latency metrics timer started after LLM call instead of before — always recorded as ~0ms. (#638)
- **SDK**: TypeScript client wrong event type in init envelope (control→init), Java client wrong message priority, Go client missing `Title()` option.

## [1.24.4] - 2026-06-03

### Summary

v1.24.4 是一次 patch 版本更新，聚焦于 **session identity 迁移的安全加固**。PR #635 补全了 WS init 路径缺失的输入清洗和长度校验，隔离了 session 恢复/启动的 context 超时预算，并统一了前端 session ID 生成逻辑。同步更新了 specs 索引和 WebSocket 集成文档。

### Changed

- **Gateway Core**: WS init path now sanitizes `title` and `sessionID` via `messaging.SanitizeText()` — REST API path already had this; the WS path was unguarded. (#635)
- **Gateway Core**: Resume→Start session fallback uses independent 30s timeout contexts instead of sharing a single budget — a slow resume no longer starves the heavier StartSession. (#635)
- **Gateway Core**: Replace hardcoded `256` with `session.MaxClientKeyLen` constant in API handler for consistency with session manager's validation. (#635)
- **WebChat UI**: Replace inline `crypto.randomUUID()` with existing `newSessionId()` utility — provides cross-browser fallback instead of silently failing. (#635)
- **WebChat UI**: Consolidate `MAIN_SESSION_CLIENT_ID`/`MAIN_SESSION_TITLE` into single `ANCHOR_SESSION_ID` constant. (#635)

### Fixed

- **Session**: Title length validation missing in WS init path — `client_session_id` had a 256-char guard but `title` did not, allowing unbounded input. (#635)

## [1.24.3] - 2026-06-03

### Summary

v1.24.3 是一次 patch 版本更新，修复了 WebSocket 客户端在消息中省略 `session_id` 或 `seq` 字段时触发验证错误的问题。AEP 解码器新增 `ValidateMinimal` 路径，允许这些字段为空——Gateway 会在解码后注入权威值。

### Fixed

- **Gateway Core**: AEP codec rejects client messages with empty `session_id`/`seq` — add `DecodeLineMinimal` path that skips these fields since the Gateway stamps authoritative values after decoding.

## [1.24.2] - 2026-06-03

### Summary

v1.24.2 是一次 patch 版本更新，聚焦于 **会话追踪能力增强** 和 **WebChat 代码质量提升**。新增 `client_key` 持久化到 sessions 表，将 WebSocket init 的 clientSessionID 存入数据库便于调试和会话关联。WebChat 完成组件拆分重构（thread.tsx → 7 个独立组件），修复了 CopyButton 定时器泄漏和会话 worker 名称显示错误。文档门户新增 mermaid.js 离线渲染和 SSO 集成最佳实践。

### Added

- **Session**: Persist `client_key` in sessions table — stores the WebSocket init envelope's `session_id` for debugging and session tracing, with 256-char length validation. (#631)
- **Docs**: Bundle mermaid.js for offline rendering in self-hosted documentation portal — no CDN dependency. (#631)

### Changed

- **WebChat UI**: Extract thread.tsx (597 lines) into 7 focused components — AssistantMessage, UserMessage, WelcomeScreen, PreAssistantIndicator, ReasoningBlock, CopyButton, MessageActions + shared helpers. (#629)
- **Docs**: Restructure WebSocket integration guide (17→15 sections), add SSO integration best practices and bidirectional client_key/clientSessionID cross-references. (#631)

### Fixed

- **WebChat UI**: CopyButton `setTimeout` timer not cleared on unmount — potential setState-after-unmount when navigating away within the 2s cooldown window. (#629)
- **WebChat UI**: Chat header showed global config worker type instead of per-session `worker_type` — now displays correct worker name (Claude/Codex/OpenCode/ACP) per session. (#629)

## [1.24.1] - 2026-06-03

### Summary

v1.24.1 是一次 patch 版本更新，聚焦于 **WebChat Skills 功能修复**。修复了 `$skills` 命令冻结 UI 和返回不完整列表两个核心问题（#624），新增 skills 本地缓存与斜杠命令菜单集成，并通过多轮 PR #626 review 修复了 worker 命令 done 事件、会话切换和错误处理的多个缺陷。

### Added

- **WebChat UI**: Skills caching with 24h TTL — fetched skills are persisted to localStorage (keyed by session ID) and merged into the slash command menu (`/` palette) alongside built-in commands.

### Changed

- **Infrastructure**: Git hooks use `core.hooksPath` instead of symlinks for reliable cross-worktree setup; pre-push validation parallelized for ~50% speed improvement.

### Fixed

- **Gateway Core**: `$skills` returns incomplete list when `homeDir` is empty — global skill directories (`~/.claude/skills`, `~/.agents/skills`, `~/.hotplex/skills`) were skipped or resolved as relative paths. (#624)
- **Gateway Core**: Worker commands (`$skills`, `$context`, `$mcp`, `$model`, `$perm`) never emit a `done` event, leaving frontend stuck in running state. Synthetic done event now scoped to `StdioSkills` only. (#624)
- **WebChat UI**: `SkillsList` event undeclared in frontend `EventKind` constants, causing gateway response to be silently dropped. (#624)
- **WebChat UI**: Slash command menu hard 8-item cap cut off all skills — container scrolling now handles arbitrary list lengths; keyboard navigation auto-scrolls selected item into view.

## [1.24.0] - 2026-06-02

### Summary

v1.24.0 是一次 minor 版本更新，聚焦于 **飞书交互式卡片** 和 **WebChat 会话状态实时性**。飞书适配器新增权限确认、问答确认和 Elicitation 三种交互式卡片按钮，用户无需输入斜杠命令即可在卡片上直接操作。WebChat 修复了 session 状态徽章映射错误和侧边栏无实时更新的问题，状态变更现通过 WebSocket 实时推送到前端并以中文标签展示。

### Added

- **Messaging/Feishu**: Interactive card buttons for permission approval, question confirmation, and elicitation — users can approve/reject/answer directly on Feishu cards without slash commands. (#620)
- **WebChat UI**: Real-time session state propagation — WebSocket `state` events now flow through the adapter to the session hook, updating sidebar status live without page refresh.

### Changed

- **Messaging/Feishu**: Unify question card to v2 `buildCard` format, matching permission/elicitation card structure. Remove deprecated `buildInteractionCard` and related dead code. (#620)

### Fixed

- **WebChat UI**: Session status badge mapped non-existent `active`/`working` states — backend returns `created`/`running`/`idle`/`terminated`/`deleted`, causing gray fallback for running sessions and zero active count.
- **WebChat UI**: Replace raw English state strings with Chinese labels in sidebar via `stateLabel()` (running → 进行中, idle → 空闲, etc.).
- **Messaging/Feishu**: Return "未知操作" feedback card for unknown card action types instead of nil. (#620)

## [1.23.2] - 2026-06-02

### Summary

v1.23.2 是一次 patch 版本更新，聚焦于 **Gateway 生命周期可靠性** 和 **安全正确性**。修复了 shutdown 竞态导致 Worker 启动后即被杀死、session/bridge 多个状态迁移缺陷、安全模块 race condition、AEP 信封 mutation、TTS 侧冷启动 contention 等问题。同时新增 ACP Worker 可插拔 stderr handler 和跨 SDK ACP 事件类型覆盖。

### Added

- **Worker/ACP**: Pluggable stderr handler — `proc.StderrHandler` interface replaces hardcoded Info logging; ACP worker gets a stateful multi-line folder that collapses system prompt echoes, XML blocks, and Python tracebacks into concise summaries. (#618)
- **Worker/ACP**: Auto-approve by default for sandboxed ACP agents — `*bool` config distinguishes absent from explicit false. (#618)
- **SDK**: Full ACP extension event coverage across Go/Java/Python/TypeScript — context_usage, skills_list, mcp_status, worker_command, tool_update, plan, mode_update, question/elicitation request/response. (#618)
- **Worker/ClaudeCode**: Emit assistant thinking content blocks as EventStream events (Claude CLI v2.1.158+ compat) with dedup guard for old CLI versions. (#618)

### Changed

- **Messaging/Feishu**: Release lock before triggering tool flush to avoid deadlock; case-insensitive tool name lookup for ACP agents. (#618)
- **Messaging/Feishu**: Truncate sdk_logger messages >400 chars to reduce dev log noise. (#618)
- **Worker/ClaudeCode**: Remove redundant `--settings` JSON from buildCLIArgs that broke CCR (claude-code-router) — env injection via cmd.Env is sufficient. (#619)

### Fixed

- **Gateway Core**: Shutdown race — split `Shutdown()` into `MarkClosed()` + `WaitForwarders()` so in-flight handlers are rejected before worker termination, preventing start-then-kill. (#613)
- **Gateway Core**: `doneReceived` reset causes false "worker exited without done" errors on single-turn platform sessions — replace immediate reset with new-turn detection. (#613)
- **Gateway Core**: Eventstore collector close order — close before SessionMgr to prevent writes to closed DB. (#613)
- **Gateway Core**: Collector.Close() double-close panic — add sync.Once protection. (#617)
- **Gateway Core**: everHadConn map unbounded growth — delete on removeSession. (#617)
- **Security**: API key allowlist/blocklist atomicity gap — replace double-lock cycles with single mutex-protected access; add RLock to ValidateBaseDir. (#619)
- **Security**: Replace real IP with LAN example in CSP comments/tests. (#619)
- **AEP**: Envelope mutation — extract `marshalEnvelope` with shallow copy to prevent Encode/EncodeJSON from mutating caller's envelope. (#619)
- **TTS**: MossProcess single-flight start — concurrent callers wait on readyCh instead of contending mutex during ~60s sidecar warmup; detach warmup context from request lifecycle. (#619)

## [1.23.1] - 2026-06-01

### Summary

v1.23.1 是一次 patch 版本更新，修复 v1.23.0 引入的两个回归问题。Claude Code Worker 的临时文件互斥锁导致自死锁，使 bot 完全无响应；ACP Worker 的 turn summary 缺少 token 和成本数据。两者均为 v1.23.0 引入的回归，影响已部署实例的日常使用。

### Fixed

- **Worker/Claudecode**: Resolve self-deadlock in `writeTempFile`/`cleanupTempFiles` — PR #606 added `w.Mu.Lock()` to both functions, but they are called from `startLocked()` which already holds `w.Mu`, causing an instant deadlock that blocks all subsequent messages. Introduce dedicated `tempFilesMu` to preserve the data-race fix without deadlocking the Start path. (#612)
- **Worker/ACP**: Align `buildStats` output with gateway `sessionAccumulator` format — token counts and cost data were missing because `buildStats()` emitted flat keys incompatible with `mergePerTurnStats()`. Wrap token data in nested `usage` map and store cost as `float64` for JSON round-trip safety. (#610)

## [1.23.0] - 2026-06-01

### Summary

v1.23.0 是一次 minor 版本更新，聚焦于 **CodexCLI 100% 协议覆盖**、**ACP Worker 三阶段补齐**、**Agent 配置注入控制**、**Gateway 核心可靠性修复** 和 **远程部署可用性**。CodexCLI 适配器实现与上游 Codex CLI 的完全协议对等（5 阶段 16 任务，+666 行）。ACP Worker 完成 Phase 1–3 全部功能。新增 inject_exclude 机制支持按文件排除 Agent 配置注入。Gateway Core 修复 session 生命周期竞态、platform conn 路由丢失、autoRetry 阻塞等关键问题，Worker 可靠性覆盖 claudecode/CodexCLI/ACP。Webchat 与 Docs 门户的 CSP 改为可配置且默认宽松，让远程部署零配置即可用并提供启动 warn 提示。

### Added

- **Worker/CodexCLI**: Full protocol upgrade — 100% coverage with upstream Codex CLI. WorkerCommander interface (Compact/Clear/Rewind), Thread lifecycle (Resume/Fork/Archive/Rollback), Turn control (Steer/Interrupt), 5 MCP methods, Review support, 12 CLI flags, 11 Thread management methods. (#592, #604)
- **Worker/CodexCLI**: Resume on existing manager process — creates new thread without restarting, preserving conversation context.
- **Worker/CodexCLI**: Exec flags wired through config pipeline — 9 exec-mode flags (color, output_file, strict_config, etc.) configurable per-bot.
- **Worker/ACP**: Phase 1 core features — complete ACP protocol support for permission bridging, priority-aware backpressure, and bidirectional ACP↔AEP mapping. (#590)
- **Worker/ACP**: Phase 2 interaction completeness — reset optimization, discovery, and error UX improvements. (#591)
- **Worker/ACP**: Phase 3 advanced features — protocol version negotiation, multi-agent coordination, debug trace, ForkSession, and JSON Schema validation. (#595)
- **Worker**: Align OCS/Codex/ACP feishu UI/UX with Claude Code worker — standardized tool names, ToolUpdate event handling, Name fallback for non-ClaudeCode agents.
- **Configuration**: Per-file injection control (inject_exclude) — configurable blacklist to skip specific agent config files, with 3-level fallback (bot → platform → global). (#594, #603)
- **Configuration**: Configurable Content-Security-Policy via `security.csp` / `HOTPLEX_SECURITY_CSP` — replaces hardcoded `localhost`-pinned policy that blocked remote deployments. Defaults to a scheme-wide-open connect-src (`http:` / `https:` / `ws:` / `wss:`) so zero-config remote setups work end-to-end, with a startup WARN when the default or a wide-open override is in effect. (#608)
- **Gateway**: GitHub webhook receiver for event-driven PR review with comprehensive access control. (#581)
- **Session/Pool**: Atomic `AcquireWithMemory` — single-lock reservation of concurrency + memory quota, replacing the two-step Acquire+AcquireMemory pattern that had rollback complexity and a TOCTOU window. (#606)

### Changed

- **Worker/CodexCLI**: Extract CodexExecFlags into dedicated struct — other worker adapters no longer see CodexCLI-specific flags in shared SessionInfo type.
- **Gateway Core**: routeMessage uses context.WithoutCancel(h.ctx) for platform conn pre-encoded path, preserving tracing propagation during shutdown drain.
- **Session**: Concurrent session GC — errgroup with concurrency limit 5 for `max_lifetime` + `idle_timeout` transitions, with O(1) reason lookup via map (max_lifetime precedence). (#606)
- **Session**: N+1 lock pattern eliminated in `ListActive` / `WorkerHealthStatuses` — readers no longer acquire per-session `ms.mu.RLock` for `ms.info` and `ms.worker` value-type fields assigned under `m.mu`+`ms.mu` on the write path. (#606)

### Fixed

- **Gateway Core**: P0 markdown table parser panic on bare "|" input — add slice bounds guard in feishu and slack table formatters.
- **Gateway Core**: forwardEvents blocking risk + StartSession orphan worker + Delete/Attach race conditions. (#599, #600, #602)
- **Gateway Core**: Platform conn routing lost OwnerID during pre-encoded JSON path, causing interactive authorization failure ("OwnerID not set") in Feishu/Slack channels.
- **Gateway Core**: dev-start clears logs before gateway starts — previous ordering deleted logs after gateway began writing, leaving running process with fd to removed inode.
- **Gateway Core**: TransitionWithInput escape hatch now calls `forceTerminateInMemory()` to release pool quota, update metrics, nil worker, and notify state change (was leaking pool slots and drifting metrics). (#598, #601)
- **Gateway Core**: Error events in claudecode/codexcli/opencodeserver mappers now guard against empty message and missing Code field; admin bot API returns unique bot_id via platform:name fallback to prevent React key collision. (#596, #597, #601)
- **Gateway Core**: PoolAcquireTotal success metric moved from pool.Acquire() to AttachWorker post-validation to eliminate double-count on memory error path. (#522, #601)
- **Cron**: SessionKey now honors `Payload.WorkerType` instead of always using `claudecode`. Prior cron sessions for non-claudecode workers (codexcli/acp) become orphaned under the old key — their session records remain in storage but are no longer reachable via SessionKey(). This is a correctness fix; no data is deleted. If you rely on history for non-claudecode cron jobs, either re-run them or migrate by updating the SessionKey column manually in your store.
- **Worker/Claudecode**: `SendControlRequest` lock-merged to eliminate TOCTOU gap between pending request registration and stdin write; `cleanupTempFiles` snapshots under lock to prevent concurrent `writeTempFile` mutation; unmarshal errors for PermissionRequest/QuestionRequest/ElicitationRequest now log instead of silently dropping. (#541, #606)
- **Worker/CodexCLI**: Wait() deadlock from unclosed doneCh, resume ref leak, lock held during IO, map mutation across goroutines, error swallowing — comprehensive fixes across 4 review rounds. (#593, #604)
- **Worker/CodexCLI**: Missing mapItemCompleted cases for WebSearch/CollabToolCall — Feishu tool_activity strip showed them as perpetually in-progress.
- **Worker/CodexCLI**: Multi-file edit visibility in Feishu cards — join all file paths with comma instead of silently discarding beyond the first.
- **Worker/CodexCLI**: nil-map / typed-nil panic guards in ACP+CodexCLI paths, `CodexItem` cleanup of dead fields, `AppServerWorker` state machine replaces legacy `started` boolean. (#604)
- **Worker/ACP**: Zombie cleanup timeout + orphan DELETED session invalid resume — cancel timeout adjusted to 2s for reliability on high-load systems.
- **Messaging/Feishu**: CJK MessageDelta terminator check — previous `0x82` short-circuit was incorrect (`？` U+FF1F / `！` U+FF01 have trailing bytes `0x9F` / `0x81`, only `。` U+3002 is `0x82`), causing line breaks to be dropped after question/exclamation marks. Now always decodes the last rune and compares against the fullwidth terminator set. (#608)
- **Infrastructure**: E2E listener registration race eliminated in all tests.

## [1.22.0] - 2026-05-30

### Summary

v1.22.0 是一次 minor 版本更新，聚焦于 **ACP Worker 全面实现**、**Cron 并发安全审计** 和 **CodexCLI 生命周期修复**。新增 ACP (Agent Client Protocol) Worker 适配器，使任何 ACP 兼容 Agent（如 Hermes）可作为一线 Worker 运行。Cron 模块经过端到端审计，修复 6 个 P1 竞态条件。CodexCLI 获得 reset 轻量化重构和 zombie 进程修复。Worker sandbox 支持 per-bot 粒度配置，Slack 适配器新增 Assistant branding 能力。

### Added

- **Worker/ACP**: ACP Worker adapter — full implementation of JSON-RPC 2.0 over stdio protocol with bidirectional ACP↔AEP mapping (11 notification types), permission bridging, priority-aware backpressure, and 12 rounds of PR review hardening. 24 tests with -race flag. (#569, #579)
- **Worker/Codex**: Per-bot sandbox field in Slack/Feishu bot configs for role-based access control.
- **Messaging/Slack**: Assistant branding — DisplayName/IconEmoji support with bot-level override for custom assistant status. DataTableBlock for skills list rendering. (#565)
- **Worker**: Add NO_PROXY to worker environment — bypass system proxy (e.g. Clash) for localhost calls, preventing 502 Bad Gateway on local CLIProxyAPI requests.

### Security

- **Worker/Codex**: Default sandbox changed from `workspace-write` to `danger-full-access`. Existing deployments upgrading to v1.22.0 will have Codex workers silently elevated to unrestricted access. Set `sandbox: "workspace-write"` per-bot or at platform level to restore the old behavior.

### Changed

- **Worker/CodexCLI**: ResetContext now uses lightweight thread swap on the same app-server process instead of full Terminate→Start cycle, eliminating 30s reset timeout. (#576)
- **Worker/CodexCLI**: Skip DEBUG log for high-frequency delta notifications — reduces codex log output from ~80% to <5% of total.
- **Cron/CLI**: `--platform-key` now merges over environment variable keys instead of replacing them. Previous behavior: CLI keys fully replaced env keys. New behavior: env keys serve as baseline, CLI keys override.
- **Cron/CLI**: `--attach` with recurring schedules (`every:`) no longer sets `DeleteAfterRun=true`. Only `at:` one-shot attach jobs auto-delete. Recurring attach jobs use `max_runs`/`expires_at` for lifecycle management.

### Fixed

- **Cron**: End-to-end audit — fix 6 P1 race conditions (slot leak, ABBA deadlock, narrow SetEnabled, cancel leak, CLI timestamps, maxJobs gate), 8 P2 design gaps (platform key merge, delivery error escalation, injection detection hardening, upsert atomicity, persist timeout), and 4 P3 edge cases (backoff off-by-one, sanitize boundary). (#574)
- **Cron**: Grace period cap reduced from 2h to `min(interval/2, 30min)` — gateway downtime exceeding 30min will skip missed cron executions. Previously daily jobs had up to 12h grace, weekly up to 84h. (#574)
- **Cron**: Resolve platform keys from env vars even with explicit --platform flag, preventing missing channel_id/chat_id in created jobs.
- **Cron**: Use background timeout in onTick persist calls — s.ctx gets cancelled during Shutdown causing silent persist failures.
- **CLI/Cron**: Improve missing-arg error messages with --help guidance instead of generic "accepts 1 arg(s), received 0".
- **Worker/CodexCLI**: Fix reset killing session + zombie process lingering 30min — ResetContext now properly rebuilds thread, Kill immediately force-kills idle singletons. (#575, #576)
- **Release**: Use awk index() for changelog extraction to prevent field-splitting errors on bracketed version strings.

## [1.21.0] - 2026-05-29

### Summary

v1.21.0 是一次 minor 版本更新，聚焦于 **ACP Worker 通用集成框架** 和 **CodexCLI 稳定性**。新增 ACP (Agent Client Protocol) Worker 集成规格（经过三源交叉审查验证），为 Hermes 等 ACP 兼容 Agent 提供通用对接路径。CodexCLI 适配器获得关键死锁修复和流式输出增强。Slack 适配器完成 DataTableBlock GA 迁移。

### Added

- **Worker/ACP**: ACP Worker integration spec — 11-section design document defining universal ACP agent connectivity via stdio JSON-RPC 2.0. Triple cross-reviewed against ACP SDK, HotPlex source, and Hermes source with 22 corrections applied. (#570)
- **Messaging/Slack**: Replace beta `TableBlock` with GA `DataTableBlock` (slack-go v0.24.0). All 6 table builders migrated, validator/sanitizer updated, `isInvalidBlocksError` helper unified across 8 call sites. (#566)

### Changed

- **Infrastructure**: Upgrade Go dependencies to latest stable versions. (#562)

### Fixed

- **Worker/CodexCLI**: Fix `readNotifications` mutex deadlock causing 30s handshake timeout — pass stdout directly instead of shared pipe. Fix `context.Background()` SIGKILL race where `defer startCancel()` killed process within 10ms of successful handshake. Expand notification coverage for reasoning, MCP tool calls, warnings, and command execution. Gate token usage by turnID to prevent cross-turn contamination. Add `approvalPolicy` to thread/start params. (#570)
- **Gateway Core**: Populate metadata in synthetic events — done/error events now include turn statistics and session context. (#563)
- **UX**: Dev mode lockout warning with upgrade guidance, WebChat 401 error handling with authentication redirect, font preload optimization. (#561)
- **Worker/CodexCLI**: Batch fix — 5 high-ROI fixes addressing handshake deadlock (#501), notification parsing (#527), thread lifecycle (#526), streaming output (#510), and context propagation (#544). (#564)

## [1.20.0] - 2026-05-29

### Summary

v1.20.0 是一次 minor 版本更新，聚焦于 **新消息平台集成** 和 **安全加固**。新增 Yuanxin (Pulsar) 消息适配器和 `hotplex install` CLI 命令，扩展平台覆盖和安装体验。安全方面修复了 XML 注入绕过、SSRF DNS 信息泄露和 SafetyGuard 数据竞争。Cron 模块经过全面并发安全重构，Admin API 不再泄露内部错误信息。测试稳定性大幅提升，消除了多个 CI flaky test。

### Added

- **Messaging**: Yuanxin messaging adapter via Apache Pulsar — consumer/producer lifecycle, MessageDelta 背压累积, cron 结果投递, multi-round code review (16 commits). (#494)
- **CLI**: `hotplex install` command — install binary to PATH with cross-platform shell profile detection, extracted shared install utilities from onboard wizard. (#555)
- **Gateway Core**: TurnCount persistence across gateway restarts — load latest turn_num from DB when in-memory counter is 0, prevent duplicate turn_nums. (#555)

### Changed

- **Cron**: Extract `handlePostExecution` template to eliminate ~80% lifecycle duplication between `executeJob` and `executeAttached`. (#543)
- **CLI**: Extract `GatewayState` to shared `pidutil` package, removing duplicates across `cmd/hotplex` and `internal/cli`. (#543)
- **Build**: webchat-embed and docs-build staleness detection via `find -newer` cache pattern, Makefile VERSION variable extraction, 3-stage build status display. (#549)
- **Tracing**: Rename misleading `SpanFromContext` to `GetTracer`, fix hardcoded version via `serviceVersion` parameter. (#552)
- **Test**: Parameterize time constants to reduce CI wait times, replace `time.Sleep`+assert with `require.Eventually` across 8 files. (#550)

### Fixed

- **Security**: XML sanitizer missed uppercase attribute tags (e.g. `<RULES x="1">`), allowing prompt injection bypass for all 10 reserved tags. (#517)
- **Security**: SSRF `ValidateURL` leaked internal DNS topology through error strings in API responses. (#537)
- **Security**: `SafetyGuard` data race on `banPatterns` and config fields in hot path — add RLock snapshot. (#531)
- **Security**: `AllowedModels`/`AllowedTools` data race — add RWMutex protection with `RegisterModel`/`RegisterTool` API. (#537)
- **Security**: Admin API error handlers leaked internal error strings to HTTP responses — add `respondStoreError` with sentinel-based 404 detection. (#552)
- **Cron**: Unified error classification, `persistCtx` race condition, panic recovery leaving jobs permanently stuck, `loadFromDB` lock ordering deadlock. (#514, #499, #534)
- **Messaging**: Feishu data race on turn metadata, lock-during-API blocking, chat queue panic leak. (#539)
- **Messaging**: `Classify()` treated all DB errors as "no record found", causing duplicate welcome messages on transient failures. (#528)
- **Worker**: codexcli stdout pipe FD leak on early return, stderr buffer `bufio.ErrTooLong` on large lines. (#547)
- **Gateway Core**: `stopGateway` failed with "no such process" on foreground-started instances — add `Terminate()` for direct PID signaling. (#549)
- **Gateway Core**: `dev` command missing duplicate-gateway guard, causing port conflicts. (#529)
- **Circuit Breaker**: `Reset()` caused state divergence with underlying breaker, `Execute` data race on `cb.breaker`. (#532)
- **Eventstore**: JSON unmarshal errors silently discarded, `flushBatch` cascading warnings on poisoned SQLite tx. (#536)
- **WebChat UI**: CronSchedule object rendered as React child, cron list grid overflow, JSON parse error on trigger, formatDuration triplication. (#555)

## [1.19.0] - 2026-05-27

### Summary

v1.19.0 是一次 minor 版本更新，聚焦于 **PostgreSQL 双数据库支持** 和 **安全修复**。新增完整的 PostgreSQL 后端（`dbutil.Dialect` 抽象层 + 5 个 PG Store + 9 个 PG 迁移文件），在保留 SQLite 默认后端的同时提供企业级数据库选项。安全方面修复了 Admin API key 认证绕过和 dev mode 重激活漏洞。Gateway Core 和 Messaging 层进行了 SOLID 原则批量重构，提升可维护性和可测试性。

### Added

- **Database**: PostgreSQL dual-database support via `dbutil.Dialect` abstraction — thin dialect layer (5 methods, 120-line Rebind state machine) isolates all SQL differences. 5 PG Store implementations (session, cron, eventstore, chat_access, api_key), 9 PG migration files, Docker Compose PG stack. (#490)
- **Database**: `db-stats` skill manual with go:embed integration — 4-step database detection, complete schema reference, 9 categories of analytics SQL templates.
- **Database**: CLI cron commands now support PostgreSQL backend via driver-aware `OpenStore`.
- **Docker**: Multi-DB Docker Compose setup with `docker-compose.pg.yml`, PostgreSQL init script, dual-mode entrypoint, and production PG config with volume persistence.

### Changed

- **Gateway Core**: SOLID principles batch refactor — extract `prepareWorkerInfo` (eliminate env injection trio), unify `SessionManager` interface hierarchy via embedding, eliminate Hub type assertions via `RouteWrite`, decompose `performInit` into four-phase dispatch, extract `forwardContext` from `forwardEvents`. (#492)
- **Gateway Core**: Decompose `handleInput` into focused sub-handlers and extract worker command handlers from switch chain for independent testability. (#492)
- **Messaging**: SOLID + test coverage — extract generic `CommandMap[T]`, move platform-specific envelope builders to adapters, split `platform_adapter.go` into 4 single-responsibility files (types, interfaces, registry, adapter), remove dead `SessionManager` type. (#491)
- **Database**: `WriteMu` becomes no-op on PostgreSQL (MVCC handles concurrency natively), PG stores use `errors.As` for unique violation detection instead of fragile string matching.

### Fixed

- **Security**: Admin API key authentication bypass — Phase 1 (`authenticateKey`) only checked config-sourced keys, database-created keys were never accepted. Add separate `dbKeys` map synced via `AddKey`/`RemoveKey` and preloaded at startup. (#495)
- **Security**: Dev mode re-activation vulnerability — add `devModeLocked` flag to `Authenticator`, preventing auth bypass when all DB keys are removed after initial configuration.
- **Security**: DBResolver PostgreSQL placeholder incompatibility — add dialect field to `DBResolver`, using `dbutil.Dialect.Rebind()` for `$1/$2/...` parameter conversion.
- **Admin**: `routes.go` omitted `APIKeyStore` and `WriteMu` on PostgreSQL path, causing admin to fall back to SQLite-style store with wrong SQL placeholders.
- **Gateway Core**: CORS preflight blocking PUT and PATCH — add both methods to `Allow-Methods` header in two locations.
- **Infrastructure**: CI webchat cache key expanded to include `context/`, `types/`, and `public/` directories to prevent stale builds.

## [1.18.1] - 2026-05-26

### Summary

v1.18.1 是一次 patch 版本更新，聚焦于 **数据库并发稳定性** 和 **Worker 协议层重构**。引入全局 WriteMu 消除 SQLite 并发写入的 SQLITE_BUSY 错误，重构 Claude Code 协议类型到独立包并实现 OCS WorkerCommander（权限控制、Compact 死锁修复）。同时修复了多步 agentic turn 的 token 统计膨胀、Cron job 永久跳过、Slack 消息处理永久阻塞等稳定性问题。

### Changed

- **Worker**: Move Claude Code protocol types from `base` to `claudecode` package, resolving SOLID/DIP violation. Implement `WorkerCommander` for OCS with `allowed_tools` permission enforcement. (#484)
- **Worker**: Harden `Compact` with context guard and 30s secondary timeout, deprecate unprotected `Stdin()` in favor of `StdinLocked()`. (#484)
- **Worker**: Fix OCS `Clear` to propagate new session ID to SSE subscription, add `sync.Mutex` to `ServerCommander` preventing data race. (#484)
- **Gateway Core**: Introduce global `WriteMu` across all SQLite stores to serialize writes and eliminate SQLITE_BUSY errors under concurrent load. (#479)
- **Gateway Core**: Extract `WriteMu.WithLock()` DRY helper consolidating 16 nil-check-lock-unlock call sites. (#479)
- **Session**: Add `source` column for differentiated cleanup — cron sessions 24h, normal sessions 7d, with configurable retention. (#477)
- **Session**: `ContextFill` now reads only from `get_context_usage` control channel, eliminating 2-3x token inflation in multi-step agentic turns. (#477)
- **Cron**: Validate `platform` matches `PlatformKey` fields (feishu requires `chat_id`, slack requires `channel_id`). (#479)

### Fixed

- **Cron**: `finishExecution` now syncs `RunningAtMs=0` to in-memory state, preventing permanently skipped jobs after timeout or failure. (#488)
- **Worker**: `turns` table `created_at=0` caused by missing `Timestamp`/`Version` in mapper — switched to `events.NewEnvelope()`. (#485)
- **Worker**: OCS `handleStepStarted` ignoring `model` field, causing empty `model_usage` in turn stats. (#485)
- **Messaging/Slack**: `handlerMu` could be held indefinitely when OCS worker start hangs — added 120s context timeout with user feedback. (#481)
- **Gateway Core**: EventStore `Collector` flush/close ordering and double-release safety net. (#479)
- **Gateway Core**: PID file accumulation on worker crash path — `proc/manager.Wait()` now calls `untrackPID`. (#479)

## [1.18.0] - 2026-05-22

### Summary

v1.18.0 是一次 minor 版本更新，聚焦于 **认证架构简化** 和 **飞书交互增强**。移除 ES256 JWT 签名体系（~3800 行），替换为 API Key + Bot ID 双字段认证模型，降低部署复杂度并消除密钥管理负担。API Key 验证使用 `subtle.ConstantTimeCompare` 防止时序攻击，resolver 调用移至锁外避免阻塞。飞书 AskUserQuestion 新增 CardKit v2 按钮交互（copy_text 一键复制选项），支持多选题提示。

### Breaking Changes

#### 认证模型变更

- **移除 JWT (ES256) 认证**：不再支持 `Authorization: Bearer <jwt>` 头。所有客户端必须使用 `X-API-Key` 头传递 API key。(#467)
- **Bot ID 传输变更**：不再通过 JWT `bot_id` claim 传递。改为 `X-Bot-ID` HTTP 头或 `bot_id` query param。服务端通过 `security.BotIDFromRequest(r)` 提取。(#467)
- **浏览器 WebSocket 客户端**：init envelope 的 `auth.token` 字段语义从 JWT 变为 API key（deferred auth）。(#467)
- **环境变量**：`HOTPLEX_JWT_SECRET` 已移除。改用 `HOTPLEX_SECURITY_API_KEY_*` 编号式环境变量。(#467)
- **用户身份**：JWT `sub` claim 的自动 per-user 隔离不再可用。默认身份为 `api_user`。多用户隔离需配置 `APIKeyResolver`（`security.SetKeyResolver()`）将 API key 映射到用户身份。(#467)
- **`--strict` 标志**：`hotplex config validate --strict` 已移除（用于检查 JWT secret 是否设置，不再适用）。

#### SDK Breaking Changes

| SDK | 移除 | 替代 |
|-----|------|------|
| Go Client | `AuthToken()` option, `token.go`, `gen-token/` | `BotID()` option — 发送 `X-Bot-ID` 头 |
| Java Client | `JwtTokenGenerator`, `.tokenGenerator()`, jjwt/bcprov 依赖 | `.apiKey()` + `.botId()` — 发送 `X-Bot-ID` 头 |
| TypeScript Client | `generate-test-token.ts`, `jose` 依赖 | `authToken` 字段语义改为 API key（deferred browser auth） |
| Python Client | — | `auth_token` 参数语义改为 API key |

#### 配置 API 变更

- `config.Load(path, LoadOptions{})` → `config.Load(path)` — 移除 `LoadOptions` 和 `SecretsProvider` 管道。
- `config.NewWatcher(log, path, sp, store, ...)` → `config.NewWatcher(log, path, store, ...)` — 移除 `SecretsProvider` 参数。
- `security.NewAuthenticator(cfg, jwtValidator)` → `security.NewAuthenticator(cfg)` — 移除 JWT validator 参数。
- `security.BotIDFromHeader(r)` → `security.BotIDFromRequest(r)` — 重命名以反映同时读取 header 和 query param。

### Added

- **Messaging/Feishu**: AskUserQuestion CardKit v2 按钮交互 — `copy_text` action 元素替代 markdown 列表，用户点击按钮即可复制选项文本，支持多选题提示和编号回退列表。(#474)

### Changed

- **Security**: API Key 验证改用 `subtle.ConstantTimeCompare` 防止时序攻击（SEC-001）。(#476)
- **Security**: `AuthenticateRequest` 将 resolver 调用移到读锁外，避免外部网络调用阻塞（CONC-004）。(#476)
- **Security**: 移除 `cliProtectedVars` 中已废弃的 `GATEWAY_TOKEN` 条目，同步更新 Env-Whitelist 文档。(#476)
- **Security**: `BotIDFromRequest` 添加信任边界文档注释（SEC-003），明确 Bot ID 未与 API Key 密码学绑定的设计决策。(#476)

### Removed

- `internal/security/jwt.go` — JWT 验证器（ES256 签名、HKDF 密钥派生、JTI 黑名单）。(#467)
- `client/token.go` — Go SDK JWT token 生成。(#467)
- `client/scripts/gen-token/` — Go SDK token 生成命令行工具。(#467)
- `client/examples/08_token_generator/` — Go SDK token 生成示例。(#467)
- `examples/typescript-client/scripts/generate-test-token.ts` — TS SDK JWT token 生成脚本。(#467)
- `examples/java-client/src/main/java/dev/hotplex/security/JwtTokenGenerator.java` — Java SDK JWT 生成器。(#467)
- `golang-jwt/jwt/v5` 依赖 — 不再需要。(#467)
- `jjwt-api/impl/jackson` + `bcprov-jdk18on` 依赖（Java SDK） — 不再需要。(#467)

### Fixed

- **Messaging/Feishu**: JSON 1.0/2.0 兼容性 — 交互卡片使用 JSON 1.0 以支持 `action` + `copy_text` 元素（JSON 2.0 不支持）。(#474)
- **Messaging/Feishu**: 选项文本经 `SanitizeText()` 清洗，防止注入。(#474)
- **SDK/Java**: `QuickStart.java` 变量名 `signingKey` 修正为 `apiKey`。(#476)
- **Docs**: `integration-patterns.md` SDK 伪代码更新为实际 API（`client.APIKey()`/`client.BotID()`）。(#476)

### Security

- API Key 恒定时间比较（`subtle.ConstantTimeCompare`），替代 `map[string]bool` lookup，消除时序攻击面。(#476)
- 移除 JWT JTI 黑名单 sweep goroutine 和 HKDF 密钥派生，减少攻击面和资源开销。(#467)

### Migration Guide

**1. 环境变量替换：**
```bash
# 旧（已移除）
export HOTPLEX_JWT_SECRET="$(openssl rand -base64 32)"

# 新
export HOTPLEX_SECURITY_API_KEY_1="your-api-key"
export HOTPLEX_ADMIN_TOKEN_1="your-admin-token"
```

**2. Go SDK 迁移：**
```go
// 旧
client.New(ctx, URL("ws://localhost:8888/ws"), WorkerType("claude_code"), AuthToken("jwt-token"))

// 新
client.New(ctx, URL("ws://localhost:8888/ws"), WorkerType("claude_code"), BotID("bot-123"))
```

**3. 多用户隔离：** 如果之前依赖 JWT `sub` 实现用户隔离，需在 Gateway 启动时配置 `APIKeyResolver`：
```go
auth.SetKeyResolver(security.NewChainResolver(dbResolver, configResolver))
```

**4. 浏览器 WebSocket：** init envelope 中 `auth.token` 从传 JWT 改为传 API key，`auth.bot_id` 保持不变。

## [1.17.0] - 2026-05-21

### Summary

v1.17.0 是一次 minor 版本更新，聚焦于 **Admin WebUI 全面升级** 和 **企业级多用户隔离**。Admin 面板新增 Gateway 重启 API、Sessions 页面重构（实时更新/搜索筛选/详情视图）、API Key 用户管理 WebUI、Login 帮助面板，并在 Chat UI 侧边栏底部集成 Admin 入口；安全层新增 APIKeyResolver 实现 API Key → 用户身份映射，激活完整的会话隔离链路（UUIDv5 session key、SEC-008 跨用户拒绝、per-user 配额）；CodexCLI Worker 修复审批死锁和竞态条件；飞书 SDK 升级至 v3.9.1 并完成交互卡片按钮规格设计。

### Added

- **Security**: APIKeyResolver for enterprise multi-user session isolation — ChainResolver (config → DB) maps API keys to user identities, activating UUIDv5 session keys, SEC-008 cross-user rejection, per-user SQL filtering, and PoolManager per-user quotas. Admin API CRUD for api_key_users table with cache coherence. (#468)
- **WebChat UI**: Admin WebUI overhaul — gateway restart endpoint (POST /admin/restart), sessions page with real-time updates/search/detail view, API Key user management page with create/edit/delete, login help accordion, admin dashboard enhancements with improved layout and stats. (#471, #473)
- **WebChat UI**: Admin entry in chat sidebar — styled admin dashboard link at sidebar footer with icon container, hover gold accent, and descriptive subtitle. (#473)
- **Messaging**: Add M/B units to FormatTokenCount for large token values — display as K/M/B suffixes with compact formatting, applied to both Go backend and webchat TS frontend. (#470)
- **Messaging**: Upgrade Lark SDK to v3.9.1 and add interactive card buttons spec — comprehensive spec for implementing card button callbacks across Feishu/Slack/WebChat, documenting SDK card handler limitation. (#466)

### Changed

- **Worker**: CodexCLI security hardening — extract timeout constants, fix sendEnvelope timer leak with time.NewTimer + Stop(), unify write-mu blocks into writeFrame(), remove personality double-default.

### Fixed

- **Worker**: CodexCLI approval deadlock — server-initiated JSON-RPC requests (approvals) were silently dropped; implement HandlePermissionResponse via RespondServerRequest, add approval method aliases. (#462)
- **Worker**: CodexCLI Input() TOCTOU race — move nil check into spawn(), enforce StartupTimeout, add 5s timeout for critical event sends, add doneCh-based lifecycle binding. (#462)
- **Messaging**: FormatTokenCount unit boundary rounding — 999999 now shows "1M" instead of "1000.0K", use threshold-based unit bumping and stale ".0" suffix cleanup.
- **Admin**: Routing, sessions UX, and login redirect loop — fix Next.js SPA directory-to-html resolution, correct admin URL default from 9090 to 9999, replace router.replace with window.location.replace to fix stale auth state.
- **Release**: Guard changelog extraction against regex bracket misparse — add line count sanity check for multi-version extraction detection.

## [1.16.0] - 2026-05-20

### Summary

v1.16.0 是一次 minor 版本更新，聚焦于 **第三 Worker 接入** 和 **管理后台**。新增 OpenAI Codex CLI Worker（exec + app-server 双模式），支持 GPT-5/o4 系列模型通过 Web/Slack/飞书使用；Agent Bot Management WebUI 提供完整的可视化管理（Bot CRUD、配置编辑、Prompt 预览、Session/Cron 管理）。EventStore 从 SQLite 视图升级为物化 turns 表（含 cache token 拆分），Claude Code input token 统计修复使其准确计入缓存 token（影响 35-60% 的用量数据）。

### Added

- **Worker**: Codex CLI Worker — third worker type supporting `codex exec --json` (one-shot) and `codex app-server` (persistent JSON-RPC) dual modes, with 11 TurnItem → AEP event mapping, lazy-start singleton process with ref counting and 30m idle drain. (#450)
- **WebChat UI**: Agent Bot Management WebUI — dashboard, bot list/detail/create/delete, agent config editor (SOUL/AGENTS/SKILLS/USER/MEMORY), system prompt preview, session management, cron management, and connection settings. (#453)
- **Admin API**: Bot config CRUD and agent config file management — `BotConfigProvider` adapter bridging admin API to messaging config, with scope hierarchy (`admin:write` → `admin:read` → `config:read`). (#453)

### Changed

- **EventStore**: Replace turns views with materialized table — pre-computed turns table written at done/input time via Collector dual-channel, replacing expensive SQLite views (13+ json_extract per row + window functions). Breaking: `TurnRecord.ID string→int64`, `before_seq→before_id` API param, new `generation`/`turn_num`/cache token fields. (#456)
- **EventStore**: Buffer and aggregate reasoning events — extend delta accumulator pattern to reasoning chunks, reducing SQLite writes from N rows per reasoning block to 1 merged row. (#455)
- **Configuration**: Change events retention default from 7 to 30 days (168h → 720h). (#456)

### Fixed

- **Gateway Core**: Include cache tokens in Claude Code input token accounting — Anthropic API reports `input_tokens + cache_creation_input_tokens + cache_read_input_tokens` as separate fields; previously only `input_tokens` was counted, dropping ~35-60% of actual usage. (#456)
- **Worker**: OCS HTTP POST 30s timeout falsely reported "server unreachable" — split error classification into `isTimeoutError`/`isUnreachableError`, add dedicated 5min sendClient for input delivery. (#449)
- **Messaging**: CardKit empty flush triggering field validation error (99992402) — skip flush when content is empty but lastFlushed has content. (#449)
- **Messaging**: Messaging bridge event drop — swap `StartPlatformSession`/`JoinPlatformSession` call order so platform conn is registered before state transitions. (#449)
- **Worker**: Codex CLI app-server mode `appConn.Send` bypassed JSON-RPC by writing raw AEP to stdin — replaced with `ErrNotImplemented` fail-fast. (#458)

## [1.15.0] - 2026-05-18

### Summary

v1.15.0 是一次 minor 版本更新，聚焦于 **OCS V2 Converter 架构升级** 和 **Phrases 话术定制系统**。OCS Worker 引入独立 Converter 结构体替代 V1 消息解析，支持结构化权限/问答事件、reasoning 阶段检测、model/schema/variant 透传；Claude Code Worker 新增 4 项 Session Flags 映射（--continue/--fork-session/--resume-session-at/--settings）；飞书消息适配器完成 Welcome Card 可配置化和 Persona Phrases 系统；Slack 修复 Reasoning 状态映射。

### Added

- **Worker**: OCS V2 Converter architecture — standalone `Converter` struct with typed event parsing, per-session `turnState` tracking, and structured tool call/result conversion, replacing V1 `message.part.delta/updated` parsing. (#434)
- **Worker**: Structured permission/question events — direct `PermissionRequestData`/`QuestionRequestData` conversion replacing raw passthrough, with Feishu elicitation explicit accept/decline keywords. (#443)
- **Worker**: Reasoning phase detection — bracket `field="text"` deltas between reasoning lifecycle events to prevent inner monologue leakage in OCS 1.15+ unified `message.part.delta`. (#437)
- **Worker**: Model/JSONSchema/variant passing — bridge 6 capability gaps between OCS Worker and OpenCode Server (AllowedModels, JSONSchema, variant/reasoning effort, reasoning events, experimental event system, image modality). (#434)
- **Worker**: Claude Code session flags — map `ContinueSession`/`ForkSession`/`ResumeSessionAt`/`ConfigEnv` to `--continue`, `--fork-session`, `--resume-session-at`, `--settings` CLI flags. (#444)
- **Messaging**: Configurable welcome card via phrases — extract hardcoded capabilities, quick commands, and closing line into 3 phrase categories with per-bot customization. (#441)
- **Messaging**: Persona phrases — `persona` and `closings` categories for placeholder status lines and random sign-off on turn completion, with abort-path omission. (#438)

### Changed

- **Worker**: OCS singleton.go reduced by 244 lines — event dispatch fully delegated to V2 Converter with simplified SSE integration. (#434)
- **Messaging**: Chat access non-blocking — `handleChatEntered` no longer propagates errors, record failures logged as Warn instead of blocking user flow. (#442)

### Fixed

- **Worker**: Fix tool failure edge case — `isFailed=true` with nil Error now reports "tool failed" instead of silently falling through to success path. (#434)
- **Worker**: Fix cross-process turnState leak — add `Converter.Reset()` called in `startProcessLocked` to prevent stale state across process restarts. (#434)
- **Worker**: Fix data race on conn.variant/allowedModel — consolidate reads and writes under single `conn.mu` lock acquisition. (#434)
- **Messaging**: Fix Slack reasoning status — add `events.Reasoning` → `StatusThinking` mapping in `notifyStatusFromEvent`. (#442)

## [1.14.0] - 2026-05-15

### Summary

v1.14.0 是一次 minor 版本更新，聚焦于 **OCS Worker 生产就绪** 和 **零 CGO 构建**。OCS Worker 从单连接 SSE 升级为全局 EventBus 架构，新增指数退避重连、LastInput 崩溃恢复、HTTP 错误分类等关键能力，配合 4 项并发修复消除生产风险。SQLite 驱动统一为纯 Go 实现（modernc.org/sqlite），所有平台支持 `CGO_ENABLED=0` 构建，Linux arm64 不再需要交叉编译器。

### Added

- **Worker**: OCS SSE EventBus — singleton global SSE reader (`GET /global/event`) with per-session channel dispatch, replacing per-worker SSE connections. (#428)
- **Worker**: SSE reconnection with exponential backoff (500ms initial, 10s max, 20% jitter) and empty-stream detection to prevent CPU-burning tight loops. (#428)
- **Worker**: LastInput caching via `InputRecoverer` interface for bridge crash recovery re-delivery. (#428)
- **Worker**: HTTP error classification — server down / 502 / 503 mapped to typed `WorkerError(ErrKindUnavailable)` for gateway retry routing. (#428)
- **Messaging**: P2P chat entered event — welcome card on first/returning user entry for Feishu (`bot_p2p_chat_entered_v1`) and Slack (`app_home_opened`), with `ChatAccessStore` analytics (new/returning/active classification, 1h cooldown). (#427, #430)

### Changed

- **Infrastructure**: Consolidate to pure-Go SQLite driver (modernc.org/sqlite), removing mattn/go-sqlite3 CGO dependency — enables `CGO_ENABLED=0` across all build targets (Makefile, Dockerfile, release CI) and simplifies cross-compilation.
- **Eventstore**: Relax collector flush interval from 100ms to 1s and delta flush from 2s to 3s, reducing disk I/O overhead.
- **Gateway Core**: Remove per-event debug log in bridge forwarding to cut log volume under high throughput.

### Fixed

- **Worker**: Fix shared pointer mutation in `dispatchToAllSubscribers` — convert envelope inside the subscriber loop instead of reusing a shared pointer, which caused all subscribers to receive the last sessionID.
- **Messaging**: Skip cumulative `message.part.updated` text to prevent Feishu card duplication — cumulative text was appended as incremental delta, causing repeated text in streaming cards.
- **Worker**: Eliminate test data race on backoff package variables — replace `t.Cleanup` restore with deferred restore after goroutine exit.
- **Worker**: Prevent tight SSE reconnect loop on empty streams — only reset attempts after reading at least one data line; on empty EOF, apply exponential backoff.

## [1.13.2] - 2026-05-15

### Summary

v1.13.2 是一次 patch 更新，聚焦于 **消息体验增强** 和 **错误处理类型安全**。新增 Phrases 模块，将硬编码的 CLI 提示语和问候语提取为可配置、可扩展的消息池（权重随机 + 级联加载 + per-bot 个性化）；Worker 层引入类型化 `WorkerError` 替代脆弱的字符串匹配错误分类。

### Added

- **Messaging**: Phrases module — extract 28 CLI tips and 8 greetings from feishu placeholder into a shared, configurable message pool with weighted random selection (bot=4, global=2, platform=1), cascade-append loading, per-bot personalization, and B-channel skill manual. (#426)

### Changed

- **Worker**: Replace `strings.Contains` error classification with typed `WorkerError` + `Kind` enum — gateway uses `errors.As` for type-safe routing instead of fragile string matching. (#426)

## [1.13.1] - 2026-05-15

### Summary

v1.13.1 是一次 patch 更新，聚焦于 **架构健壮性与可观测性**。新增 per-bot 配置覆盖能力，支持多 Bot 场景下独立凭证与权限控制；Brain ConfigSpec 注册表消除 35+ 环境变量的静默降级；Gateway 新增 7 项 Prometheus 指标和 admin 审计日志；messaging/eventstore 层完成 DRY 重构减少约 140 行重复代码。

### Added

- **Config**: Per-bot configuration with platform-level fallback — override credentials, work directory, and access control per bot instance, with `${VAR}` env expansion fix. (#415)
- **Gateway Core**: 7 Prometheus metrics — session starts/errors/duration, init handshake latency, retry attempts/exhaustion, worker creation duration. (#417)
- **Admin API**: Structured audit logging for write operations + request-level middleware logging (method, path, status, duration, IP). (#417)

### Changed

- **Brain**: ConfigSpec registry (38 entries) replaces 35+ hardcoded `os.Getenv` calls with startup validation and structured warnings. (#420)
- **Service**: CommandRunner interface — 16+ `exec.Command` calls now injectable for testability. (#420)
- **Eventstore**: Extract `accumulateDelta`/`getOrCreateAccum` helpers, replace manual reverse with `slices.Reverse`, add dropped counter and `TurnQuerier` interface. (#419)
- **Messaging**: DRY — extract `createAndEnable` helper from streaming card lifecycle (~45 lines), shared interaction `SendResponse` factory + metadata builders (~96 lines). (#418)
- **Admin API**: Cron handler test coverage — 28 table-driven tests, 0% → covered. (#420)

### Fixed

- **Config**: Scope `.gitignore` `skills/` to repo root, update MessagingConfig comment. (#416)

## [1.13.0] - 2026-05-14

### Summary

v1.13.0 是一次 minor 版本更新，核心主题是 **多 Bot 支持** 和 **性能优化**。新增 Multi-Bot 架构，允许单个 Gateway 同时运行多个独立 Bot 实例（独立凭证、Soul、Worker 类型），完全向后兼容现有单 Bot 配置。同步引入 Brain/Session 锁优化（lock-dropping）降低锁竞争延迟，以及 Worker 接口去重简化适配器开发。

### Added

- **Messaging**: Multi-bot support — run multiple independent bot instances per platform (Slack/Feishu) with separate credentials, soul, worker type, and STT/TTS config. Backward compatible: single-bot configs auto-wrap into `bots[]` via `normalizeSlackBots`/`normalizeFeishuBots`. (#410)
- **Messaging**: `BotRegistry` — concurrent-safe runtime registry with Register/Unregister/Get/List/UpdateStatus/UnregisterAll lifecycle operations. (#410)
- **Admin API**: Bot status endpoints — `GET /admin/bots` lists all registered bots, `GET /admin/bots/{name}` returns single bot details. (#410)
- **CLI**: Multi-bot config diagnostic checker — validates credential completeness, bot count limits, and name uniqueness. (#410)
- **Config**: `SlackBotConfig`/`FeishuBotConfig` types with per-bot STT/TTS inheritance via `propagateBotDefaults`. (#410)

### Changed

- **Brain**: Lock-dropping optimization — memory compression and context pruning now release locks before expensive LLM calls, reducing contention on high-concurrency sessions. (#409)
- **Session**: `runningIndex` + terminated eviction + attach lock-dropping — O(1) active session lookup by key, automatic cleanup of terminated sessions, and non-blocking session attach. (#408)
- **Worker**: Deduplicate `ControlRequester` and `WorkerCommander` interfaces into unified `Controllable` interface, simplifying worker adapter implementation. (#406)
- **Cron**: Admin adapter DRY — extract shared adapter logic, add test coverage. (#409)

## [1.12.0] - 2026-05-13

### Summary

v1.12.0 是一次 minor 版本更新，聚焦于 **Gateway 运维可靠性**。新增 detached restart helper（Worker-initiated restart 独立 PGID 进程），解决 Worker 进程执行 gateway restart 时被连带杀死的长期问题。同步修复 JWT audience 类型匹配、admin CIDR 热重载和 cron trigger 配置路径解析三个 bug。

### Added

- **Gateway Core**: Detached restart helper — fork 独立 PGID 进程完成 `gateway restart --detached`，在 Gateway/Worker shutdown 后存活并重启。60s 冷却期防止重启循环，跨平台支持 Unix (Setpgid) / Windows (CREATE_NEW_PROCESS_GROUP)。 (#404)

### Fixed

- **Security**: JWT `hasAudience` 类型匹配 — type switch 改为匹配 `jwt.ClaimStrings`（named `[]string` type），修复 audience 验证始终失败的问题。 (#403)
- **Config**: Admin `allowed_cidrs` 热重载 — 新增 hot-reload callback，配置变更即时生效无需重启。 (#403)
- **Cron/CLI**: TriggerViaAdmin 配置路径解析 — 通过 gateway PID 文件定位实际 config path，修复 CLI 从不同目录运行时 `.env` 路径不匹配问题。 (#403)

## [1.11.4] - 2026-05-13

### Summary

v1.11.4 是一次 patch 更新，聚焦于 **Cron 迁移修复、投递身份和 Java SDK JWT 同步**。修复 Cron migration 005-008 的 SQLite 列序错位（4 个迁移合并为单一 005）；修复 Cron 飞书投递身份（`--as bot`）；同步 Java SDK JWT 密钥派生至 HKDF (RFC 5869) 与 Gateway 一致；更新 JWT 安全文档。

### Fixed

- **Cron**: Migration column order corruption — consolidating migrations 005-008 into a single 005 that creates the final schema directly, eliminating ALTER TABLE ADD COLUMN + SELECT * misalignment. Data fix script provided for existing deployments.
- **Cron**: Feishu delivery identity — executor adds `--as bot` to lark-cli command so cron results are sent as bot identity instead of user identity. (#402)
- **SDK/Java**: JWT key derivation synced to HKDF-SHA256 (RFC 5869) with info "hotplex-ecdsa-p256", matching Go gateway and Go client SDK. Previous direct-copy derivation produced incompatible keys, causing all Java-generated tokens to be rejected by v1.11.3+ gateways. (#402)

### ⚠️ Migration Notice (v1.11.0-v1.11.3 users)

If you deployed any version from v1.11.0 to v1.11.3, your `cron_jobs` table has corrupted column data. See `scripts/fix-cron-column-order.sh` for the one-time repair script.

## [1.11.3] - 2026-05-12

### Summary

v1.11.3 是一次 patch 更新，聚焦于 **Cron attached session**、**安全加固双批次** 和 **文档中心演进**。新增 attached session 支持定时注入现有会话（`--attach` CLI，`at:+N` 相对时间语法）；两个安全批次覆盖 config reflection panic、dead validators 激活、飞书 goroutine panic recovery、admin body size limit 等 11 项修复；文档中心新增 sidebar audience grouping、构建时 link validation、Java SDK 参考文档。

### Added

- **Cron**: Attached session — inject cron prompts into existing live sessions via `--attach` CLI flag. Supports state-based dispatch (Running→InjectInput, Idle→ResumeAndInput), auto-fills from `$GATEWAY_SESSION_ID`, cascade-deletes on session terminate. (#377)
- **Cron**: `at:+N` relative time syntax (min 1min, max 72h) for one-shot attached jobs with `--attach`. (#377)
- **Cron**: Prometheus metric `hotplex_cron_attached_total` for attached session dispatch tracking. (#377)
- **Admin**: `admin:write` scope added to default scopes for cron trigger authorization. (#393)
- **Docs**: Java SDK reference documentation (`docs/reference/sdk-java.md`). (#393)

### Fixed

- **Config**: Reflection panic on env-mapping typo — `setField`/`setBoolField`/`setSliceField` no longer panic on non-existent struct fields, return descriptive errors instead. Startup `Validate()` warns on misconfigurations (negative retention_period, zero max_size). (#390)
- **Security**: Dead validators activated — `ValidateTools` and `ValidateModel` enforced at init handshake, previously zero production callers. Defense-in-depth model validation in claudecode worker. (#386)
- **Security**: JWT key derivation upgraded to HKDF (RFC 5869) — Go Client SDK synchronized to match gateway implementation. (#391)
- **Security**: `GenerateToken`/`GenerateTokenWithJTI` auto-fill `aud` claim when audience configured. (#391)
- **Security**: `SanitizeArg` preserves non-ASCII characters (CJK/Emoji). (#391)
- **Admin**: Request body size limit (1MB) added to cron create/update handlers to prevent memory exhaustion. (#380)
- **Worker**: URL path escaping added to OCS question/elicitation handlers. (#388)
- **Messaging/Feishu**: Panic recovery added to 3 unprotected goroutine entries (sendTurnSummaryCard, TTS Process, ChatQueue.runWorker). (#378)
- **Brain**: Structured logging when SafetyGuard blocks threats — previously only incremented atomic counter. (#381)
- **Brain**: Configurable fail-closed policy (`FailClosedOnBrainError`) + `atomic.Pointer` for global singletons to eliminate data race. (#379)
- **Cron**: Double metric counting fixed, error path persist fix, migration 008 CHECK constraint two-step table replacement. (#377)

### Changed

- **Cron**: Rename `PayloadAgentTurn` → `PayloadIsolatedSession`, `agent_turn` → `isolated_session` in SQLite migration 008. (#377)
- **Cron**: Skill manual trimmed 24% — removed creator-unnecessary sections for tighter Agent consumption. (#377)
- **Config**: `setField`/`setBoolField`/`setSliceField` signatures changed from void to error return type. (#390)
- **Docs**: Sidebar audience grouping, last-modified timestamps, build-time link validation (zero broken links enforced), CI path expansion for docs changes. (#393)


## [1.11.2] - 2026-05-11

### Summary

v1.11.2 是一次 patch 更新，聚焦于 **Cron 执行器可靠性** 和 **飞书适配器代码健康**。修复 cron executor stdin 未关闭导致 `--print` 模式 worker 永久挂起的关键 bug，同时持久化 auto-disable 标志防止 `rebuildIndex` 重新启用已失败的 job。飞书适配器完成 SOLID 重构，将 1562 行 god file 拆分为 5 个职责明确的模块。

### Fixed

- **Cron**: Executor stdin EOF — close stdin after writing cron prompt so `--print` mode workers exit cleanly instead of hanging indefinitely. Persist auto-disable flag to SQLite so `rebuildIndex` does not re-enable failed jobs. (#372)
- **Worker**: Base conn `CloseStdin()` method for cross-worker stdin shutdown.

### Changed

- **Messaging/Feishu**: SOLID + DRY refactoring — split `adapter.go` (1562→353 lines) into `conn.go`, `handler.go`, `media.go`, `ws.go`. Extract 184-line `WriteCtx` dispatcher into per-event handlers, consolidate Lark IM API helpers to eliminate 4× duplication. (#370)

## [1.11.1] - 2026-05-11

### Summary

v1.11.1 是一次 patch 更新，聚焦于 **Cron 定时任务可靠性** 和 **WebSocket 连接稳定性**。核心改动包括：Cron 生命周期约束系统（防止无限执行、race condition 修复、mergeJobState 原子状态更新）、Cron executor 传递 job 实际 platform 以正确加载 agent 配置、WebSocket backpressure 优化（TryWriteMessage 防止慢客户端因 delta 丢弃而断连）。影响 Cron 调度器、Gateway Core 和 Agent Config 加载层。

### Added

- **Cron**: Lifecycle constraint system — recurring jobs require `max_runs` + `expires_at`, auto-default to max-runs=10 / expires-at=now+24h, auto-disable after 10 consecutive execution failures or 5 schedule errors. (#369)
- **Cron**: WorkDirResolver — platform-aware workdir resolution (explicit > platform-specific > worker default) via `config.ResolvePlatformWorkDir`.
- **Cron**: `mergeJobState` atomic state update — prevents executeJob goroutines from overwriting concurrent CLI state changes (e.g. disable).
- **Gateway Core**: TryWriteMessage — non-blocking write that silently drops droppable events (message.delta, raw) instead of disconnecting slow clients.

### Changed

- **Cron**: Executor passes job's actual platform (e.g. "feishu", "slack") to agent config loader instead of hardcoded "cron", enabling correct 3-tier config fallback (global → platform → bot).
- **Cron**: Session detection for env injection and MCP suppression switched from `platform == "cron"` to `platformKey["cron_job_id"]` presence check.
- **Cron**: Skill manual restructured with XML sections (`<critical_rules>`, `<intent_recognition>`, `<prompt_assembly>`, `<lifecycle_inference>`) for clearer Agent consumption.
- **Gateway Core**: Extract `bufferOrReject` helper to eliminate duplication between WriteMessage and TryWriteMessage closed-check + init-phase buffering.

## [1.11.0] - 2026-05-11

### Summary

v1.11.0 是一次 minor 版本更新，聚焦于 **文档门户基础设施和 MCP 配置治理**。新增静态文档门户（51 篇文档，go:embed + gateway 原生托管）、MCP 配置基础设施（cron 自动抑制 MCP 节省 ~600 MB/worker）、以及 WebChat/docs 启动 banner 的文档链接覆盖。同时修复 Slack 适配器在 Clone 重构后丢失工具信息的关键问题。

### Added

- **Docs Portal**: Static documentation portal — `cmd/build-docs` generates HTML from Markdown, `internal/docs` embeds and serves at `/docs/` route. 51 docs across guides, tutorials, reference, and explanation sections. (#367)
- **Worker**: MCP config infrastructure — user-configurable MCP server control and automatic MCP suppression for cron workers, saving ~600 MB per cron worker. `config.MCPServerConfig` validation, hot-reload via `atomic.Value`, and pre-serialized JSON injection. (#367)
- **WebChat UI**: Documentation links in three surfaces — header gold pill badge, sidebar footer, and welcome screen "Read Documentation" button.
- **CLI**: Startup banner now shows `Docs` line with gateway `/docs/` URL alongside Gateway/Admin/WebChat addresses.

### Changed

- **Docs**: Restructure docs directory — archive 20 legacy docs, delete 4 stale files, merge STT-SETUP into voice-features, consolidate assets. MISE audit fixed 126 issues (12 P0 + 56 P1 + 58 P2) including ghost CLI commands, phantom API endpoints, and hallucinated config fields.

### Fixed

- **Messaging/Slack**: Restore tool info in assistant status messages and turn summaries — `events.Clone` struct copy broke three type assertion paths (`ToolCallData`/`ToolResultData` pointer mismatch, `map[string]int` in `map[string]any` container, `float64` vs `int` in summary). Fix via `events.DecodeAs[T]` and `events.ToInt64`. (#364)
- **Gateway Core**: `extractDeltaContent`/`extractMessageContent` handle typed struct data from `Clone()` deep copy (was only handling `map[string]any`). (#368)

## [1.10.2] - 2026-05-10

### Summary

v1.10.2 是一次 patch 更新，修复 cron session 全局碰撞导致 100% 超时失败的关键 bug，同时包含 Gateway 热路径性能优化（handler 拆分、异步写管道消除 HOL 阻塞、Clone 3200x 提速）和 cron `--silent` 结构化字段替代文本前缀。

### Fixed

- **Cron**: Session key derivation ignored job ID — all cron jobs for the same owner/bot/workDir derived the same key, causing 100% timeout failures. Introduce CronNamespace UUIDv5 with per-execution isolation. (#362)
- **Gateway Core**: Head-of-line blocking — one slow WebSocket client could stall Hub broadcast for up to 10s. Per-conn writeCh (cap 64) decouples Hub.Run from write latency; slow clients are disconnected immediately. (#325)

### Changed

- **Gateway Core**: Split handler.go (852 lines) into 4 files by responsibility — handler.go, commands.go, worker_cmds.go, errors.go. Move EncodeJSON outside c.mu.Lock to reduce lock hold time. (#352)
- **Gateway Core**: Event.Clone() replaces JSON round-trip with struct copy — 3200x faster, 0 allocs for typed events; recursive deep copy for map[string]any nested values. Add CloneDeep() for full JSON independence. (#325)
- **Cron**: Promote `silent` from magic `[SILENT]` prompt prefix to structural `Silent bool` field — CLI `--silent` flag, YAML `silent: true`, Admin API support, `cron get` human-readable output. (#366)

## [1.10.1] - 2026-05-10

### Summary

v1.10.1 是一次 patch 更新，修复 CLI 创建的 cron job 因 `Platform` 硬编码为 `"cron"` 导致执行结果无法投递到 Slack/飞书的关键 bug。同时包含 Gateway 和 Messaging/Slack 模块的 DRY 重构。

### Fixed

- **CLI**: Cron job creation resolves target platform from env vars (`GATEWAY_PLATFORM`/`GATEWAY_CHANNEL_ID`/`GATEWAY_THREAD_ID`) and `--platform`/`--platform-key` flags, fixing silent delivery failure to Slack/Feishu. (#360)

### Changed

- **Messaging/Slack**: Extract `SlackAPI` interface for testability — adapter tests can mock Slack API calls without Socket Mode. (#361)
- **Messaging/Slack**: SOLID + DRY refactor — extract `WriteCtx` decomposition into `conn_events.go`, make `TTLCache` generic, deduplicate validator logic. (#356)
- **Gateway**: DRY refactor — extract Slack context injection, auth boilerplate, and workdir validation helpers. (#357)

## [1.10.0] - 2026-05-10

### Summary

v1.10.0 是一次 minor 版本更新，核心变更是 **AI-native Cronjob 调度器** 正式投产（SQLite 持久化、at/every/cron 三种调度、Slack/飞书结果投递、7 个 Admin API 端点 + CLI）。安全方面修复了 cron handler 缺失 scope 校验（任意认证请求可管理定时任务）、`mustGenerateJTI` 静默失败、updater 绕过注入的 HTTP client。可靠性方面修复了 Brain 模块多处 data race、Gateway crashTracker 无限增长、WebChat 双实例心跳异常、onboard 向导无限挂起。

### Added

- **Cron**: AI-native cronjob scheduler — timer-driven tick loop with at-most-once semantics, SQLite persistence via goose migration, three schedule types (at/every/cron with timezone), concurrency cap, graceful shutdown with running-job drain. (#340)
- **Cron/CLI**: `hotplex cron` subcommand — list/get/create/update/delete/trigger/history operations with direct SQLite CRUD and Admin API trigger.
- **Cron/Admin API**: 7 new endpoints — CRUD + trigger + run history for cron job management.
- **Cron/Metrics**: Prometheus counters — `cron_fires_total`, `cron_errors_total`, `cron_duration_seconds`.
- **Gateway**: Context env var injection (`GATEWAY_*`) for worker processes, normalizing Slack/Feishu platform differences.

### Changed

- **Admin API**: DI improvements — `startedAt` and `LogCollector` injected via `Deps` instead of package-level globals; `requireScope` helper eliminates duplicated scope-check boilerplate; `LogCollector.Recent` merged with `Total` to avoid double lock acquisition.
- **Worker/OCS**: DRY lifecycle helpers — extract `checkNotStarted`, `acquireServer`, `startSSE`, `release` to eliminate ~55 lines of duplicated Terminate/Kill/Start/Resume code. (#344)
- **Worker**: Unified grace period constant — `proc.DefaultGracePeriod` as canonical source, replacing three scattered 5s definitions. (#258)
- **Brain**: Replace `sync.Once` + direct assignment in `IntentRouter` with `atomic.Pointer` to eliminate dual-init data race. (#339)
- **Security**: DRY path getters — `GetAllowedBaseDirs`/`GetForbiddenWorkDirs` unified from platform-specific files to shared `path.go`.
- **Config**: Unexport field registries (`hotReloadableFields`/`staticFields`), remove dead `MustLoad`/`ReadFile` API. (#253)
- **Events**: Unexport `ValidTransitions` → `validTransitions`; callers use `IsValidTransition()`. (#252)
- **Service**: DRY unix helpers — extract `stopAndUnload` (darwin) and `writeServiceFile` (shared unix). (#263)

### Fixed

- **Security**: All 7 cron handler endpoints lacked scope validation — any authenticated request could create, delete, or trigger cron jobs. Read ops now require `admin:read`; write ops require `admin:write`. (#259, #353)
- **Security**: `mustGenerateJTI` silently returned empty string on `crypto/rand` failure instead of panicking. (#341, #353)
- **Security**: Cron admin token isolated from worker env — `HOTPLEX_ADMIN_TOKEN` only injected when `platform=="cron"`, preventing credential leak to all worker processes.
- **Admin API**: `clientIP` broke IPv6 bracketed addresses (`[::1]:8080`) — replaced `strings.Cut` with `net.SplitHostPort`. (#347)
- **Brain**: Data races in `IntentRouter` (`enabled` → `atomic.Bool`), `ContextCompressor` (`config.Enabled` → `atomic.Bool` reads), and `SafetyGuard` (`Close` → `sync.WaitGroup` for cleanup goroutine). (#346)
- **Gateway**: `crashTracker` entries never deleted — unbounded map growth on long-running gateways; `pendingError` silently lost when worker recv channel closed; state notification sends now log warnings on failure. (#345)
- **WebChat**: Duplicate `BrowserHotPlexClient` instances on re-render — useEffect guard disconnects lingering client before creating new one. (#334)
- **Updater**: `Download()` bypassed injected `u.Client` with hardcoded `http.Client`, making it untestable. Now uses `u.Client` with context deadline. (#251, #353)
- **Session**: Phantom `DeleteExpiredEvents` mock (method doesn't exist on interface) removed; `Kill()` error now logged in `DeletePhysical`. (#243, #353)
- **Cmd**: Onboard wizard hung indefinitely on closed stdin — replaced bare `context.Background()` with 5-minute timeout. (#245)
- **Cron**: Data races in Scheduler (`Clone+putJob`), Delivery (`sync.Mutex`), `executeJob` context propagation; at-schedule busy-loop debounce when concurrency cap hit; `persistCtx` context leak fixed.

### Security

- Cron handler endpoints now enforce JWT scope checks (admin:read / admin:write). Previously any valid token could manage cron jobs. (#259)
- Cron admin token isolated from generic worker environment to prevent credential exposure. (#340)

## [1.9.1] - 2026-05-10

### Summary

v1.9.1 是一次 patch 版本更新，聚焦于 **热路径性能优化** 和 **运维可观测性修复**。Worker stdio 热路径消除双重 JSON 解析（每 turn 省约 1000 次分配），SafetyGuard 采用 RLock 快路径 + lazy limiter eviction 防止内存无限增长。WebChat 新增 stop button 取消反馈动画，飞书修复 THINKING 反应泄漏，EventStore/AgentConfig 修复静默错误吞没。

### Added

- **WebChat UI**: Stop button shows spinner animation during 600ms cancellation window, giving clear visual feedback before send button appears.

### Changed

- **Worker**: Eliminate double JSON parse in claudecode `readOutput` hot path — single `json.Unmarshal` + reuse for type routing, control_response, and error extraction. Saves ~1000 allocs/turn. (#327)
- **Brain**: SafetyGuard uses `RLock` fast path for concurrent lookups and adds lazy limiter eviction — entries not seen within 1 hour are cleaned up when map exceeds 100 entries. (#328)
- **WebChat UI**: Remove ghost message pattern that caused `MessageRepository` ID collisions.

### Fixed

- **Messaging/Feishu**: THINKING reaction from silence timer was never cleaned up — both Typing and THINKING emojis accumulated on user messages.
- **EventStore**: `QueryTurnStats` silently skipped scan errors, returning partial or misleading results. Now logs warning with error details. (#293)
- **CLI**: `status` command hardcoded `http://` for health check URL, reporting "unreachable" when TLS is enabled. (#297)
- **Agent Config**: Config content exceeding 8KB/40KB limits was silently truncated with no indication — now logs warning with file name and size details. (#247)
- **CI**: Webchat build was skipped on cache miss when `build-webchat` was false, leaving `internal/webchat/out` absent and failing `go vet`.

## [1.9.0] - 2026-05-09

### Summary

v1.9.0 是一次 minor 版本更新，聚焦于 **跨平台工具格式化统一** 和 **消息通道稳定性**。核心变更是提取共享 `toolfmt` 包，消除飞书与 Slack 适配器约 360 行重复代码并统一工具调用/结果的 UX 呈现。飞书新增 streaming placeholder 卡片（消除了"黑洞"静默等待）和实时 tool activity strip，WebChat 获得 ErrorBoundary 防御上游 assistant-ui 崩溃、双层消息去重和完整的类型系统统一，Gateway Core 修复了多个并发缺陷（WritePump race、Bridge.accum 泄漏、端口冲突静默崩溃）。

### Added

- **Messaging**: Streaming placeholder card sent immediately on message receipt — eliminates perceived silence during worker processing, displays "⏳ 正在思考..." with in-place updates as content arrives.
- **Messaging/Feishu**: Tool activity strip in streaming cards — real-time emoji + short text display of tool calls with 2-entry scrolling window and CJK-aware visual truncation.
- **Messaging**: Shared `toolfmt` package for cross-platform tool formatting — `FormatCall`, `FormatResult`, `ShortenPath`, `TruncateRunes` with consistent UX across Feishu and Slack.
- **WebChat UI**: ErrorBoundary component to catch assistant-ui MessageRepository crash (#2380) with recovery UI.
- **WebChat UI**: Structured JSON logger replacing 15 console calls, connection state indicator (connected/connecting/disconnected) in thread footer.

### Changed

- **Messaging/Feishu**: Rich placeholder intros with randomized CLI tips covering slash commands, gateway lifecycle, service management, and multi-platform capabilities.
- **WebChat UI**: Unified `HotPlexMessage<P>` generic type, version synced from v1.1.0 to v1.8.1, all 10 `as-any` casts eliminated with typed accessors.
- **Messaging/Slack**: Removed per-tool formatter functions and unused `aepEventToStatus` helper, now uses shared `toolfmt` package.

### Fixed

- **Gateway Core**: WritePump panic recovery data race on `Conn.closed` — recovery defer now uses `c.Close()` for locking and idempotency. (#301)
- **Gateway Core**: Bridge.accum map grew without bound — entries now deleted in `cleanupCrashedWorker` on all worker exit paths. (#298)
- **Gateway Core**: Silent kill on port conflict — `ensureNotRunning()` check fails with clear message instead of silent crash.
- **Messaging/Feishu**: Streaming card lifecycle regression fixes — nil streamCtrl guard, flush loop lazy start via `sync.Once`, `PhaseCreationFailed` handling, concurrent card prevention, turn number preservation after TTL rotation, false-positive rate-limit warning on close.
- **Messaging/Slack**: `flushBuffer` rate-limit reordering — content extraction moved inside lock to prevent message reordering. (#299)
- **Messaging/Slack**: NativeStreamingWriter TOCTOU race, broken byte accounting from chunk prefixes, and fallback content loss. (#318)
- **WebChat UI**: Two-layer message dedup (exact ID + content-signature) and messageStart adoption of pending streaming messages for assistant-ui lifecycle consistency.
- **Admin API**: DB probe errors now logged and conditionally included in health response. (#292)
- **Client SDK**: Init rejection errors (e.g., auth failure) now propagated instead of silently ignored.

## [1.8.1] - 2026-05-09

### Summary

v1.8.1 是一次 patch 版本更新，聚焦于 **配置层 DRY 重构** 和 **STT/TTS 稳定性修复**。提取 `MessagingPlatformConfig` 共享结构体并引入消息层共享默认值（`FillFrom` 传播模式），将 Slack/Feishu 的 STT/TTS/WorkerType 配置统一声明一次。同时修复 STT 持久进程死锁、MOSS TTS idle monitor 死锁、ONNX 模型补丁过期、以及飞书卡片首次 turn 标题分支问题。新增 git 分支检测工具和 doctor TTS 诊断。

### Changed

- **Configuration**: Extract `MessagingPlatformConfig` embedded struct to deduplicate 9 shared fields between SlackConfig and FeishuConfig. (#316)
- **Configuration**: Introduce messaging-level shared defaults (WorkerType, STT, TTS) with `FillFrom()` zero-value propagation. Three-level priority: platform > messaging > Default().
- **Configuration**: Rename `TTSConfig.Enabled` → `TTSEnabled` to resolve Go struct embedding field shadowing.
- **Configuration**: Demote `dm_policy`/`group_policy` to platform-level only — no longer propagated from messaging level.

### Fixed

- **STT**: Resolve `PersistentSTT` deadlocks in `Transcribe`, `idleMonitor`, and `Close` — mutex contention caused goroutine hang on concurrent transcription requests.
- **TTS**: Resolve `MossProcess` `idleMonitor` deadlock and add active request check to prevent premature subprocess shutdown.
- **STT**: Fix ONNX model patch staleness — `fix_onnx_model.py` now patches the model in-place with SHA-256 verification, and `stt_server.py` validates patch before loading.
- **Messaging/Feishu**: Fix card header branch logic on first turn — branch name was incorrectly resolved when git context was not yet available.
- **Configuration**: Quote `STT_LOCAL_CMD` in `env.example` — unquoted values with spaces trigger `VAR=value command` shell semantics when `dev.sh` sources `.env`, causing the STT server to start eagerly on every `make dev`/`dev-stop`/`status` invocation.

### Added

- **Messaging**: Git branch detection utility for enriched card headers.

## [1.8.0] - 2026-05-09

### Summary

v1.8.0 是一次 minor 版本更新，聚焦于 **飞书卡片体验升级** 和 **Gateway 安全加固**。飞书适配器新增 CardKit v2 彩色标题栏（生成中/完成/取消/权限/提问/MCP 六色状态）、交互卡片消息解析、以及 enriched card headers（turn/model/branch/workdir 元数据标签）。Gateway Admin API 迁移到独立端口（9999）并加固中间件链。WebChat 移除 localStorage 缓存层，全面依赖服务端 SQLite 持久化。多语言 SDK 客户端统一重构。

### Added

- **Messaging/Feishu**: CardKit v2 header for all card types — color-coded title bars (wathet=generating, blue=done, grey=cancelled, orange=permission, yellow=question, violet=MCP) via unified card builder with streaming state machine. (#311)
- **Messaging/Feishu**: Interactive card message type support — parse `msg_type=interactive` with schema 1.0/2.0, extract text/div/markdown/note/column_set elements and embedded images.
- **Messaging/Feishu**: Enriched card headers with turn/model/branch/workdir metadata tags — progressive tag display during streaming, preserved across card lifecycle.
- **SDK**: Multi-language client SDK refactoring — Go client async API (`SendInputAsync`), Python transport reconnect, TypeScript backoff/errors modules, Java API updates. Fix reasoning content duplication in Worker parser.

### Changed

- **Messaging/Feishu**: Deduplicate bot API calls — merged `fetchBotOpenID` + `resolveBotName` into single `fetchBotInfo` (one API call instead of two).
- **WebChat**: Remove localStorage message cache — server SQLite is the authoritative persistence layer, eliminating cache-sync bugs (~150 LOC removed).
- **Gateway**: Admin API runs on dedicated address (default `localhost:9999`) with its own HTTP server instead of sharing gateway port. (#289)
- **Messaging/TTS**: `SharedSynthesizer` ref-counted sharing across Feishu/Slack adapters to avoid MOSS port conflict.
- **STT**: Enable INT8 quantization in stt_server.py (~400MB vs ~900MB), reduce idle TTL from 1h to 15m.

### Fixed

- **Gateway**: Admin server hardening — apply middleware chain (CORS, panic recovery, rate-limit, IP-whitelist, token auth), add ReadTimeout/WriteTimeout, parallel gateway+admin shutdown. (#289)
- **Gateway**: `serverErr` channel buffer increased 1→2 to prevent silent drop of concurrent goroutine failures; restore `cfg.Admin.Enabled` guard.
- **WebChat**: Ghost message orphan on `sendInput` failure — remove both user+ghost messages instead of only ghost. (#303)
- **WebChat**: Context-usage data silently discarded — inject into last assistant message parts instead of creating standalone message. (#303)
- **WebChat**: Missing permission/question/elicitation interaction handlers and response routing. (#303)
- **Messaging/Feishu**: Data races in streaming card `Close`/`Abort` — capture mutable state under mutex before use outside critical section.
- **Config**: JWT secret validation relaxed from `==32` to `>=32` bytes to fix doctor false-negative for valid non-32-byte secrets.
- **Config**: Warning log deduplication — JWT secret and normalize path warnings now emit once per process via `sync.Map`.
- **Dev**: Banner detection filters Go default log format (`log.Printf`) in addition to slog patterns.

## [1.7.2] - 2026-05-08

### Summary

v1.7.2 是一次 patch 版本更新，聚焦于 **TTS 管线可靠性与安全加固**。Edge TTS 原生实现替代了失效的第三方依赖，MOSS-TTS-Nano sidecar 替代 Kokoro-82M 提供本地 CPU 合成。安全方面修复了 SEC-008 session 劫持防护、跨 5 模块的 panic/data race/FD leak 问题。Worker 修复了 SDK tool_result 解析缺失，Slack 修复了流式表格重复渲染。

### Added

- **Messaging/TTS**: MOSS-TTS-Nano (Fudan/OpenMOSS) sidecar — Python FastAPI HTTP IPC with lazy start, idle timer, PGID isolation. Replaces Kokoro-82M ONNX (net -826 LOC). (#283)
- **Messaging/Feishu**: Card Header specification (v2.0) — header types, streaming state transitions, unified card builder design.

### Changed

- **Messaging/TTS**: Native Edge TTS implementation replaces broken `lib-x/edgetts` dependency — implements Microsoft's WS + token auth protocol directly. (#294, #295)
- **Messaging/TTS**: TTS summary prompt split into system/user levels; speech pipeline enhanced with Markdown sanitization and large number normalization.
- **Messaging/TTS**: Stream rotation TTL increased from 6min to 500s for Feishu and Slack adapters.
- **CLI/Onboard**: Wizard TTS dependency check validates MOSS-TTS-Nano requirements (python3 + model dir). (#283)

### Fixed

- **Security**: SEC-008 session hijack prevention — user ownership verification on WS reconnect; panic/data race/FD leak fixes across 5 modules (auth, JWT, dedup, proc, Slack event loop). (#288, #291)
- **Gateway**: Inbound event persistence now functional — Bridge wired into Handler for all message paths. (#288)
- **Gateway**: `ShouldRetry` data race — config patterns now snapshotted under mutex. (#288)
- **Session**: Store errors logged instead of silently swallowed; max-turns kill force-sets TERMINATED to prevent orphans. (#288)
- **Worker**: SDK `tool_result` payload parsing — tool results previously discarded producing empty events. (#287)
- **Messaging/Slack**: Streaming table duplication — simplified stop flow eliminates `block_mismatch` errors.
- **Messaging/TTS**: MOSS sidecar race conditions — PID cleanup on terminate, context propagation, mutex-protected activeWg. (#302)

### Security

- SEC-008: User ownership check on WebSocket session reconnect prevents session hijack when key collision occurs. (#288)
- Protected env vars (`HOME`, `CLAUDECODE`, `GATEWAY_TOKEN`) filtered from `.env` loading. (#291)

## [1.7.1] - 2026-05-08

### Summary

v1.7.1 是一次 patch 版本更新，聚焦于 **稳定性修复** 和 **TTS 体验优化**。修复了 Gateway 事件持久化缺失、Worker 进程资源泄漏、Admin API 数据竞争等 5 个生产级缺陷。新增 `ChatWithOptions` LLM 参数控制接口精准约束 TTS 摘要输出长度，TTS 默认启用语音回复并优化为 150 字符上限（~37s 音频）。Onboard 向导重构为 go:embed + YAML AST 单一数据源。

### Added

- **Brain**: `ChatWithOptions(ctx, prompt, opts)` interface — `MaxTokens` and `Temperature` (*float64, nil=default) thread through the entire decorator chain (retry → cache → rate limit → circuit → metrics → Brain wrapper). TTS callers use `SummaryChatOpts{MaxTokens: 256, Temperature: 0.3}`. (#282)
- **Messaging/TTS**: Voice replies enabled by default for voice-triggered turns; max_chars lowered to 150 (~37s audio) to fit Feishu's 60s voice cap. Shared summary prompt extracted to `tts/prompt.go`. (#282)
- **CLI/Onboard**: TTS dependency checker — validates ffmpeg and edge-tts availability with cross-platform install hints. (#282)

### Changed

- **CLI/Onboard**: Wizard config generation refactored from 314-line string builder to `go:embed` + YAML AST manipulation — `configs/config.yaml` is now the single source of truth. (#282)
- **Worker**: OCS `Wait()` now blocks with 2s grace window instead of immediate return, preventing silent zombie sessions after crash. Metadata dispatch unified into `base.InputMetadataHandler`. (#285)

### Fixed

- **Gateway**: Inbound events never stored — messaging envelopes constructed without Seq assignment, causing `v_turns` aggregation failure. Also capture interaction responses (permission/question/elicitation) as inbound events. (#275)
- **Worker**: `proc.Manager` resource leak — ctx.Done() path now calls `Kill()` instead of returning early; `cmd.Wait()` protected by `sync.Once` against concurrent calls. (#285, #269)
- **Admin**: Data race in rate limiter and CIDR allowlist (mutex-protected → `atomic.Value`); `HandleStats` returns 503 instead of 200+empty when session list fails. (#285, #272)
- **Skills**: `Locator.Close()` panic on double invocation — channel close protected by `sync.Once`. Goroutine leak on gateway shutdown — `Close()` added to shutdown sequence. (#279, #281)
- **Brain**: `ChatOptions.Temperature` changed from `float64` to `*float64` — supports explicit deterministic output (temp=0) without zero-value ambiguity. Cache key precision fixed (`strconv.FormatFloat` replaces `%.2f`). (#282)

## [1.7.0] - 2026-05-07

### Summary

v1.7.0 是一次 minor 版本更新，聚焦于 **LLM 编排能力** 和 **语音交互管道**。新增 Brain 模块作为 Gateway 内置的 LLM 编排层（支持 OpenAI/Anthropic 双协议，含重试/缓存/限流/熔断装饰器链），实现飞书和 Slack 双平台的 voice-in → voice-out 全链路管道（STT → Brain 摘要 → Edge TTS → Opus 编码 → 音频消息回传）。Agent Config 获得 Windows 命令注入防护和 XML 标签清洗，Brain 模块经过严格清理删除 ~2700 行死代码并修复 4 个并发安全缺陷。

### Added

- **Brain**: LLM orchestration module — lightweight Chat()/ChatStream() interface with OpenAI and Anthropic protocol support, decorator chain (retry → cache → rate limit → circuit breaker → metrics), model routing, and cost estimation. Fail-open design: missing API key = graceful degradation. (#273)
- **Messaging/Feishu**: TTS voice reply pipeline — voice input detected → Brain text summary → Edge TTS synthesis → FFmpeg MP3→Opus conversion → Feishu audio message upload. Configurable voice, max chars, and concurrency limiter. (#273)
- **Messaging/Slack**: TTS voice reply pipeline mirroring Feishu behavior — identical voice-triggered flow with Slack file upload integration. (#273)
- **Configuration**: Onboard wizard enhanced with environment-backed worker commands and contextual tips in generated config and .env files.

### Changed

- **Agent Config**: Windows command injection prevention — switch from inline `--append-system-prompt` to temporary file injection (`--append-system-prompt-file`) to avoid cmd.exe argument mangling.
- **Agent Config**: XML tag sanitization in system prompt builder — unreserved XML tags are HTML-escaped to prevent prompt injection through config files.
- **Agent Config**: META-COGNITION.md enhanced with P0–P4 decision tree, config modification SOP, and self-correction protocol. Moved to B-channel (directives) for higher authority.
- **Brain**: Simplified API key resolution to 3 clear priority levels — dedicated `HOTPLEX_BRAIN_*` env vars → worker config file extraction (`~/.claude/settings.json`) → system env vars (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`). Removed `PROVIDER_TYPE` concept.
- **Brain**: Removed ~2700 lines of dead code — 7 unused components (failover, priority, budget, health, observable, builder, presets), 3 dead interfaces, duplicate LLM client wrappers. Extracted shared helpers, promoted atomic counters, consolidated token estimation.

### Fixed

- **Brain**: 4 critical concurrency issues — ChatStream context cancel causing goroutine leaks, globalBrain unprotected concurrent access, Anthropic streaming goroutine leak on ctx cancellation, CachedClient silently swallowing json.Marshal errors. (#273)
- **Brain**: API key not wired to LLM client constructors — hardcoded empty string bypassed config resolution entirely.
- **Brain**: IntentRouter data race (int64 counters → atomic.Int64) and MetricsCollector nil panic on OTel instrument creation failure.
- **Messaging/TTS**: `voiceTriggered` data race (bool → atomic.Bool), TTS goroutine using platform writer's 30s context instead of independent 60s context, text truncation off-by-3.
- **Messaging/Feishu**: Content() extraction moved before Close() to prevent empty text on TTS pipeline, brain input truncated to 5× maxChars before LLM summary.
- **CLI**: Onboard wizard missing worker commands in generated config.yaml and .env files.

### Security

- **Agent Config**: Windows cmd.exe argument injection mitigated via temporary file-based prompt injection. Unreserved XML tags in agent configs are sanitized at prompt assembly time.

## [1.6.1] - 2026-05-06

### Summary

v1.6.1 是一次 patch 版本更新，聚焦于 **Worker 环境变量安全隔离重构**。将脆弱的 allow-by-default 白名单替换为 deny-by-default 黑名单，修复 Windows 平台因系统变量被过滤导致的 Worker 启动失败。新增 `HOTPLEX_WORKER_` 前缀剥离机制，让用户在 .env 中配置的密钥（如 `HOTPLEX_WORKER_GITHUB_TOKEN`）安全注入 Worker 子进程。清理了 735 行死代码，修复了 `allowed_envs` 配置项的语义反转 bug，并全面同步了文档和规格。

### Added

- **Worker**: `HOTPLEX_WORKER_` prefix stripping — secrets in .env prefixed with `HOTPLEX_WORKER_` are automatically stripped and injected into worker subprocess env (e.g., `HOTPLEX_WORKER_GITHUB_TOKEN` → `GITHUB_TOKEN`). Gateway-internal `HOTPLEX_*` vars are never exposed.
- **Messaging**: `turn_summary_enabled` config toggle to control whether turn summary messages are sent after each completed turn (default: true). (#223)

### Changed

- **Worker**: Environment variable filtering inverted from whitelist to blocklist — all `os.Environ()` vars pass through by default; only explicitly blocked entries are filtered. This fixes Windows compatibility where system vars (SystemRoot, ProgramFiles, etc.) were previously stripped. (307ae85)
- **Worker**: Blocklist entries ending with `_` are treated as prefix matches (e.g., `HOTPLEX_` blocks all `HOTPLEX_*` vars).
- **Worker**: `BuildEnv` optimized — single `os.Environ()` snapshot instead of 3 calls; Phase 7 reuses `setOrAppend` helper.
- **Messaging**: TurnSummary rendering deduplicated — extracted `Fields()` method consolidating field selection across Slack/Feishu paths. (#223)
- **Security**: Removed dead code — `SafeEnvBuilder`, `BuildWorkerEnv`, `IsSensitive`, `env_builder.go` (~735 lines). `base.BuildEnv` is now the sole env construction path.
- **Configuration**: Removed broken `allowed_envs` field (its entries were incorrectly merged into blocklist, blocking vars instead of allowing them). Use `env_blocklist` or `HOTPLEX_WORKER_` prefix instead.
- **Docs**: All documentation synchronized from whitelist to blocklist terminology — Env-Whitelist-Strategy rewritten as Blocklist Strategy, spec docs (SEC-030~036), config reference, and README updated.

## [1.6.0] - 2026-05-06

### Summary

v1.6.0 是一次 minor 版本更新，聚焦于 **Onboard 体验重构** 和 **Gateway 可靠性加固**。Onboard Wizard 经历全面改造：新增步骤注册表支持增量重跑、凭据校验、binary PATH 安装，并修复了安全默认值（localhost 绑定）和测试二进制覆盖 PATH 的严重 bug。Gateway Core 获得多层 panic recovery 和 FD leak 修复。Slack 适配器新增流式轮转消除 Stream expired 降级。Worker 新增 permission_prompt 可配置和 interaction chain 三个 P0 修复。

### Added

- **CLI**: Onboard wizard step registry with three run modes — full config, keep existing, and incremental step selection. (#203)
- **CLI**: Binary install step — auto-installs hotplex to PATH after onboard (SHA256 verification, cross-platform rc file config).
- **CLI**: Credential validation for Slack (xoxb-/xapp-) and Feishu (cli_) tokens during onboard input.
- **CLI**: Agent config template deployment during onboard with 3-level fallback documentation.
- **Worker**: `permission_prompt` config option under `worker.claude_code` to control `--permission-prompt-tool stdio`. (#201)
- **Worker**: `permission_auto_approve` list for silent tool approval (default: ExitPlanMode). (#201)
- **Messaging**: Slack stream rotation — proactive 6min wallclock rotation aligned with Feishu pattern, eliminating Stream expired fallback. (#211)

### Changed

- **Messaging**: Interaction chain hardening — close active stream writer before sending interaction events (Slack/Feishu), strip nested backticks from args preview, fallback candidate matching for lost request IDs. (#201)
- **Messaging**: Feishu permission text matching expanded vocabulary (added "同意", "ok", "好的", "确认", "取消" etc.).
- **Messaging**: Turn summary refactored — extracted clampContextPct/tokenPair helpers, renamed Timer field.
- **Gateway Core**: WritePump, Hub.Run, and bridge w.Wait goroutine all now have panic recovery with graceful cleanup. (#221)
- **Gateway Core**: handleInput returns error on worker failure instead of silent nil. (#221)
- **Worker**: Pi-mono adapter removed — was a stub with no real implementation, cleaned from 27 files.

### Fixed

- **CLI**: Onboard generated `gateway.addr: ":8888"` binding to 0.0.0.0 — now correctly uses `localhost:8888`. (#203)
- **CLI**: Onboard generated `db.busy_timeout: 500ms` — now matches code default of `5s`.
- **CLI**: Onboard `stepBinaryInstall` copied test binary to PATH during `go test` — added `.test` suffix guard.
- **CLI**: Onboard hallucination audit — corrected Feishu Open ID lookup instructions, hot reload claims, and STT URL.
- **Session**: Data race in `ListActive` — missing RLock on SessionInfo field reads. (#220)
- **Session**: Silent error swallow in `TransitionWithInput` max-turns cleanup and `scanSession` unmarshal. (#220)
- **Worker**: Panic in `readOutput` goroutine without recovery (claudecode adapter). (#220)
- **Worker**: FD leak — stdin/stdout/stderr file descriptors not closed after process exit. (#220)
- **Event Store**: Nil map panic in `Collector.Capture` after `Close()`. (#220)

## [1.5.4] - 2026-05-06

### Summary

v1.5.4 是一次 patch 版本更新，聚焦于 **交互系统安全加固与数据完整性**。修复交互管道两级数据丢失 bug（Question/Elicitation 字段丢失 + OwnerID 未持久化）形成的连锁失败链，飞书/Slack 双平台增加 OwnerID 验证防止群聊未授权操作。Worker stdin 写入统一为单 mutex 序列化防止 NDJSON 交错。同时修复 OCS 响应安全限制、turn summary 显示问题、config audit 内存增长等多项 bug。

### Added

- **Messaging/Slack**: Text-based interaction fallback — 当 Block Kit 按钮不可用时，用户可通过文本（"allow/deny/accept/decline \<requestID\>"）响应交互。(#197)
- **Messaging/Feishu**: Card 发送失败自动回退纯文本 — 与 Slack invalid_blocks fallback 对齐。(#197)

### Changed

- **Config**: STT 模式自动推断 — 移除 `stt_local_mode` 配置项，由 `stt_local_cmd` 中是否包含 `{file}` 占位符自动判断 ephemeral/persistent 模式。(#186)
- **Messaging**: Turn Summary 共享 helper 提取 — `FormatDurationParts`/`FormatTokenUsage` 统一至 turn_summary.go，消除飞书/Slack 重复格式化代码。(#194)
- **Messaging/Feishu**: Turn Summary 改用专用卡片格式，cooldown 检查使用 atomic CAS 消除 TOCTOU 竞态。(#194)
- **Client/SDK**: `decodeAs` 迁移至 `events.DecodeAs[T]` — 支持泛型类型透传，删除冗余测试。(#178)

### Fixed

- **Messaging**: 交互管道数据丢失 — `ExtractQuestionData`/`ExtractElicitationData` 在 `map[string]any` 分支丢失 `questions` 字段，改用 `events.DecodeAs[T]` 修复。(#195)
- **Session**: OwnerID 未持久化 — `upsert_session` SQL ON CONFLICT 不更新 `owner_id`，导致 session resume 后交互响应静默失败。(#196)
- **Gateway Core**: OwnerID 未注入 Worker 信封 — worker 发出的 permission/question/elicitation envelope 缺少 OwnerID，交互响应无法路由。(#191)
- **Gateway Core**: `handleCD` 路径提取错误 — `map[string]any` 分支读取 `d["path"]` 而非 `d["details"]["path"]`。(#197)
- **Gateway Core**: TruncatePath 显示误导 — 含隐藏目录的深路径截断显示 `/.hotplex/workspace` 而非 `~/.hotplex/workspace`，增加 home 目录替换。(#185)
- **Gateway Core**: Detached HEAD 显示空分支 — 改为显示短 commit hash。(#194)
- **Worker/OCS**: 4 处未限制的错误响应读取 — 添加 `io.LimitReader(resp.Body, 4096)` 防止 OOM，URL path segment 使用 `url.PathEscape` 防止注入。(#145)
- **Worker/ClaudeCode**: Stdin 写入交错 — `Conn.Send`/`SendUserMessage` 改为持锁写入，`ControlHandler` 共享同一 mutex 统一序列化。(#197)
- **Messaging**: CancelAll 后 timeout goroutine 复活 — 新增 `cancelCh` 机制，`CancelAll` 关闭 channel 停止 `watchTimeout`。(#197)
- **CLI**: `loadEnvFile` 重复写入 + 受保护变量过滤 — `.env` 重复 key 消除，跳过 HOME/PATH 等受保护变量。(#150)
- **Config**: Audit log 无限增长 — 添加 `maxAuditLen(256)` 上限，static callbacks 合并为单 goroutine 触发。(#146)

### Security

- **Messaging**: 飞书/Slack 交互响应 OwnerID 验证 — 飞书 `checkPendingInteraction` 增加 userID 参数，Slack `handleInteractionEvent` 改为 Get→验证→Complete 防止 TOCTOU 竞态。(#197)
- **Session**: Admin 所有权绕过增加审计日志 — 记录 admin_user_id、session_id、session_owner。(#143)

## [1.5.3] - 2026-05-05

### Summary

v1.5.3 是一次 patch 版本更新，聚焦于 **消息通道稳定性** 和 **EventStore 架构简化**。修复 Slack 流式输出死锁、消息静默丢弃、DM 线程丢失三个高频问题。EventStore 合并至共享数据库消除双文件运维负担，Turn Summary 统一为 Slack/飞书共享的多行卡片布局。同时修复安全路径穿越、Worker 控制请求映射、per-turn token 统计等多项 bug。

### Added

- **Gateway Core**: `GatewayEventsNoSubscribersDropped` Prometheus counter — 追踪因无订阅连接而静默丢弃的事件，提升消息丢失可观测性。(#182)
- **STT**: Auto-patch ONNX models — 首次加载时自动修补模型文件，添加 `.patched` marker 避免重复修补。(#182)
- **STT**: Audio STT failure fallback — STT 失败时下载音频到磁盘并提示 Worker 使用 `stt_once.py` 转录，防止音频内容静默丢失。(#182)

### Changed

- **EventStore**: Events 表合并至 session 共享数据库 — 新增 goose migration 002/003，`source` 列追踪事件来源（normal/crash/timeout/fresh_start），`v_turns` SQL VIEW 替代 ConversationStore。(#171)
- **Messaging**: Turn Summary 统一为 Slack/飞书共享的多行卡片布局 — 导出 `FormatDuration`/`FormatToolNames` 共享 helper，消除 47 行 Slack 重复代码。(#181)
- **Gateway Core**: Bridge 拆分 `captureEvent`/`CaptureInbound` 使用 `json.RawMessage` 减少重复序列化，`events.DecodeAs[T]` 泛型 helper 替代 6 处 map→struct JSON round-trip。(#176)
- **Slack**: Streaming start 使用首个 Write payload 作为初始内容，移除 "Thinking..." 占位符。(#182)

### Fixed

- **Slack**: `closeStreamWriter` 不可重入死锁 — `sync.Mutex` 持锁调用 `Close()` 触发 `onComplete` 回调再次获锁，导致所有后续 turn 永久阻塞。(#182)
- **Slack**: `flushBuffer` 在 `w.closed=true` 后跳过最终 flush — 移除 closed 检查，确保退出时缓冲内容完整发送。(#182)
- **Slack**: `appendWithRetry` 无限阻塞 — 添加 per-call 10s 超时防止 Slack API 无响应时 Close() 永久挂起。(#182)
- **Slack**: DM 流式输出失败 — `StartStreamContext` 缺少 `thread_ts` 和 `team_id` 导致 DM 消息 API 报错。(#174)
- **Slack**: Fallback PostMessage 缺少 `thread_ts` — 非流式回退消息发到 DM 顶层而非线程内。(#182)
- **Gateway Core**: `JoinPlatformSession` 死连接未检测 — `pcEntry` writeLoop 退出后事件仍被静默丢弃，新增 `select on done` 检测并替换。(#182)
- **Gateway Core**: Worker cold-start 计入 Turn 1 时长 — `turnStartTime` 改为 firstEvent 时重置，排除 worker 启动开销。(#176)
- **Gateway Core**: Per-turn token delta 与累计总量相同 — `resetPerTurn()` 被标记为死代码未调用，Done 后补充调用修复。(#176)
- **Gateway Core**: `pendingError` 双重 JSON 编码 — captureEvent 内部已 Marshal，调用方重复 Marshal 导致数据损坏。(#176)
- **Worker/ClaudeCode**: `HasSessionFiles` 空目录误判 — 空 `session-env/<id>` 目录触发无效 resume 浪费 ~14s。(#173)
- **Worker**: `control_request` 字段映射错误 — SDK 使用 "request" 非 "response"，导致 `can_use_tool` 阻塞无限等待。(#170)
- **Security**: Slack/飞书媒体处理路径穿越 — `filepath.Clean` 前缀检查和 `filepath.Base` 文件名清洗。(#167)
- **WebChat**: `message.parts` 为 null 时 filter/find 崩溃 — 新增 null guard 防止反序列化异常。(#177)

## [1.5.2] - 2026-05-04

### Summary

v1.5.2 是一次 patch 版本更新，聚焦于 **安全加固** 和 **交互授权链修复**。修复 REST API 所有权绕过、JWT 弱密钥接受、Admin IP 欺骗和时序侧信道攻击四个安全漏洞。交互授权链全路径 5 个断裂点全部修复，Slack/飞书/WebChat 用户现在可正常响应 Permission/Question/Elicitation 请求。同时新增 Slack delete-file CLI 命令和 Block Kit 表格渲染，WebChat 和 EventStore 获得性能优化。

### Added

- **Messaging/Slack**: `hotplex slack delete-file --file-id <F_FILE_ID>` 命令 — 调用 Slack files.delete API，支持 AI agent 清理过时上传文件。(#164)
- **Messaging/Slack**: Markdown 表格渲染为 Block Kit TableBlock — 流式输出结束后自动升级为表格+Markdown 混合布局，header-only/列不匹配等边界安全降级为代码块。(#148)

### Changed

- **Gateway Core**: bridge.go 分解为 4 个职责文件（bridge/bridge_worker/bridge_forward/bridge_retry），SessionManager 拆分为 5 个子接口（ISP），提升可读性和可测试性。(#153)
- **WebChat**: delta rAF 批处理、React.memo 非流式消息跳过重渲染、highlight.js 懒加载（~200KB → 按需 8 语言）。(#141)
- **EventStore**: delta 优化 — 捕获 seq 后的快速路径、三触发器 flush（大小/计时器/事件结束）、LLM 重试时丢弃过期 accumulator。(#153)

### Fixed

- **Gateway**: 交互授权链全路径修复 — 添加 `--permission-prompt-tool stdio` 到 Claude Code Worker 参数、修复 OwnerID 空值、替换静默错误丢弃为结构化日志、handleInput metadata 路由短路、WebChat 新增 Question/Elicitation 事件类型。(#160)
- **Gateway**: SIGTERM（exit code 143）误报为用户可见错误 — 正常退出/bridge 发起的终止改为 debug 日志。(#160)
- **Messaging/Slack**: Markdown 表格在 header-only 或列不匹配时触发 OOB panic。(#148)
- **Messaging/Slack**: joinSegments 末尾多余空白 — 未修剪换行符导致消息间距异常。

### Security

- **API**: GetSession/DeleteSession 端点缺失所有权检查 — 任何已认证用户可访问/删除他人 session。ListSessions limit 上限 500 防止资源滥用。(#155)
- **Security**: JWT secret 弱密钥检查 — 拒绝非 32 字节密钥，config audit 日志脱敏 api_keys/jwt_secret 字段。(#156)
- **Admin**: X-Forwarded-For 无条件信任导致 IP 白名单绕过 — admin token 验证改用常量时间比较消除时序侧信道。(#149)

## [1.5.1] - 2026-05-04

### Summary

v1.5.1 是一次 patch 版本更新，修复 v1.5.0 引入的 **agent 配置注入丢失** 和 **.env 路径展开失败** 两个关键问题。Bridge 闭包捕获导致 agent config SystemPrompt 在 worker Start/Resume 时被静默丢弃；Viper env override 绕过路径规范化导致 `~` 前缀路径无法解析。同时修复了 dev.sh 与 gateway PID 文件 JSON 格式不兼容导致的 `make dev` 启动失败。

### Fixed

- **Gateway**: Agent config SystemPrompt 在 worker Start/Resume 时被静默丢弃 — `workerStartFunc` 闭包捕获了 injection 前的 `workerInfo` 副本，注入的 SystemPrompt 未传递到 worker 进程。(#139)
- **Configuration**: `.env` 中 `~` 前缀路径（如 `~/.hotplex/...`）未展开 — Viper env override 在路径规范化之后写入原始值，导致 agent config、STT 等目录解析失败。(#138)
- **Configuration**: Worker 和 STT 环境变量通过 `.env` 设置时被静默忽略 — 缺少 `BindEnv` 调用导致 `claude_code.command`、`opencode_server.*` 和 `messaging.*.stt_*` 字段无法从环境变量读取。(#138)
- **Agent Config**: Agent config 启用但无对应文件时无告警 — 新增 `HasGlobalFiles()` 检测和 `agentConfigGlobalFilesChecker` 诊断检查。(#138)
- **Dev Tooling**: `make dev` 启动失败 — gateway PID 文件升级为 JSON 格式后 dev.sh `cat` 读取失败，误判进程已死。新增 `read_pid()` 兼容两种格式。

## [1.5.0] - 2026-05-03

### Summary

v1.5.0 是一次 minor 版本更新，聚焦于 **Slack 运维自服务、Agent 配置灵活性、反馈连续性诊断、Session 生命周期可靠性**。新增 `hotplex slack` CLI 子命令组（10 个命令），Agent 配置升级为 per-bot 3-level 目录 fallback（BREAKING：session ID 变化），hotplex-diagnostics skill 引入反馈链路时间线交叉验证检测静默中断。Gateway 修复了 resume 盲重试导致的 5 秒延迟（P0），飞书流式卡片解耦 Write/Flush 消除速率限制丢帧。

### Added

- **CLI**: `hotplex slack` 子命令组 — 10 个命令（send-message, update-message, schedule-message, upload-file, download-file, list-channels, search, canvas, bookmark, react），支持 env var 自动解析 channel/thread 上下文。Gateway 通过 bridge 注入 `HOTPLEX_SLACK_CHANNEL_ID`/`HOTPLEX_SLACK_THREAD_TS` 到 Worker 环境。(#137, #131)
- **Configuration**: Per-bot 3-level 目录 fallback — Agent 配置按文件独立解析 `global → platform/<platform>/ → <platform>/<botID>/`，替代旧的后缀追加机制（`SOUL.slack.md`）。BotID 贯穿 Slack/飞书全链路并纳入 session key 派生。新增 `agentConfigSuffixChecker` 检测废弃后缀文件。(#129)
- **Skills**: hotplex-diagnostics 运行时诊断 skill — 7 步方法论（进程 → Session → 反馈连续性 → 日志 → 适配器 → 源码 → Issue），FEEDBACK_STALL 分类（PIPELINE_STALL/BACKPRESSURE_DROP/ADAPTER_FAILURE/CLIENT_DISCONNECT），时间线交叉验证检测静默中断。
- **Gateway**: 精确 context 用量 — `get_context_usage` control channel 查询替代聚合 Done 事件统计，消除跨 turn 累积导致的 context fill 虚高。Turn summary 工具名显示限制为 top 5（"+N" 提示）。(#132)
- **CLI**: Gateway 子命令分解 — `runGateway()` 拆分为 initLogging/initOrphanCleanup/initStores/shutdownGateway，PID 文件升级为 JSON 格式存储 config path 和 dev mode。admin_adapters.go 从 routes.go 提取。
- **WebChat**: TurnSummaryCard 组件 — 前端渲染 per-turn session 数据（模型、context fill、时长、工具调用）。

### Changed

- **Gateway**: Worker 清理事件改为 Error 替代合成 Done — 语义正确，相同的 UI 清理（清除指示器、关闭流式卡片）但不触发 turn summary。提取 `sendError` 辅助函数替换 7 处内联 Error 信封创建。新增 `ErrCodeTurnTimeout` 常量。
- **Agent Config**: Context fill 计算修正 — Claude Code SDK `usage.input_tokens` 已含 cached tokens（billing breakdown 非叠加），移除重复计数消除 ContextFill 超过 ContextWindow 的不可能值。
- **Configuration**: `Bridge.adapter` 改用 `atomic.Value` 保证线程安全；`SetAdapter` 在 platform mismatch 时返回 error 而非仅日志 Warn。
- **CI**: GitHub Actions 升级至 Node.js 24 兼容版本 — actions/cache v5, upload/download-artifact v7。

### Fixed

- **Gateway**: Session 生命周期修复 — resume 盲重试消除（P0: 新增 `SessionFileChecker` 接口，bridge 恢复前检查 session 文件存在性，zombie GC 删除文件时自动降级为全新启动）；GC race 修复（handleGC 原子重读 session state 避免与 cleanupCrashedWorker 竞争）；空 session ID 验证。(#135, #133, #134)
- **Messaging/Feishu**: Write/Flush 解耦为后台 150ms timer loop — 防止 CardKit 100ms 速率限制静默丢帧和误报 integrity warning，修复 `bufRunes` 计数器 flush 后未重置。(#128)
- **WebChat**: 消息去重 — adapter 层在传入 assistant-ui ExternalStoreAdapter 前按 ID 去重，修复事件处理器竞争导致的 `MessageRepository same id already exists` 错误。

### Security

- **CLI/Slack**: `loadEnvFile` 防止覆盖已有环境变量；Worker 白名单使用精确条目替代宽泛前缀匹配；`download-file` 使用认证的 `client.GetFileContext` 替代裸 `http.Get`；rune 截断确保 CJK 字符正确处理。

## [1.4.0] - 2026-05-03

### Summary

v1.4.0 是一次 minor 版本更新，聚焦于 **运维自服务能力、Gateway 稳定性与并发安全、SQLite OOM 韧性、Messaging 层架构治理**。新增 CLI 自更新命令（GitHub Releases API + SHA256 校验 + 原子替换）、Turn Summary 紧凑单行摘要（per-turn stats + context fill 百分比）、智能目录切换安全策略、config-driven Worker 环境变量注入、SQLite CGo 双驱动自动降级。Gateway 全面加固并发安全（pcEntry race condition、Session panic recovery、WriteBufferFull 信号化），上下文占用率改用当前 turn token 计算以对齐 Claude Code 行为。Messaging 层提取 BaseAdapter 消除 connPool 重复、统一 `$context` 命令输出。CI 流水线并行化和缓存优化显著缩短构建时间。

### Added

- **CLI**: `hotplex update` 自更新命令 — GitHub Releases API 集成、SHA256 校验验证、原子二进制替换、服务自动重启支持（`--check`、`-y`、`--restart` 标志）。(#115)
- **Messaging**: Turn Summary per-turn stats — Done 事件后生成紧凑单行摘要（`Model · N% · ⏱ Xs · 🔧 N`），从 session accumulator 提取模型、上下文占用率、时长、工具调用数。(#122, #118)
- **Security**: 智能目录切换安全策略 — 支持用户主目录和 `/usr/local` 约定优于配置的智能判断，跨平台路径验证。(#110 相关)
- **Configuration**: Worker 环境变量注入 — `worker.environment` 配置驱动，支持 `${VAR}` 展开和 `env_whitelist` 过滤，默认注入 `BUN_RUNTIME_NV=disable_avx512`。
- **Configuration**: `db.events_path` 可配置 — events.db 路径支持自定义，脱离默认数据目录。
- **SQLite**: CGo 双驱动自动降级 — CGo 构建使用 mattn/go-sqlite3（性能优先），纯 Go 构建使用 modernc.org/sqlite（OOM 韧性），build tag 自动选择。
- **Messaging**: `$context` 命令输出美化 — 共享格式化层（severity 级别、进度条、友好 token 计数、操作建议），Slack/飞书/WebChat 统一输出。WebChat 新增 ContextUsageCard 玻璃态组件。(#110)
- **Messaging**: events.Message 处理 — 飞书和 Slack 适配器新增 handler/bridge 发起的独立消息处理（cd 确认、命令反馈）。
- **Messaging**: 控制命令详细错误消息 — 用户友好的错误提示替代静默失败。

### Changed

- **Messaging**: 提取 BaseAdapter[C] 泛型结构体 — 消除 Slack/飞书适配器约 30 行 connPool 重复代码，提供 `InitConnPool`/`GetOrCreateConn`/`DrainConns`/`DeleteConn` 统一生命周期管理。(#114)
- **Gateway**: 模块拆分 — 从 hub.go 提取 SeqGen 和 pcEntry 到独立文件，提取 `createAndLaunchWorker`、`requireActiveOwner` 辅助函数，Handler 改用 SessionManager 接口替代具体类型。
- **Gateway**: 上下文占用率改用 `ContextFill`（当前 turn input tokens）替代累计 TotalInput 计算 — 对齐 Claude Code per-turn 上下文跟踪语义，新增 `context_fill` 字段到 session stats snapshot。
- **Gateway**: 移除 TurnIdleTimeout idle detection — 合成 Done 机制不可靠，execution_timeout 30m 仍是僵尸会话安全网。
- **Gateway**: 消除 `toInt64`/`toFloat64`/`formatTokenCount`/`extractSessionStats` 重复实现 — 替换为 events 包 `ToInt64`/`ToFloat64` 等价函数，移除死代码。
- **Session**: 提取 `audit_store.go`（审计追踪方法）、`stores.go`（store 工厂）、`updateSession` 辅助函数，store.go 减少 182 行。
- **SQLite**: 提取 `sqlutil` 包统一 DB 初始化和 PRAGMA 调优，消除冗余零值回退。
- **Security**: 提取 `resolveSigningKey` 辅助函数，移除无用的 `DevAllowedTools`。
- **Admin**: 提取 CORS 处理到中间件。
- **Metrics**: 移除 `session_id` label 防止 delta 指标基数无限增长。
- **CI**: 流水线优化 — 移除 Gate 阶段、步骤并行化、Go/WebChat 缓存。
- **Messaging**: 关闭 turn_timeout 默认值（原 15m 过于激进，execution_timeout 30m 已足够捕获僵尸会话）。
- **Service**: systemd WorkingDirectory 对齐 worker.default_work_dir 而非硬编码 `$HOME`。(#109)

### Fixed

- **Service**: ExecStart 路径解析 — `ResolveBinaryPath()` 优先使用 PATH 查找（`exec.LookPath`），修复 build 目录路径写入 systemd unit 的问题。(#113)
- **Configuration**: `${VAR}` 环境变量展开 — ExpandEnv 已定义但未在加载路径中调用，worker.environment 值注入为字面量而非展开值。(#111)
- **Configuration**: Workdir fallback 链统一 — messaging 适配器路径正确展开 `~` 为用户主目录。
- **Gateway**: pcEntry WriteCtx/Close race condition — 并发写入和关闭导致 panic，新增错误分类辅助函数。关闭 data channel 解除 writeLoop 阻塞。
- **Session**: 并发安全加固 — UpdateWorkDir/ClearContext 添加 RLock 保护早期返回字段读取；回调函数添加 panic recovery；级联删除包裹在事务中；WriteBuffer 满时返回 `ErrWriteBufferFull` 替代静默丢弃。
- **Worker**: RLIMIT_AS 自限 bug 修复 — 网关进程被自身内存限制崩溃；禁用 RLIMIT_AS 修复 Bun 运行时崩溃。(#112 相关)
- **Worker**: OCS SSE 超时和启动问题修复；releaseOnce 行为测试覆盖。(#112 相关)
- **Messaging**: watchTimeout panic recovery 和 idleMonitor context race 修复。
- **Messaging/Feishu**: ChatQueue `wg.Add` 移入 mutex lock 内防止 race；非用户流式卡片清理路径用 Close() 替代 Abort()；控制命令错误静默失败修复。
- **Messaging/Feishu**: `sendTurnSummary` 改用 fresh context 并在活跃流式卡片中追加摘要，避免重复消息和 stale context 问题。
- **Agent Config**: `readFile` 区分 IsNotExist 和其他错误类型。
- **CLI**: wizard stepAgentConfig 写入和关闭错误处理。

### Security

- **Configuration**: GH_TOKEN/GITHUB_TOKEN 重命名为 HOTPLEX_WORKER_GH_TOKEN/HOTPLEX_WORKER_GITHUB_TOKEN 前缀，防止 shell 环境污染影响 `gh` CLI keyring 认证。(#111)

## [1.3.0] - 2026-04-30

### Summary

v1.3.0 是一次 minor 版本更新，聚焦于 **跨平台 Windows 支持、Gateway 安全默认值与生命周期管理、WebChat 嵌入式 SPA + AI Native UX、Messaging 适配器 DRY/SOLID 重构、Session History REST API、DI 构造函数注入**。新增 Windows amd64/arm64 一等公民支持（Job Object 进程树管理、纯 Go SQLite、跨平台路径安全验证）。Gateway 全面加固安全基线（localhost-only 默认绑定）、新增系统服务管理（systemd/launchd/SCM）和守护进程模式。WebChat 完成嵌入式 SPA 部署（`go:embed`）和 AI Native UX 升级（ghost assistant、AgentTool/TodoTool、glassmorphism 设计）。Messaging 层完成 issue #65 七阶段重构，提取共享 Pipeline/ConnPool/Streaming/Dedup/Backoff/Gate 到基础包。DI 层从 setter 链迁移到构造函数注入。

### Added

- **Platform**: Windows amd64/arm64 一等公民支持 — Job Object 进程树清理、纯 Go SQLite (modernc.org)、跨平台 TTY 检测、路径安全验证、pipe 错误检测。(#64, 794a8ff, de8b097, 1e75218, 6229be9)
- **Gateway**: 安全默认值加固 — localhost-only 默认绑定、daemon 模式 (`-d`)、统一进程生命周期管理 (stop/restart/status/service)。(#62, 3204cfc)
- **Gateway**: 系统服务管理 — install/uninstall/start/stop/restart 支持 systemd/launchd/Windows SCM，user/system 级别。(#62, 3204cfc)
- **Gateway**: `GET /api/sessions/{id}/history` — 会话历史 REST 端点，支持 cursor 分页查询 ConversationStore。(#60, d17b90d, 440525e)
- **Gateway**: `GetBySessionBefore` — ConversationStore 新增基于序列号的分页查询，用于历史回放。(#60, 440525e)
- **WebChat**: 嵌入式 SPA — Next.js 静态导出通过 `go:embed` 嵌入 gateway 二进制，SPA fallback 路由，零外部依赖部署。(#62, 5d0a414)
- **WebChat**: Ghost assistant — 后端静默期间显示 skeleton 占位符，消除"黑洞效应"。(#62, 0ce448c, 171963f)
- **WebChat**: AI Native UX — thinking indicator 重设计、glassmorphism copy button、inline message actions、AgentTool 和 TodoTool 结构化任务渲染。(#62, 08fa8ed, cf6a2b6, 3003c0b, 2f49dfa, aa727b6)
- **WebChat**: History runtime adapter — L1 (React state) + L2 (LocalStorage) 双层缓存，cursor 分页历史加载。(#60, a5bbb72, 686e1fa, ed7aecb)
- **Skills**: Skills 发现整合到独立 `skills` 包 — 可配置 TTL 缓存 + CWD 目录扫描。(#67, f1538fe)
- **CI**: OpenCode + OMO 离线 bundle — GitHub Release 自动打包和安装脚本，支持离线环境部署。(#59, ffaf168, 3412b22)
- **Infrastructure**: `make hooks` 目标自动安装 git hooks，`make quickstart` 适配非开发者用户。(#62, c61b03e, ca5b698)

### Changed

- **Messaging**: 七阶段 DRY/SOLID 重构 (issue #65) — 提取共享 Pipeline、ConnPool、StreamingCard 抽象、Dedup/Backoff/Gate 到 `internal/messaging/` 基础包；Feishu/Slack 适配器代码量大幅缩减。(#67, 02f239b, 1be2d0f, c0892b4, 37751dc, e8f0ae3, 24cf4eb)
- **DI**: 构造函数注入替代 setter 链 — Handler/Bridge/Adapter 改用 `Deps` struct 注入，编译期保证完整性。(#61, f906d30)
- **Logging**: 结构化日志优化 — gateway 栈统一 slog key-value 风格 (snake_case)，注入 channel 标识到所有组件 logger，14 个可恢复错误从 Error 降级为 Warn。(#67, baf4546)
- **Security**: TOCTOU 修复 + SSRF DNS mock — ExpandAndAbs 解析符号链接防止 workdir 竞态，6 个 Manager 方法 copy-then-write 模式消除 lock-DB-I/O。(#67, a290bb3)
- **Security**: 跨平台路径验证重构 — `ValidateWorkDir` 拆分为 `path_unix.go`/`path_windows.go`，Windows 大小写不敏感。(#64, 6229be9, d702753)
- **Session**: 移除 EventStore + PostgresStore，ConversationStore 增加统计查询 — 简化持久化架构。(#67, a7b6eda)
- **Gateway**: PID 文件容错 — restart 遇到 stale PID 时 warning 并继续启动而非失败。(#62, 0165d17)
- **Config**: `~` 展开修复 — 所有 YAML 路径字段正确展开波浪号为 home 目录，防止创建字面 `~` 目录。(#62, dad790c)
- **Hooks**: pre-push gate 增强 — 新增 fmt + lint + go vet + go mod verify + race test，本地拦截 CI 失败。(#62, 05961c1, c61b03e, 6e08233)

### Fixed

- **Messaging**: 飞书消息无响应三类根因 — ResumeSession 输入丢失、Error 事件静默丢弃、长时间无输出无提示。(#68, e96640d)
- **Gateway**: `handler.sm` nil guard — conn.go idle 转换检查添加空指针防护。(#67, 9203060)
- **Slack**: `TestShortenPaths` data race — mutex-guarded `SetWorkDir` 消除竞态条件。(#68, 0e30a30)
- **WebChat**: 多项 UX 修复 — 额外换行、tool 折叠/展开、terminal 状态生命周期、reasoning markdown 渲染、scroll button 定位。(#62, various)
- **WebChat**: 依赖更新和类型兼容性修复 — pin ai SDK 版本，修复 ConversationRecord success 类型。(#68, fab6be6, 632f180)
- **Windows**: 进程管理加固 — 修复跨平台 STT Job Object 清理、PID 文件路径、signal 处理、pipe 错误检测。(#64, 031eab2, f602b6a, de8b097)
- **CI**: large file guard 扫描 HEAD tree only 避免误报；OMO 插件通过 OpenCode auto-discovery 注册。(#59, d4b6818, 1883866)

## [1.2.0] - 2026-04-29

### Summary

v1.2.0 是一次 minor 版本更新，聚焦于 **会话历史持久化、Skills 体验升级和仓库安全加固**。新增 MessageStore（事件级持久化）和 ConversationStore（轮次级持久化）双层架构，为会话回放和审计提供数据基础。Gateway 新增 Skills 发现与列表展示能力（缓存 + DRY 共享模块）。WebChat 完成 UX v5 重构（智能折叠、高级动画、GenUI 工具组件）。安全层面完成 5 层大文件防御体系（gitignore → gitattributes → pre-commit → pre-push → CI guard），并从历史中彻底清除 46MB 误提交二进制。测试覆盖率持续提升至中高风险路径全覆盖。

### Added

- **Session**: MessageStore — event-level persistence with async batch writer, SQLite backend, Postgres stub; `EventStoreEnabled` config flag (default: true). (4f20233)
- **Session**: ConversationStore — turn-level persistence (user input + assistant response with tools, tokens, cost, duration); cascade delete on session removal. (4f20233)
- **Session**: Admin stats endpoint — `GET /admin/sessions/{id}/stats` for aggregated session statistics. (4f20233)
- **Gateway Core**: SkillsLocator — project/user/plugin directory scanning with configurable TTL cache. (c7446b3, ef58fe7)
- **Gateway Core**: Skills listing event dispatch — `/skills` command in WebSocket and messaging platforms with paginated display. (36699d5)
- **Messaging**: Shared skills helpers — DRY consolidation of Feishu/Slack skills list formatting into `messaging/skills_helpers.go`. (2bf2bbd)
- **WebChat**: Hybrid architecture v5 — smart collapsing, advanced animations, and layout optimization. (4fea33b)
- **WebChat**: GenUI tool components — ListTool and TodoTool for enhanced tool rendering. (5164387)
- **WebChat**: Message cache and turn replay utilities for client-side session history. (4f20233)
- **Security**: 5-layer large file defense — `.gitignore` + `.gitattributes` + pre-commit hook + pre-push scan + CI large-file-guard job; blocks all files >1MB from entering the repository. (b410de5, 0b3e866)
- **Security**: `RegisterCommand` validation — path separator and dangerous character checks for worker command whitelist. (e9f6415)
- **Gateway Core**: Configurable Claude Code startup command via `worker.claude_code.command`. (e9f6415)
- **Agent Config**: Built-in metacognition via `go:embed` — agent self-knowledge (architecture, mechanisms) injected as C-channel. (879298d)
- **Docs**: Feishu and Slack integration guides (Chinese + English bilingual). (4f20233)

### Changed

- **Gateway Core**: Skills parsing deduplication — eliminated duplication between skills_locator and gateway handler. (624ca02)
- **Gateway Core**: Control request timeout isolated — prevent global timeout from affecting individual control commands. (d940e91)
- **Messaging**: Feishu streaming card error handling — error events now close streaming card to prevent stale cards after TURN_TIMEOUT. (879298d)
- **Messaging**: Silent message drop prevention — check session state before assuming worker is alive on resume. (879298d)
- **Session**: Reset ExpiresAt on session resume — prevent GC max_lifetime from killing reactivated sessions. (879298d)
- **Infrastructure**: Removed 46MB `hotplex` binary from git history; `.git` size reduced from 52MB to 8MB. (0b3e866)
- **Testing**: Coverage expansion — medium-ROI packages (cli/checkers, skills, worker/claudecode, feishu) now at 67–89%. (e9f6415, 879298d)

### Fixed

- **Gateway Core**: Frontmatter parsing fixed — skills display format unified across platforms. (db98ffa)
- **Gateway Core**: Claude Code skills properly discovered from all skill directories (user, project, plugin). (63c7b64)
- **Gateway Core**: LLM retry false positives — `ShouldRetry` now matches only ErrorData, not turn text containing error-like strings. (879298d)
- **Gateway Core**: Proc double-log on exit — guarded by `exited` flag to prevent duplicate log lines. (879298d)
- **Worker**: OCS compact auto-resolves model from message history; rewind auto-resolves last assistant messageID. (b7569f1)
- **Messaging**: Slack `sendSkillsList` method added for SkillsList event handling. (a6a0b2c)
- **WebChat**: Bot avatar alignment, rendering fixes, and useAuiState migration. (34d6f7c)
- **WebChat**: Default workDir passed when creating sessions. (c7146a4)
- **Security**: 22 non-functional issues resolved from comprehensive audit (errcheck, gocritic, gofmt). (175671e)

## [1.1.2] - 2026-04-26

### Summary

v1.1.2 是一次 patch 版本更新，聚焦于 **会话数据持久化与连接稳定性**。新增 Conversation Store（异步批量写入会话轮次数据）和 Session Stats API（token/延迟/成本统计），为 WebChat 和管理端提供会话级别的洞察。Gateway Core 修复了多个关键稳定性问题（CAS race guard、fast reconnect、session ID 一致性、mapper 事件丢失），并引入 title-based session management 和 startup session repair。Session 层完成 SQLite 性能优化（PRAGMA 调优、级联删除、events TTL、自动 VACUUM）。测试覆盖率从 68% 提升至 84%+。

### Added

- **Gateway Core**: Title-based session management — thread title parameter through bridge, session manager, REST API, and WebSocket init path; deterministic UUIDv5 session IDs from (userID, workerType, title, workDir). (f4761c66)
- **Gateway Core**: `RepairRunningSessions` on startup — stale running sessions transitioned to terminated, preventing ghost sessions after gateway restart. (4fd59ee5)
- **Gateway Core**: `GetSessionsByState` store query + migration 003 backfill for NULL `work_dir` values. (4fd59ee5)
- **Gateway Core**: REST API tests — 15 HTTP handler tests covering CreateSession, DeleteSession, ListSessions, GetSession, SwitchWorkDir endpoints. (8d701565)
- **Gateway Core**: Session manager tests — coverage for RepairRunningSessions, DetachWorkerIf CAS, GetSessionsByState, work_dir round-trip, migration idempotency. (be7eb9e9, 4ac803d8)
- **Messaging**: Feishu streaming card TTL rotation — proactive 6-minute card replacement with async abort and reply_to threading to bypass Feishu's 10-minute server limit. (2bccd702)
- **Session**: Conversation store — async batch writer for turn-level persistence (user input + assistant response with tools, tokens, cost, duration); 3 recording paths (normal done, crash/timeout, fresh start). (ce02d0eb)
- **Gateway Core**: Session stats API — aggregated turn statistics from done events (`GET /api/sessions/{id}/stats`). (ce02d0eb)

### Changed

- **Session**: SQLite storage optimization — PRAGMA tuning (32MB cache, 256MB mmap), cascade delete for events/audit on session deletion, events TTL cleanup (30 days), automatic VACUUM when free pages exceed 20%. (2d569a8b)
- **Gateway Core**: Fast reconnect for idle sessions — skip terminate+resume cycle when worker is still alive, transition directly back to running. (0a71a61b)
- **Gateway Core**: CAS semantics for DetachWorker — prevents old forwardEvents goroutines from clobbering a concurrently replaced worker. (0a71a61b)
- **CLI**: Agent config templates migrated from Go constants to `embed.FS` files, onboard wizard streamlined v3→v4. (9f56623d)
- **Gateway Core**: Code quality pass — extract `IsDeadProcessError` helper, merge accumulator locks, skip tracing spans for high-frequency pings, promote bare strings to constants. (120d2487, e9b05625)

### Fixed

- **Gateway Core**: ClaudeCode mapper silently discarded `EventSystem` and `EventSessionState` — payload type mismatch (`string` vs `json.RawMessage`) caused all state transitions to be dropped. (2bccd702)
- **Gateway Core**: Worker crash recovery — transient `INTERNAL_ERROR` suppressed, `RESUME_RETRY` handled with automatic fresh-start fallback in UI. (0a71a61b)
- **Gateway Core**: Skip LLM retry for empty output from resumed workers and exit code 143 (SIGTERM from connection replacement). (0a71a61b)
- **Messaging**: Feishu streaming card write failure now gracefully falls back to static IM delivery instead of returning error to caller. (2bccd702)
- **WebChat**: Connection stability — deterministic session IDs across REST/WS paths, browser console warnings eliminated, frontend crash guards for undefined message roles. (0a71a61b, 14575983, 4fd59ee5)
- **WebChat**: CommandMenu filter bug — inconsistent `/` prefix variable caused slash commands to not filter correctly. (120d2487)
- **WebChat**: useCopyToClipboard timeout leak on unmount; useSessions panel state stabilized with useCallback. (120d2487)
- **Session**: `errors.Is` for `sql.ErrNoRows` comparison (errorlint compliance). (45ddac6f)
- **E2E**: Flaky state event assertion removed from SendInputReceiveEvents test. (01ffdec7)

## [1.1.1] - 2026-04-26

### Added

- **WebChat**: "Obsidian" dark theme redesign — glassmorphism design system, Outfit + JetBrains Mono typography, framer-motion spring animations across messages, tool cards, and reasoning blocks.
- **WebChat**: GenUI tool rendering — TerminalTool (stdout/stderr split, auto-collapse), FileDiffTool (syntax-aware diff with copy), SearchTool (match highlighting), and PermissionCard (approve/reject MCP events interactively).
- **WebChat**: Slash command palette (`CommandMenu`) with fuzzy search across all commands (`/gc`, `/reset`, `/cd`, `/skills`, `/new`) and worker skills.
- **WebChat**: MetricsBar — live token counts, turn latency, and wall-clock time extracted from AEP `done.stats` events.
- **WebChat**: NewSessionModal with worker type selector, workdir input, recent directories dropdown, and nuqs URL deep linking for one-click session setup.
- **WebChat**: Code block folding, syntax highlighting, and copy-to-clipboard in Markdown rendering.
- **Gateway**: OpenCode Server singleton process model — all sessions share one lazily-started `opencode serve` process with ref counting and 30m idle drain, replacing per-session process spawning.
- **Gateway**: `/cd <path>` in-session directory switching with path validation and security guard; `/skills` command to list available worker skills.
- **Gateway**: Agent config XML injection with B/C channel architecture (`<directives>` for SOUL/AGENTS/SKILLS, `<context>` for USER/MEMORY); platform variants (e.g. `SOUL.slack.md`) auto-appended.
- **Gateway**: Session `work_dir` persistence — working directory stored in SQLite, enabling session stickiness across page reloads and idempotent session re-creation via `DeletePhysical`.
- **CLI**: Onboard wizard auto-generates agent config files (SOUL/AGENTS/SKILLS/USER/MEMORY) during setup.

### Changed

- **Infrastructure**: install.sh rewritten as binary-only installer (851→113 lines); uninstall.sh streamlined (189→102 lines) with `--purge` and PID cleanup.
- **Configuration**: Agent config size limits tightened to 8K/file, 40K total.

### Fixed

- **Gateway**: Nil pointer panic in claudecode worker `Resume()` — race condition where `w.Proc` was nil'd by concurrent `Terminate()` while `Resume()` called `Start()`.
- **Gateway**: Worker crash recovery — transient `INTERNAL_ERROR` suppressed; `RESUME_RETRY` handled gracefully in UI with automatic fresh-start fallback.
- **Gateway**: SQLite session migration silent failure — batch SQL split to per-statement execution, fixing missing `work_dir` column on upgrade.
- **WebChat**: Composer input frozen after slash command interaction — state synchronization restored IME compatibility and keyboard responsiveness.
- **WebChat**: User-facing error messages for terminal states (SESSION_BUSY, TURN_TIMEOUT, INTERNAL_ERROR) replace raw error codes.
- **WebChat**: Minor fixes — CommandMenu visibility, NewSessionModal dropdown overflow, Jump-to-Last button positioning, code block wrapping, turbopack serialization warnings.
