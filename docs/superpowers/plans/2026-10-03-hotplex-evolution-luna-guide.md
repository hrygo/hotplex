---
title: "HotPlex 可信运行与交付进化实施方案（Luna Guide）"
weight: 10
description: "基于 ed588ee 的分阶段实施指导，覆盖 CI 选择、输入确认、持久交付、权威运行计划、隔离、发布与执行控制台。"
date: 2026-10-03
status: proposed
---

# HotPlex 可信运行与交付进化实施方案

交付对象：获得实施授权后的 Luna。本文是实施建议，尚未批准为新产品契约。

依据：用户引用的 ChatGPT 对话《HotPlex 进化路径》（conversation ID `6ac0df59-6afc-83e8-90ec-96d0b6c8fe3e`）、当前仓库源码、既有 Runtime Operations Contract，以及 2026-10-03 查询的 GitHub Issue/PR 状态。

核验基准：

| 项目 | 本轮核验 |
| --- | --- |
| 仓库与分支 | `hrygo/hotplex`，本地 `main` |
| HEAD | `ed588ee000a9526659de6fcf09f72e3ad6cf960b`，与原对话基准一致 |
| 开始时工作区 | `git status --short` 无输出 |
| 图谱 | `Users-hrygo-hotplex`，generation `2026-10-01T23:35:45Z`；26 个相关路径已做 coverage 检查 |
| 图谱局限 | WebChat runtime adapter 第 152 行有 parse gap，已直接读取补证；其他路径无记录缺口，不等于穷尽证明 |
| 历史记忆 | 项目知识页列表、相关搜索均为空；背景取自原对话及仓库设计文档 |
| 本轮执行 | 只读源码/契约/远端状态核验、CI 匹配微型复现、方案落盘 |
| 未执行 | 业务修改、业务回归、真实 PostgreSQL、真实 Worker/消息渠道、平台 smoke、提交、推送、发布 |

原对话报告的 8,207 条 Go pass、27 条 SDK pass、96 个契约场景通过属于原分析的 Linux 历史证据。本轮未取得其原始附件，也未重新执行，不能作为下面任一新实现的验收结果。

## 1. 问题结论

推荐目标是把现有网关做成**输入去向、实际配置、执行结果和最终交付均可解释的运行网关**。保持 Gateway → Session/Execution → Worker 主体分层；优先补齐已有链路，逐步形成执行控制台。

原对话提出的六个发现中，当前源码仍能确认以下边界：

| 编号 | 现状与影响 | 推荐动作 | 优先级 |
| --- | --- | --- | --- |
| E01 | CI 反向依赖匹配包含字面单引号，漏选关联包；目前还只扫描直接 `.Imports` | 先修匹配，再用包图计算包含测试依赖的反向闭包 | P0 |
| E02 | buffered supplement 仅在内存中，却用 `input.ack status=delivered` 确认 | 先分清接受、耐久性与派发，再扩展 canonical execution 为持久排队输入 | P0 → P1 |
| E03 | Cron 重试队列在内存中，且具备 CLI delivery 配置时跳过 Gateway delivery | 首个切片统一 Cron 最终交付 owner，落地 EffectLedger 与提供方回执 | P1 |
| E04 | `ResolvePlan` 已实现，但 WS/REST 只 shadow；公开 hash 不含模型/工具明细 | 保留公开 hash，新增内部启动指纹，逐步由同一解析结果驱动启动 | P1 |
| E05 | `BuildEnv` 继承 host env 后过滤；不能证明 OS 隔离 | 增量增加 strict env 与能力证据报告，按实际 backend 能力 fail closed | P1 |
| E06 | release 与 offline bundle 依赖 build，外部运行时版本动态解析 | 同 SHA 门禁、版本锁、原生产物 smoke、SBOM/签名/provenance | P1 |

后续两项是产品增量：E07 执行时间线与安全恢复；E08 在条件满足后提供两个版本化研发 Recipe。

顺序建议：**E01 → E02 的语义切片 → E03 的持久交付 → E04/E05 → 持久队列 → E07 → 有条件 E08**。E06 的源码门禁可以在 E01 后推进，完整平台发布证明独立交付。单执行者按这个顺序串行即可，无需为方案或实施自动创建子代理。

## 2. 当前实现与根因

### 2.1 E01：测试选择器

文件：`.github/workflows/ci.yml`，`Determine test scope` 与 `Run tests with race detector + coverage`。

- PR 使用 `git diff --name-only origin/main...` 取 `.go` 路径，再拼 import path。
- 反向补选执行 `go list -f ... .Imports ...`，然后 `grep -q "'"$changed"'"`。
- Go 模板输出没有单引号；当前模式要求匹配带单引号的字符串。
- 本轮微型实测：同一个 import path 输入，原匹配退出码 1；`grep -Fqx "$changed"` 对照退出码 0。
- `.Imports` 没有覆盖 `.TestImports`、`.XTestImports`；直接匹配也没有求传递闭包。
- 删除包、根目录 Go 文件、`go.mod/go.sum`、工作流和测试脚本变更均需要明确处理，不能用“未选中”说明无需验证。

当前 `ALL_PKGS` 排除 `internal/worker/proc`、`internal/worker/pi`、`cmd/hotplex`、`e2e`。本方案不猜测其历史原因，也不静默删除排除项；实施时为每项提供独立必跑任务或写明覆盖缺口。特别是 `cmd/hotplex` 的 bootstrap/wiring 必须有门禁。

### 2.2 E02：补充输入

文件与符号：

- `internal/gateway/handler.go`：`handleSupplementOnBusy`、`ackSupplement`、`DeliverReplay`。
- `internal/gateway/pending_buffer.go`：`PendingBuffer`、`BeginSupplement`、`DrainForReplay`、`RequeueIfCurrent`、`Clear`。
- `internal/gateway/bridge.go`：`replayPending`、`CurrentWorkerBinding`、`ClearPending`。
- `webchat/lib/adapters/hotplex-runtime-adapter.ts`：`handleInputAck`。
- `webchat/lib/adapters/follow-up-queue.ts`：客户端 follow-up 队列，不能当作服务端持久队列。

当前已经有补充输入 payload 去重、并发 reservation、容量上限、Skill replay、reset/shutdown lifecycle token，以及 CC/Codex 的 mid-turn fence。不要重复实现或移除这些保护。

直接问题在确认语义：无论 injected 还是 buffered，`ackSupplement` 都输出合成的 `supplement-<client_id>` 与 `delivered`；重复确认不带原 mode。缓冲上限 20、去重记录上限 128 均为进程内状态。`DrainForReplay` 会合并多条输入并以最后一个 envelope 为模板，不能直接移作每条持久 execution 的身份。

普通输入的 `internal/execution` 则有 canonical 唯一键、delivery/runtime 分层、owner lease、single-active gate 和 fence。`execution_inputs` 的 partial unique index 约束 `pending/running` 或 fenced 的单 session 单 active；排队实现必须协调这一约束，不能把等待队列直接写成第二个 `pending`。

### 2.3 E03：Cron 最终交付

文件与符号：

- `internal/cron/executor.go`：`Execute`、`HasCLIDelivery`、`buildDeliverySuffix`。
- `internal/cron/timer.go`：`Scheduler.executeJob`。
- `internal/cron/delivery.go`：`Delivery`、`Deliver`、`deliverResult`、`enqueue`、`flushPending`。
- `cmd/hotplex/gateway_run.go`：Cron extractor/deliverer wiring。
- `internal/messaging/slack/adapter.go`、`internal/messaging/feishu/adapter.go`：`SendCronResult`。

