---
title: "PR #1036 生命周期审查修复"
weight: 99
description: "六项审查发现、复现证据和修复验证记录。"
---

# PR #1036 生命周期审查修复

- 基准：`2db214fb0b2b9ae46dba9afdb7d72c5fabed7486`。
- 授权：2026-10-09 用户要求执行六项修复、推送当前 PR 并确保 CI 全绿。
- 交付：[PR #1036](https://github.com/hrygo/hotplex/pull/1036)；[Issue #1038](https://github.com/hrygo/hotplex/issues/1038)。
- 证据：Source 为基准代码；Test 为隔离数据库/Go overlay 复现；Live 未操作业务数据。

| ID | 发现与触发 | 验收 |
|---|---|---|
| R1 | 迁移 revision 与 Manager 不同；新输入不续期。180 天会话/1 天正文下两天后退役 | 真实 Apply 后热/冷 Manager 新输入推进会话与归档期限；不可提前退役 |
| R2 | timeout 重置 forwarder 普通聚合字段；`-race` 报 Builder/hasRealText 冲突 | 可变聚合只有 forwarder 写；超时终态与并发回归通过 |
| R3 | 未注册 cleaner 返回 nil，清理进度误报 complete | 无能力明确 unsupported，汇总不得 complete |
| R4 | GC 采用重启时新 cutoff；配置 7 天改 1 天清掉旧两天正文 | 行级不可变保留窗口/revision + 结算时钟；旧无快照行保护；双数据库迁移与结算路径一致 |
| R5 | AcceptQueued 成功并 ACK 未记录活动；过期未派发后仍用旧期限 | 首次持久接受入队即续期；重复请求不续期 |
| R6 | 分批迁移中提高 legacy TTL，已处理行既不更新又阻塞根切换 | 跨批策略变化可收敛，既有正文期限不缩短 |

## 修复与验证台账

所有步骤先补失败回归再修复，实施结果在各步骤完成后回填。生产配置、存量 apply 与部署不属于本轮操作。

- R1/R6：真实迁移的 hot/cold、共享/旧 revision 四条路径，分批迁移提高 legacy TTL，均已先复现失败再通过定向 race 测试。
- R3：未知 Worker 不再返回成功；unsupported 原子持久化后停止自动重试。已覆盖失败重试与 stale lease，定向 race 测试通过。
- R4：SQLite/PostgreSQL 成对 migration 045 保存新 obligation 的保留窗口与 revision。使用 `settled_at/finished_at + 行级窗口` 判断过期，避免另加数据库 trigger 和重复写结算路径。旧 NULL 快照不自动补填。
- R2/R5：真实 forwardEvents 超时单终态及真实队列首次接受、重复与失败无 ACK/buffer 的定向 race 测试通过。R2 的真实事件测试在基线 50 次未稳定触发 race，Builder/hasRealText 竞态的 RED 证据为原审查 overlay probe，未将稳定终态回归冒称为竞态复现。
- R5 复审补充：取消后的 same-ID 重试会误报 queued，已先登记 Issue #1038、补 RED 再修复。首次生命周期失败的同 ID 重试继续返回失败；已取消/已派发的普通重复输入返回真实状态。覆盖同进程缓存与冷读两条路径，定向 race 通过。
- 相关包 `-race -shuffle=on -count=1` 通过；`make build`、`make docs-lint` 通过。首轮完整质量检查发现旧 PostgreSQL mock 匹配旧入队 SQL，修正后统一重跑质量门禁。
- 真实 PostgreSQL 回归新增在 `internal/execution/retention_snapshot_pg_test.go`，覆盖 Accept/AcceptQueued 的 90 天大整数快照、7 天投递正文、缩短后新旧记录区别、重复请求不改快照及 NULL legacy 保护。本机缺少专用测试 DSN，编译通过并明确跳过；实际执行由 PR 的 PostgreSQL CI 门禁验证。
- R4 独立复审无新增 P0/P1/P2；R1/R3/R5/R6 复审新增的重试发现已修复并复核关闭。最终提交及远端 CI 结果回填 Issue #1038，以 PR 当前 HEAD 的检查为准。
- 最终本地门禁（2026-10-09）：`make quality` 成功，lint 为 0 issues，全仓 `-race -count=1 -shuffle=on` 通过；最终 `make build`、`make docs-lint` 成功。未跳过 hooks。

## PostgreSQL CI 补充发现

- 基准：修复提交 `15986c5591dd4610de36c0bfca5cf3301daed392`。
- Test：新增真实 PostgreSQL 回归在首次 `AcceptQueued` 时失败，错误为 `refresh queue budget mirror: syntax error at or near "WHERE"`；[失败 CI](https://github.com/hrygo/hotplex/actions/runs/37866767486/job/113615275975)。
- Source：队列 budget mirror 更新的 `?` 占位符未经过既有 `s.rebind`，SQLite 可执行，PostgreSQL 拒绝并回滚整个排队事务。
- 先登记于 [Issue #1038](https://github.com/hrygo/hotplex/issues/1038#issuecomment-6072064596)，再补齐该更新的方言转换；预算锁、事务与容量决策语义保留。既有新增 PostgreSQL 回归不变，以其在新 HEAD 的成功执行为验收。
- 增量提交及本地、远端验证结果回填 Issue #1038；未修改生产运行态或数据。
- 第二轮 CI（`8cfa8a763c59bbfa9a92183650d58146b1a77cf3`）已完成入队，随后在 `ClaimQueued` 删除队列行时发现同类占位符遗漏；[失败 CI](https://github.com/hrygo/hotplex/actions/runs/37867885313/job/113618965463)。全量核对 execution/effect 的直接参数化 SQL 后，还定位到取消/清空/过期共用的删除语句及会话队列深度查询。[登记证据](https://github.com/hrygo/hotplex/issues/1038#issuecomment-6072206139)后一次补齐三条语句，并扩展真实 PostgreSQL 回归，覆盖 claim 前后深度、cancel/clear/expire 的终态、队列移除与保留快照。
