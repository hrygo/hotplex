---
title: "D07：Codex传输关闭后pending RPC仍等待完整超时"
weight: 7
description: "根因、触发路径、修复与验收证据。"
---

# D07 — Codex传输关闭后pending RPC仍等待完整超时

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / new
- 状态：修复已通过定向与相关测试，未合并
- 证据：Test；源码分析与运行复现已相互印证，非Live
- 实施PR：[统一 PR #986](https://github.com/hrygo/hotplex/pull/986)；关联 Issue #987
- 位置：`internal/worker/codexcli/manager.go`

## 根因与影响

Call写出后只等response/ctx/timer；Shutdown和stdout EOF没有给等待广播传输不可用，握手及其他请求可能占用完整CallTimeout。

## 复现路径

无deadline请求已写出并等待，触发Shutdown或EOF；不推进到CallTimeout即可观察退出。

## 验收条件

绑定每个传输代际的一次性关闭信号；及时释放所有等待及pending；旧代关闭不取消新请求；不关闭仍可能被dispatcher写入的响应通道。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。


## 本次验证

父提交：`012aafa8d236f27014c8c9b1d0b3509c54251897`。验证运行：https://github.com/hrygo/hotplex/actions/runs/34693369860

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 修复前定向回归 | 0 | 3 | 0 |
| 修复后定向回归，5轮race/shuffle | 20 | 0 | 0 |
| 相关包短测，race/shuffle | 273 | 0 | 4 |

计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。

实际跳过：
- `TestManagerAcquireStartsProcess`
- `TestManager_PerThreadConverterIsolation/concurrent_dispatch_no_data_race`
- `TestIntegrationKillImmediatelyTerminatesIdleProcess`
- `TestIntegrationStartSavesSessionAndResetRestarts`

## 统一交付复核

修复提交：`b6ec8c7d3f07be1473c1b2184216d61a70a81510`。十项连续提交的完整代码树：`34f937028c3796eef35e87575618b134d39051cb`。

[统一验证运行](https://github.com/hrygo/hotplex/actions/runs/34694026803) 再次执行所有 D01–D10 定向测试（5 轮 race/shuffle）、相关模块、96 场景契约矩阵及 Go SDK。
结果与实际跳过名单见同目录 `verification-20260912.json`；原始日志保存在运行 artifact。提交/推送结果以该运行最终状态与 PR HEAD 为准。未合并 main，未连接真实平台或收费模型。
