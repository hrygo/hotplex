---
title: "D04：PostgreSQL 下会话 GC 每分钟失败一次"
weight: 4
description: "delete_terminated 清理任务入队持续报 driver: bad connection。"
---

# D04 — PostgreSQL 下会话 GC 持续失败

- 优先级：P2
- 基准：`1698d2c2`
- 证据：Live（真实 PostgreSQL dev 库）
- 状态：已修（`1a3672c8`，待验证）
- 位置：`internal/session/manager.go:1757`（gc）、远端清理 outbox 入队路径

## 实测

网关每 60 秒一次，稳定失败：

```
ERROR session: gc (delete_terminated) failed
  err="session cleanup: enqueue: driver: bad connection"
```

`origin/main` 同样出现，非本分支引入。

## 影响

终止会话的远端清理任务无法入队，本地会话长期滞留。实测中这一点的后果是
可见的：会话池按用户限制空闲会话数，多轮实测后新会话被拒
（`session: attach rejected kind=user_quota_exceeded`），
只能靠管理接口逐个删除才能继续。

注意 `DELETE /admin/sessions/{id}` 返回 204 成功，
但清理任务同样未能入队——本地删除成功与远端清理入队是两件事，
当前响应无法区分二者。

## 待查

- `bad connection` 来自连接池中的失效连接，还是 `pgx` 在事务外使用的连接；
- 与 SQLite 路径的差异（SQLite 下不复现）；
- 入队失败是否应使本地删除失败，或至少在响应与指标中可见。

## 根因（2026-10-04 实测确认）

`DELETE ... RETURNING` 的结果集被扫描后未关闭，同一事务上的
`session_cleanup_tasks` INSERT 随即执行。pgx（database/sql）每个
`*sql.Tx` 复用一个服务端连接，未关闭的结果集会占用它，后续语句以
`driver: bad connection` 失败而非等待。modernc/sqlite 在客户端缓冲
行数据，所以相同代码在 SQLite 下正常。最小探针（同一 tx 上“开着
rows 做 INSERT”）精确复现：关闭 rows 则成功，开着则失败。

修复：扫描完成后先关闭结果集再执行 INSERT；删除 + 入队仍在同一事务
提交。`ClaimCleanupTasks` 先收集行再更新、无需改动。回归测试
`TestPGStore_DeleteTerminatedEnqueuesCleanupTask`（`pg` tag，需
`HOTPLEX_TEST_PG_DSN`）走真实 PG 路径，变异验证通过。

## 验收条件

- PostgreSQL 下连续多个 GC 周期无错误；
- 清理任务可被 `CleanupRunner` 领取并完成，含退避重试；
- 池统计在 GC 失败时可观测，不静默。

