# HOTPLEX 2.0 Roadmap

> HotPlex 2.0 将现有 Gateway、Session、Worker、AEP、Execution、Audit、Observability 和 Cron 收敛为稳定的 Agent Runtime Gateway 契约。

## 文档定位

本文是 HotPlex 2.0 产品定位、当前事实、目标状态、阶段边界、实施优先级和完成定义的唯一主线。

- [Implementation Roadmap](./IMPLEMENTATION-ROADMAP.md) 展开交付切片、依赖和阶段闸门。
- [Architecture](./ARCHITECTURE.md) 定义组件职责、事实所有权、数据流和兼容边界。
- Approved specs 定义尚未交付能力的冻结实现契约。
- `docs/research/` 保存源码、测试、Issue、外部规范和 ROI 证据，不覆盖本文决策。

状态口径：

| 状态 | 含义 |
| --- | --- |
| 已交付 | 当前代码、测试和发布记录均存在 |
| 已冻结 | 契约已批准，实现尚未全部完成 |
| 当前实施 | 已进入明确 Issue 和交付切片 |
| 暂缓 | 不进入当前阶段，具备显式启动条件 |
| 非目标 | 不属于 HotPlex 2.0 产品职责 |

## 产品定位

HotPlex 2.0 是 **self-hosted Agent Runtime Gateway**：

- 在受控 workspace 中运行 Claude Code、OpenCode Server、Codex CLI 和 ACP agent；
- 提供远程会话、工作区绑定、身份与权限边界、AEP 事件流、执行可靠性、审计、可观测性和恢复能力；
- 为 WebChat、Slack、Feishu、HTTP/WebSocket 集成提供统一 runtime；
- 以单 Gateway 的稳定运行时契约为核心，平台化能力受运行时事实和 SLO 约束。

核心用户与价值：

| 用户 | 主要诉求 | HotPlex 2.0 价值 |
| --- | --- | --- |
| 开发团队 | 远程、安全、可恢复地运行 coding agents | workspace-bound session、provider-neutral worker、resume/context recovery |
| 平台与运维 | 管理、诊断和审计 agent 行为 | execution facts、runtime diagnostics、audit/trace correlation |
| 集成开发者 | 通过 WebChat、消息平台和 API 接入 runtime | stable AEP、runtime metadata、adapter-neutral contract |
| 企业安全 | 解释谁在何处以什么权限执行了什么 | identity、policy evidence、isolation report、redacted audit |

## 非目标

| 非目标 | 产品边界 |
| --- | --- |
| 模型聚合网关 | 模型路由不是 HotPlex 的核心差异 |
| 通用 workflow/DAG SaaS | 编排能力只能建立在稳定 runtime contract 之上 |
| 独立 memory 产品或向量知识库 | RuntimeContext 复用 eventstore、turns 和 worker history |
| Agent/skill marketplace | Capability contract 和安全 materialization 先于 registry |
| 平行 event bus、identity service 或 scheduler | AEP、eventstore、现有身份链和单机 runtime 是事实基础 |
| 形式化隔离承诺 | 只报告可观察、可验证的 enforcement 能力 |

## 当前已交付能力

当前代码已经形成 HotPlex 2.0 的运行时内核：

