---
title: "D02：Codex重置建线程失败泄漏共享进程引用"
weight: 2
description: "根因、触发路径、修复与验收证据。"
---

# D02 — Codex重置建线程失败泄漏共享进程引用

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A04
- 状态：已登记，待修复
- 证据：Source；本批运行证据尚未生成
- 实施PR：待创建
- 位置：`internal/worker/codexcli/worker.go`

## 根因与影响

reset建线程失败先设置closed，release以released||closed提前返回；连接关闭状态与Acquire引用所有权混用。

## 复现路径

另一会话持有1引用，Worker.Start使其为2，reset线程创建失败，再重复Terminate/Kill，预期仅剩另一会话的1。

## 验收条件

独立建模引用持有；Start/Resume/Reset失败及重复终止恰好释放一次；未Acquire实例不能减少他人引用。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。
