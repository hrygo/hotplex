---
title: "D03：Codex排队写入未绑定进程传输快照"
weight: 3
description: "根因、触发路径、修复与验收证据。"
---

# D03 — Codex排队写入未绑定进程传输快照

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A03
- 状态：已登记，待修复
- 证据：Source；本批运行证据尚未生成
- 实施PR：待创建
- 位置：`internal/worker/codexcli/manager.go`

## 根因与影响

writeFrame取得writeMu后才读m.stdin，而启动和退出用另一把锁更新stdin；未取消旧请求可写入替代进程，nil writer还可能panic。A02取消fencing不等于代际绑定。

## 复现路径

请求等待写锁，替换传输后放行；另测没有stdin的Call/Notify。

## 验收条件

请求捕获不可变writer及generation，开始编码前验证；旧请求不写新writer；nil传输返回错误；启动握手不自锁；race通过，不自动重放unknown。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。