当前链路：

1. Cron 创建独立 session，直接调用 `w.Input`，不是普通 Gateway input accept 路径。
2. prompt 可追加 `hotplex slack send-message` 或 `lark-cli` 发送说明。
3. `executeJob` 在 `HasCLIDelivery(job)` 为 true 时不调用 Gateway `Delivery`。
4. Gateway delivery 返回值只有 `error`，内存 retry queue 重启丢失。
5. Slack 的主动发送丢弃 `PostMessageContext` 返回的 channel/message timestamp；飞书 helper 调用方也丢弃消息响应。
6. extractor 当前取 `QueryTurns(sessionID, 1, 0)` 的末条 content；它没有以 execution ID 精确锁定 assistant 最终输出。

因此不能只替换 retry queue。必须先确定**谁负责最终发送、发送哪一次执行的结果、提供方确认是什么**。`HasCLIDelivery` 只是“目标参数足够”，不是“已发送”的事实。

### 2.4 E04：运行计划

文件与符号：

- `internal/agentspec/resolve.go`：`Input`、`InitMetadata`、`Resolver.Resolve`。
- `internal/agentspec/plan.go`：`ResolvePlan`、`Redacted`、`CanonicalPlanHash`。
- `internal/agentspec/map.go`：`MapToStartParams`、`MapToSessionInfo`。
- `internal/gateway/agentspec.go`：`BuildWebChatInput`、`ShadowResolvePlan`。
- `internal/gateway/conn.go`、`api.go`：WS/REST 入口。
- `internal/gateway/bridge.go`：`startPreparedSession`、`prepareWorkerInfo`、`buildWorkerInfo`。
- `internal/gateway/bridge_worker.go`：`createAndLaunchWorker`、`capturePermissionCeiling`、workspace 配置与权限解析。

`EffectiveRuntimePlanView` 特意省略 command、model、tool lists、budget、路径；`CanonicalPlanHash` 基于这个 view。不同内部配置可拥有相同公开 hash，这是诊断投影的边界，不是哈希算法故障。本轮依据源码确认，未复跑原对话的 Go hash 探针。

`MapToSessionInfo` 尚未接入实际启动，且 mapper 只覆盖拥有的字段。模型执行也有 adapter 差异：CC/OCS 使用 `AllowedModels[0]`，Codex 启动参数使用自身 `cfg.Model`，ACP 支持 session model command。禁止把 `AgentSpec.Worker.Model` 当作已经统一生效。

权限优先级与授权上限必须分开：当前 resolver 的显式 init 值可以优先成为诊断意图，真正 Worker 仍受 session/workspace ceiling 限制。直接切换 mapper 会把诊断规则变成运行规则，必须先补 authority-aware resolution。

### 2.5 E05/E06/E07 的现有基础

- `internal/worker/base/env.go:BuildEnv` 已有 blocklist、prefix 注入、session/config override 和 nested-agent 清理；strict 不能改坏 compat。
- `internal/worker/codexcli/manager.go`、`internal/worker/opencodeserver/singleton.go` 有共享运行时 env 路径，不能只修 base。
- `.github/workflows/release.yml` 在 Ubuntu 交叉编译；正式 release 与 offline bundle 均只依赖 build；offline bundle 动态获取 OpenCode latest 与 npm 当前版本。
- `scripts/pack-offline-bundle.sh` 还有缺包时的 npm version fallback；仅给 workflow 传版本仍不足以冻结最终包。
- `internal/admin/runtime_plan.go`、`fences.go` 与 `internal/execution` 已有只读计划、fence inspect/resolve/abandon。控制台要消费这些事实，不重做恢复状态机。
- `internal/eventstore/collector.go` 采用异步收集；“放入 collector”不能用来证明持久接受。`TurnRecord` 已存 conversation content，控制事实表依然必须 content-free。

### 2.6 跟踪关系与已完成范围

2026-10-03 只读查询结果：

| 跟踪 | 状态 | 本方案关系 |
| --- | --- | --- |
| PR #986 | MERGED，2026-09-12，merge `358c5e07216d47b6e9311565ecb9a00be3e5c0f2` | D01–D10 不重复规划，不把新范围交付到已合并 PR |
| #946 | CLOSED，2026-08-06 | 已交付诊断 first slice；authoritative launch 应新建增量跟踪，关联 #946/#849 |
| #947 | OPEN | E03 首个 durable effect 切片 |
| #851 | OPEN | E02 持久排队增量，不重复 single-active gate |
| #867 | OPEN | E05 env/isolation |
| #871 | OPEN | E06 发布证明 |
| #849 | OPEN | 按切片同步实际新增 runtime events，不能声称完整事件契约已完成 |
| #868 | OPEN | E07 控制台 |
| #870/#948 | OPEN，有启动条件 | E08 与能力库存后续；不在本轮基础可靠性范围展开 |

新发现先登记独立 Issue 再修，记录基准、Source/Test/Live、复现和验收。本轮仅产出方案，未创建远端 Issue。既有 `docs/issues/2026-09-runtime-audit` 是旧批次真相源，不拿来登记本批新编号或擅改旧历史结果。

## 3. 目标行为

### 3.1 输入与执行

- ACK 分别表达接收方式、durability、派发阶段。只进入内存时显示“已暂存，重启可能丢失”；不能显示“已送达 Worker”。
- 同一 `(session_id, client_message_id)` 的同 payload 重试返回原事实；不同 payload 返回冲突。
- 已持久排队的输入在重启后可查询；尚未开始 dispatch 的 queue item 可恢复调度。
- 进入 dispatch 边界后无法确认 Worker 接受，保持 `unknown` 与 fence，不自动重投。
- native mid-turn injection 保留当前能力，确认不冒充一次新的完整 execution；其重启耐久性在首个切片仍明确为 volatile。
- 排队 Skill 保留原生命令身份、参数、权限校验和 materialization，不能拼成普通文本。
- `/stop` 只停止当前 turn；排队取消使用独立动作。`/reset`、session 删除、权限收紧、shutdown 对排队的语义明确。

### 3.2 外部交付

- Agent 完成、交付 planned/started、provider accepted、可外部核验、用户已读分别展示。
- 一个 Cron/Webhook occurrence 的同一逻辑交付只有一个 effect；retry attempt 不生成新 effect。
- 响应丢失、超时、过期 lease 产生可解释的 `unknown`；没有安全幂等/查询能力时停止自动发送。
- final result 与 effect intent 在可恢复的持久事务中绑定；重启不靠重新运行 Agent 生成内容。
- 用户自由编写的额外 CLI 外部动作属于 Worker-private effect，不能冒充 Gateway 已管理的交付。

### 3.3 配置、安全、发布

- 同一启动使用一份 immutable resolved plan 和其有效 materialization；诊断、launch、bootstrap evidence 指向同一 revision。
- 请求优先级不能突破 ownership/workspace/session ceiling。
- strict env 不继承未知 host keys；env 清理不能被描述为 filesystem/network isolation。
- release、SDK、WebChat、原生 smoke 与产物 provenance 关联同一 source SHA。外部运行时版本和 digest 由已验证的 lock manifest 指定。
- 控制台依据 canonical facts 给出安全动作；恢复不会直接把 unknown 写成 success。

## 4. 推荐解决方案

### 4.1 分阶段交付与依赖

