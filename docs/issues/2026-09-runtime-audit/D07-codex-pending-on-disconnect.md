---
title: "D07：Codex传输关闭后pending RPC仍等待完整超时"
weight: 7
description: "根因、触发路径、修复与验收证据。"
---

# D07 — Codex传输关闭后pending RPC仍等待完整超时

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / new
- 状态：已登记，待修复
- 证据：Source；本批运行证据尚未生成
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
