---
title: "D06：飞书Adapter关闭时丢失shutdown期限"
weight: 6
description: "根因、触发路径、修复与验收证据。"
---

# D06 — 飞书Adapter关闭时丢失shutdown期限

- 优先级：P2
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A08
- 状态：修复及回归已完成，发布状态见运行结果
- 证据：Test；原实现失败、修复后通过；未执行Live联调
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

## 本批修复与验证

对应 GitHub Issue #993；实现基准 `fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3`。独立验证运行：https://github.com/hrygo/hotplex/actions/runs/34692685711。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| red | 1 | 2 | 0 |
| green | 15 | 0 | 0 |
| related | 917 | 0 | 1 |

计数包含重复轮次和父子用例。相关跳过项见同一提交的架构验证报告。完整lint、原始hooks和发布尚由后续步骤确认；不因文档存在就宣称远端已交付。
