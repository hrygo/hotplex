---
title: "D01：runtime.execution.completed 先于 done 送达，与 AEP 契约不符"
weight: 1
description: "契约要求终态事实在 done 之后送达；实测相反，并导致 seq 与到达顺序倒置。"
---

# D01 — runtime.execution.completed 先于 done 送达

- 优先级：P1
- 基准：`1698d2c2`（在 `origin/main` 上同样复现，非本分支引入）
- 证据：Live（真实 PostgreSQL + 真实 Claude Code Worker）＋ 契约文档 ＋ Source
- 状态：已修（`668aa36c`，待验证）
- 位置：`internal/gateway/bridge_forward.go`（`processForwardedEvent` / `finishRuntimeOnDone`）

## 契约

`docs/reference/aep-protocol.md:385-386`：

- `started` 在 `input.ack(delivered)` 之后、`done` 之前发送；
- `completed` / `failed` 在 `done` **之后**发送（终态通知，与 run 结果分离）。

第 582 行的完整时序同样以 `done → runtime.execution.{completed,failed}` 结尾。

## 实测

真实 Worker 的一轮会话，客户端按到达顺序实际收到：

```
message.delta              seq=9
runtime.execution.completed seq=10
done                       seq=11
```

`runtime.execution.completed` 先于 `done` 到达，与契约相反。

## 影响

1. 顺序倒置在修复前还会让 `done` 拿到比 `runtime.execution.completed` 更小的 seq，
   依赖 seq 单调递增的客户端（本仓库 Go SDK、WebChat）会丢弃终态 `done`，
   表现为「回合永不结束」。该症状已由 `60b7be7a` 修复并有回归测试。
2. 顺序本身仍偏离契约：客户端看到「执行已完成」时，Agent 尚未宣告本轮结束。
   这正是本计划要排除的「Agent done ≠ 交付完成」类错误。

## 根因

`processForwardedEvent` 在处理 `Done` 的过程中先调用 `finishRuntimeOnDone`
（`bridge_forward.go:543`），后者持久化 `FinishRuntime` 后立刻投递
`runtime.execution.completed`；`Done` 自身则在函数末尾才投递。

`finishRuntimeOnDone` 同时承担三件事，顺序调整并非纯粹搬动一行：

1. `FinishRuntime` 持久化写入；
2. `replayPending` —— 释放 active gate 并重放缓冲的补入输入（第二轮的驱动力）；
3. 投递 `runtime.execution.{completed,failed}`。

## 修复（2026-10-04，`668aa36c`）

把「持久化 + 放行」和「投递终态事实」拆开：`finishRuntimeOnDone` 仍在
`done` 发送前持久化 `FinishRuntime` 并释放 active gate（`replayPending` +
`dispatchQueued`，第二轮的驱动力不变），但不再直接发送终态事实，而是
返回 `pendingRuntimeFact`，由 `processForwardedEvent` 在 `done` 上线后投递。
失败轮次（`done.success=false` ＋ pending Error）中 Error 是用户可见终态，
失败事实跟在 Error 之后。持久化失败仍不投递、走 repairer 重试；被重试取代
的 Done 仍不持久化（执行归属最终终止的那次尝试）。

既有 seq 测试此前显式 pin 住旧顺序（注释写明“当前网关的发送顺序，非契约
顺序”），现已改为断言契约顺序：done 先、completed 后、seq 同序递增。
变异验证：stash 修复后测试红，`completed` 先到——正是线上症状。

## 验收条件

- 客户端观察到的顺序为 `done` 先于 `runtime.execution.completed`，
  且 seq 同序递增；
- 持久化 `FinishRuntime` 与投递解耦：durable 写失败时不得投递终态事实
  （现有行为已如此，调整后须保持）；
- `replayPending` 仍在 `done` 之后被触发，第二轮不被吞掉；
- 失败轮次（`done.success=false` ＋ pending Error）与崩溃恢复重试路径行为不变；
- 三渠道 × 四 Worker 契约矩阵与既有 seq 测试全绿，并新增一条断言上述顺序的回归。

