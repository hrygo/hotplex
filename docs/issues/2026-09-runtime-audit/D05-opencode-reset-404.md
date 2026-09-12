---
title: "D05：OpenCode reset把HTTP404误报为上下文清空成功"
weight: 5
description: "根因、触发路径、修复与验收证据。"
---

# D05 — OpenCode reset把HTTP404误报为上下文清空成功

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A06
- 状态：已登记，待修复
- 证据：Source；本批运行证据尚未生成
- 实施PR：待创建
- 位置：`internal/worker/opencodeserver/worker.go`

## 根因与影响

对/session/{id}/reset的404返回成功并递增generation、注入内部reset事件；404不证明原生历史清空。

## 复现路径

fake对reset返回404并保留历史，断言结果、generation及内部事件。

## 验收条件

404/500/取消不得成功或更改generation；原会话权限保留；200/204扩展协议兼容；不伪造上游reset能力。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。

## 上游参考

https://opencode.ai/docs/server/（2026-09-12核验；运行实例版本需单独记录。）