| 阶段 | 工作单元 | 退出条件 |
| --- | --- | --- |
| S0 证据与确认 | A 测试选择器；B 补充 ACK；F1 同 SHA 发布门禁 | 关键回归必跑，确认不超出事实，未经验证的 SHA 无法发布 |
| S1 运行与交付 | C Cron durable effect；D authoritative launch；E strict/capability；Q 持久输入队列；F2 完整发布证明 | 重启/重复/响应丢失有确定处理，plan 真正驱动启动，unknown 不盲目重放 |
| S2 操作投影 | G 执行时间线；已有 operator API 接入；新增 effect/queue 受控动作 | 从一次 execution 能定位故障阶段和安全下一步 |
| S3 条件产品化 | H 两个 Recipe 试点 | 满足既有 #870 启动条件，有真实使用证据及可核验交付 |

持久队列的内容存储/事务 helper 可与 C 共用，但 queue 调度本身在 C 的 effect 边界稳定后交付。新增通用 abstraction 只从首条垂直链路提取。

### 4.2 核心设计决策

1. `execution_inputs` 仍是 input 去重、delivery、runtime 与 fence 的唯一事实源；为 queue 增加状态和一对一调度扩展，不另建 input idempotency ledger。
2. `EffectLedger` 是不同领域的外部副作用事实；不把 `worker.done` 当成 effect 成功。
3. conversation payload 与控制事实分离。拟在 `internal/eventstore` 增加受原 session/workspace ACL 与 retention 管理的内部 payload 存储；execution/effect/queue 只保存引用与指纹。
4. 公开 `plan_hash` 保持既有含义；新增内部 `launch_fingerprint`，覆盖实际执行相关的非敏感决定及内容/配置 revision，默认不向公共接口输出。
5. 先将旧逻辑构造的有效启动参数与 plan 做字段级 shadow 比较，再按入口与 Worker 切换。不得直接把当前诊断 resolver 的 precedence 当作授权规则。
6. 首个 effect consumer 选择 **isolated Cron → Slack 单条最终消息**。飞书/Webhook 随后复用稳定接口；未覆盖的组合明确保留 legacy guarantee。
7. 发布质量门禁运行于固定 source SHA；可复用 workflow 不绑定浮动 main 来替代目标 tag 的源码。

### 4.3 大小与成本

以下是熟悉仓库的单工程师规划估计，包含实现、聚焦测试、必要文档和一轮审查，不是承诺；不包含真实平台权限准备、维护等待与复杂 OS sandbox 实现。

| 单元 | 估计工程日 | 成本来源 |
| --- | --- | --- |
| A | 1–2 | 匹配回归、闭包工具、workflow fixtures |
| B | 1–3 | AEP/四 SDK/前端兼容 |
| C | 6–10 | occurrence identity、成对迁移、effect lease、payload事务、Slack回执、故障测试 |
| D | 5–8 | 输入补齐、权限上限、adapter映射、所有 launch路径 |
| E | 3–5 | strict env、共享进程边界、capability evidence；不含新隔离后端 |
| Q | 5–8 | canonical queued状态、有界恢复、原生Skill、取消与迁移 |
| F1/F2 | 3–6 | 同SHA门禁、版本锁、原生产物smoke、SBOM与签名 |
| G | 4–6 | 查询投影、鉴权、前端时间线与恢复 |
| H | 2–4，满足条件后 | 两个模板与dry-run，无新调度层 |

S0 可先控制在约 3–6 工程日；全部基础闭环不能当作一次小修，建议分多个逻辑 PR。外部缓存/并行优化、强 OS 隔离、新渠道、新多 Agent 编排均不包含在估计内。

## 5. 详细实施步骤

所有“新增”文件、接口、字段、枚举和命令在本文中均为拟新增项，不能当作当前仓库已存在。

### 5.1 开工与交付纪律

1. 核对执行时 HEAD、目标分支、工作区与远端 Issue。原分析基准有变化时只复核受影响范围。
2. 读取实际修改目录的 `AGENTS.md`。本方案不替代 module 局部规则。
3. 登记 E01/E02 与 authoritative launch 增量 Issue；C/E/F/G/Q 关联已有开放 Issue并写明 first-slice范围。不重新打开 #946，不追加 #986。
4. 用户批准新能力实施后、写业务代码前，按可用的项目 Hindsight 入口 capture 已批准范围；bug fix、小修不机械创建 initiative。
5. 每个自洽逻辑单元把实现、回归与必要文档一起提交；先验证、检查 staged diff，再本地 commit，仅包含本任务内容。
6. 一旦实施授权成立，非 main 实施分支按项目规则验证后 commit + push；不跳过 hooks。首次 clone 按项目规则安装 hooks。合并、发布和真实外部消息仍按各自授权范围执行。
7. 同文件出现并行改动暂停该处写入并核实；不能整文件覆盖或回退清除他人改动。

### 5.2 A：CI 测试选择器（两个原子提交）

**A1：补上当前失败路径。**

- 修改 `.github/workflows/ci.yml` 的匹配为固定字符串、整行匹配；Go 模板改为每个 import path 独占一行。避免 `grep -Fqx` 对带尾随空格的旧输出再次不匹配。
- 拟新增 `scripts/ci/test_select_go_tests.py`（标准库 unittest）与回归 fixture，先使旧谓词失败，证明直接依赖包会漏选。
- 若 A2 可以同一个短迭代完成，可将 A1/A2 合为一个实现提交；不能只修引号便宣称闭包已完成。

**A2：用确定性选择器替换嵌套 shell。**

拟新增 `scripts/ci/select_go_tests.py`，仅用标准库；读取 `go list -json ./...` 连续 JSON对象以及 NUL 分隔 diff清单，不使用 `shell=True`。

- 以 `ImportPath` 为节点，`Imports ∪ TestImports ∪ XTestImports` 为边，BFS/DFS求反向传递闭包。
- changed Go文件由 `Dir` 映射包；根目录包不要拼出 `/.'`。
- push、依赖manifest、selector/workflow、公共protocol/schema、无法映射的删除/rename、包图失败一律全量候选集合。文档-only可选完整测试或明确文档任务，不返回假成功。
- 输出排序去重的包数组、fallback reason；空选择仅在确认没有相关变更时合法。
- 原有排除包必须在独立任务验证；不能把禁止列表算作“完整覆盖”。
- 按PR实际base SHA取diff，显式checkout/fetch证据。fork PR不提供发布或签名权限。
- 更新 workflow 与 fixtures；比较预计包列表，不拿测试命令退出码代替选择集合断言。

完成：直接、间接、内部/外部测试依赖、删除、根包、依赖/协议变更的 fixtures 全部覆盖。提交 `fix(ci): select reverse test dependency closure`。

### 5.3 B：补充输入 ACK 与 UI（一个协议提交）

影响：`pkg/events/events.go` 与测试、`pkg/aep/schema` corpus、`client`、三种示例 SDK、WebChat transport/runtime adapter、`handler.go`、`pending_buffer.go`，必要的平台 supplement 文案与测试。

在 `InputAckData` 拟增 optional typed 字段：

```text
input_mode   = primary | injected | buffered | queued
durability   = durable | volatile
parent_execution_id = 可选；native injection关联正在执行的turn
```

具体行为：

| 路径 | status | input_mode | durability | 显示 |
| --- | --- | --- | --- | --- |
| 既有普通输入 | 维持原 accepted/delivered/unknown/failed | primary（可省略） | durable（旧消息可省略） | 原语义 |
| 原生注入成功 | delivered | injected | volatile | 已提交至当前运行，不证明效果已完成 |
| 内存缓冲成功 | accepted | buffered | volatile | 已暂存，尚未派发，重启可能丢失 |
| 将来持久队列 | accepted | queued | durable | 已排队，尚未派发 |

