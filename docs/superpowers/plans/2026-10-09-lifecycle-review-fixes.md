---
title: "生命周期审查修复实施计划"
weight: 99
description: "PR #1036 六项审查修复的范围、实现契约和验收步骤。"
---

# Lifecycle Review Fixes Implementation Plan

执行证据记录在 `docs/issues/2026-10-data-lifecycle/PR-review-fixes.md`。

**Goal:** 修复 PR #1036 的 R1–R6，并验证当前 HEAD 的 CI 全绿。

**Architecture:** 复用 Session、Execution、effect 的现有事务和期限更新路径。迁移与输入使用一致的版本契约；聚合字段归 forwarder 单一所有者；清理能力和行级固定期限明确建模。

**Tech Stack:** Go、testify、SQLite、PostgreSQL、GitHub Actions。

**Spec:** `docs/superpowers/plans/2026-10-08-hotplex-data-lifecycle-luna-guide.md`，本轮已批准的六项审查发现。

## Global Constraints

- SQLite/PostgreSQL migration 成对；未知执行不重投；旧无可靠期限记录不追溯删除。
- 修改配置只决定新记录/新输入期限，既有 deadline 不缩短。
- 锁顺序 `m.mu` → `ms.mu`；`t.Parallel()`、require 和无 sleep 异步测试。
- 当前分支增量交付，保留工作树和业务数据；质量门禁和 hooks 不跳过。

## Review Focus

- 迁移前缓存 legacy、迁移后冷加载和配置修订均可记录新输入。
- queue 重复接受、续期失败、ACK 顺序，不得重复执行。
- timeout 触发时已有 content event，在竞争终态时仍保持聚合单一所有者。
- Worker cleaner 未注册与真实失败必须分别报告 unsupported/retrying。
- 重启缩短保留期、旧行无 deadline、晚到终态与新 target revision不能损坏恢复义务。

### Task 1: R1/R6 迁移和输入续期

**Files:** `internal/config/lifecycle_revision.go`（新），`internal/session/lifecycle_policy.go`、`manager.go`、`internal/lifecycle/migration.go` 与对应测试。

**Interfaces:** 共用 `config.LifecyclePolicyRevision(config.LifecycleConfig) string`；`RetentionPolicy.PolicyRevision` 由路由传入；Manager 以持久策略/revision 更新期限，拒绝 legacy 隐式升级。

- [x] RED：真实 migration → 热/冷 Manager → `RecordInputAccepted`，180d/1d 下 `RetireExpiredLifecycleSession(now+2d)` 返回 nil；跨批将 Legacy 从30d增至180d后迁移收敛。
- [x] GREEN：迁移写相同 revision，输入重新读取生命周期字段；原子更新保持持久 revision，并以 max 推进 deadline。正文更新与就绪条件统一使用 `max(content, legacy)`。
- [x] Verify：`go test ./internal/session ./internal/lifecycle ./internal/config -race -shuffle=on -count=1`。

### Task 2: R2 timeout 聚合所有权

**Files:** `internal/gateway/bridge_forward.go`、`turn_deadline_test.go`。

**Interfaces:** timeout callback 只更新已有原子字段和线程安全 accumulator；不写 `forwardContext` 普通聚合字段。

- [x] RED：overlay probe 复现实际字段访问模式的 Builder/hasRealText race；真实 handler 与事件流测试验证单终态，但基线 50 次 race 运行未稳定命中。
- [x] GREEN：删除 callback 中两处聚合重置；保留持久结算、绝对 deadline 和 terminal fence。
- [x] Verify：`go test ./internal/gateway -race -shuffle=on -count=1`。

### Task 3: R3 清理能力

**Files:** `internal/worker/session_cleanup.go`、`internal/session/cleanup_outbox.go`、`purge.go` 与对应测试。

**Interfaces:** `worker.ErrSessionCleanupUnsupported`；可选 store 能力将 leased cleanup item 标为 unsupported 并结束任务，已有 Store/mock 接口兼容。

- [x] RED：未注册 cleaner 的真实 CleanupRunner 后 `worker_session` 必须 unsupported、job blocked。
- [x] GREEN：缺 cleaner 返回哨兵错误，runner 条件更新 unsupported，真实网络失败继续 retrying。
- [x] Verify：`go test ./internal/session ./internal/worker -race -shuffle=on -count=1`。

### Task 4: R4 不可变结算期限

**Files:** 新 migration045 两种方言；`internal/effect/` 结算/GC；`internal/execution/` 终态/GC；`cmd/hotplex/gateway_run.go`；配置/用户文档和测试。

**Interfaces:** 启动前通过 store retention setter 注入配置；新记录创建事务保存 `payload_retention_ms`、`facts_retention_ms` 和 revision；GC 用结算时钟加不可变窗口判断过期，保护 NULL legacy 行。这个实现覆盖全部既有结算路径，不引入数据库 trigger。

- [x] RED：同一库先7d后1d策略，新结算记录采用新期限，旧两天记录不被删除；attempt/execution 同类回归。
- [x] GREEN：成对 additive migration，保留窗口在创建时固化，晚到事件不重写窗口；GC 只删除已到期且没有恢复义务的行。
- [x] Verify：SQLite 回归及相关包 race 通过；PostgreSQL 新增回归本机编译通过，专用 DSN 缺失而 skip。真实执行结果纳入 PR PostgreSQL 门禁和 Issue #1038 的交付记录。

### Task 5: R5 queue 接受时钟

**Files:** `internal/gateway/queue.go`、`queue_test.go`、`handler.go`。

**Interfaces:** 首次 `AcceptQueued` 成功后、ACK 前调用 `SessionInputLifecycleRecorder.RecordInputAccepted`；重复请求不续期。

- [x] RED：真实 SQLStore queue 首次记录一次，duplicate 仍一次；更新失败撤销队列发送承诺，不能被当作普通 buffer fallback。
- [x] GREEN：首次队列接受调用生命周期记录接口；持久失败显式返回错误并结算/取消新 queued record。
- [x] Verify：`go test ./internal/gateway ./internal/execution -race -shuffle=on -count=1`；复审追加 same-ID 生命周期失败重试与已取消队列的真实状态回归。

### Task 6: 完整验证、审查和交付

- [x] 回填 Issue 台账，文档与 migration 契约核对。
- [x] 当前修复范围独立审查并关闭有价值发现；完整 `make quality`（lint 0 issues、全仓 race/shuffle）、build、docs-lint 通过。
- 完整 `make quality`、commit + push（保留 hooks）、PR body 更新及最新 HEAD 的 PostgreSQL/全部 CI 检查，按远端 [Issue #1038](https://github.com/hrygo/hotplex/issues/1038) 记录最终结果，避免源文件保存会过期的 CI 状态。
