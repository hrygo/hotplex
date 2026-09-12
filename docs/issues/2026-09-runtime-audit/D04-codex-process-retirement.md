---
title: "D04：Codex旧进程清理和空闲回收与新Acquire交错"
weight: 4
description: "根因、触发路径、修复与验收证据。"
---

# D04 — Codex旧进程清理和空闲回收与新Acquire交错

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A05
- 状态：已登记，待修复
- 证据：Source；本批运行证据尚未生成
- 实施PR：待创建
- 位置：`internal/worker/codexcli/manager.go`

## 根因与影响

monitorProcess清理订阅和converter前先发布idle；新代状态可被旧清理删除。idle kill检查零引用后解锁，Acquire可能取得即将被杀的进程；旧timer可清掉新timer。

## 复现路径

阻塞旧代清理后尝试新Acquire/Subscribe；回收认领后Acquire；重排idle timer。

## 验收条件

显式回收认领，monitor绑定进程并验证所有权；清理完成再发布idle；timer核对身份；新引用不被旧kill误伤；Shutdown后不复活。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。