- `ackSupplement` 接受真实 disposition，不靠字符串缺失猜测；重复 ACK带原 mode/durability。
- 合成 `supplement-` ID只是本轮correlation；不能被控制台当作真实 durable execution查询。
- 无字段的旧普通 ACK保持旧语义；绝不能为了旧客户端继续给 buffered发送虚假的 delivered。
- WebChat将 receipt 状态与 Agent运行状态分开。buffered accepted可结束此次提交等待，保留正在运行的parent；不标记queued dispatch已送达，不创建虚假已完成assistant turn。
- 新服务端与旧SDK解码需兼容；旧UI必须验证不死锁。若旧UI无法处理新语义，提供明确的客户端版本/能力门槛并拒绝volatile补充（仍可收到现有`SESSION_BUSY`），不能恢复虚假ACK作为fallback。
- 更新 `docs/reference/aep-protocol.md`、`events.md`；说明 `accepted` 需结合durability解释，旧普通输入的durable保证保持。
- 同步 canonical schema、additive/missing optional/unknown Kind fixtures；文案中英文一致。

完成：normal ACK不回归；buffered不报告delivered；重复字段一致；旧/新客户端行为均有测试。提交 `fix(aep): report supplement receipt and durability accurately`。

### 5.4 C：首条 Cron durable effect 闭环

**C1：稳定 trigger identity 与统一 input dispatch。**

- 当前 `Execute` 用当前时间生成新session key；需要先持久化 occurrence。拟新增 `internal/cron/occurrence.go` 及SQLite/PG实现，配套成对migration。
- 定时触发键包含job ID、持久schedule revision、scheduled UTC时刻；manual trigger使用请求nonce；Webhook使用已验证的source/event ID。缺稳定source ID不能宣称Webhook端到端去重。
- occurrence在Worker启动前持久记录；重复trigger返回已有occurrence与execution，不生成第二个run。执行retry与operator rerun用显式新generation，不能偷改idempotency key。
- 拟在Gateway增加内部可信调用 `DispatchSystemInput`，复用ordinary accept/ACK-independent dispatch/owner lease/terminal correlation。不要从Cron直接走公开HTTP或伪造用户envelope绕过授权。
- Cron通过typed依赖调用，移除直接`w.Input`的受控路径；保留print Worker的EOF/termination规则。
- `LastRunID`仍可保留展示用途，不拿它充当完整run事实。完成等待必须关联execution/worker_run，不只看session的全局IDLE状态。

完成：并发重复trigger一次accept；Worker启动失败不丢occurrence；不安全unknown不重新启动Agent。提交C1。

**C2：payload与effect intent持久事务。**

拟新增 `internal/effect/{store,sqlite_store,pg_store}.go`、测试与成对migration。共享当前数据库和`sqlutil.WriteMu`，不新开数据库服务。

- `effect_id`随机ID；唯一business key含occurrence、逻辑投递序号、target revision。attempt不参与该key。
- 表只存execution/worker_run关联、owner lease/version、状态、attempt、provider/evidence引用、bounded原因、payload引用/指纹、target引用。
- 在`internal/eventstore`拟增内部payload存储和transaction helper；保存final assistant文本/有界发布内容，不保存原始provider request、完整工具参数或凭证。target从已授权job配置投影，凭证仍由当前adapter获取。
- 严格按execution关联选取assistant最终输出；过滤user turn、历史generation、synthetic crash/error和未完成partial结果。
- final内容snapshot与effect planned在同一事务提交，不能先ACK planned后依赖异步collector补正文。
- 若assistant终态已经持久化但effect intent未写入，recovery按stable execution/occurrence key补齐同一个effect。外部发送只能发生在intent提交后。
- 内容写入失败、空结果、未知schema或超容量都产生明确bounded错误并停在发送前，不能悄悄回退CLI。

完成：提交点两侧crash注入均能恢复相同effect与payload；事务失败不发送。提交C2。

**C3：provider回执与唯一交付owner。**

拟新增internal receipt接口，不破坏现有`CronResultSender`调用方；legacy接口通过wrapper继续返回error，新受控路径必须返回typed结果：

```text
SendResult:
  outcome = accepted | rejected | unknown
  provider_ref, evidence_ref
  rejection_class = safe_retry | permanent | unspecified
  retry_after（可选）
ProviderCapabilities:
  idempotency / lookup / receipt / idempotency_window
```

- Slack首切片保存成功响应中的channel与timestamp，并尊重`thread_ts`；收据表示provider accepted，不表示用户已读。
- 返回网络timeout、response loss或无法判断提交程度的5xx，默认unknown。不能只看`IsTransient`自动重发。
- 唯一owner策略拟增Cron `delivery_mode=legacy_cli|gateway`：旧任务缺字段按legacy兼容；新任务默认gateway，前提是选定target adapter具备受控能力。
- gateway模式不追加`buildDeliverySuffix`，`executeJob`不再按`HasCLIDelivery`跳过；直接由Gatewayeffect发送。
- 两种mode互斥，同一次occurrence记录mode。模式切换只影响下一次未开始occurrence。
- legacy任务不自动迁移、不宣称已有effect保证。先dry-run展示待迁移目标、adapter能力，再按已批准任务范围迁移。
- 用户prompt自身包含CLI发送指令不属于网关可证明范围；不得用关键词删除用户正文。迁移内置suffix与权限边界，不以文本扫描宣称阻止一切副作用。
- 不把slack`client_msg_id`或飞书`uuid`名称当作永久幂等保证；集成时核对官方契约、去重窗口和权限，写入capability与测试。无充分证据时策略是unknown后停止自动发送。

完成：gateway mode一次发送、legacy一次原路径；收据可查询；Agent done与delivery不同字段。提交C3。

**C4：lease/recovery/operator与扩展。**

- planned经条件claim进入started，记录token/version/attempt；发送在事务外，完成回写必须匹配token和effect。拟增不可混淆的attempt事实，保留每次发送的lease token、开始时间与typed结果。
- active发送attempt的lease过期进入unknown，不改回planned；旧执行者不可开始第二次发送。已有可信回执的晚到结果可用同effect/attempt关联收敛，不能创建新effect。
- 明确safe-retry的拒绝记录在attempt上，effect仍为started并记录next_attempt_at；当前attempt已明确未提交、无in-flight lease时，条件ClaimRetry才能增加attempt。permanent拒绝或attempt耗尽才将effect置failed，failed终态不自动回退。
- planned未执行项重启后可claim；safe-retry必须同时满足provider证据、backoff、attempt上限与stop condition。
- 拟增 `runtime:read` effect查询；effect operator动作独立于execution fence action。必须reason/evidence/version与audit，409后重新inspect，不自动重试write。
- 首切片沿用当前delivery预算：最大3个send attempts、初始30s退避、最大5min；unknown不消费为新发送机会，禁止淘汰已接受effect。
- 以注入clock驱动测试；有界poll/并发，不在shutdown期间继续派发。
- Slack验收完成后，再把同接口用于飞书、Webhook触发的isolated execution。Webhook输入accept、Agent执行与最终消息effect三个事实分开。
- 为runtime effect事件增量同步AEP/SDK/文档与低基数metrics。

完成：SQLite/PG竞争、重启、DB错误、响应丢失、晚到收据均通过；unsupported provider明确unknown/fenced。提交C4。#947只记录首切片完成，不提前关闭其跨control/connector全范围。

### 5.5 D：运行计划成为启动事实来源

**D1：内部指纹与authority-aware input。**

