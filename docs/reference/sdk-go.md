---
title: Go Client SDK 参考
weight: 6
description: 基于 AEP v1 协议的 HotPlex Worker Gateway Go 客户端 SDK 完整参考
---

# Go Client SDK 参考

> `github.com/hrygo/hotplex/client` — HotPlex Gateway 的官方 Go 客户端，实现 AEP v1 WebSocket 全双工协议。

> **SDK 位置**：Go SDK 位于仓库根目录的 `client/`（独立 Go module），Python / TypeScript / Java SDK 位于 `examples/` 目录。

## 安装

```bash
go get github.com/hrygo/hotplex/client
```

依赖：`gorilla/websocket`（WebSocket）。

## 快速开始

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/hrygo/hotplex/client"
)

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    c, err := client.New(ctx,
        client.URL("ws://localhost:8888"),
        client.WorkerType("claude_code"),
        client.APIKey(os.Getenv("HOTPLEX_API_KEY")),
        client.AutoReconnect(true),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer c.Close()

    ack, err := c.Connect(ctx)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("session: %s, state: %s\n", ack.SessionID, ack.State)

    ch := c.Events()
    defer c.Unsubscribe(ch)

    if err := c.SendInput(ctx, "用 Go 写一个 hello world"); err != nil {
        log.Fatal(err)
    }

    for evt := range ch {
        switch evt.Type {
        case client.EventMessageDelta:
            if d, ok := evt.AsMessageDeltaData(); ok {
                fmt.Print(d.Content)
            }
        case client.EventDone:
            return
        case client.EventError:
            if d, ok := evt.AsErrorData(); ok {
                fmt.Fprintf(os.Stderr, "error: %s: %s\n", d.Code, d.Message)
            }
            return
        }
    }
}
```

## 核心 API

### 创建客户端

使用 Functional Options 模式。`URL` 和 `WorkerType` 为必填项：

```go
c, err := client.New(ctx,
    client.URL("ws://localhost:8888"),              // 必填：Gateway WebSocket 地址
    client.WorkerType("claude_code"),                // 必填：Worker 类型
    client.BotID("bot-123"),                         // Bot ID for multi-bot setups
    client.APIKey("ak-xxx"),                         // X-API-Key header（可选）
    client.AutoReconnect(true),                      // 启用指数退避自动重连
    client.PingInterval(54*time.Second),             // 心跳间隔（默认 54s）
    client.ClientSessionID("my-stable-id"),          // 确定性 session ID
    client.Metadata(map[string]any{"env": "test"}),  // Init 附加元数据
    client.Logger(slog.Default()),                   // 自定义 slog 日志
)
```

### 连接与会话

```go
// 新建连接 — 发送 init 握手，返回 InitAckData
ack, err := c.Connect(ctx)
// ack.SessionID   — 服务端分配的 session ID（sess_xxx 格式）
// ack.State       — 初始状态（created / running）
// ack.ServerCaps  — 服务端能力集

// 恢复已有 session — 跳过 worker 冷启动
ack, err := c.Resume(ctx, "sess_xxxx")

// 查询运行时状态
c.SessionID()  // 当前 session ID
c.State()      // 当前 SessionState
```

### 发送方法

| 方法 | 事件类型 | Priority | 说明 |
|------|---------|----------|------|
| `SendInput(ctx, content, metadata?)` | `input` | data | 发送用户输入 |
| `SendInputAsync(ctx, content, metadata?)` | `input` → 阻塞等待 | — | 发送并等待 `done`/`error`，返回 `*DoneData` |
| `SendPermissionResponse(ctx, id, approved, reason)` | `permission_response` | control | 响应工具权限请求 |
| `SendQuestionResponse(ctx, id, answers)` | `question_response` | control | 响应问题请求 |
| `SendElicitationResponse(ctx, id, action, content)` | `elicitation_response` | control | 响应 MCP 触发请求 |
| `SendControl(ctx, action)` | `control` | control | 发送控制指令（`terminate` / `delete`） |
| `SendReset(ctx, reason)` | `control` | control | 清空上下文，Worker 自行决定 in-place 或重启 |
| `SendGC(ctx, reason)` | `control` | control | 归档会话，Worker 终止但保留历史 |

### 事件订阅

`Events()` 返回 `<-chan Event`，支持多 listener。使用 `Unsubscribe(ch)` 停止接收并释放资源：

`Events()` 每调用一次都会注册一个新的 listener，请只调用一次并持续排空；
只注册不排空的通道会填满（容量 `SendChannelCap`），此后 SDK 为保证
`done` / `error` / `state` 不丢而阻塞投递，接收泵随之停滞。
`Unsubscribe` 只停止投递，不关闭该通道（关闭通道会与正在投递的事件竞争，
导致向已关闭通道发送而 panic）；通道由 `Close()` 在接收泵停止后统一关闭，
所以 `for range ch` 仍会随 `Close()` 结束。

```go
ch := c.Events()
defer c.Unsubscribe(ch)

