---
title: "D03：Codex排队写入未绑定进程传输快照"
weight: 3
description: "根因、触发路径、修复与验收证据。"
---

# D03 — Codex排队写入未绑定进程传输快照

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A03
- 状态：修复已通过定向与相关测试，未合并
- 证据：Test；源码分析与运行复现已相互印证，非Live
- 实施PR：对应分支 `fix/987-d03-codex-transport-generation`；目标分支 `fix/987-d10-interaction-timeout-generation`（堆叠依赖）
- 位置：`internal/worker/codexcli/manager.go`

## 根因与影响

writeFrame取得writeMu后才读m.stdin，而启动和退出用另一把锁更新stdin；未取消旧请求可写入替代进程，nil writer还可能panic。A02取消fencing不等于代际绑定。

## 复现路径

请求等待写锁，替换传输后放行；另测没有stdin的Call/Notify。

## 验收条件

请求捕获不可变writer及generation，开始编码前验证；旧请求不写新writer；nil传输返回错误；启动握手不自锁；race通过，不自动重放unknown。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。


## 本次验证

父提交：`4f3bd9d31054b2c74813a2b34a1fb4aeb890cb62`。验证运行：https://github.com/hrygo/hotplex/actions/runs/34693369860

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 修复前定向回归 | 0 | 1 | 0 |
| 修复后定向回归，5轮race/shuffle | 10 | 0 | 0 |
| 相关包短测，race/shuffle | 265 | 0 | 4 |

计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。

基线仅执行排队写入代际用例；缺失stdin安全性用例在修复后执行，避免旧版异步panic终止整包。

实际跳过：
- `TestManagerAcquireStartsProcess`
- `TestManager_PerThreadConverterIsolation/concurrent_dispatch_no_data_race`
- `TestIntegrationKillImmediatelyTerminatesIdleProcess`
- `TestIntegrationStartSavesSessionAndResetRestarts`