- `plan.go`保留`PlanHash`公开含义；拟新增versioned内部`LaunchFingerprint`与coverage标志，禁止复用公开hash做审批/授权缓存。
- `Input`补齐实际cfg snapshot、resolved bot/platform、workspace ceiling、session已捕获ceiling、可验证backend capability及request intent。
- 先resolve desired，再clamp到授权上限；无法满足请求则blocked，不静默升级。workdir/owner/origin沿现有验证链。
- 列出field ownership表：worker type、模型、工具、permission、sandbox、budget、env profile、config/skill materialization revision。确认哪一层负责requested/effective/default，避免重算。
- 禁止将`AllowedModels`白名单直接当作“当前选择模型”字段；拟增独立`RequestedModel`，或使用当前adapter实际支持的typed model机制，四Worker契约测试验证。
- Map nil/explicit-empty必须区别：nil继承；用户或配置显式clear由带presence字段表达，不能被nonzero mapper丢弃。

完成：模型/工具变化改变内部指纹；公开view仍脱敏；权限超上限blocked；指纹只标识计划，未冒充已应用。提交D1。

**D2：bridge一次解析与shadow parity。**

- 在`bridge_worker.go:createAndLaunchWorker`附近集中prepare已验证plan与materialization，覆盖fresh/resume/reset/crash-recovery。
- before launch绑定同一个cfg revision与agent-config/skill revision；注入材料变化时重新resolve或拒绝，不允许diagnostic旧revision启动新配置。
- shadow比较**legacy最终有效参数**与plan映射后的有效参数；只记录field enum和相等/不等，不能日志打印raw工具列表、路径、env值或prompt。
- observed bootstrap绑定worker_run与launch指纹；现有`capturePermissionCeiling`保持保护，bootstrap不足输出unknown/partial。
- identity/snapshot仍走既有session reserved context；诊断区分“该run当时的plan”与“当前配置试算”，不能resume时重算冒充历史事实。
- 建议rollout配置明确为shadow与authoritative，按入口/worker allowlist；不匹配组合继续明确shadow，不能误报完整上线。

完成：所有launch调用进入同一准备函数；差异fixture能被发现；不改变dispatch时行为等价。提交D2。

**D3：逐组合切换authoritative。**

- 先WebChat WS/REST等价输入与一个Worker，再消息入口、Cron和剩余Worker；每个组合独立fake launch参数断言。
- authoritative中`ErrPlanBlocked`必须在真正Start/Input前阻止；禁止吞error后legacy fallback。
- 将plan拥有的字段映射到实际adapter启动/首条request；被backend拒绝或不支持的必需字段blocked。
- strict singleton差异不能按session伪覆盖共享进程：同runtime env/isolation profile不兼容时拒绝，或使用明确分池能力；首切片选拒绝，不擅自重启共享进程。
- `Admin/runtime_plan`、doctor和dry-run消费同resolver，但无实际launch则只返回planned。

完成：每个入口×Worker在bootstrap证据中可关联同revision；blocked零Start；未覆盖组合显式显示。每个自洽组合单元可提交，不把所有gateway重构塞进一次提交。

### 5.6 E：strict env 与隔离报告

- 修改base env与四Worker实际env路径，拟增`EnvProfile`、显式allow keys和typed报告。默认compat保留历史行为，strict显式启用。
- strict从最小系统allowlist开始；拟推荐POSIX `PATH/HOME/TMPDIR/LANG/LC_*`，Windows补`SystemRoot/WINDIR/TEMP/TMP/PATHEXT/USERPROFILE`。这是测试起点，不能继承任意prefix或认为HOME意味着目录隔离。
- 依次合并allowlisted host→显式worker注入→可信session injection→config override；nested-agent禁用项在全部merge后强制清除。输入key/value校验，Windows key大小写按实际语义去重。
- 普通client metadata不得指定secret值或env allowlist；credential来源保持现有配置/受控引用，不增加新存储。
- 对Codex/OCS共享进程报告process范围env，不编造session独立env。
- 拟增可选`IsolationReporter`接口与默认unknown报告；noop/mocks/registry同步。若变更Worker主接口，则四adapter全实现。
- report区分declared/observed/enforced/partial/unavailable/unknown；filesystem/network报告由实际backend evidence产生，不由permission mode推断。
- strict只要求env时可在无OS隔离后端运行；配置要求filesystem/network强隔离却无法enforce时拒绝启动。实现新容器/sandbox backend是另项设计。

完成：未知host sentinel不进入strict；compat行为保持；override不能恢复强制禁用项；singleton边界真实；跨OS env/path测试。提交env构造与能力报告两个逻辑单元。

### 5.7 Q：有界持久排队输入

前置B、C的payload/事务基础及稳定runtime事件。范围限单节点、单session FIFO，不推进分布式scheduler。

**Q1：扩展canonical execution与事务接受。**

- `internal/execution/store.go`拟增`RuntimeStatusQueued`，`Record`相应projection。
- `execution_inputs`仍有唯一`(session_id, client_message_id)`；拟增`execution_queue`一对一调度表，存queue序号、enqueue/expire时间、session lifecycle revision、payload ref，不重复delivery/runtime/idempotency state。
- 原active index继续只覆盖pending/running/fence，queued不占active slot。claim在同事务把队首queued改pending并获取现有owner lease；unique constraint竞争失败保留queued。
- SQLite的既有runtime CHECK不允许queued。须成对migration更新CHECK，按数据库支持采取保留数据/index/FK的表迁移，测试失败回滚；不得只改Go枚举。
- queue正文存eventstore内部payload；canonical执行row、队列扩展与正文引用同事务提交后才durable ACK。允许持久化的字段白名单明确，丢弃任意envelope metadata，凭证不落表。
- 原Recovery对accepted输入置unknown的逻辑必须排除从未dispatch的queued；否则重启会把安全待发项错误fence。
- proposed默认：每session最多20条（保留现有数值），全实例1000条，单条64KiB，未派发TTL24h；容量/TTL/bytes做有界配置，所有限制事务内检查。PG按session串行化队列序号与每session容量，全局容量使用共享budget row的条件增减，不能仅凭无锁COUNT判断；SQLite沿用WriteMu。未接受时拒绝，不能淘汰已ACK输入。

完成：重复/冲突、事务失败、migration和restart恢复通过。提交Q1。

**Q2：队首派发、取消和生命周期。**

- 每条输入独立execution，首切片不合并。enqueue序号由DB分配，不用用户时钟排序。
- gate释放只唤醒队首；replay先重新检查owner/session revision、权限、Skill capability/materialization。不可用项明确失败/取消，不降级成普通prompt。
- dispatch开始后的lease过期遵循unknown/fence；未开始dispatch的queued可恢复。多进程访问共享DB也须唯一claim，但不承诺多实例调度/连接所有权完整上线。
- stop保留后续queue；拟新增单项cancel/clear-queue API，queued可条件取消，pending/running调用现有stop或返回conflict，不能伪装未执行。
- reset取消尚未派发的旧lifecyclequeue，并保留取消事实；session删除先停dispatch、按已有事务删除内容/队列与cleanup outbox，防旧token复活。shutdown停止claim，queued留存。
- native injection继续通过B明确volatile；默认fallback转durable next-turn queue。其injection失败是否安全fallback必须依Worker证据判断，不能将不确定注入自动再排一次。
- UI客户端queue未发送项仍是客户端事实；收到durable queued ACK后移交server tracking。断线不能同时从两端再次生成新ID派发。

完成：FIFO/stop/reset/delete/permission-change/native Skill/mid-turn ambiguity全部覆盖。提交Q2。此首切片不等于#851所有timeout/retry/crash统一工作全部完成。

