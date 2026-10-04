---
title: "D05：Go SDK 默认无法连接网关（Origin 校验）"
weight: 5
description: "网关要求 Origin 匹配，gorilla 客户端不发送 Origin，直接 403。"
---

# D05 — Go SDK 默认无法连接网关

- 优先级：P2
- 基准：`1698d2c2`
- 证据：Live
- 状态：已修（`f896c630`，待验证）
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

## 契约决策与修复（2026-10-04，`f896c630`）

修服务端，不放宽 CORS：浏览器升级必带 Origin，非浏览器客户端（Go SDK、
CLI）不带。`Origin` 的作用是让服务端区分浏览器来源，而非认证原生客户端
（它们走 AEP / 应用层鉴权）。单一规则收敛在
`security.CheckWebSocketOrigin`，网关 upgrader 直接调用：

- `*` 允许一切（含缺失 Origin）；
- 精确匹配允许浏览器升级；
- 缺失 Origin 在固定名单下允许；
- 出现但不在名单的 Origin 仍拒绝；空名单拒绝一切。

CORS（浏览器）侧不变：`ResolveOrigin` 对固定名单仍拒绝空 Origin。
`docs/reference/sdk-go.md` 新增「连接与 Origin 校验」一节。

实测（真实网关，白名单固定为 `https://app.example.com`）：无 Origin 连接
成功（此前 403）、匹配 Origin 连接成功、`https://evil.com` 仍 403。

## 验收条件

- 默认 `allowed_origins` 配置下 SDK 可连接；
- 配置了具体 origin 白名单时，非白名单 Origin 仍被拒绝；
- 缺失 Origin 的语义在文档中写明。

