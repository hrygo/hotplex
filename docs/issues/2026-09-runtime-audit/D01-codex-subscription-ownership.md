---
title: "D01：Codex订阅通道多方关闭与发送竞态"
weight: 1
description: "根因、触发路径、修复与验收证据。"
---

# D01 — Codex订阅通道多方关闭与发送竞态

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A01
- 状态：修复已通过定向与相关测试，未合并
- 证据：Test；源码分析与运行复现已相互印证，非Live
- 实施PR：对应分支 `fix/987-d01-codex-subscription-ownership`；目标分支 `fix/985-worker-channel-hardening`（堆叠依赖）
- 位置：`internal/worker/codexcli/manager.go`, `internal/worker/codexcli/worker.go`

## 根因与影响

manager.Shutdown/monitorProcess和appConn.Close都直接关闭同一recvCh，appConn.TrySend没有同步；recover不建立send/close同步。

## 复现路径

实际Worker.Start后manager.Shutdown再connection.Close；关闭后TrySend；满队列critical发送与关闭交错。

## 验收条件

共享订阅对象拥有channel和发送关闭同步；顺序及并发关闭无panic/race；关闭唤醒阻塞发送；缓冲事件仍可读；旧订阅引用不能关闭新订阅。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。


## 本次验证

父提交：`fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3`。验证运行：https://github.com/hrygo/hotplex/actions/runs/34693369860

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 修复前定向回归 | 0 | 3 | 0 |
| 修复后定向回归，5轮race/shuffle | 15 | 0 | 0 |
| 相关包短测，race/shuffle | 261 | 0 | 4 |

计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。

实际跳过：
- `TestManagerAcquireStartsProcess`
- `TestIntegrationStartSavesSessionAndResetRestarts`
- `TestManager_PerThreadConverterIsolation/concurrent_dispatch_no_data_race`
- `TestIntegrationKillImmediatelyTerminatesIdleProcess`
