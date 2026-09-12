---
title: "D02：Codex重置建线程失败泄漏共享进程引用"
weight: 2
description: "根因、触发路径、修复与验收证据。"
---

# D02 — Codex重置建线程失败泄漏共享进程引用

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A04
- 状态：修复已通过定向与相关测试，未合并
- 证据：Test；源码分析与运行复现已相互印证，非Live
- 实施PR：对应分支 `fix/987-d02-codex-reference-release`；目标分支 `fix/987-d01-codex-subscription-ownership`（堆叠依赖）
- 位置：`internal/worker/codexcli/worker.go`

## 根因与影响

reset建线程失败先设置closed，release以released||closed提前返回；连接关闭状态与Acquire引用所有权混用。

## 复现路径

另一会话持有1引用，Worker.Start使其为2，reset线程创建失败，再重复Terminate/Kill，预期仅剩另一会话的1。

## 验收条件

独立建模引用持有；Start/Resume/Reset失败及重复终止恰好释放一次；未Acquire实例不能减少他人引用。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。


## 本次验证

父提交：`3da1511e7428ec277be5aafe830f946bf8d893c3`。验证运行：https://github.com/hrygo/hotplex/actions/runs/34693369860

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 修复前定向回归 | 1 | 1 | 0 |
| 修复后定向回归，5轮race/shuffle | 10 | 0 | 0 |
| 相关包短测，race/shuffle | 263 | 0 | 4 |

计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。

实际跳过：
- `TestManagerAcquireStartsProcess`
- `TestIntegrationKillImmediatelyTerminatesIdleProcess`
- `TestIntegrationStartSavesSessionAndResetRestarts`
- `TestManager_PerThreadConverterIsolation/concurrent_dispatch_no_data_race`
