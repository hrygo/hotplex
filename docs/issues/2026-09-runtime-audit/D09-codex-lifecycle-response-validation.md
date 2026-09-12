---
title: "D09：Codex启动接受缺少原生线程或轮次ID的响应"
weight: 9
description: "根因、触发路径、修复与验收证据。"
---

# D09 — Codex启动接受缺少原生线程或轮次ID的响应

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / new
- 状态：修复已通过定向与相关测试，未合并
- 证据：Test；源码分析与运行复现已相互印证，非Live
- 实施PR：对应分支 `fix/987-d09-codex-lifecycle-response-validation`；目标分支 `fix/987-d08-codex-interrupt-rpc`（堆叠依赖）
- 位置：`internal/worker/codexcli/worker.go`

## 根因与影响

startNewThread对空对象/null仅Unmarshal后建空ID订阅；startTurn解析失败或缺少turn.id仅Debug仍返回成功，可能保留旧turnID。

## 复现路径

thread/start返回{}、null、空白ID；turn/start缺失ID或类型错误；检查订阅、引用、成功状态及旧turnID。

## 验收条件

提交状态前验证ID；无空订阅、虚假ready或旧turnID复用；已发送turn请求的无效响应按unknown/timeout处理，禁止自动重投；不泄漏响应正文。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。


## 本次验证

父提交：`12e5ef37994ec32481559cc126db52de9bb73d4f`。验证运行：https://github.com/hrygo/hotplex/actions/runs/34693369860

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 修复前定向回归 | 1 | 9 | 0 |
| 修复后定向回归，5轮race/shuffle | 50 | 0 | 0 |
| 相关包短测，race/shuffle | 287 | 0 | 4 |

计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。

实际跳过：
- `TestManagerAcquireStartsProcess`
- `TestIntegrationKillImmediatelyTerminatesIdleProcess`
- `TestManager_PerThreadConverterIsolation/concurrent_dispatch_no_data_race`
- `TestIntegrationStartSavesSessionAndResetRestarts`
