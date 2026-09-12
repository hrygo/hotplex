---
title: "D01：Codex订阅通道多方关闭与发送竞态"
weight: 1
description: "根因、触发路径、修复与验收证据。"
---

# D01 — Codex订阅通道多方关闭与发送竞态

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A01
- 状态：已登记，待修复
- 证据：Source；本批运行证据尚未生成
- 实施PR：待创建
- 位置：`internal/worker/codexcli/manager.go`, `internal/worker/codexcli/worker.go`

## 根因与影响

manager.Shutdown/monitorProcess和appConn.Close都直接关闭同一recvCh，appConn.TrySend没有同步；recover不建立send/close同步。

## 复现路径

实际Worker.Start后manager.Shutdown再connection.Close；关闭后TrySend；满队列critical发送与关闭交错。

## 验收条件

共享订阅对象拥有channel和发送关闭同步；顺序及并发关闭无panic/race；关闭唤醒阻塞发送；缓冲事件仍可读；旧订阅引用不能关闭新订阅。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。
