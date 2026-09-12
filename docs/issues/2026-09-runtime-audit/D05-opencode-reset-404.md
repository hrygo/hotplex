---
title: "D05：OpenCode reset把HTTP404误报为上下文清空成功"
weight: 5
description: "根因、触发路径、修复与验收证据。"
---

# D05 — OpenCode reset把HTTP404误报为上下文清空成功

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A06
- 状态：修复已通过定向与相关测试，未合并
- 证据：Test；源码分析与运行复现已相互印证，非Live
- 实施PR：对应分支 `fix/987-d05-opencode-reset-404`；目标分支 `fix/987-d03-codex-transport-generation`（堆叠依赖）
- 位置：`internal/worker/opencodeserver/worker.go`

## 根因与影响

对/session/{id}/reset的404返回成功并递增generation、注入内部reset事件；404不证明原生历史清空。

## 复现路径

fake对reset返回404并保留历史，断言结果、generation及内部事件。

## 验收条件

404/500/取消不得成功或更改generation；原会话权限保留；200/204扩展协议兼容；不伪造上游reset能力。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。

## 上游参考

https://opencode.ai/docs/server/（2026-09-12核验；运行实例版本需单独记录。）


## 本次验证

父提交：`c56454028f5e097dacd892c72e732267f12b17ad`。验证运行：https://github.com/hrygo/hotplex/actions/runs/34693369860

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 修复前定向回归 | 3 | 2 | 0 |
| 修复后定向回归，5轮race/shuffle | 25 | 0 | 0 |
| 相关包短测，race/shuffle | 375 | 0 | 3 |

计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。

本次明确将404改为失败并保持generation不变，保留200/204兼容扩展；不宣称已实现上游没有承诺的原地reset或新会话替换。

实际跳过：
- `TestOpenCodeServerWorker_Start_WithBinary`
- `TestOpenCodeServerWorker_Resume_WithBinary`
- `TestSingletonProcessManager_IdleDrain_KillsWithoutProcMuDeadlock`
