---
title: "D06：飞书Adapter关闭时丢失shutdown期限"
weight: 6
description: "根因、触发路径、修复与验收证据。"
---

# D06 — 飞书Adapter关闭时丢失shutdown期限

- 优先级：P2
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / A08
- 状态：修复已通过定向与相关测试，未合并
- 证据：Test；源码分析与运行复现已相互印证，非Live
- 实施PR：[统一 PR #986](https://github.com/hrygo/hotplex/pull/986)；关联 Issue #987
- 位置：`internal/messaging/feishu/adapter.go`, `internal/messaging/feishu/chat_queue.go`

## 根因与影响

ChatQueue.Close本身约定graceful drain；但Adapter.Close(ctx)调用无期限Close，任务从Background派生十分钟期限，使上层shutdown预算失效。

## 复现路径

活跃context-aware任务和积压任务存在时耗尽Adapter.Close的context。

## 验收条件

保留兼容graceful Close，提供有期限关闭；期限到达取消任务，按预算返回可识别错误并继续其他资源清理；关闭拒绝入队；不通过无限goroutine掩盖不合作任务。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。


## 本次验证

父提交：`6ad409c28d503c1f986434fa2b41239afd408577`。验证运行：https://github.com/hrygo/hotplex/actions/runs/34693369860

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 修复前定向回归 | 1 | 1 | 0 |
| 修复后定向回归，5轮race/shuffle | 15 | 0 | 0 |
| 相关包短测，race/shuffle | 917 | 0 | 1 |

计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。

旧Close仍无限期graceful drain；Adapter使用CloseContext遵守预算。取消后未执行任务计入DiscardedTasks。不合作的既有任务仍可能存活，但不会让本次CloseContext越过预算，也不会按关闭调用次数新建无限等待goroutine。

实际跳过：
- `TestAudioToPCM_Success`

## 统一交付复核

修复提交：`695c0ea6c7b32790ddd4b2623d9e2cfa44fa1464`。十项连续提交的完整代码树：`34f937028c3796eef35e87575618b134d39051cb`。

[统一验证运行](https://github.com/hrygo/hotplex/actions/runs/34694026803) 再次执行所有 D01–D10 定向测试（5 轮 race/shuffle）、相关模块、96 场景契约矩阵及 Go SDK。
结果与实际跳过名单见同目录 `verification-20260912.json`；原始日志保存在运行 artifact。提交/推送结果以该运行最终状态与 PR HEAD 为准。未合并 main，未连接真实平台或收费模型。
