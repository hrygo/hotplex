---
title: "D02：会话删除语义依赖缓存状态"
weight: 2
description: "缓存命中、缓存未命中与 WebChat 删除接口采用不同删除路径，可能级联丢失执行事实或允许历史继续读取。"
---

# D02 — 会话删除语义依赖缓存状态

- 优先级：P1
- 基准：`4c7c685a`
- 证据：Source、Test；未在用户实例 Live 复现
- 状态：实现完成；真实 PostgreSQL 与运行实例验证待执行
- 位置：`internal/session/{manager.go,cleanup_outbox.go,purge.go,purge_store.go,purge_runner.go}`；`internal/gateway/api.go` 的 Delete/cleanup-status 与会话授权入口；Admin Delete handler

## 现象与触发

显式删除的结果取决于会话是否已经进入内存缓存：缓存命中时，`Manager.Delete` 将会话标记为 `DELETED` 并入队 Worker 清理；缓存未命中时，当前实现调用 `DeletePhysicalWithCleanup`，直接删除 sessions 根行，可能级联删除仍需保留的 execution facts。WebChat 的 `DELETE /api/sessions/{id}` 也绕过统一删除路径，先转换状态再调用 `DeletePhysical`。

`sessions.upsert_session` 目前只在 `deleted_at IS NULL` 时限制更新，但显式删除未可靠写入 `deleted_at`；当没有远端 Worker 清理任务时，旧 Upsert 仍可能把 `DELETED` 会话恢复为活跃状态。历史 API 的统一授权入口也没有拒绝 `DELETED` 会话。

## 影响

- 同一删除请求可能因为缓存冷热而保留或级联删除执行事实。
- 删除后历史正文可能仍能从 turns/events 读取。
- 远端 Worker 清理无任务时，迟到或重放的 Upsert 可能复活会话。
- REST、AEP 与管理端的删除状态和完成含义不一致。

## 验收条件

- 缓存命中与未命中执行相同的持久删除屏障，不在业务删除入口直接物理删除 sessions 根行。
- 已删除会话立即从历史、事件和会话详情读取中隐藏，不能续聊或被 stale Upsert 复活。
- 删除事务失败时保留原会话；成功后持久化 `deleted_at` 和清理意图。
- SQLite 与 PostgreSQL 的删除更新、清理任务入队及重启行为一致。
- 最终物理清理不得在执行事实、未知投递或清理义务仍受保护时级联移除根行。

## 实施记录

- Source（基准）：`Manager.Delete` 缓存未命中分支调用 `DeletePhysicalWithCleanup`；命中分支调用 `MarkDeletedWithCleanup`。`GatewayAPI.DeleteSession` 调用 `DeletePhysical`。
- Source（修复后）：缓存命中/未命中统一先安装内存删除屏障，再事务更新 `state`、`deleted_at` 并入清理 outbox；数据库使用权威存储行创建 Worker 清理意图。REST 删除调用 `Manager.Delete`；历史、事件、详情和续聊复用的授权入口拒绝已删除会话；upsert 条件拒绝恢复 deleted 行。
- Source（修复后）：Gateway REST 与 Admin 删除入口均调用 `Manager.Delete`；缓存冷热一致地事务写入 tombstone、Worker cleanup outbox 与持久 purge job/items。后台分别清理 Worker 会话和事件/轮次正文，状态接口只返回清理阶段、次数与受限错误码。
- Test（修复后）：缓存冷热删除、Worker 清理重试、正文清理重试、stale lease fencing、已删除历史隐藏、WebChat/Admin 删除与状态读取回归通过；`make test-short` 通过。
- Remaining：真实 PostgreSQL 并发、进程重启及远端 Worker 清理验收尚未执行。内部生命周期维护仍有 `DeletePhysical` 路径，不作为公开用户删除入口；其数据范围按调用场景单独核验。
- Live：未检查实际数据库是否存在已删除后可读、可复活或已级联丢失事实的记录。
