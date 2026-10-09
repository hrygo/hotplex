---
title: Session 管理
weight: 17
description: Session 生命周期、5 状态机、资源配额与 GC 回收的深度指南
---

# Session 管理

> 面向开发者的 Session 生命周期、状态机、资源管理深度指南

## 概述

Session 是 HotPlex Gateway 的核心抽象。每个 Session 代表一次用户与 AI Worker 之间的持续对话，由 5 状态机管理生命周期，SQLite 持久化，后台 GC 自动回收资源。

Session ID 使用 UUIDv5 确定性生成（`session.DeriveSessionKey`），确保相同输入参数始终映射到同一 Session。

## 生命周期时钟

v2 将运行资源与会话历史分开计时，默认值如下：

| 内容 | 默认期限 | 起算点与行为 |
|------|----------|-------------|
| Worker 空闲回收 | 60 分钟 | 无 Worker I/O 时释放运行资源；会话记录和历史不因此删除 |
| 单轮运行上限 | 30 分钟 | 从本轮输入开始计算，流式输出不会重置绝对 deadline |
| 会话归档标记 | 7 天 | 最近一次有效输入后标记归档；仍可查看和续聊 |
| 会话保留期限 | 180 天 | 从最近一次有效输入起算，新输入可延长会话期限 |
| 单条聊天正文 | 180 天 | 每条事件/轮次按自身创建时间计算；后续输入不会延长旧正文 |

查看历史、分页或修改标题不续期。`session.retention_period` 是兼容的运行到期配置，不等于会话历史期限。当前代码会按逐条期限过滤过期正文；用户主动删除后的异步清理进度可通过 `GET /api/sessions/{id}/cleanup` 查询。会话期限到期后的自动退役和物理清理仍在实施中。

### 删除后的清理进度

`DELETE /api/sessions/{id}` 会立即隐藏会话、阻止续聊并排入后台清理；接口返回 `204` 表示删除已受理，不代表所有副本已清除。所有者可以查询 `GET /api/sessions/{id}/cleanup`，直到整体 `status` 变为 `complete`。响应仅包含各清理部分的状态、重试次数和安全错误代码，不返回聊天正文或提供方原始错误。

## 5 状态机

```
CREATED → RUNNING ⟷ IDLE → TERMINATED → DELETED
   ↑                    ↓            ↑
   └─── RESUME ←────────┘    │
          └──────────────────────┘
```

| 状态 | IsActive | 含义 | 典型停留时间 |
|------|----------|------|-------------|
| `CREATED` | true | 已创建，未启动 Worker | 瞬态（<1s） |
| `RUNNING` | true | Worker 正在执行 | 整个 Turn 执行期间 |
| `IDLE` | true | 等待用户输入 | `idle_timeout` 到期前 |
| `TERMINATED` | false | Worker 已终止，历史期限独立计算 | Legacy `term_retention` 或 v2 会话期限 |
| `DELETED` | false | 终态，记录已删除 | 永久 |

### 合法状态转换

| From → To | 触发条件 |
|-----------|---------|
| `CREATED → RUNNING` | `init` 握手完成，Worker 启动成功 |
| `RUNNING → IDLE` | Worker 执行完毕，Turn 结束（`done` 事件） |
| `IDLE → RUNNING` | 收到新 `input`（`TransitionWithInput` 原子操作） |
| `IDLE → TERMINATED` | `idle_timeout` / GC 回收 |
| `RUNNING → TERMINATED` | `/gc`、Worker 崩溃、`max_turns` 限制 |
| `RUNNING → DELETED` | Admin API 强制删除 |
| `TERMINATED → RUNNING` | Resume（重启 Worker 进程，`--resume` 恢复对话） |
| `TERMINATED → DELETED` | Legacy 终止记录 GC / Admin API 删除 |
| `IDLE → DELETED` | Admin API 强制删除 |

## /gc 与 /reset：何时使用

### /gc（归档会话）

