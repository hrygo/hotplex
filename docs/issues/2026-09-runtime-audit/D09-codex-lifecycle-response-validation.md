---
title: "D09：Codex启动接受缺少原生线程或轮次ID的响应"
weight: 9
description: "根因、触发路径、修复与验收证据。"
---

# D09 — Codex启动接受缺少原生线程或轮次ID的响应

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / new
- 状态：修复与回归已通过；发布结果见验证运行和关联PR
- 证据：Test；同一回归修复前失败、修复后通过；未执行Live验收
- 实施PR：待创建
- 位置：`internal/worker/codexcli/worker.go`

## 根因与影响

startNewThread对空对象/null仅Unmarshal后建空ID订阅；startTurn解析失败或缺少turn.id仅Debug仍返回成功，可能保留旧turnID。

## 复现路径

thread/start返回{}、null、空白ID；turn/start缺失ID或类型错误；检查订阅、引用、成功状态及旧turnID。

## 验收条件

提交状态前验证ID；无空订阅、虚假ready或旧turnID复用；已发送turn请求的无效响应按unknown/timeout处理，禁止自动重投；不泄漏响应正文。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。

## 本批独立修复证据

GitHub Issue #1004；基准 `fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3`；验证运行：https://github.com/hrygo/hotplex/actions/runs/34693059163。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| red | 2 | 12 | 0 |
| green | 70 | 0 | 0 |
| related | 349 | 0 | 4 |

计数包含父子测试和重复轮次。原有断言不削弱，不以编译失败冒充复现。相关跳过、lint、契约和原始提交/推送检查以架构报告与运行日志为准，未合并或部署。
