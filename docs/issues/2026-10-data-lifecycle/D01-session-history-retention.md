---
title: "D01：会话运行回收与历史保留期耦合"
weight: 1
description: "空闲运行终止后，会话索引可能先于聊天正文到期，导致用户历史不可访问。"
---

# D01 — 会话运行回收与历史保留期耦合

- 优先级：P1
- 基准：`4c7c685a`
- 证据：Source；用户报告历史会话消失，当前尚无对应实例 Live 复现
- 状态：实施中
- 位置：`internal/session/manager.go` 的 `CreateWithBot`、`ResetExpiry`、`gc`；`internal/gateway` 历史授权与读取路径；`cmd/hotplex/gateway_run.go` 的 `runEventsGC`

## 现象与触发

运行管理和用户历史保留共用或互相覆盖到期语义。会话进入 IDLE 后，Worker 空闲回收会终止运行；终止记录 GC 与事件正文 GC 又使用独立 TTL。若 `sessions` 根行先被删除，历史接口在读取 turns 前无法通过会话归属授权，即使正文仍处于事件保留期也会表现为历史消失。

新反馈要求不占用运行资源的冷会话仍应长期可读、可恢复。目标默认策略为：空闲 60 分钟释放 Worker；7 天无有效用户输入后归档；用户会话从最后一次有效输入起保留 180 天；聊天正文从各自创建时间起保留 180 天。以上期限可配置。

## 影响

- 用户无法从历史列表进入仍未到正文期限的聊天。
- 为了保留会话而延长 Worker 生命周期会增加运行资源占用。
- 单纯延长 event TTL 无法避免 session root 被较短的终止记录 GC 删除。
- 跨 SQLite/PostgreSQL 的策略不一致可能造成数据过早不可见。

## 验收条件

- Worker 空闲回收、会话归档和正文清理各用独立时钟；运行停止不删除会话。
- 默认 180 天内的冷会话可查看并开启新 run；读取历史不续期。
- 新用户输入延长会话到期时间；不改变既有消息的到期时间。
- 未到期正文保护历史入口；已到期正文在物理 GC 前也不会从任何读取路径泄露。
- SQLite、PostgreSQL、重启恢复、分页与并发新写入使用一致的期限计算。
- 默认时长通过 YAML 和环境变量配置；升级存量数据不被提前删除，延长期限经预览及独立 apply。

## 实施记录

- 基线 Source：确认默认 `session.retention_period=168h`、`session.term_retention=168h`、`events.retention=720h`；运行时 `worker.idle_timeout=60m`。
- Live：未对用户实例做配置或数据库检查；用户报告的具体会话 ID、创建时间和实例配置未知。
- Source：新增 v2 lifecycle deadline 与逐条正文期限；Legacy TERMINATED GC 排除 v2，事件/轮次 GC 优先使用每条记录的 `expires_at`。
- Source：单轮绝对 deadline 绑定 Worker run 并持久化；恢复时沿用旧 deadline。超时收尾先终止 Worker，再持久化 `runtime_status=failed`，按序发送错误和失败事实。
- Test：`go test ./internal/gateway -run 'FinishTurnTimeout|RestoredTurnDeadline|TurnTimeout|TurnDeadline' -count=1` 通过（2026-10-08）；`go test ./internal/config ./internal/execution ./internal/gateway -run 'EffectiveTurnTimeout|SetTurnDeadline|TurnDeadline|TurnTimeout|InputExecution|SystemInput' -count=1` 此前通过。
- Source：v2 会话到期现在由 GC 复核会话期限、正文期限和执行/队列/清理义务后退役，并原子写入删除屏障与 cleanup outbox。新输入与到期退役共享会话行锁；过期 session 拒绝新输入，已接受的重复 ID 仍保持幂等。
- Test：SQLite/PostgreSQL SQL mock 覆盖候选筛选及退役复核；`go test ./internal/session ./internal/execution ./internal/eventstore ./internal/gateway ./internal/config ./internal/effect ./internal/messaging/slack ./internal/messaging/feishu ./internal/worker/acp ./cmd/hotplex -count=1` 通过（2026-10-08）。到期退役竞态的 session/execution race 测试，以及 lifecycle 指标变更后的 `go test -race ./internal/session ./internal/observability -count=1` 通过（2026-10-08）。
- Source：成功退役会话记录删除计数和期限到退役延迟直方图；查询/退役失败按固定阶段计数。存量期限迁移已提供只读 Admin 预览与独立双重确认 apply；按批延长事件/轮次期限，并确保会话根策略在其所有正文都达到新旧有效保留期后才切换。
- Test：`go test ./internal/lifecycle -count=1` 通过（2026-10-08）；覆盖缺失输入时钟、已删除/孤立内容、双重确认、stale 快照、批次续跑、正文先于会话根迁移、策略变化回滚及敏感信息不出现在预览。
- Source：生命周期 GC 暴露 eligible/blocked 积压、最老到期延迟及 unknown/fenced execution 阻塞 gauge；超过 `gc.max_lag` 记录聚合告警，status 查询失败使用固定 phase 计数。分类处理量 Counter 覆盖会话、正文/事件/轮次、投递事实、审计事实、trace/media 文件和会话清理项；标签集合固定，不含会话或用户标识。
- Test：SQLite/PostgreSQL store 单元及 SQL mock 覆盖过期/阻塞计数和最老期限；真实 PostgreSQL 查询仍未运行验证。
- Test：`go test ./internal/session ./internal/audit ./internal/observability ./internal/messaging/slack ./internal/messaging/feishu ./cmd/hotplex -race -shuffle=on -count=1` 与 `make docs-lint` 通过（2026-10-08）。
- Remaining：execution/Cron/effect 幂等根记录的保留边界见 D02；provider-specific unsupported/远端清理结果没有独立指标，真实 PostgreSQL 生命周期与多实例仍未验证。
- Test：真实 PostgreSQL 生命周期迁移与多实例运行尚未验证；本地 SQL mock 不替代该验证。
- Live：未执行真实实例配置变更、存量迁移、数据删除或 Worker 远端清理。