| 能力 | 当前事实 | 状态与来源 |
| --- | --- | --- |
| Gateway | WebSocket + HTTP session 入口，执行 AEP 分发、init 握手、用户/workspace 校验 | 已交付 |
| Session | `CREATED/RUNNING/IDLE/TERMINATED/DELETED` 状态机，SQLite/PostgreSQL 持久化，用户与 workspace 绑定 | 已交付 |
| Worker | `claude_code`、`opencode_server`、`codex_cli`、`acp` 注册式适配器 | 已交付 |
| AgentSpec | config、init metadata、workspace 和 worker 选项的只读归一化模型 | 已交付，[#847](https://github.com/hrygo/hotplex/issues/847) |
| EffectiveRuntimePlan | desired-state plan 的 redacted 投影 + canonical hash；WS/REST 影子解析、Admin/doctor 只读路径（dispatch 仍以 legacy 参数为准） | shadow first slice 已交付，[#946](https://github.com/hrygo/hotplex/issues/946) |
| AgentIdentity | identity 贯穿 session context、runtime metadata、audit 和 trace | 已交付，[#848](https://github.com/hrygo/hotplex/issues/848) |
| AEP | `pkg/events` 是唯一 wire contract；Envelope、Metadata、OwnerID 和最小 `runtime.execution.*` 事件可用 | 已交付基线；完整 runtime observability 仍由 [#849](https://github.com/hrygo/hotplex/issues/849) 跟踪 |
| Event Store | inbound/outbound 事件持久化、turn 聚合、崩溃/超时 synthetic turn | 已交付 |
| Durable Ingress / Execution | input payload 指纹、owner instance、lease、delivery/runtime 状态分离、single-active gate、`SESSION_BUSY`、ambiguity fence、late convergence、有界 repairer | 已交付，[#878](https://github.com/hrygo/hotplex/issues/878) |
| Runtime Operations | fenced execution operator escape hatch：`fences list` / `resolve` / `abandon`，`fence_version` 条件并发保护（409 冲突）、审计（actor/reason/evidence_ref）、Admin API + CLI + doctor 检查 | 已交付，[#877](https://github.com/hrygo/hotplex/issues/877) |
| RuntimeContext | 从 eventstore、turns、worker session 和 workspace metadata 读取上下文的持久化接口 | 已交付，[#852](https://github.com/hrygo/hotplex/issues/852) |
| Observability | OpenTelemetry bootstrap、runtime spans、Prometheus worker/session/gateway 指标 | 已交付，[#850](https://github.com/hrygo/hotplex/issues/850) |
| Security / Audit | API key、cookie admin fallback、workspace owner 校验、tool/user/admin audit、链式完整性校验 | 已交付 |
| Cron | 定时任务、执行超时、重试、平台投递和指标 | 已交付；durable external effect 仍未形成统一契约 |
| Multitenancy | 用户、workspace、OAuth/SSO、多 bot、多 workspace 配置 | 已交付 |

## 目标运行时模型

HotPlex 2.0 使用五层事实闭环：

```text
Authority / Scope / Capability
              ↓
Desired Runtime Plan
              ↓
Durable Execution / Gateway-owned Effect
              ↓
Observed Worker / Provider / Delivery State
              ↓
Reconciliation / Repair / Fence / Operator Action
```

| 事实层 | 作用 | Canonical owner |
| --- | --- | --- |
| Authority / Scope | 确定 principal、workspace、bot、session 和 capability ownership | 现有 identity/config/session 边界 |
| Desired Runtime Plan | 表达 redacted、可 hash、可解释的期望 worker、policy、sandbox、env 和 capability | `EffectiveRuntimePlan` resolver |
| Durable Execution | 表达 input 是否 accepted、owned、delivered、running 或 terminal | `internal/execution` |
| Gateway-owned Effect | 表达 delivery、Webhook、Cron、control、recipe 等外部副作用的 claim、attempt、idempotency 和 outcome | `EffectLedger` |
| Observed State | 表达实际 worker/backend/artifact、provider receipt、delivery acknowledgement 和 evidence | worker/connector adapter |
| Reconciliation | 依据 desired 与 observed 差异执行 query、repair、fence 或 operator action | 有界 reconciler/repairer |

`plan_hash` 只证明期望计划；Worker `done` 只证明 worker turn 终态；HTTP 成功、audit 记录和 timeout 均不能单独证明外部 effect 已成立。证据不足的结果保持 `unknown`，并进入 reconcile 或 fence。

## 设计不变量

1. **演进现有内核**：扩展 Gateway、Session、Worker、AEP、Execution 和 config，不绕开它们。
2. **唯一事实源**：authority、plan、execution、effect、observed state 和 reconciliation 各有唯一写入者；UI、adapter 和 admin 只读投影。
3. **AEP 唯一 wire contract**：新字段增量兼容，并同步 Go SDK、三种示例 SDK、协议文档和双向测试。
4. **敏感数据最小化**：execution、effect、audit、event 和 Cockpit 不保存 prompt、metadata value、secret、credential、raw provider request、完整 tool args 或 raw worker error。
5. **双数据库一致**：SQLite/PostgreSQL migration、条件更新、跨实例和故障测试成对实现。
6. **安全能力诚实**：authentication、authorization、capability declaration、filesystem/network enforcement、credential injection 和 audit 分开建模；无法证明的能力是 `partial` 或 `unavailable`。
7. **未知结果优先安全**：timeout、连接中断和响应丢失不自动等于 failed；repair/reconcile 具备 lease、attempt cap、backoff、stop condition 和 operator escape hatch。
8. **低基数可观测性**：metrics 只使用有限枚举；plan hash、workspace 和 provider/evidence ref 进入脱敏 trace/log/audit correlation。
9. **有界运营成本**：plan、effect、audit、snapshot 和 Cockpit 具备 retention、分页、payload、并发和数据库连接预算。
10. **小切片可回滚**：新事实层先 read-only 或 shadow，保持旧版本读取和现有 dispatch 语义，具备暂停与回退路径。

## Stage 1: Runtime Contract

### 当前事实

- AgentSpec、AgentIdentity、RuntimeContext 和 runtime spans 已交付；
- #946 shadow first slice 已交付：`EffectiveRuntimePlan` resolver（redacted view + canonical hash + fail-closed blocked codes）在 WS/REST 入口影子运行，Admin `runtime-plan` 与 doctor `runtime.effective_plan` 提供只读投影；dispatch 仍以 legacy SessionStartParams 为准；
- AEP 已具备最小 execution metadata/events，完整 runtime observability 契约仍未完成；
- 四类 Worker 继续使用现有 `worker.Register()` 和 adapter contract。

### 冻结契约

- [#849](https://github.com/hrygo/hotplex/issues/849)：补齐 agent/runtime/security/audit 事件和协议兼容；
- [#946](https://github.com/hrygo/hotplex/issues/946)：`EffectiveRuntimePlan` 统一 WS、REST、doctor、worker start、admin diagnostics 和 recipe dry-run；
- [#867](https://github.com/hrygo/hotplex/issues/867)：显式 worker environment allowlist、compat/strict profile 和 isolation capability report；
- [Runtime Operations Contract](../superpowers/specs/2026-08-04-runtime-operations-contract.md)：定义 plan、preflight、observed bootstrap 和 redaction。

### 完成定义

- 等价 WS/REST 输入产生相同 canonical plan hash；
- doctor、worker start、admin diagnostics 和 recipe dry-run 使用同一 resolver；
- 四类 Worker 均报告 declared/observed/enforced/partial/unavailable；
- 旧 AEP v1 客户端可忽略新事件并继续工作；
- prompt、secret、raw worker error 和完整 tool args 不进入 durable facts。

## Stage 2: Runtime Reliability

### 当前事实

- #878 已交付 single-active gate、owner lease、delivery/runtime 分离、ambiguity fence、late convergence 和有界 repairer；
- #877 已交付 fenced execution 的 operator escape hatch：fence 持久化（`fence_created_at`/`fence_version`）、条件决策（resolve/abandon，跨实例 409 冲突）、审计、Admin API、CLI 与 doctor 检查；fence 永不伪装成功、不自动重投；
- Cron/platform delivery 仍依赖进程内 retry 路径，不能作为重启和多实例后的最终事实源。

### 冻结契约

- [#877](https://github.com/hrygo/hotplex/issues/877)：为 fenced execution 提供有审计的 operator escape hatch；
- [#947](https://github.com/hrygo/hotplex/issues/947)：Gateway-owned `EffectLedger`；
- 第一条垂直切片覆盖 Cron、Webhook 和消息 delivery 的 `desired → effect → observed → reconcile`；
- worker-private tool/model-provider 内部调用不进入 Gateway EffectLedger。

### 完成定义

- claim、attempt、effect fact、idempotency key 和 external evidence 明确分离；
- 重启、多实例竞争、DB 短暂失败、lease expiry、provider timeout/5xx、成功但响应丢失和晚到确认均有 fault-injection 测试；
- external ack 只在 effect 已确认成功后写入，无法确认时保持 `unknown`；
- SQLite/PostgreSQL migration 和条件状态迁移成对通过；
- repair/reconcile 可停止、可限流、可 fence、可由 operator 接管。

## Stage 3: Runtime Operations

### 当前事实

- RuntimeContext、eventstore、execution、audit、trace 和 metrics 已提供运营事实基础；
- fence operator action（#877）已按统一 authorization（Bearer + `runtime:*` scope）、redaction（无密列表投影）和 audit（`user_activity` + slog 双记录）交付，是本 stage "diagnostics、reconciliation 和 operator action 统一约束" 的第一个落地切片；
- 当前 admin/runtime UI 尚未形成统一 execution timeline。

### 冻结契约

- [#851](https://github.com/hrygo/hotplex/issues/851)：bounded `ExecutionQueue`，负责 FIFO ordering metadata、attempt、timeout/retry reason 和 queue state，不重复 #878；
- [#868](https://github.com/hrygo/hotplex/issues/868)：只读 Execution Cockpit，消费 canonical plan/execution/effect/observed/audit/trace facts；
- diagnostics、reconciliation 和 operator action 使用统一 authorization、redaction 和 audit。

### 完成定义

- 同一 session 的输入顺序可解释、可审计、可恢复；
- Cockpit 能区分 input acceptance、worker completion、external effect、unknown、fence 和 reconciled；
- 查询具备 snapshot、分页、时间窗、payload 上限、workspace/session authorization 和数据库预算；
- queue 与 Cockpit 不创建第二套 execution、effect、event bus 或 idempotency state；
- API、WebChat、authorization、中文与英文文案测试通过。

## Stage 4: Conditional Platform Expansion

### 冻结契约

- [#948](https://github.com/hrygo/hotplex/issues/948)：scope-aware capability inventory、precedence、hash、safe materialization 和 admin-gated promotion；
- [#870](https://github.com/hrygo/hotplex/issues/870)：基于 Cron、Webhook、Session、Worker、EffectiveRuntimePlan 和 EffectLedger 的 versioned Coding Ops Recipes；
- [Recipes 模板草案](RECIPES-TEMPLATES.md)：两个试点的模板与 fail-closed 条件。**启动条件未满足**（2026-10-04 核对），仓库内没有 registry、manifest 校验或 dry-run 执行器；
- [Scope-aware Capability Inventory Contract](../specs/Scope-Aware-Capability-Inventory-Spec.md) 定义 capability 的解释与安全投影边界。

### 启动条件

- Stage 1–3 的 plan divergence、unknown age、reconcile、delivery loss/duplicate、数据库容量和定位能力具有持续运行证据；
- 至少一个真实业务场景证明单 Agent runtime + tools/context 无法满足；
- 新能力复用现有 Gateway、Session、Execution、AEP、eventstore、audit 和 observability，不建立平行控制面。

### 暂缓能力

- 跨 session 分布式 scheduler；
- multi-agent workflow/DAG；
- 外部 memory product；
- Agent/skill marketplace 和 public registry；
- Kubernetes operator 或独立多租户 SaaS control plane；
- SPIFFE/SPIRE 等跨主机 workload identity，直至跨节点信任成为硬需求。

## 当前实施优先级

| 顺序 | 交付切片 | Issue | 状态 | 依赖关系 |
| ---: | --- | --- | --- | --- |
| 1 | Fenced execution operator escape hatch | [#877](https://github.com/hrygo/hotplex/issues/877) | 当前实施 | 消除无界 session lockout |
| 2 | EffectiveRuntimePlan + observed bootstrap | [#946](https://github.com/hrygo/hotplex/issues/946) | 已冻结 | 复用 #847，约束 #867/#868/#870 |
| 3 | Explicit env allowlist + isolation report | [#867](https://github.com/hrygo/hotplex/issues/867) | 已冻结 | 由 #946 的统一 plan 驱动 |
| 4 | Cron/Webhook/message durable effect | [#947](https://github.com/hrygo/hotplex/issues/947) | 已冻结 | 复用 #878 identity/lease/fence |
| 5 | Bounded ExecutionQueue | [#851](https://github.com/hrygo/hotplex/issues/851) | 已冻结 | 消费 #947 effect facts，不重复 #878 |
| 6 | Execution Cockpit | [#868](https://github.com/hrygo/hotplex/issues/868) | 已冻结 | 消费 #946/#947/#851 canonical facts |
| 7 | Coding Ops Recipes | [#870](https://github.com/hrygo/hotplex/issues/870) | 已冻结 | 依赖 #946 dry-run 与 #947 effect contract |
| 8 | Capability inventory/materialization | [#948](https://github.com/hrygo/hotplex/issues/948) | 已冻结 | 依赖稳定 plan、scope 和 isolation vocabulary |

通用抽象只由已验证的垂直切片提取。第一条 durable effect 闭环完成前，不扩展分布式 queue、workflow 或 marketplace。

## 成功指标

### Runtime Contract

- 四类 Worker 共享 AgentSpec、EffectiveRuntimePlan 和 capability vocabulary；
- AEP event、trace、audit 和 eventstore 使用一致的 agent/session/execution correlation keys；
- declared、observed、enforced、partial、unavailable 不混用；
- 旧客户端和现有 worker config 保持兼容。

### Runtime Reliability

- 关键副作用能区分 desired、claimed、attempted、observed、unknown、fenced 和 reconciled；
- Worker `done` 不被记录为外部 delivery 成功；
- 至少一条 Cron/Webhook/message delivery 在重启、多实例、timeout 和晚到确认下收敛；
- SQLite/PostgreSQL 和四 Worker 的 fault/race 测试覆盖一致。

### Runtime Operations

- 操作者能从 session/execution 定位 init、dispatch、worker、effect、provider 和 delivery 的故障段；
- unknown age、reconcile result、delivery loss/duplicate、queue state 和 DB pool saturation 可观测；
- Cockpit 和 diagnostics 的查询、权限、redaction 和容量边界可验证。

### 文档与发布

- ROADMAP、Implementation Roadmap、Architecture、approved specs 和 GitHub Issues 可双向追踪；
- AEP/Worker/数据库变更同步代码、测试、SDK 和文档；
- `make check`、`make docs-build` 和受影响模块 `-race -count=1` 通过；
- 发布、迁移和 repair 均具备暂停、回退和 operator action 路径。

## Issue 与 Spec 追踪矩阵

| Issue / Spec | 状态 | 路线归属 | 终态职责 |
| --- | --- | --- | --- |
| [#847](https://github.com/hrygo/hotplex/issues/847) AgentSpec | closed | Stage 1 | runtime config normalized view |
| [#848](https://github.com/hrygo/hotplex/issues/848) AgentIdentity | closed | Stage 1 | identity correlation |
| [#849](https://github.com/hrygo/hotplex/issues/849) Runtime AEP Events | open | Stage 1 | 完整 runtime event contract |
| [#850](https://github.com/hrygo/hotplex/issues/850) Runtime Observability | closed | Stage 1 | runtime spans/metrics |
| [#852](https://github.com/hrygo/hotplex/issues/852) RuntimeContext | closed | Stage 3 基线 | context persistence/read facade |
| [#878](https://github.com/hrygo/hotplex/issues/878) Durable Ingress | closed | Stage 2 基线 | input ledger、lease、fence、repair |
| [#877](https://github.com/hrygo/hotplex/issues/877) Fence Escape Hatch | open | Stage 2 | operator recovery |
| [#946](https://github.com/hrygo/hotplex/issues/946) EffectiveRuntimePlan | open | Stage 1 | plan/preflight/observed bootstrap |
| [#867](https://github.com/hrygo/hotplex/issues/867) Isolation Profiles | open | Stage 1 | env allowlist/isolation report |
| [#947](https://github.com/hrygo/hotplex/issues/947) EffectLedger | open | Stage 2 | Gateway-owned external effects |
| [#851](https://github.com/hrygo/hotplex/issues/851) ExecutionQueue | open | Stage 3 | bounded ordering/attempt/queue state |
| [#868](https://github.com/hrygo/hotplex/issues/868) Execution Cockpit | open | Stage 3 | canonical runtime facts view |
| [#870](https://github.com/hrygo/hotplex/issues/870) Coding Ops Recipes | open | Stage 4 | versioned runtime recipes |
| [#948](https://github.com/hrygo/hotplex/issues/948) Capability Inventory | open | Stage 4 | scope-aware inventory/materialization |
| [Runtime Operations Contract](../superpowers/specs/2026-08-04-runtime-operations-contract.md) | approved | Stage 1–3 | plan/effect/observed/reconcile contract |
| [Scope-aware Capability Inventory Contract](../specs/Scope-Aware-Capability-Inventory-Spec.md) | approved | Stage 4 | capability projection contract |

## 设计依据

- [HotPlex 2.0 Architecture](./ARCHITECTURE.md)
- [HotPlex 2.0 Implementation Roadmap](./IMPLEMENTATION-ROADMAP.md)
- [HotPlex 2.0 终版路线图文档架构](../superpowers/specs/2026-08-04-final-roadmap-documentation-architecture-design.md)
- [qm 深度研究证据](../research/2026-08-04-qm-hotplex-deep-research-report.md)
- [qm 事实闭环证据](../research/2026-08-04-qm-hotplex-second-order-calibration.md)
- [qm 正交架构审查证据](../research/2026-08-04-qm-hotplex-orthogonal-architecture-review.md)
- OpenAI Agents SDK: https://openai.github.io/openai-agents-python/
- Model Context Protocol: https://modelcontextprotocol.io/specification/2025-11-25
- OpenTelemetry semantic conventions: https://opentelemetry.io/docs/specs/semconv/
- Temporal durable execution: https://docs.temporal.io/
- Kubernetes controllers: https://kubernetes.io/docs/concepts/architecture/controller/
- NIST Zero Trust Architecture: https://csrc.nist.gov/pubs/sp/800/207/a/final