for evt := range ch {
    switch evt.Type {
    case client.EventMessageDelta:
        if d, ok := evt.AsMessageDeltaData(); ok {
            fmt.Print(d.Content)
        }
    case client.EventPermissionRequest:
        if d, ok := evt.AsPermissionRequestData(); ok {
            // 自动批准
            c.SendPermissionResponse(ctx, d.ID, true, "")
        }
    case client.EventQuestionRequest:
        if d, ok := evt.AsQuestionRequestData(); ok {
            answers := map[string]string{
                d.Questions[0].Question: d.Questions[0].Options[0].Label,
            }
            c.SendQuestionResponse(ctx, d.ID, answers)
        }
    case client.EventState:
        // recvPump 自动同步 c.State()，此处可选记录日志
    }
}
```

**背压策略**：`done`/`error`/`state` 阻塞投递（永不丢弃），`message.delta`/`raw` 在通道满时静默丢弃。`done` 事件通过 `Dropped` 字段指示是否有 delta 被丢弃。

### 连接生命周期

`Connect` 成功后启动 3 个后台 goroutine：

| Goroutine | 职责 |
|-----------|------|
| `recvPump` | 循环 `NextReader()` → `aep.DecodeLine` → `deliver` 到所有 listener |
| `sendPump` | 循环 `range sendCh` → `WriteMessage`（带背压） |
| `pingPump` | 定时发送 WebSocket Ping（默认 54s 间隔） |

**关闭顺序**：`cancel ctx` → `close ws` → `close sendCh` → `wg.Wait` → `close listeners`。

### 连接与 Origin 校验

Go SDK 基于 gorilla WebSocket 实现，握手默认不发送 `Origin` 头。网关把缺失
`Origin` 视为非浏览器客户端（SDK、CLI 工具），在白名单固定为具体域名时也
允许连接；浏览器升级必带 `Origin`，仍按 `security.allowed_origins` 校验，
名单外来源一律拒绝。非浏览器客户端的身份与权限仍由 AEP / 应用层鉴权保证，
`Origin` 只用于区分浏览器来源，不做客户端认证。

## 事件类型

### AEP v1 事件类型一览

> **注意**：上表列出的是 Go SDK 中导出了 `Kind` 常量的事件类型。部分 AEP 协议事件（如 `context_usage`、`mcp_status`、`skills_list`、`worker_command`、`question_request`、`question_response`、`elicitation_request`、`elicitation_response`）在 Go SDK 中未导出 `Kind` 常量。处理这些事件时需使用字符串形式匹配 `evt.Type == "context_usage"` 等。

| Kind | 方向 | Go Data 类型 | 说明 |
|------|------|-------------|------|
| `init` | C→S | `map[string]any` | 握手初始化 |
| `init_ack` | S→C | `InitAckData` | 握手响应 |
| `error` | S→C | `ErrorData` | 错误通知 |
| `state` | S→C | `StateData` | Session 状态变更 |
| `input` | C→S | — | 用户输入 |
| `message.start` | S→C | `MessageStartData` | 流式消息开始 |
| `message.delta` | S→C | `MessageDeltaData` | 流式内容片段 |
| `message.end` | S→C | `MessageEndData` | 流式消息结束 |
| `message` | S→C | `MessageData` | 完整消息（非流式） |
| `tool_call` | S→C | `ToolCallData` | Worker 调用工具 |
| `tool_result` | S→C | `ToolResultData` | 工具执行结果 |
| `reasoning` | S→C | `ReasoningData` | Agent 推理/思考过程 |
| `step` | S→C | `StepData` | 执行步骤标记 |
| `done` | S→C | `DoneData` | 任务完成（Turn 终止符） |
| `permission_request` | S→C | `PermissionRequestData` | 请求用户授权 |
| `permission_response` | C→S | `PermissionResponseData` | 用户授权/拒绝 |
| `question_request` | S→C | `QuestionRequestData` | 请求用户回答问题 |
| `question_response` | C→S | `QuestionResponseData` | 用户回答 |
| `elicitation_request` | S→C | `ElicitationRequestData` | MCP 请求用户输入 |
| `elicitation_response` | C→S | `ElicitationResponseData` | MCP 用户响应 |
| `control` | 双向 | `ControlData` | 控制指令 |
| `ping` / `pong` | 双向 | — | 心跳 |

### Session 状态机

```
created → running ⇄ idle → terminated → deleted
                ↘ terminated ↗ running (resume)
