---
title: "D05：PostgreSQL 生命周期归档未重绑定清理 SQL 占位符"
weight: 5
description: "PostgreSQL 到期归档路径把问号占位符直接交给 pgx，导致清理 outbox SQL 语法失败并回滚归档事务。"
---

# D05 — PostgreSQL 生命周期归档未重绑定清理 SQL 占位符

- 优先级：P1
- 基准：`4c7c685af12524c8dea5c031b2570a09cf923ee0`
- 证据：Source + Test；本地 PostgreSQL 集成复现，测试只使用独立临时数据库
- 状态：已修复；SQLite 与 PostgreSQL 生命周期回归验证通过
- 位置：`internal/session/cleanup_outbox.go::retireExpiredLifecycleSession`

## 现象与触发

对 PostgreSQL 中已到期的 v2 会话执行 `RetireExpiredLifecycleSession` 时，生命周期事务锁定并检查会话后，在原子写入清理 outbox 的阶段失败：

```text
session cleanup: enqueue: ERROR: syntax error at or near "," (SQLSTATE 42601)
```

该路径将原始 `*sql.Tx` 传给 `markDeletedAndEnqueueCleanupLocked`。其中 outbox INSERT 和会话脱敏 UPDATE 使用 `?` 占位符；SQLite 接受该语法，PostgreSQL 必须经 `DialectPostgres.Rebind` 转为 `$N`。其它 PostgreSQL 删除入口使用 `pgExec` 做重绑定，但到期归档路径遗漏了该包装。

## 影响

- PostgreSQL 上的到期会话无法完成归档/删除事务。
- 会话脱敏、cleanup outbox 与 purge job 无法原子落库。
- 清理 runner 不能接管该会话，生命周期 GC 会持续重试并积压。

## 验收条件

- PostgreSQL 到期归档路径对 cleanup outbox INSERT 和 session UPDATE 使用 dialect rebind。
- 真实 PostgreSQL 测试验证归档成功、敏感会话字段被清空、cleanup task 与 purge job 同事务产生。
- 两个独立 PostgreSQL store 实例并发执行新输入与到期归档时，输入成功则归档必须被活跃执行事实阻止；归档成功则新输入必须失败。
- SQLite 到期归档和删除行为保持不变。

## 实施记录

- Source（基准）：`retireExpiredLifecycleSession` 通过 `markDeletedAndEnqueueCleanupLocked(ctx, tx, tx, ...)` 传入原始 `*sql.Tx`；PostgreSQL 删除的其它入口则使用带 `DialectPostgres.Rebind` 的 `pgExec`。
- Test（2026-10-08）：在独立临时数据库运行 `HOTPLEX_TEST_PG_DSN=… go test -tags pg -p 1 -parallel 1 -race -shuffle=on -count=1 ./internal/session -run 'TestPGStore_RetirementSerializesWithInputAcceptanceAcrossInstances|TestPGStore_DeleteFencesConcurrentLateUpsertAcrossInstances'`，到期归档事务以 SQLSTATE `42601` 失败；错误定位到原始 `?` 占位符未重绑定。
- Fix：共享到期归档事务现将 cleanup outbox 与会话脱敏 SQL 交给 dialect-aware executor；SQLite 使用 identity rebind，PostgreSQL 使用 `$N` 占位符。
- Test（修复后，2026-10-08）：新增的跨实例“输入接受 vs 到期归档”与“删除 vs 迟到 Upsert”真实 PG 竞争测试通过；`session/sql`、`session`、`audit`、`effect`、`execution`、`eventstore` 六个包以 `-race -shuffle=on` 在独立临时 PostgreSQL 数据库通过。
- Live：未读取或修改运行实例数据库；未访问已有业务库。