### 5.8 F：发布证明

**F1：同source SHA门禁。**

- `.github/workflows/release.yml`建立validate→build→artifact smoke/verify→publish有向依赖；offline bundle纳入publish的共同前置。
- 拟提取reusable validation workflow，CI/release调用同一套质量逻辑；release按tag/version解析后的不可变SHAcheckout。不能用最新main最近绿灯代替target SHA。
- gate至少含Go质量/必跑wiring、AEPschema与四SDKconformance、WebChatunit/矩阵、三渠道×四Worker契约矩阵。
- 取消/跳过/未运行均不能记为通过；故意失败fixture必须阻断publish。
- PR任务仅read权限；publish job再授contents write，attestation相关权限限定到所需job。

完成：故意失败的SDK/平台任务阻止publish；所有产物声明同source SHA。提交F1。

**F2：版本锁、产物smoke与来源证明。**

- 拟新增`configs/worker-runtime-lock.json`，存每个offline运行时/package精确version、平台、download source、digest/integrity、必要transitive依赖。具体版本由已验证兼容矩阵选定，本文不指定未经验证的“最新版本”。
- release脚本启用显式locked模式；禁止`latest`、`npm view`和missing-package版本fallback。旧交互式打包若保留动态模式必须显式选择并标记non-release。
- 当前交叉编译产物在Linux/macOS/Windows原生runner上下载并验证checksum，然后startup、health、fresh SQLitemigration、旧fixture升级、graceful shutdown；不在smoke里重新build替代被发布产物。
- macOS/Windows runner能启动哪个arch就验哪个；其余arch仅build状态明确，不冒充native smoke。项目现有Intel release矩阵本切片不擅自删改。
- 固定build输入/依赖与构建工具版本，记录source/tree SHA、工具链、lock digest、产物digest；若尚未证明bit-for-bit reproducibility，只称可追溯固定输入。
- 为binary与bundle分别生成SBOM，签名checksum/产物，生成可验provenance。若采用GitHubattestation或keyless签名，依实际repository支持和官方契约实现，不自动购买服务或创建私钥。
- 先准备verification脚本与fixture，执行损坏/错SHA/无签名负例；真实publication与签名身份配置单独授权。

完成：所有下载产物可验证；锁清单外访问失败；tamper失败；发布说明记录尚未native验的架构。提交F2。

### 5.9 G：执行时间线与安全动作

- 现有Admin：`runtime_plan.go`、`fences.go`、`middleware.go`、`audit.go`；routes在`cmd/hotplex/routes.go`。
- 拟新增`internal/admin/executions.go`及projection/query接口；WebChat拟增`app/admin/executions/page.tsx`、`detail/page.tsx`和组件，接入现有admin-nav/locales。
- 拟API：`GET /admin/executions`、`GET /admin/executions/{id}/timeline`；复用`runtime:read`、workspace/session ownership校验。查询参数cursor、limit默认50/最大100、time window、event/effect数量上限。
- 对execution、eventstore、effect、plan snapshot、audit做有界batch投影。禁止每条timeline单独查DB、无界trace扫描或用当前session状态覆盖历史run事实。
- timeline项标明phase、source、fact time与observed time、execution/worker_run/effect关联、redacted evidence、是否截断/缺失。
- 既有记录没有plan/effect字段时显示“无历史证据”，不推定失败或成功。
- 允许actions由服务端按当前state/version/权限给出；UI不能自行把unknown归为retryable。
- execution fence动作继续走现有resolve/abandon；queue取消走Q；effect查询/收敛走C。每项展示将做的具体动作、reason/evidence和scope，不提供“强制success”。
- Agent完成、provider accepted、externally verified与read receipt使用分别的标记；没有read evidence时不显示“已读”。
- 新metrics通过现有`sync.Once`accessor，有限result/phase/reason labels；ID/hash/ref不进labels。

完成：故障样本无需翻多份日志即可判断阶段；跨workspace访问拒绝；stale version409；UI中英文一致。API和UI各一原子提交。

### 5.10 H：两个有条件 Recipe

现有#870要求Stage1–3运行证据，并要求真实场景无法仅由现有单Agent+tools/context满足。这个条件比“觉得模板有用”更严格；没有样本时**停留在文档模板，不启动Recipe registry/manifest管理能力**。

条件满足后：

- 拟新增版本化manifest与校验，复用Cron/Webhook、resolved plan、execution、effect。字段含schema/recipe version、trigger、workspace ref、Worker、permission profile、template ref/revision、timeout、delivery ref。
- 试点1：失败构建诊断，只读分析已有构建证据，输出原因/证据/建议报告。
- 试点2：仓库健康巡检，用finding fingerprint与last published checkpoint输出增量发现，checkpoint仅在effect confirmed后推进。
- dry-run只resolve，不启动Worker或发送消息；invalid owner/path/worker/policy/target fail closed。
- 不自动改代码、合并、发布；不实现DAG或distributed scheduler。
- 指标：每workspace每周provider-confirmed任务数、accepted→完成、delivery confirmed、unknown数量/年龄、queue等待、人工恢复时间。明确分母与receipt级别，无真实数据时不给生产成功率目标。

完成：重复trigger稳定、dry-run零副作用、失败交付不推进checkpoint；单独提交。#948先保持read-only解释范围，不顺手实现promotion/marketplace。

## 6. 关键实现说明

### 6.1 排队输入状态

```text
durable accept:
  delivery=accepted, runtime=queued
        ↓ queue claim + active gate + owner lease（同事务）
  delivery=accepted, runtime=pending
        ↓ Worker接受证据
  delivery=delivered, runtime=running
        ↓
  runtime=completed / failed / unknown
```

queued取消/到期：在开始dispatch前写delivery failed与runtime failed，bounded code标明`QUEUE_CANCELLED/QUEUE_EXPIRED`，UI不解释为“Worker执行失败”。这些是拟新增code。

持久排队不是把现有accepted recovery全部改成retry：**只有明确runtime=queued且从未开始dispatch的记录安全恢复**。pending/started派发边界保留现有unknown/fence。

### 6.2 Effect条件更新

```text
PlanOnce(business_key, payload_ref, target_ref)  // UNIQUE business key
Claim(effect_id, expected_version, owner, lease) // planned -> started
SendOutsideTransaction(...)
Complete(effect_id, attempt, lease_token, typed_receipt)
```

DB提交claim后进程crash即进入不确定窗口。lease过期不能证明外部未发送。成功回执落库失败也不能再发一次；先query/reconcile，否则unknown。

retry属于同effect下的下一次attempt：仅当上一attempt有明确safe rejection证据、无in-flight发送、到达next_attempt_at且未超过cap，才能ClaimRetry。effect在等待可安全retry时保持started；succeeded/failed/reconciled终态不自动回退，unknown只能查询/收敛或进入operator处理。

晚到回执仅按原effect/attempt及其真实性校验收敛；旧lease不能覆盖较新已验证结果。operator fenced的记录也不删除历史；如已有可信成功证据，显示证据与当前fence状态，动作规则需条件版本校验。

### 6.3 指纹与配置材料

内部launch指纹至少覆盖：schema/resolver revision、Worker/backend选择、effective model、ordered/explicit tool policy、permission ceiling、sandbox/budget、env profile/允许keys、opaque workspace/config/skill/materialization revision。

