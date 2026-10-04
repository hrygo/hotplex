---
title: "D03：Go SDK Unsubscribe 与事件投递竞争，可触发 send on closed channel"
weight: 3
description: "deliver 在锁外发送，Unsubscribe 在锁内关闭通道，二者可并发。"
---

# D03 — Unsubscribe 与投递竞争

- 优先级：P1
- 基准：`1698d2c2`
- 证据：Test（`-race` 稳定复现）＋ Source
- 状态：本分支已修
- 位置：`client/client.go`（`deliver` / `Unsubscribe`）

## 根因

`deliver` 复制监听者快照后在锁外发送；`Unsubscribe` 在锁内把通道从列表移除
并 `close(ch)`。二者交错时，`deliver` 可能向已关闭通道发送。

`-race` 报告：

```
Write at ... by goroutine 13:
  client.(*Client).Unsubscribe()  client.go:377
Previous read by goroutine 39:
  client.(*Client).deliver()      client.go:706
```

生产表现是进程 panic（向已关闭通道发送），而非仅数据竞争。
`SendInputAsync` 自身即 `defer c.Unsubscribe(eventsCh)`，属普通用法。

## 修复

监听者改为 `subscriber{ch, done}`：`Unsubscribe` 只关闭 `done`，
`deliver` 在发送前与发送中同时 select `done`，不再关闭事件通道。

`Close()` 仍关闭全部通道——该路径先 `c.wg.Wait()`，接收泵已退出，
不存在并发发送，因此 `for range ch` 的终止语义不变。

行为变更：`Unsubscribe` 不再关闭通道。已同步 `client/client.go` 注释、
`client/README.md` 与 `docs/reference/sdk-go.md`（含「只订阅一次并持续排空」
的告诫：只注册不排空的通道填满后，SDK 为保证 `done`/`error`/`state` 不丢
而阻塞投递，接收泵随之停滞）。

## 验收条件

- `-race` 下并发投递与取消订阅无竞争、无 panic；
- `Close()` 后 `for range ch` 结束；
- 多 listener 场景下互不影响。