- **行为**：终止 Worker 进程，Session 进入 `TERMINATED`，**保留完整对话历史**
- **适用场景**：暂时离开对话、释放系统资源、下班前归档
- **Resume 行为**：下次发送消息时，Gateway 通过 `--resume` 恢复完整上下文，Worker 自动重建对话状态
- **资源影响**：Worker 进程被终止（释放 ~512MB 内存），Session 记录保留在 SQLite

### /reset（重置会话）

- **行为**：清空 `SessionInfo.Context`，Worker 自行决定 in-place 清空或 terminate+restart
- **适用场景**：对话方向完全错误、需要全新开始、上下文已严重污染
- **Resume 行为**：不保留任何历史，等同于全新对话
- **资源影响**：可能复用已有 Worker 进程（in-place reset），无需重新 fork

### 选择建议

```
需要保留上下文？ → /gc
需要全新开始？   → /reset
```

## Resume 行为详解

当 Session 处于 `TERMINATED` 状态时，发送新 `input` 会自动触发 Resume 流程：

1. Gateway 检测到 `TERMINATED` 状态 + 有新 input
2. 通过 `TERMINATED → RUNNING` 合法转换
3. 重新 fork Worker 进程，传入 `--session-id` + `--resume` 参数
4. Worker 从磁盘恢复对话历史（Claude Code 使用 `~/.claude/projects/<hash>/sessions/` 目录）
5. 将用户 input 投递到恢复的 Worker

**Fast Reconnect 优化**：如果 WebSocket 断线重连时 Worker 进程仍然存活，直接复用，跳过 terminate + resume 周期。

**Resume 失败降级**：如果 Resume 失败（如 session 文件已被清理），Gateway 自动降级为 Start（全新会话）。Resume 和 Start 使用独立的 30 秒超时 context，互不影响——Resume 失败不会消耗 Start 的超时预算。

## Session 高级控制（CC Worker）

Claude Code Worker 的 `buildCLIArgs()` 将 `SessionInfo` 字段映射为 CLI flags。以下 4 个字段在 v1.14.0 新增支持：

| 字段 | CLI Flag | 说明 |
|------|----------|------|
| `ContinueSession` | `--continue` | 恢复当前目录最新 session，不需要 session ID |
| `ForkSession` | `--fork-session` | Resume 时 fork 为新 session（保留原始 session） |
| `ResumeSessionAt` | `--resume-session-at <msg_id>` | 恢复到指定 assistant 消息，丢弃后续历史 |
| `ConfigEnv` | `--settings '{"env":{...}}'` | 注入环境变量到 Claude Code 的 managed settings |

### 约束

- `ForkSession` 和 `ResumeSessionAt` 仅在 resume 模式下生效（需要 `--resume`），新建 session 时忽略
- `ContinueSession` 与 `--resume`/`--session-id` 互斥，优先级最高
- `ForkSession` + `ResumeSessionAt` 可以同时使用：fork 并回滚到特定消息
- `ConfigEnv` 在所有模式下可用；格式错误的条目（缺少 `=`）被静默跳过

### ConfigEnv 双通道注入

`ConfigEnv` 同时通过两条路径生效：

1. **OS 环境变量**：`base/env.go` 的 `BuildEnv()` 将 `ConfigEnv` 注入到子进程的 `cmd.Env`
2. **CC managed settings**：`buildCLIArgs()` 将 `ConfigEnv` 序列化为 `--settings '{"env":{"KEY":"VALUE"}}'`

两条路径互补——OS 环境变量影响所有子进程，CC managed settings 能覆盖 Claude Code 内部管理的变量（如 `MAX_THINKING_TOKENS`）。

## 工作目录管理

使用 `/cd <path>` 切换工作目录：

1. **安全验证**：`ExpandAndAbs`（展开环境变量 + 绝对路径）+ `ValidateWorkDir`（路径安全检查）
2. **派生新 SessionKey**：基于新目录生成新的确定性 Session ID
3. **终止旧 Worker**：不删除原 Session 记录
4. **启动新 Session**：在新 key 下创建 Session，启动 Worker
5. **注入上下文**：自动将最后输入注入新 Session

