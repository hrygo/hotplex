---
title: "D02：Go SDK 在首个 done 后停止读取，多回合会话不可观测"
weight: 2
description: "done 是回合终态而非会话终态；SDK 读到第一个 done 就退出接收泵。"
---

# D02 — Go SDK 在首个 done 后停止读取

- 优先级：P1
- 基准：`1698d2c2`（`client/client.go` 未改动，与本分支无关）
- 证据：Live（真实 Worker，补入输入触发第二轮）＋ Test（变异验证）
- 状态：本分支已修
- 位置：`client/client.go`（`recvPump`）

## 触发路径

一轮会话中，客户端在首轮进行期间补发一条输入。网关把它作为
`input_mode=injected` / `durability=volatile` 的补入输入注入，
首轮 `done` 后由 `replayPending` 驱动第二轮，第二轮再产生一个 `done`。

代理抓帧证实第二轮的两帧确实在链路上：

```
09:44:02.902  message.delta  seq=9
09:44:02.902  runtime.execution.completed seq=10
09:44:06.320  done           seq=11   ← SDK 收到的最后一个事件
09:44:06.479  message.delta  seq=12   ← SDK 从未投递
09:44:06.479  done           seq=13   ← SDK 从未投递
```

`recvPump` 在读到第一个 `done` 后 `return`，第二轮对调用方完全不可见，
表现为会话挂死。

## 根因

`recvPump` 用 `aep.IsTerminalEvent` 判定会话结束，而该函数包含 `Done`。
AEP 中 `done` 是**回合**终态，会话生命周期等于 WebSocket 生命周期。

## 修复

`recvPump` 仅在 `error` 时返回。新增 `TestClientSurfacesEventsAfterTheFirstDone`：
一个真实 WebSocket fixture 在同一连接上发 `done → message.delta → done`，
断言三轮事件全部投递。变异验证——反向应用修复后该测试失败，
实际收集到 `[]`，与实测症状一致。

`error` 未一并改动：网关对可重试错误会 `autoRetry` 并继续会话，
其语义上同样不是会话终态，但尚无 Live 证据，按「无证据不改」保留原行为。

## 验收条件

- 同会话内多个 `done` 全部可观测；
- `Close()` 后接收泵退出、通道关闭，`for range` 正常结束；
- TS / Python / Java 示例 SDK 若存在同类终止判定，需同样核对。

