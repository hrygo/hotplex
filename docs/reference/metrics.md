---
title: "Metrics 参考"
description: "HotPlex Gateway 暴露的所有 OTel 指标完整参考"
---

# Metrics 参考

> 所有 OTel 指标的名称、类型、标签和使用场景参考。

## 概述

HotPlex Gateway 通过 `internal/observability/` 包使用 OTel Meter API 注册指标。所有指标前缀为 `hotplex.`（点分隔符），通过 OTel Prometheus Exporter 以标准 Prometheus 格式暴露。应用代码零直接依赖 `prometheus/client_golang`。

## 语义键与基数契约

> 单一真相源：`internal/observability/keys.go`。跨 AEP 事件元数据、slog 日志字段、OTel span/metric 属性的字符串键名必须取自此处，禁止散落字面量（`worker_type` 曾出现 40+ 次、`session_id` 60+ 次、`execution_id` 12 次、`trace_id` 3 次且无常量）。

HotPlex 的三平面关联（AEP 事件元数据、结构化日志、分布式追踪/指标）共享同一套语义键。`keys.go` 把它们集中为常量，并以机器可校验的方式声明基数契约：

**低基数键（metric-safe）** — 可自由用作 metric label、slog 字段、span 属性、AEP 元数据：

| 键 | 用途 |
|----|------|
| `worker_type` | `claude_code` / `codex_cli` / `opencode_server` / `acp` |
| `platform` | `webchat` / `feishu` / `slack` / `yuanxin` / `cron` |
| `reason` / `status` / `result` | 终止 / 状态 / 结果枚举 |
| `exit_code` / `raw_exit_code` | Worker 退出分类 / OS 原始退出码 |
| `delivery_status` / `runtime_status` / `terminal_status` | 执行投递 / 运行时 / turn 终态枚举 |
| `stage` / `first_output` / `tool` | turn 阶段 / 首输出类型 / 工具名（受注册表约束） |
| `error_type` / `event_type` / `direction` | 错误分类 / AEP 事件 Kind / 入站·出站 |

**高基数键（禁止作 metric label）** — 仅可作 span 属性、slog 字段、AEP 元数据、审计 detail：

| 键 | 说明 |
|----|------|
| `agent_id` / `user_id` / `workspace_id` | 身份与多租户锚（值与 `agentspec.MetadataKey*` 同源，由 `keys_consistency_test.go` 锁定不漂移，#848） |
| `execution_id` | 单次输入执行 id（#878） |
| `session_id` | Gateway 会话 id |

**关联标识** — `trace_id` 与 `span_id` 由 `Hub.SendToSession` 注入 AEP 元数据（#850 在原有 `trace_id` 基础上补 `span_id`），使下游能把事件关联到产出它的精确 span，而不止于 trace。两者在采样窗口内低基数，但不作 metric label（无有意义的分布划分）。

基数契约由 `MetricSafeKeys` 白名单 + `IsMetricSafe()` 在代码层强制；`keys_metric_test.go` 断言无高基数键混入白名单，`keys_consistency_test.go` 断言身份键与 agentspec 不漂移。新增 metric label 是显式行为，须同步更新本节。下文每张指标表的 label 均须来自上述低基数键集合。

## 采集端点

指标通过 Admin API 暴露：

```
GET http://localhost:9999/admin/metrics
```

Prometheus scrape 配置示例：

```yaml
scrape_configs:
  - job_name: 'hotplex-gateway'
    static_configs:
      - targets: ['localhost:9999']
    metrics_path: '/admin/metrics'
    scrape_interval: 15s
```

## Session 指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.session.created` | Counter | 累计创建 Session 数，label: `worker_type` |
| `hotplex.session.terminated` | Counter | 终止 Session 数，label: `reason` |
| `hotplex.session.deleted` | Counter | GC 物理删除 Session 数 |
| `hotplex.session.start.attempts` | Counter | Session 启动尝试次数，label: `worker_type` |
| `hotplex.session.start.errors` | Counter | Session 启动错误数 |
| `hotplex.session.start.duration` | Histogram | Session 启动耗时 |

### 终止原因标签

| reason | 说明 |
|--------|------|
| `idle_timeout` | 空闲超时 |
| `max_lifetime` | 最大生命周期 |
| `client_kill` | 客户端主动终止 |
| `admin_kill` | Admin API 终止 |
| `zombie` | Zombie 检测（无响应） |
| `crash` | Worker 进程崩溃 |

### 常用查询

```promql
# Session 创建速率
rate(hotplex_session_created_total[5m])

# 异常终止率
rate(hotplex_session_terminated_total{reason=~"crash|zombie"}[5m])

# Session 启动 P99 延迟
histogram_quantile(0.99, rate(hotplex_session_start_duration_bucket[5m]))
```