- 无序集合排序去重；语义有序的命令/步骤保序；nil与explicit empty按语义区分。
- 不哈希后公开secret值。凭证变化用受控opaque revision标识；无法获revision就标partial，不声称完整配置同一。
- 原始command/path/prompt/metadata value不写进plan/effect/audit。必要材料digest由已有校验/materialization边界生成；绝不把自由文本warning写入canonical内部身份。
- 指纹不是approval token、权限缓存key或enforcement证明。授权继续基于当前owner/scope/ceiling。
- runtime model/policy切换后创建新的effective revision；历史execution引用旧revision，不悄悄改写历史plan。

### 6.4 内容、GC与容量

payload存储属于内容域：按session/workspace ACL、原有保留政策与内部引用管理。control API只输出ref/digest与枚举。

effect/queue需要内容时，GC不得先清内容留下仍承诺可恢复的控制项。到期必须以事务明确cancel/expire，或保留unknown事实但标记content不可用；不自动重构已过期结果。用户删除内容优先，不因为outbox无限保留用户数据。

拟新增容量默认仅作first-slice配置建议；上线前通过测试负载确认。DB满、事务冲突、限流、shutdown都是显式结果，不能用内存接受fallback维持虚假的durable保证。

### 6.5 事件并发与回退

沿用per-session Seq、worker_run frozen connection、lifecycle generation和shutdown顺序。Effect/Queue新事件走既有eventstore/audit，事件投影丢失后可从canonical事实重建，不能使业务事实依赖event delivery成功。

数据迁移发布后优先关闭新写入/派发、保留新版兼容reader；不能直接downgrade到不理解queued状态的旧binary，更不能删新表作为回退。

## 7. 测试方案

下面均为实施待执行项；拟新增测试名/文件明确标注。

| 单元 | 现有测试/拟新增位置 | 必须证明 |
| --- | --- | --- |
| A | 拟`tests/fixtures`位于`scripts/ci`，拟`test_select_go_tests.py` | exact匹配、传递链、TestImports/XTestImports、删除/rename/root、manifest/protocol fallback、graph错误不空跑 |
| B | `handler_test.go`、`pending_buffer_test.go`、`pkg/events/events_test.go`、SDKconformance、WebChat runtime/browser client测试 | volatile buffered≠delivered；duplicate回原mode；cap拒绝不成功ACK；旧字段/新字段解码；UI不假done/死锁 |
| C1 | `cron/executor_test.go`、拟occurrence tests、`gateway/runtime_facts_test.go` | trigger重复一个run；Cron走canonical输入；同runterminal关联；EOF不回归 |
| C2/C4 | 拟`internal/effect/*_test.go`与`pg_store_test.go`、eventstore payload tests | effect唯一键、payload原子性、双实例claim、DB错误、lease过期、重启、成功response loss、late receipt、unknown不自动send |
| C3 | `cron/delivery_test.go`、`delivery_retry_test.go`、Slack/Feishu adapter tests | legacy/gateway互斥；thread target；provider ref；reject/unknown分类；SDK隐式重试边界 |
| D | `agentspec/{plan,map,resolver}_test.go`、`gateway/agentspec_test.go`、`bridge_worker_test.go`、四Worker测试 | config precedence与authority分别校验；hash覆盖与redaction；WS/REST等价；final launch字段；resume/reset/crash均覆盖 |
| E | `base/env_test.go`、Codexmanager/OCSsingleton tests、四Workercapability tests、doctor tests | strict未知key排除、override禁用项、Windows key处理、共享runtime不假隔离、required capability不足blocked |
| Q | 拟execution queue SQLite/PGtests、gateway replay tests、WebChat follow-up queue tests | FIFO、事务接受、single-active、safe recovery/unknown fence区分、duplicate/conflict、stop/reset/delete、Skill、容量/TTL |
| F | 拟workflow fixtures、lock验证、native smoke、verification负例 | targetSHA、skipped≠passed、SDK失败阻发、发布产物而非重build、lock无latest、checksum/SBOM/签名/provenance可验 |
| G | 拟`admin/executions_test.go`、现有fences/audit tests、WebChat timeline tests | 鉴权、分页、有界batch、stale409、history evidence缺失、Agent/provider/read分离、安全actions |
| H | 条件满足后拟recipe validate/dry-run tests | 无副作用dry-run、只读policy、stabletrigger、delivery确认后checkpoint |

异步测试使用channel/barrier、`require.Eventually`或注入clock；禁止`time.Sleep`。通常table-driven、`t.Parallel()`；全局env修改测试不能盲目parallel，应把env源注入以便并行。不启动收费模型或向真实渠道发送测试消息。

必须建立的故障矩阵：

| 故障窗口 | 期望结果 |
| --- | --- |
| durable接受事务之前crash | 未报告durable，重试可重新accept |
| queue已接受、dispatch未开始crash | queued恢复，身份不变 |
| dispatch已claim、Worker响应丢失 | unknown/fence，不重投 |
| effect planned已提交、未send重启 | 可claim原effect |
| provider实际成功、response丢失 | unknown，可查询则收敛，无证据则停止 |
| provider回执成功、DB写失败 | 不重复send；reconcile原effect |
| stale owner回写或reset后旧replay | 条件更新失败/拒绝，旧内容不复活 |
| 新owner与late receipt竞争 | 同effect/attempt可验证收敛，不创建第二effect |
| queue/effect到期与内容GC竞争 | 明确expire/cancel/content unavailable，无不可解释缺口 |
| strict singleton已有不兼容env | 启动拒绝，现有runtime不被静默重配 |

## 8. 验收标准

### 8.1 本方案已有证据

- [x] 原对话完整正文已读取，原报告附件未冒充本轮证据。
- [x] 当前HEAD与原分析基准一致；关键路径已源码核验。
- [x] CI原匹配失败/固定整行匹配成功的微型复现已执行。
- [x] #986 merged、#946 closed与相关开放Issue已只读查询。
- [x] 所有业务验收与Live项在本文中均标为待执行。

### 8.2 实施命令与运行位置

以下原生命令在仓库根执行；执行者按本机适用RTK规则路由，持久方案不复制本机wrapper。新增包/测试工具须对应单元已实现后才执行。不要从本文复制任何DSN、token或固定用户配置。

```bash
# A，拟新增选择器测试实现后
python3 -m unittest discover -s scripts/ci -p 'test_*.py'

# B：协议与Gateway
go test ./pkg/events ./pkg/aep/... ./internal/gateway -short -count=1 -race -shuffle=on
go -C client test ./... -count=1 -race -shuffle=on
pnpm -C webchat test
npm --prefix examples/typescript-client test -- tests/conformance.test.ts
# 在 examples/python-client：
python3 -m pytest tests/test_conformance.py -v
# 在 examples/java-client：
mvn -B test -Dtest=dev.hotplex.conformance.AepCorpusConformanceTest

# C/Q，internal/effect实现后
go test ./internal/cron ./internal/effect ./internal/execution ./internal/eventstore ./internal/gateway ./internal/session/sql -short -count=1 -race -shuffle=on
go test -tags pg -p 1 ./internal/effect ./internal/execution ./internal/eventstore ./internal/cron ./internal/session/sql -count=1 -race -shuffle=on

# D/E/G
go test ./internal/agentspec ./internal/gateway ./internal/worker/... ./internal/admin ./internal/cli/checkers ./cmd/hotplex -short -count=1 -race -shuffle=on

# 涉及渠道或Worker契约
make test-contract-matrix

# 每个提交做diff卫生，相关模块聚焦验证；准备PR交付时完整门禁
git diff --check
make quality
make build
make docs-lint
```

说明：

