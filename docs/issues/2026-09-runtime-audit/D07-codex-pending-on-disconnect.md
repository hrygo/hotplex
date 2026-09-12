---
title: "D07：Codex传输关闭后pending RPC仍等待完整超时"
weight: 7
description: "根因、触发路径、修复与验收证据。"
---

# D07 — Codex传输关闭后pending RPC仍等待完整超时

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / new
- 状态：修复与回归已通过；发布结果见验证运行和关联PR
- 证据：Test；同一回归修复前失败、修复后通过；未执行Live验收
- 实施PR：待创建
- 位置：`internal/worker/codexcli/manager.go`

## 根因与影响

Call写出后只等response/ctx/timer；Shutdown和stdout EOF没有给等待广播传输不可用，握手及其他请求可能占用完整CallTimeout。

## 复现路径

无deadline请求已写出并等待，触发Shutdown或EOF；不推进到CallTimeout即可观察退出。

## 验收条件

绑定每个传输代际的一次性关闭信号；及时释放所有等待及pending；旧代关闭不取消新请求；不关闭仍可能被dispatcher写入的响应通道。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。

## 本批独立修复证据

GitHub Issue #994；基准 `fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3`；验证运行：https://github.com/hrygo/hotplex/actions/runs/34693276874。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| red | 0 | 4 | 0 |
| green | 25 | 0 | 0 |
| related | 340 | 0 | 4 |

计数包含父子测试和重复轮次。原有断言不削弱，不以编译失败冒充复现。相关跳过、lint、契约和原始提交/推送检查以架构报告与运行日志为准，未合并或部署。