## Worker 指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.worker.starts` | Counter | Worker 启动次数，label: `worker_type`, `result` |
| `hotplex.worker.execution.duration` | Histogram | Worker 执行时长，label: `worker_type` |
| `hotplex.worker.crashes` | Counter | Worker 崩溃次数，label: `worker_type`, `exit_code`, `raw_exit_code` |
| `hotplex.worker.memory.bytes` | ObservableGauge | Worker 估算内存，label: `worker_type` |
| `hotplex.worker.creation.duration` | Histogram | Worker 进程创建耗时 |

### 常用查询

```promql
# Worker 可用性 SLO
1 - (rate(hotplex_worker_crashes_total[5m]) / rate(hotplex_worker_starts_total[5m]))

# P95 执行时长
histogram_quantile(0.95, rate(hotplex_worker_execution_duration_bucket[5m]))

# Worker 内存总量
sum by (worker_type) (hotplex_worker_memory_bytes)
```

## Gateway Forwarder 指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.gateway.forwarder.panics` | Counter | Worker 事件转发 goroutine 已恢复的 panic 数，label: `worker_type` |

## Turn TTFT 指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.turn.ttft` | Histogram | Gateway 收到输入至首个可见 Worker 输出的耗时，label: `worker_type`, `first_output`（`reasoning` / `text`） |
| `hotplex.turn.first_text_latency` | Histogram | Gateway 收到输入至首个文本 delta 的耗时，label: `worker_type` |
| `hotplex.turn.stage_duration` | Histogram | admission、dispatch、first_output 三个阶段耗时，label: `worker_type`, `stage` |
| `hotplex.turn.without_output` | Counter | 未产生可见输出即终止的 turn，label: `worker_type`, `terminal_status` |

TTFT 仅使用 Gateway 侧时间戳；浏览器绘制时间属于独立客户端遥测，不能与本指标混合。所有标签均为有限枚举，严禁添加 session、execution、user、workspace、提示词或原始错误等高基数字段。Worker 尚未绑定即终止的 turn 使用固定的 `worker_type="unknown"`。

阶段边界依次为 Gateway 收到输入、持久化接受完成、`Worker.Input` 成功（包括 Worker 的 `turn/start` 接受）以及首个可见输出；`first_output` 仅统计最后一段至首输出的耗时。

### 常用查询与评审阈值

```promql
# 按 Worker 类型查看 p50 / p95 / p99 TTFT
histogram_quantile(0.50, sum by (le, worker_type) (rate(hotplex_turn_ttft_bucket[15m])))
histogram_quantile(0.95, sum by (le, worker_type) (rate(hotplex_turn_ttft_bucket[15m])))
histogram_quantile(0.99, sum by (le, worker_type) (rate(hotplex_turn_ttft_bucket[15m])))

# p95 的首个输出前阶段拆分
histogram_quantile(0.95, sum by (le, stage, worker_type) (
  rate(hotplex_turn_stage_duration_bucket{stage=~"admission|dispatch|first_output"}[15m])
))

# 无输出终止占比
sum(rate(hotplex_turn_without_output_total[15m])) by (terminal_status, worker_type)
/
sum(rate(hotplex_turn_ttft_count[15m])) by (worker_type)
```

以同一 Worker 类型、相同流量窗口的基线为准：p95 TTFT 连续 30 分钟高于基线 20%，或 p99 连续 15 分钟超过 30 秒时必须评审；先用阶段指标定位 admission、dispatch 或 provider/Worker 首输出，再决定是否投入冷路径优化。

## Gateway 指标

### 连接与消息

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.gateway.connections` | ObservableGauge | 当前 WebSocket 连接数 |
| `hotplex.gateway.webchat.session_owner_connections` | ObservableGauge | 当前拥有 WebChat WebSocket owner 的 Session 数 |
| `hotplex.gateway.messages` | Counter | 消息总数，label: `direction`, `event_type` |
| `hotplex.gateway.events` | Counter | 转发事件总数，label: `event_type`, `direction` |
| `hotplex.gateway.init.handshake.duration` | Histogram | WS 握手耗时 |

### 背压指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.gateway.deltas.dropped` | Counter | 因背压丢弃的 `message.delta` 事件数 |
| `hotplex.gateway.platform.dropped` | Counter | 平台连接缓冲区满时丢弃的事件数，label: `event_type` |
| `hotplex.gateway.no_subscribers.dropped` | Counter | 无订阅者时丢弃的事件数，label: `event_type` |

