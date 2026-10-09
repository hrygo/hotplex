---
title: "D03：新旧审计事实没有独立保留期"
weight: 3
description: "新审计事实需要独立 180 天保留期，不能用同一条不可变旧链的 GC 阈值缩短历史证据。"
---

# D03 — 新旧审计事实没有独立保留期

- 优先级：P1
- 基准：`4c7c685a`
- 证据：Source + Test；使用独立临时库运行 PostgreSQL migration 测试，未访问实例业务库
- 状态：实现与 SQLite/PostgreSQL 验证已完成
- 位置：`internal/audit/{store.go,gc.go,verify.go}`；SQLite/PostgreSQL migration 044

## 现象与触发

现有 `user_activity` 与 `audit_chain_checkpoints` 是一条不可变哈希链，GC 只能以 checkpoint 锚定方式删除链前缀。配置中的旧审计保留期约为三年；生命周期 v2 需要新审计事实默认保留 180 天。直接把旧 GC 阈值改为 180 天会提前删除仍在旧保留期内的证据，也会混淆旧链和新策略的验证边界。

## 影响

- 新旧审计事实无法采用各自的保留期限。
- 旧链正文不能安全逐行改写或删除。
- 管理端查询与审计验证需要能识别不同链 epoch。

## 验收条件

- 生命周期 v2 的新审计事实进入独立、无正文的哈希链，默认按 `lifecycle.audit.facts_retention` 保留 180 天。
- legacy `user_activity` 链继续遵循既有配置，不被 v2 GC 清理。
- 两条链有独立 genesis、checkpoint、验证和 GC；各自的 GC 都先写 checkpoint，再原子删除前缀。
- v2 新事实在写入时固定并哈希保护 `expires_at`；变更保留期只影响新事实，不追溯改写存量。
- v2 GC 只删除已到期的连续前缀；未到期记录会阻止其后的到期记录被提前删除。
- 管理端查询可跨 epoch 聚合并返回链 epoch；相同数据库 ID 不会造成客户端行键冲突。
- 新链与旧链写入、验证和清理均有 SQLite/PostgreSQL 回归覆盖。
- 旧链明文迁移、删除和存量期限缩短不在本问题的自动处理范围内。

## 实施记录

- Source（基准）：旧链的删除触发器要求 checkpoint 锚定；直接改旧 GC retention 会提前删除 legacy 数据并覆盖其原保留要求。
- Source（修复后）：Gateway 将新事实写入独立 v2 链，legacy 与 v2 各自运行 GC 和 Verifier；Admin 聚合查询同时返回两种 `chain_epoch`。
- Source（修复后）：v2 每条记录在写入时固定并哈希保护 `expires_at`。GC 仅剪除已到期连续前缀，避免配置变更后误删其后的未到期记录。
- Test：`go test ./internal/audit ./internal/session/sql -race -shuffle=on -count=1` 通过；SQLite migration 验证表、索引、不可变触发器和 checkpoint 删除约束；PostgreSQL 存储 SQL 有 mock 覆盖。
- Test（发现，2026-10-08）：PostgreSQL 集成测试最初在读取 v2 新插入 ID 时失败：`LastInsertId is not supported by this driver`；夹具已改用 `INSERT … RETURNING id`。
- Test（修复后，2026-10-08）：本地 PostgreSQL 18.6 独立临时数据库上的 `TestMigrations_PG_AuditChainsStayImmutable` 通过；覆盖 UPDATE、未锚定 DELETE 与 checkpoint 锚定 DELETE。生命周期 PG 包集以 `-race -shuffle=on` 再次通过。
- Test（基准）：`4c7c685af12524c8dea5c031b2570a09cf923ee0`；失败来自该基准之上的未提交生命周期改动。
- Live：未读取运行实例数据、配置或真实审计日志。
