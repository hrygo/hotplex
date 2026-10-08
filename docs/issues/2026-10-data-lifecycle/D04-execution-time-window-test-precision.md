---
title: "D04：执行记录时间窗测试依赖毫秒级时钟恰好分离"
weight: 4
description: "ListRecent 时间窗测试连续创建记录后假设其 created_at 毫秒值不同，导致窗口断言不稳定。"
---

# D04 — 执行记录时间窗测试依赖毫秒级时钟恰好分离

- 优先级：P2
- 基准：`4c7c685af12524c8dea5c031b2570a09cf923ee0`
- 证据：Source + Test；在本地 PG 集成验证期间复现于 SQLite 单元测试
- 状态：已复现并修复；重复、race 和 shuffle 验证通过
- 位置：`internal/execution/sql_console_test.go::TestListRecent_TimeWindow`、`internal/execution/sql_store.go::Accept`

## 现象与触发

`acceptN` 连续接受两条记录，`Accept` 使用 `time.Now().UnixMilli()` 生成时间。测试以第一条记录时间 `pivot` 构造 `[pivot, pivot+1)` 窗口，并断言只返回一条；两次接受落在同一毫秒时，两条记录都会命中。

## 影响

- 时间窗测试偶发或连续失败，影响执行记录分页/筛选相关质量门禁。
- 失败来自测试数据对墙钟精度的假设；本次运行没有证据表明生产筛选 SQL 存在边界错误。

## 验收条件

- 测试显式写入确定且相邻的 `created_at` 值，不使用 `time.Sleep` 等待时钟跨毫秒。
- 窗口边界仍验证 `SinceMs` inclusive、`UntilMs` exclusive，并且 `[pivot, pivot+1)` 仅命中一条记录。
- 单测在重复执行、race 和 shuffle 模式下稳定通过。
- 本修复不改变执行记录生产时间或 `ListRecent` 查询语义。

## 实施记录

- Source：`internal/execution/sql_store.go::Accept` 将创建时间按 Unix 毫秒取值；`ListRecent` 使用 `created_at >= SinceMs` 与 `created_at < UntilMs`。
- Test（2026-10-08）：`go test ./internal/execution -run TestListRecent_TimeWindow -count=10` 失败 8 次，均在窗口断言处返回 2 条记录。
- Fix：测试改为显式将相邻记录的 `created_at` 写为 `pivot` 与 `pivot+1`，不再依赖系统时钟跨毫秒。
- Test（修复后，2026-10-08）：`go test ./internal/execution -run TestListRecent_TimeWindow -count=10` 通过；完整 `execution` PG 包集使用 `-race -shuffle=on` 通过。
- Live：未读取或修改运行实例数据库；本地 SQLite 测试使用临时数据库。