### Delta 聚合指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.gateway.delta.coalesced` | Counter | 被 coalescer 合并的 delta 事件数 |
| `hotplex.gateway.delta.flush` | Counter | 合并后刷新到平台连接的次数 |

### 错误指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.gateway.errors` | Counter | 错误总数，label: `error_code` |
| `hotplex.gateway.webchat.duplicate_connection_rejected` | Counter | 因已有 WebChat owner 被拒绝的 init 数 |
| `hotplex.gateway.webchat.non_owner_ingress_rejected` | Counter | 非 owner 的敏感入站事件拒绝数 |
| `hotplex.gateway.webchat.owner_release_not_current` | Counter | 非当前 owner 尝试释放 owner 的次数 |

### 常用查询

```promql
# 每秒消息速率
rate(hotplex_gateway_messages_total[5m])

# Delta 丢弃率
rate(hotplex_gateway_deltas_dropped_total[5m])

# 按事件类型的消息分布
sum by (event_type) (rate(hotplex_gateway_messages_total{direction="outgoing"}[5m]))
```

## Pool 配额指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.pool.acquire` | Counter | 配额获取尝试次数，label: `result` |
| `hotplex.pool.release.errors` | Counter | 双重释放错误数（表示 bug） |
| `hotplex.pool.utilization` | ObservableGauge (float) | 资源池利用率（0-1），活跃会话数 / `max_size` |
| `hotplex.pool.active_sessions` | ObservableGauge | 活跃 worker 会话数（全局，含 platform/cron） |
| `hotplex.pool.distinct_users` | ObservableGauge | 至少有一个活跃会话的去重用户数 |
| `hotplex.pool.distinct_workspaces` | ObservableGauge | 至少有一个活跃会话的去重 WebChat workspace 数（不含 platform 会话，spec ⑤） |
| `hotplex.pool.memory_reserved_bytes` | ObservableGauge | per-user 内存配额下预留的字节数（仅 `max_memory_per_user` 设置时累加；512MB/worker 估算值，非实际 RSS，spec ⑤） |

### 获取结果标签

| result | 说明 |
|--------|------|
| `success` | 成功获取 |
| `pool_exhausted` | 全局 Worker 数已满 |
| `user_quota_exceeded` | 单用户 Session 数已满 |
| `workspace_quota_exceeded` | 单 workspace Session 数已满（`max_per_workspace`，spec ①） |
| `memory_exceeded` | 单用户内存配额超限（`max_memory_per_user`） |

### 常用查询

```promql
# 配额拒绝率
rate(hotplex_pool_acquire_total{result!="success"}[5m])

# 双重释放检测
increase(hotplex_pool_release_errors_total[1h])
```

## Cron 指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.cron.fires` | Counter | 任务触发次数，label: `job_name` |
| `hotplex.cron.errors` | Counter | 执行错误数，label: `job_name`, `error_type` |
| `hotplex.cron.duration` | Histogram | 执行时长，label: `job_name` |
| `hotplex.cron.attached` | Counter | Session-attached cron 投递次数 |
| `hotplex.cron.delivery.result` | Counter | 投递结果，label: `status`（success / exhausted / permanent） |

### 常用查询

```promql
# Cron 成功率
rate(hotplex_cron_fires_total[5m]) - rate(hotplex_cron_errors_total[5m])

# 按任务名的平均执行时长
rate(hotplex_cron_duration_sum[5m]) / rate(hotplex_cron_duration_count[5m])
```

## Streaming Card 指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.streaming.card.rotations` | Counter | Streaming Card TTL 触发的旋转次数 |
| `hotplex.streaming.card.rotation_failures` | Counter | 旋转失败数，label: `phase` |
| `hotplex.streaming.card.flush_fallbacks` | Counter | CardKit 降级到 IM Patch 的次数 |
| `hotplex.streaming.terminal_failures` | Counter | 平台 streaming 终态失败次数（Feishu card / Slack stream），label: `platform`（当前仅 Slack 路径携带，值为 `slack`；Feishu 路径不携带该 label）+ `fallback_result`（`sent` / `failed` / `skipped_body_presented`） |

## ACP 指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.acp.prompt_tokens` | Counter | ACP Worker 处理的 prompt token 数 |
| `hotplex.acp.tool_calls` | Counter | ACP Worker 工具调用次数 |
| `hotplex.acp.permission_requests` | Counter | ACP 权限请求次数 |
| `hotplex.acp.handshake.duration` | Histogram | ACP JSON-RPC 握手耗时 |

## LLM 重试指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.retry.attempts` | Counter | LLM 重试尝试次数 |
| `hotplex.retry.exhaustion` | Counter | 重试耗尽（最终失败）次数 |

