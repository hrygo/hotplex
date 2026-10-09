---
title: "D06：删除范围未说明备份与外部平台副本"
weight: 6
description: "会话清理只覆盖 HotPlex 自有数据和 Worker 会话，当前生命周期文档没有明确备份及 Slack/飞书消息的边界。"
---

# D06 — 删除范围未说明备份与外部平台副本

- 优先级：P1
- 基准：`4c7c685af12524c8dea5c031b2570a09cf923ee0`
- 证据：Source + Docs；只读检查当前清理实现及可达用户文档
- 状态：已修正文档并通过文档构建与链接验证
- 位置：`docs/explanation/session-lifecycle.md`、`docs/reference/admin-api.md`、`docs/guides/enterprise/disaster-recovery.md`

## 现象与触发

用户主动删除会话后，HotPlex 会隐藏本地历史、阻止续聊，并通过持久清理任务删除 HotPlex 正文和 Worker 侧会话。当前说明没有明确指出，该操作不会删除 Slack/飞书等外部平台上已发送的消息，也不会改写此前生成的数据库备份。

源码确认 `CleanupRunner` 只调用 Worker 类型对应的远端会话清理器；消息适配器没有删除平台消息的会话清理路径。灾备文档把数据库快照作为独立备份处理，生命周期删除不会追溯修改这些快照。

## 影响

- 用户可能把 HotPlex 本地删除误解为所有副本均已擦除。
- 从旧备份恢复后，被删会话数据可能重新出现。
- 运维人员可能忽略平台自带保留策略和备份介质到期/销毁策略。

## 验收条件

- 生命周期说明明确 HotPlex 删除覆盖本地 HotPlex 数据及关联 Worker 会话。
- 明确说明外部平台消息和既有备份不随会话删除；恢复旧备份可能恢复已删除数据。
- 指明外部平台消息保留由平台策略负责，备份过期和销毁由部署方备份策略负责。
- Admin API 删除与清理状态文档不再暗示跨系统完整擦除。

## 实施记录

- Source：`CleanupExecutor` 只接收 Worker 类型和 Worker session ID；对话正文由 `PurgeContentExecutor` 清理本地会话内容。检索 Slack/飞书适配器未发现会话删除时调用平台消息删除 API 的路径。
- Docs：`docs/explanation/session-lifecycle.md` 和 `docs/reference/admin-api.md` 描述本地正文与 Worker 清理状态，但未写备份或外部平台副本边界；灾备文档将数据库备份作为独立快照管理。
- Fix：生命周期说明、Admin API 清理状态说明和灾备指南现明确外部平台消息与既有备份不随 HotPlex 会话删除，并说明恢复旧备份可能重新出现数据。
- Test：`make docs-build` 与 `make docs-lint` 通过；文档只读构建和链接校验通过。
- Live：未连接或读取外部平台账号、备份文件或运行实例。
