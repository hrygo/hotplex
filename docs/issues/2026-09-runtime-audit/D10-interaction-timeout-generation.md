---
title: "D10：交互旧超时回调可能拒绝同ID的新请求"
weight: 10
description: "根因、触发路径、修复与验收证据。"
---

# D10 — 交互旧超时回调可能拒绝同ID的新请求

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / new
- 状态：修复与回归已通过；发布结果见验证运行和关联PR
- 证据：Test；同一回归修复前失败、修复后通过；未执行Live验收
- 实施PR：待创建
- 位置：`internal/messaging/interaction.go`

## 根因与影响

Complete/CompleteClaimed删除map不终止旧watcher；timeout按ID而非对象身份Claim和CompleteClaimed，可消费后来注册的同ID请求并使用旧交互类型。

## 复现路径

注册短TTL旧请求并完成，再注册相同ID长TTL新请求；只推进旧TTL，新请求必须仍pending且未发送拒绝。

## 验收条件

完成时终止watcher；超时比较身份、认领、移除在锁内原子执行；CancelAll及重注册不误拒绝；正常超时仅一次，人工claim不重复；race通过。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。

## 本批独立修复证据

GitHub Issue #1002；基准 `fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3`；验证运行：https://github.com/hrygo/hotplex/actions/runs/34693473176。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| red | 1 | 4 | 0 |
| green | 30 | 0 | 0 |
| related | 529 | 0 | 0 |

计数包含父子测试和重复轮次。原有断言不削弱，不以编译失败冒充复现。相关跳过、lint、契约和原始提交/推送检查以架构报告与运行日志为准，未合并或部署。
