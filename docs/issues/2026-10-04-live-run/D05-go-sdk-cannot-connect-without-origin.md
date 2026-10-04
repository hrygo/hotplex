---
title: "D05：Go SDK 默认无法连接网关（Origin 校验）"
weight: 5
description: "网关要求 Origin 匹配，gorilla 客户端不发送 Origin，直接 403。"
---

# D05 — Go SDK 默认无法连接网关

- 优先级：P2
- 基准：`1698d2c2`
- 证据：Live
- 状态：未修
- 位置：`internal/gateway/hub.go:219`（`CheckOrigin`）、`client/client.go`（dial）

## 实测

Go SDK 连接 `ws://127.0.0.1:8888/ws` 返回 403。gorilla 的 dialer 不发送
`Origin` 头，而网关的 `CheckOrigin` 要求 Origin 命中
`security.allowed_origins`。只有把 `security.allowed_origins` 设为 `*`
才能连接。

## 影响

按默认安全配置，SDK 客户端无法连接网关；文档与示例未提示这一点。
`origin/main` 同样如此，非本分支引入。

## 待查（修哪一侧需先定契约）

- 服务端：允许缺失 Origin（等同 gorilla 默认行为），仅在 Origin 存在时校验；
  还是保持严格，改为要求 SDK 显式发送 Origin；
- 客户端：是否应默认发送 `Origin`。

两种取向的安全含义不同，应先定契约再改，不宜在实测中顺手放宽。

## 验收条件

- 默认 `allowed_origins` 配置下 SDK 可连接；
- 配置了具体 origin 白名单时，非白名单 Origin 仍被拒绝；
- 缺失 Origin 的语义在文档中写明。

