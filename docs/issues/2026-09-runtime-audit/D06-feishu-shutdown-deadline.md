---
title: "D06：飞书Adapter关闭时丢失shutdown期限"
weight: 6
description: "根因、触发路径、修复与验收证据。"
---

# D06 — 飞书Adapter关闭时丢失shutdown期限

- 优先级：P2
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A08
- 状态：已登记，待修复
- 证据：Source；本批运行证据尚未生成
- 实施PR：待创建
- 位置：`internal/messaging/feishu/adapter.go`, `internal/messaging/feishu/chat_queue.go`

## 根因与影响

ChatQueue.Close本身约定graceful drain；但Adapter.Close(ctx)调用无期限Close，任务从Background派生十分钟期限，使上层shutdown预算失效。

## 复现路径

活跃context-aware任务和积压任务存在时耗尽Adapter.Close的context。

## 验收条件

保留兼容graceful Close，提供有期限关闭；期限到达取消任务，按预算返回可识别错误并继续其他资源清理；关闭拒绝入队；不通过无限goroutine掩盖不合作任务。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。