工作目录持久化在 `SessionInfo.WorkDir` 字段，跨 Resume 保持一致。

## Turn 追踪

每个 "用户输入 → Worker 完成" 周期算一个 Turn：

- **开始**：收到 `input`，`TurnCount++`（在 `TransitionWithInput` 中原子递增）
- **结束**：收到 `done` 事件
- **限制**：`max_turns` 可配置（0 = 无限），超出时自动触发 anti-pollution restart

```go
// TransitionWithInput 内部
ms.TurnCount++
if maxTurns > 0 && ms.TurnCount > maxTurns {
    // 自动终止，防止无限循环
}
```

## 多 Session 与 Pool 管理

### PoolManager 配额控制

| 配额维度 | 配置项 | 说明 |
|---------|--------|------|
| 全局最大 Worker 数 | `pool.max_size` | 所有用户的 Worker 总上限（0 = 无限） |
| 单用户最大 Session 数 | `pool.max_idle_per_user` | 防止单用户占用过多资源（0 = 无限） |
| 单用户最大内存 | `pool.max_memory_per_user` | 每个 Worker 估算 512MB（RLIMIT_AS） |

### 配额错误

| 错误 | 触发条件 |
|------|---------|
| `ErrPoolExhausted` | 全局 Worker 数已达上限 |
| `ErrUserQuotaExceeded` | 该用户 Session 数已达上限 |
| `ErrMemoryExceeded` | 该用户总内存估算超限 |

### 并发安全

- **锁顺序**：`Manager.mu → managedSession.mu`（固定顺序，防止死锁）
- **原子操作**：状态转换和 input 处理在同一 mutex 内完成（`TransitionWithInput`）
- **CAS 语义**：`DetachWorkerIf` 使用 compare-and-swap 防止过期 goroutine 覆盖新 Worker

## Session 调试

### /context 命令

通过 `worker_command` 事件请求 Worker 报告当前 context 使用情况：

```
/context
```

返回 `context_usage` 事件，包含：
- `total_tokens`：当前总 token 数
- `max_tokens`：模型最大 token 数
- `percentage`：使用百分比
- `categories`：按类别细分的 token 用量

### Admin API

Gateway 暴露 Admin API（默认 `localhost:9999`），支持深度 Session 检查：

- **列出所有 Session**：`GET /admin/sessions`
- **查看 Session 详情**：`GET /admin/sessions/:id`
- **查看 Worker 健康状态**：`GET /admin/workers/health`
- **Pool 利用率**：`GET /admin/stats`

### 调试快照

`Manager.DebugSnapshot()` 安全获取 Session 调试信息（不暴露 mutex）：

```go
type DebugSessionSnapshot struct {
    TurnCount    int
    WorkerHealth worker.WorkerHealth
    HasWorker    bool
}
```

## GC 自动回收

后台 GC goroutine 按 `gc_scan_interval`（默认 60s）定期扫描：

| 检查项 | 条件 | 动作 |
|--------|------|------|
| Zombie 检测 | `RUNNING` Session 的 `LastIO()` 超过 `execution_timeout` | → TERMINATED |
| Max Lifetime | `expires_at ≤ now` | → TERMINATED |
| Idle Timeout | `idle_expires_at ≤ now` | → TERMINATED |

> **注意**：`max_lifetime`（即 `expires_at` 到期）作用于**所有状态**的 Session（包括 `RUNNING`、`IDLE`），不限于 `IDLE → TERMINATED` 转换。它是一个全局生命周期上限，确保 Session 不会无限存活。

**注意**：Legacy `TERMINATED` Session 会按 `term_retention` / `cron_term_retention` 自动清理，v2 会话不会被这两项提前删除。v2 的到期自动退役尚未接线；当前仍应把它们视为可能可恢复的 Session。用户主动删除时，Worker 侧记录由对应 adapter 的异步清理任务处理，进度可通过 `/api/sessions/{id}/cleanup` 查看。