## Execution 指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.execution.accept` | Counter | 新输入被持久化接受的次数 |
| `hotplex.execution.duplicate` | Counter | 幂等去重（相同 Envelope ID + payload 静默抑制） |
| `hotplex.execution.conflict` | Counter | Payload 冲突（相同 Envelope ID，不同 SHA-256 hash） |
| `hotplex.execution.session_busy` | Counter | Active gate 拒绝（session 已有 pending/running execution） |
| `hotplex.execution.mid_turn_injected` | Counter | 用户追问在 busy 时被透传注入当前 turn（worker 支持 mid-turn） |
| `hotplex.execution.supplement_buffered` | Counter | 用户追问在 busy 时被暂存待 done 后重投（worker 不支持 mid-turn 的兜底） |
| `hotplex.execution.delivery_outcome` | Counter | Worker 投递结果，label: `delivery_status`（delivered / unknown / failed） |
| `hotplex.execution.runtime_outcome` | Counter | Worker 运行终态，label: `runtime_status`（completed / failed / unknown） |
| `hotplex.execution.delivery_latency` | Histogram | accept 到 delivery outcome 耗时（秒），bounds: 0.01–10 |
| `hotplex.execution.runtime_duration` | Histogram | Worker turn 执行耗时（秒），bounds: 0.5–300 |

### 常用查询

```promql
# 投递成功率
sum(rate(hotplex_execution_delivery_outcome_total{delivery_status="delivered"}[5m]))
/
sum(rate(hotplex_execution_delivery_outcome_total[5m]))

# 冲突率（可能为客户端 bug 或重放攻击）
rate(hotplex_execution_conflict_total[5m])

# Active gate 拒绝率
rate(hotplex_execution_session_busy_total[5m])

# P95 投递延迟
histogram_quantile(0.95, rate(hotplex_execution_delivery_latency_bucket[5m]))
```

## Lease-Repair 指标

Durable ingress 的 owner lease 续约与终态修复子系统。

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.lease.renew_failure` | Counter | Owner lease 续约失败次数 |
| `hotplex.lease.expired_recovery` | Counter | Lease 过期后恢复（runtime 置为 unknown + 设置 fence） |
| `hotplex.repair.attempts` | Counter | 终态修复尝试次数 |
| `hotplex.repair.success` | Counter | 终态修复成功次数 |
| `hotplex.repair.timeout` | Counter | 终态修复超时（超过 MaxLifetime 放弃） |
| `hotplex.repair.dropped` | Counter | 终态修复入队失败（队列满，回退 lease recovery） |

### 常用查询

```promql
# Lease 续约失败率
rate(hotplex_lease_renew_failure_total[5m])

# 修复成功率
rate(hotplex_repair_success_total[5m]) / rate(hotplex_repair_attempts_total[5m])

# 修复超时率（indicating systemic Worker stuck）
rate(hotplex_repair_timeout_total[5m]) / rate(hotplex_repair_attempts_total[5m])
```

## Runtime Operations 指标（#877 / #946）

Operator fence 决策与 EffectiveRuntimePlan 影子解析子系统。全部低基数：
不含 execution ID、session ID、actor、plan hash（plan hash 只出现在日志与
Admin 响应中，永不作为指标 label）。

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.runtime.fence_actions` | Counter | 通过 Admin API 应用的 operator fence 决策，labels: `decision`（resolve\|abandon）、`result`（ok\|conflict\|error） |
| `hotplex.runtime.fence_conflicts` | Counter | fence-version 冲突（409，条件更新匹配 0 行）；上升说明 operator 或网关重启在 inspect 与 action 之间发生竞态 |
| `hotplex.runtime.effect_actions` | Counter | 通过 Admin API 应用的 operator 外部交付决策，labels: `decision`（abandon\|mark_delivered\|requeue）、`result`（ok\|conflict\|error） |
| `hotplex.runtime.effect_conflicts` | Counter | effect 状态冲突（409，effect 已不再是 unknown）；上升说明多个 operator 或恢复流程与人工决策竞争同一笔交付 |
| `hotplex.runtime.plan_resolutions` | Counter | EffectiveRuntimePlan 影子解析次数，label: `result`（ok\|blocked） |
| `hotplex.runtime.plan_blocked` | Counter | fail-closed 拦截的 plan，label: `code`（unknown_worker_type\|invalid_permission_mode\|invalid_sandbox_mode\|facts_missing_or_conflicting\|capability_unverifiable\|secret_shaped_value） |
| `hotplex.runtime.plan_observed` | Counter | plan 读路径返回的 observed bootstrap 状态，label: `state`（planned\|unknown\|declared\|partial\|enforced） |