```

状态通过 `client.StateXxx` 常量访问（`StateCreated` / `StateRunning` / `StateIdle` / `StateTerminated` / `StateDeleted`）。

### 类型安全的 Event 解析

每个 `Event` 提供对应的 `AsXxxData()` 方法，返回 `(T, bool)`：

```go
d, ok := evt.AsMessageDeltaData()    // MessageDeltaData
d, ok := evt.AsDoneData()            // DoneData
d, ok := evt.AsErrorData()           // ErrorData
d, ok := evt.AsToolCallData()        // ToolCallData
d, ok := evt.AsPermissionRequestData()  // PermissionRequestData
d, ok := evt.AsQuestionRequestData()    // QuestionRequestData
d, ok := evt.AsElicitationRequestData() // ElicitationRequestData
d, ok := evt.AsStateData()           // StateData
d, ok := evt.AsReasoningData()       // ReasoningData
d, ok := evt.AsStepData()            // StepData
d, ok := evt.AsMessageStartData()    // MessageStartData
d, ok := evt.AsMessageEndData()      // MessageEndData
d, ok := evt.AsInitAckData()         // InitAckData
```

## 错误处理

### 错误码常量

SDK 导出 `client.ErrCodeXxx` 常量，与网关错误码一一对应：

| 常量 | 值 | 含义 |
|------|----|------|
| `ErrCodeSessionBusy` | `SESSION_BUSY` | Session 正忙，稍后重试 |
| `ErrCodeSessionAlreadyConnected` | `SESSION_ALREADY_CONNECTED` | 已有 WebChat 连接；关闭原连接后由用户显式重试，不能自动重连 |
| `ErrCodeInternalError` | `INTERNAL_ERROR` | 网关内部错误 |
| `ErrCodeUnauthorized` | `UNAUTHORIZED` | 认证失败 |
| `ErrCodeSessionNotFound` | `SESSION_NOT_FOUND` | Session 不存在 |

完整错误码列表参见 `pkg/events/events.go` 中的 `ErrorCode` 定义。

### 错误处理模式

```go
// 1. 连接错误
ack, err := c.Connect(ctx)
if err != nil {
    // err 可能包含 "client: init rejected: CODE: message"
    log.Fatal(err)
}

// 2. 发送错误
if err := c.SendInput(ctx, "hello"); err != nil {
    if errors.Is(err, client.ErrNotConnected) {
        // 未连接
    }
}

// 3. 网关错误事件
for evt := range ch {
    if evt.Type == client.EventError {
        if d, ok := evt.AsErrorData(); ok {
            switch d.Code {
            case client.ErrCodeSessionBusy:
                // Session 忙，可延迟重试
            case client.ErrCodeUnauthorized:
                // Token 过期，需重新认证
            default:
                log.Error("gateway error", "code", d.Code, "msg", d.Message)
            }
        }
    }
}
```

## Bot ID（多 Bot 设置）

在多 Bot 环境中，使用 `BotID` 选项指定目标 Bot：

```go
c, err := client.New(ctx,
    client.URL("ws://localhost:8888"),
    client.WorkerType("claude_code"),
    client.APIKey("ak-xxx"),
    client.BotID("bot-123"),   // 指定 Bot ID
)
```

## 完整示例

### 权限处理

```go
c, _ := client.New(ctx,
    client.URL("ws://localhost:8888"),
    client.WorkerType("claude_code"),
    client.APIKey("test-api-key"),
    client.AutoReconnect(true),
)
defer c.Close()

ack, _ := c.Connect(ctx)
fmt.Printf("Session: %s\n", ack.SessionID)

done := make(chan struct{})
go func() {
    defer close(done)
    for evt := range c.Events() {
        switch evt.Type {
        case client.EventMessageDelta:
            if d, ok := evt.AsMessageDeltaData(); ok {
                fmt.Print(d.Content)
            }
        case client.EventPermissionRequest:
            if d, ok := evt.AsPermissionRequestData(); ok {
                // 按工具名策略审批
                approved := d.ToolName == "Read" || d.ToolName == "Glob"
                c.SendPermissionResponse(ctx, d.ID, approved, "")
            }
        case client.EventDone:
            return
        case client.EventError:
            if d, ok := evt.AsErrorData(); ok {
                fmt.Fprintf(os.Stderr, "Error: %s\n", d.Message)
            }
            return
        }
    }
}()

c.SendInput(ctx, "Read the file go.mod and tell me the Go version")
<-done
```

### 多轮对话

```go
c, _ := client.New(ctx,
    client.URL("ws://localhost:8888"),
    client.WorkerType("claude_code"),
    client.APIKey("test-api-key"),
)
defer c.Close()

ack, _ := c.Connect(ctx)

ready := make(chan struct{}, 1)
ready <- struct{}{}

go func() {
    for evt := range c.Events() {
        switch evt.Type {
        case client.EventMessageDelta:
            if d, ok := evt.AsMessageDeltaData(); ok {
                fmt.Print(d.Content)
            }
        case client.EventState:
            if d, ok := evt.AsStateData(); ok && d.State == client.StateIdle {
                select { case ready <- struct{}{}: default: }
            }
        case client.EventDone, client.EventError:
            return
        }
    }
}()

scanner := bufio.NewScanner(os.Stdin)
for {
    <-ready
    fmt.Print("> ")
    if !scanner.Scan() { break }
    c.SendInput(ctx, scanner.Text())
}
```
