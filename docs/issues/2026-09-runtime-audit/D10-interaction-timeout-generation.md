---
title: "D10：交互旧超时回调可能拒绝同ID的新请求"
weight: 10
description: "根因、触发路径、修复与验收证据。"
---

# D10 — 交互旧超时回调可能拒绝同ID的新请求

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / new
- 状态：修复已通过定向与相关测试，未合并
- 证据：Test；源码分析与运行复现已相互印证，非Live
- 实施PR：[统一 PR #986](https://github.com/hrygo/hotplex/pull/986)；关联 Issue #987
- 位置：`internal/messaging/interaction.go`

## 根因与影响

Complete/CompleteClaimed删除map不终止旧watcher；timeout按ID而非对象身份Claim和CompleteClaimed，可消费后来注册的同ID请求并使用旧交互类型。

## 复现路径

注册短TTL旧请求并完成，再注册相同ID长TTL新请求；只推进旧TTL，新请求必须仍pending且未发送拒绝。

## 验收条件

完成时终止watcher；超时比较身份、认领、移除在锁内原子执行；CancelAll及重注册不误拒绝；正常超时仅一次，人工claim不重复；race通过。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。


## 本次验证

父提交：`911fd09c116f6ddc5a276a4bd9a8639d7c52c346`。验证运行：https://github.com/hrygo/hotplex/actions/runs/34693369860

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 修复前定向回归 | 1 | 2 | 0 |
| 修复后定向回归，5轮race/shuffle | 20 | 0 | 0 |
| 相关包短测，race/shuffle | 527 | 0 | 0 |

计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。

相关包无跳过。

## 统一交付复核

修复提交：`4f3bd9d31054b2c74813a2b34a1fb4aeb890cb62`。十项连续提交的完整代码树：`34f937028c3796eef35e87575618b134d39051cb`。

[统一验证运行](https://github.com/hrygo/hotplex/actions/runs/34694026803) 再次执行所有 D01–D10 定向测试（5 轮 race/shuffle）、相关模块、96 场景契约矩阵及 Go SDK。
结果与实际跳过名单见同目录 `verification-20260912.json`；原始日志保存在运行 artifact。提交/推送结果以该运行最终状态与 PR HEAD 为准。未合并 main，未连接真实平台或收费模型。