- Python/Java命令在标注的各SDK目录执行，其余在仓库根；依赖安装按对应lockfile与现有CI，不擅自升级。
- Go嵌入产物按现有构建流程准备；缺产物时记录环境失败，不拿package跳过伪装通过。
- 真实PG测试使用已有安全配置的`HOTPLEX_TEST_PG_DSN`；未配置、skip或连接失败必须标“未验证”，不是PG通过。不为了写方案启动/改本机数据库。
- 每模块≤5s是项目目标；长测试先拆聚焦范围，实际超时如实记录，不能缩小验证以隐藏缺口。
- WebChat变更增加针对相关flow的Playwright测试；真实发布smoke在CIrunner隔离目录与临时配置，不能启动/重启用户当前服务。
- 新event同步schema与四SDKcorpus，必须检查各SDK实际测试数非零。工作流中不使用`-DfailIfNoTests=false`来掩盖缺失测试。
- 本机RTK可用时`make quality/build/docs-lint/hooks`分别按项目规定经`rtk proxy`执行；其他环境使用以上原生命令。

### 8.3 行为完成 Checklist

- [ ] A：受影响闭包与fallback fixtures证明无已知漏选，排除包有明确验证归属。
- [ ] B：buffered只确认volatile接受，UI/SDK兼容与重复ACK完整。
- [ ] C：Cron occurrence→execution→final payload→effect→receipt可查询；重启不丢已承诺的交付意图，unknown不自动重发。
- [ ] D：声明authoritative的每个组合实际使用同plan revision；权限上限保护；diagnostic/public hash与内部fingerprint分开。
- [ ] E：strict未知host key排除，能力等级符合实际证据，共享runtime差异显式拒绝。
- [ ] Q：queued正文与控制事实原子接受，FIFO、cancel/reset/delete、safe recovery与unknown均可验证。
- [ ] F：故意失败阻发；所有artifact关联同SHA与lock；原生smoke和verification覆盖明确。
- [ ] G：授权、有界时间线、安全动作与历史证据缺口均正确展示。
- [ ] H：满足启动条件后才实现，dry-run零副作用，交付确认后推进checkpoint。
- [ ] SQLite/PG成对migration及真实PG条件竞争验证完成。
- [ ] 所有新增API/event/log/audit默认无prompt、secret、metadata value、rawWorker错误与tool args。
- [ ] 每单元Issue回填实际commit、Source/Test/Live、命令/结果与未覆盖项；不把first slice说成整个Issue完成。

## 9. 风险与注意事项

1. **协议兼容成本真实存在。** buffered ACK改变status后，旧UI可能依赖delivered。发布必须先完成兼容矩阵或设能力门槛；新字段解码兼容不等于交互行为兼容。
2. **内容存储扩大了持久化范围。** 本方案推荐只复用conversation内容域、显式ACL/retention，不将正文塞进execution/effect。实施批准前应把payload字段白名单与删除/保留行为列入具体评审；不能读取当前用户prompt/凭证作样本。
3. **旧binary不理解queued。** Q迁移与rollback reader必须成套；回退采用停新dispatch、保留兼容新版读取，不能强制执行Down迁移删记录。
4. **provider幂等有边界。** 超时/5xx不一定表示未发送；没有官方保证与观测证据时只停unknown。不得声称端到端exactly-once。
5. **Cron模式迁移可能重复交付。** occurrence持久记录mode、同次只一个owner；不能在执行中的legacy occurrence切换gateway。
6. **Plan提升为authoritative会改变语义。** 每字段先做effective参数parity、authority校验与presence表达；回退开关只影响新launch，不改现有run/snapshot。
7. **env strict可能破坏Worker启动。** default compat、按Worker/OS迁移；共享进程环境不能按session隔离。新OS隔离后端需额外设计，不隐含授权安装或重启。
8. **发布证明不等于完全可重现。** 固定版本/签名/provenance分别证明不同事实；未做两次独立构建比对不宣称bit-for-bit。
9. **故障控制台可能变成无界查询。** 所有source有分页/预算，缺证据诚实显示；不把rawtrace或大payload推给浏览器。
10. **范围控制。** 不新增Worker/渠道、模型推理服务、通用Agent框架、DAG、分布式scheduler、K8soperator或数据库替换；不借此整理大文件。

回退顺序：停新queue/effect claim → 保留只读事实与兼容reader → 新launch回到已验证的legacy/shadow组合 → 关闭新增写scope。已发送消息、receipt、audit与canonical事实不能回滚抹除。发布撤回/reissue另需精确目标授权。

参考契约：

- `docs/superpowers/specs/2026-08-04-runtime-operations-contract.md`
- `docs/superpowers/specs/2026-08-05-runtime-operations-next-iteration-design.md`
- `docs/v2/{ROADMAP,IMPLEMENTATION-ROADMAP,ARCHITECTURE}.md`
- 当前目录/模块`AGENTS.md`与`docs/reference/{aep-protocol,events,admin-api,metrics,configuration,cli}.md`

平台接入前必须核对当时官方contract；本轮检索到的入口包括Slack `chat.postMessage`、飞书“发送消息”与GitHub“Reusing workflows / Artifact attestations”。本方案不凭搜索摘录授予永久幂等或签名可用保证。

## 10. Luna 执行清单

取得相应实施授权后，按以下顺序执行；本文本身不触发实施。

1. [ ] 对照§5.1核对HEAD/分支/工作区/Issue，读取受影响module规则，登记本批新发现与first-slice边界。
2. [ ] A：先RED匹配回归，再包图闭包与fallback，验证实际选择集合；完成原子commit。
3. [ ] B：typedmode/durability与duplicate事实、SDK/schema/UI同步；验证旧客户端行为；完成协议commit。
4. [ ] F1：同SHA验证→build→verify→publish依赖与故意失败负例；完成workflowcommit。
5. [ ] C1：stableoccurrence与canonicalsysteminput，保留print/EOF；完成inputcorrelationcommit。
6. [ ] C2：payload/effect intent成对migration与事务、crash恢复；完成storagecommit。
7. [ ] C3：Slackreceipt、threadtarget、legacy/gateway互斥；完成deliveryownercommit。
8. [ ] C4：lease/unknown/reconcile/operator与故障矩阵，真PG验证；再扩飞书/Webhook，分别记录覆盖。
9. [ ] D1–D3：内部指纹/authority、一次prepare与shadowparity、逐组合authoritative；每自洽单元验证提交。
10. [ ] E：compat/strictenv与实际能力报告，singleton边界与跨OS tests；验证提交。
11. [ ] Q1–Q2：canonicalqueued migration、事务ACK、FIFO/lifecycle/cancel/Skill/UI移交；真PG/故障测试后提交。
12. [ ] F2：workerlock、原生发布产物smoke、SBOM/签名/provenance验证；未授权不实际发布。
13. [ ] G：有界Adminprojection、timeline与安全actions；鉴权/409/中英文/UI验证后提交。
14. [ ] H：先核对#870真实启动条件；不满足时只保留模板说明，满足后实现两个版本化试点。
15. [ ] 每单元检查stageddiff仅含本任务，hooks完整；实施分支按项目规则验证后push，逐轮审查当前HEAD的P0/P1与有价值P2。
16. [ ] 最终汇总：实际完成范围、commit hashes、对应Issue/PR、每项Source/Test/Live证据、SQLite/真实PG/平台覆盖、剩余unknown与未执行项。不能沿用原对话测试计数作为本轮完成证明。

首个可执行交付包为 **A+B+F1**；下一包为 **C1–C4**。共享工作区或企业严格权限上线以前，D/E必须同时满足。G/H按前置证据推进，不以新页面数量衡量基础闭环完成。