### 常用查询

```promql
# 当前被 fence 阻塞的处置速率（ok 为成功决策，conflict 需人工复查后重试）
sum by (decision, result) (rate(hotplex_runtime_fence_actions_total[5m]))

# fence 冲突率（持续非零说明存在并发 operator 或重启竞态）
rate(hotplex_runtime_fence_conflicts_total[5m])

# 不确定交付的处置速率（ok 为成功决策，conflict 需人工复查后重试）
sum by (decision, result) (rate(hotplex_runtime_effect_actions_total[5m]))

# 交付决策冲突率（持续非零说明同一笔交付被并发决策）
rate(hotplex_runtime_effect_conflicts_total[5m])

# plan 拦截率（影子模式下非零即配置边界问题，blocked plan 永不静默成功）
sum(rate(hotplex_runtime_plan_blocked_total[5m]))
/
sum(rate(hotplex_runtime_plan_resolutions_total[5m]))
```

## Turn-Integrity 诊断指标

Turn-Integrity 子系统（Fix E）用于检测和量化空 success turn、stale forwarder
事件分割、assistant snapshot 漂移以及平台终态回退。

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.worker.empty_success_total` | Counter | 成功但未产出可显示内容和工具调用的 turn，label: `worker_type`, `platform` |
| `hotplex.gateway.stale_forwarder_event_total` | Counter | `/reset` 替换 Conn 后仍被旧 forwarder 观察到的事件数 |
| `hotplex.worker.assistant_snapshot_drift_total` | Counter | Full assistant snapshot 非前缀扩展被重新完整下发而非静默吞咽 |
| `hotplex.messaging.platform_terminal_fallback_total` | Counter | 平台适配器发出的合成终态消息回退：空内容占位替换（Feishu），或 streaming 关闭失败且正文未展示时的固定短文本（Slack） |

### 常用查询

```promql
# 空 success turn 占比
rate(hotplex_worker_empty_success_total_total[5m])
/
rate(hotplex_worker_starts_total[5m])

# Stale forwarder 事件（不为零表示冻结绑定未能阻止双消费者）
rate(hotplex_gateway_stale_forwarder_event_total_total[5m])
```

## Audit 指标

审计子系统（`internal/audit/`）暴露 5 个专属指标（前缀 `hotplex.audit.*`，Prometheus 导出名 `hotplex_audit_*_total`）。

| 指标 | 类型 | 说明 |
|------|------|------|
| `hotplex.audit.events` | Counter | 审计事件写入总数，label: `action`, `outcome` |
| `hotplex.audit.chain_breaks` | Counter | Hash Chain 完整性违规检测，label: `reason`（`prev_hash_mismatch` / `self_hash_mismatch`） |
| `hotplex.audit.spill` | Counter | 队列满时 Spill 到 WAL 的事件，label: `action` (`spill_ok` / `spill_failed`) |
| `hotplex.audit.write_failures` | Counter | 审计 DB 写入失败，label: `action` (`begin_tx` / `append` / `commit`) |
| `hotplex.audit.sink_failures` | Counter | AlertSink 扇出投递失败（队列满丢弃），label: `sink`（sink 类型名） |

> `chain_breaks` 的 `reason` 属性描述首个断裂点的类型（`prev_hash_mismatch` = 链间隙/删行，`self_hash_mismatch` = 篡改）。审计告警状态机与处置建议见 [合规与审计](../guides/enterprise/audit.md)。

### 常用查询

```promql
# Hash Chain 完整性告警（新断裂）
increase(hotplex_audit_chain_breaks_total[1h]) > 0

# Spill 频率过高（内存缓冲不足，建议增大 channel_cap）
rate(hotplex_audit_spill_total{action="spill_ok"}[5m]) > 10

# DB 写入失败
increase(hotplex_audit_write_failures_total[10m]) > 5

# Sink 投递失败（队列满丢弃事件）
increase(hotplex_audit_sink_failures_total[10m]) > 50
```

## SLO 参考

| SLO | 指标 | 目标 |
|-----|------|------|
| Session 创建成功率 | `session.start.attempts` vs `session.start.errors` | >= 99.5% |
| P99 执行延迟 | `worker.execution.duration` P99 | < 5s |
| Worker 可用性 | `1 - crashes/starts` | >= 99% |
| Pool 拒绝率 | `pool.acquire{result!="success"}` | < 0.1% |

## 参考

- [可观测性配置](../guides/enterprise/observability.md)：日志、追踪和指标配置
- [资源限制](../guides/enterprise/resource-limits.md)：资源配额详解
