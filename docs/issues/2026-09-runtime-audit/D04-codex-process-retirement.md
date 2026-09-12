---
title: "D04：Codex旧进程清理和空闲回收与新Acquire交错"
weight: 4
description: "根因、触发路径、修复与验收证据。"
---

# D04 — Codex旧进程清理和空闲回收与新Acquire交错

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A05
- 状态：修复已通过定向与相关测试，未合并
- 证据：Test；源码分析与运行复现已相互印证，非Live
- 实施PR：对应分支 `fix/987-d04-codex-process-retirement`；目标分支 `fix/987-d06-feishu-shutdown-deadline`（堆叠依赖）
- 位置：`internal/worker/codexcli/manager.go`

## 根因与影响

monitorProcess清理订阅和converter前先发布idle；新代状态可被旧清理删除。idle kill检查零引用后解锁，Acquire可能取得即将被杀的进程；旧timer可清掉新timer。

## 复现路径

阻塞旧代清理后尝试新Acquire/Subscribe；回收认领后Acquire；重排idle timer。

## 验收条件

显式回收认领，monitor绑定进程并验证所有权；清理完成再发布idle；timer核对身份；新引用不被旧kill误伤；Shutdown后不复活。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。


## 本次验证

父提交：`695c0ea6c7b32790ddd4b2623d9e2cfa44fa1464`。验证运行：https://github.com/hrygo/hotplex/actions/runs/34693369860

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 修复前定向回归 | 1 | 2 | 0 |
| 修复后定向回归，5轮race/shuffle | 20 | 0 | 0 |
| 相关包短测，race/shuffle | 269 | 0 | 4 |

计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。

回收交错测试创建并终止本测试专用子进程，没有向任意宿主PID发信号。旧monitor替代进程隔离另有修复后定向断言。

实际跳过：
- `TestIntegrationStartSavesSessionAndResetRestarts`
- `TestManager_PerThreadConverterIsolation/concurrent_dispatch_no_data_race`
- `TestIntegrationKillImmediatelyTerminatesIdleProcess`
- `TestManagerAcquireStartsProcess`
