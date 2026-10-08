---
title: "D02：执行与投递幂等事实缺少安全保留边界"
weight: 2
description: "不能在来源回放窗口未受约束时按年龄删除仍承担幂等职责的根记录。"
---

# D02 — 执行与投递幂等事实缺少安全保留边界

- 优先级：P1
- 基准：`4c7c685a`
- 证据：Source；未对实际部署实例进行 Live 数据验证
- 状态：安全细节清理已实现；幂等根记录期限待来源回放契约
- 位置：`internal/execution` 的 `execution_inputs`、`internal/cron` 的 `cron_occurrences`、`internal/effect` 的 `effects` / `effect_attempts`

## 现象与触发

生命周期配置规划了已结算事实 90 天保留，但输入 ID、Cron trigger key 和投递业务键同时承担去重职责。当前没有强制的客户端或来源回放截止时间，若仅按记录年龄删除这些根记录，迟到重放可能再次接受输入、运行 Cron 或发送外部消息。

## 影响

- 删除执行输入的唯一键可能让同一客户端消息 ID 被当作新输入。
- 删除 Cron occurrence 可能让重复 webhook 或计划触发再次创建执行。
- 删除投递 effect 可能让恢复或重试路径再次发送已完成消息。
- `unknown`、活动租约及缺少可靠结算时间的旧记录不能被当成普通终态数据。

## 验收条件

- 区分承载幂等的根记录与可独立过期的详细 attempt 证据。
- 仅在 effect 已进入终态、结算时间可靠、租约已关闭且 attempt 已结束后，按可配置期限清除详细 attempt。
- `unknown`、活动租约、未完成 attempt 和缺少结算时钟的 legacy 记录保持不变。
- 清理详细 attempt 后，effect 根记录仍能阻止重复投递。
- 只有回放期限已被强制执行，或具备可验证的最小墓碑时，才允许缩短根记录期限。
- SQLite 与 PostgreSQL 使用相同安全条件，并有并发和恢复回归覆盖。

## 实施记录

- Source：确认 `execution_inputs` 以 `(session_id, client_message_id)` 去重，`cron_occurrences` 以 `(trigger_key, generation)` 去重，`effects` 以 `(occurrence_id, delivery_ordinal, target_revision)` 去重。
- Source：确认计划默认 90 天只能安全适用于独立的结算尝试详情；根记录继续保留，直到其来源回放期限有明确契约。
- Source：新增 `DeleteSettledAttempts`，SQLite 与 PostgreSQL 共用相同的有界选择条件；Gateway 在 v2 策略下按 `lifecycle.facts.retention_after_settlement` 周期运行。
- Source：新增 `CompactSettledFacts`，对已结算且无 fence/队列引用的 `execution_inputs` 清除 owner、错误明细与单轮诊断时钟，保留 client message ID、payload hash、worker run ID、最终状态和结算时钟；仍可能迟到收敛的 `failed` delivery + `completed` runtime 行不会压缩。
- Test：覆盖期限边界、未知结果、活动租约、未结束 attempt、缺少结算时钟及删除 attempt 后 effect 仍阻止重复投递；执行记录压缩测试确认旧 client message ID 仍保持幂等。
- Remaining：execution/Cron/effect 根记录在来源回放期限被强制前继续保留；阻塞事实积压报告、辅助表清理和真实 PostgreSQL 运行验证尚未完成。
- Live：未访问或修改任何生产数据库、配置或外部平台消息。
