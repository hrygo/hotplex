---
title: "D02：Codex重置建线程失败泄漏共享进程引用"
weight: 2
description: "根因、触发路径、修复与验收证据。"
---

# D02 — Codex重置建线程失败泄漏共享进程引用

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A04
- 状态：修复及回归已完成，发布状态见运行结果
- 证据：Test；原实现失败、修复后通过；未执行Live联调
- 实施PR：待创建
- 位置：`internal/worker/codexcli/worker.go`

## 根因与影响

reset建线程失败先设置closed，release以released||closed提前返回；连接关闭状态与Acquire引用所有权混用。

## 复现路径

另一会话持有1引用，Worker.Start使其为2，reset线程创建失败，再重复Terminate/Kill，预期仅剩另一会话的1。

## 验收条件

独立建模引用持有；Start/Resume/Reset失败及重复终止恰好释放一次；未Acquire实例不能减少他人引用。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。

## 本批修复与验证

对应 GitHub Issue #990；实现基准 `fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3`。独立验证运行：https://github.com/hrygo/hotplex/actions/runs/34692685711。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| red | 2 | 1 | 0 |
| green | 15 | 0 | 0 |
| related | 338 | 0 | 4 |

计数包含重复轮次和父子用例。相关跳过项见同一提交的架构验证报告。完整lint、原始hooks和发布尚由后续步骤确认；不因文档存在就宣称远端已交付。
