---
title: Admin API 参考
weight: 5
description: HotPlex Gateway 管理 API 完整参考：Session 管理、Cron 任务、系统配置与监控端点
---

# Admin API 完整参考

HotPlex Admin API 提供网关运维管理能力：会话管理、健康检查、监控指标、配置审计、日志查询和定时任务控制。默认监听 `localhost:9999`，独立于网关主端口（`8888`）。

> **交互式 API 控制台**：可直接在浏览器中浏览和测试所有端点 → [打开 API 控制台](api-console.html)
> （基于 OpenAPI 规范自动生成，与代码保持同步）

## 认证

所有 Admin 端点（`/admin/health` 和 `/admin/health/ready` 除外）均需 Bearer Token 认证。Token 通过以下两种方式传递：

```bash
# 方式一：Authorization header（推荐）
curl -H "Authorization: Bearer <token>" http://localhost:9999/admin/stats

# 方式二：Query string（适用于浏览器场景）
curl http://localhost:9999/admin/stats?access_token=<token>
```

Token 使用 `crypto/subtle.ConstantTimeCompare` 进行常量时间比较，防止时序攻击。

> **Cookie 认证兜底（WebChat 多租户部署）**：当请求未携带 Bearer Token 时，若已启用 WebChat 多租户（`CookieAuth` 与本地账号身份提供者就绪），中间件会转而解析 chat session cookie，校验为 **active 且角色为 `admin`** 的用户后放行，并授予完整 scope 集合——嵌入式 WebChat 中已登录的管理员可直接访问 Dashboard/Bots/Cron，无需另发 admin token。Bearer 仍优先；standalone/CLI 部署（未启用多租户）保持仅 Bearer 行为。该 cookie 通道的写方法同样适用下文的「CSRF 同源校验」；跨源 cookie 调用还需在 CORS `allowedOrigins` 显式列出 WebChat origin（`*` 通配会抑制 `Allow-Credentials`，浏览器将拒绝发送 session cookie）。

### Token 配置

```yaml
admin:
  enabled: true
  addr: "localhost:9999"
  tokens:                          # 简单 token，使用 default_scopes
    - "my-admin-token"
  token_scopes:                    # 细粒度 scope token
    "ops-token": ["session:read", "stats:read", "health:read"]
    "admin-token": ["session:read", "session:write", "session:delete", "stats:read", "health:read", "admin:write"]
  default_scopes: ["session:read", "session:write", "session:delete", "stats:read", "health:read", "admin:write"]
```

### Scope 权限矩阵

不同的 Scope 控制着对 Admin API 不同模块的访问级别。

| Scope Token | Health | Sessions | Stats | Config | Debug | Cron | 覆盖的核心端点 (Endpoints) |
|:---|:---:|:---:|:---:|:---:|:---:|:---:|:---|
| `health:read` | 🟢 Read | - | - | - | - | - | `/admin/health/workers` |
| `session:read` | - | 🟢 Read | - | - | - | - | `GET /admin/sessions`<br>`GET /admin/sessions/{id}/stats` |
| `session:write` | - | 🟠 Write | - | - | - | - | `POST /admin/sessions/{id}/terminate` |
| `session:delete` | - | 🔴 Delete | - | - | - | - | `DELETE /admin/sessions/{id}` |
| `stats:read` | - | - | 🟢 Read | - | - | - | `GET /admin/stats`<br>`GET /admin/metrics`<br>`GET /admin/sessions/pool` |
| `config:read` | - | - | - | 🟢 Read | - | - | `POST /admin/config/validate` |
| `config:write` | - | - | - | 🟠 Write | - | - | `POST /admin/config/rollback` |
| `runtime:read` | - | - | - | - | - | - | `GET /admin/executions/fences`<br>`GET /admin/effects`<br>`GET /admin/effects/{id}`<br>`GET /admin/sessions/{id}/runtime-plan`（需与 `session:read` 同时持有） |
| `runtime:write` | - | - | - | - | - | - | `POST /admin/executions/{id}/fence-action`<br>`POST /admin/effects/{id}/action` |
| `admin:read` | - | - | - | - | 🟢 Read | 🟢 Read | `GET /admin/logs`<br>`GET /admin/debug/...`<br>`GET /admin/bots`<br>`GET /admin/cron/jobs` |
| `admin:write` | - | - | - | - | - | 🟠 Write | `POST/PATCH/DELETE /admin/cron/jobs`<br>`POST /admin/cron/jobs/{id}/run` |

> 💡 **Scope 蕴含**：`admin:write` 蕴含 `admin:read`、`runtime:read`、`runtime:write`；`admin:read` 蕴含 `config:read`（#877）。

> 💡 **图例**：🟢 **Read** (只读查询) | 🟠 **Write** (状态变更/操作) | 🔴 **Delete** (物理删除)

## 安全中间件

按以下顺序执行：

1. **CORS** — `Access-Control-Allow-Origin: *`，允许 `GET/POST/DELETE/OPTIONS`
2. **Panic Recovery** — `defer recover()` 捕获 handler panic，返回 `500 Internal Server Error`
3. **Rate Limiting** — 令牌桶算法（默认 10 req/s，burst 20），超限返回 `429 Too Many Requests`
4. **IP Whitelist** — CIDR 匹配（默认 `127.0.0.0/8`, `10.0.0.0/8`），使用 `r.RemoteAddr` 防止 X-Forwarded-For 伪造
5. **Token Auth** — Bearer Token 提取 + scope 校验

Rate Limit 和 IP Whitelist 支持配置热重载，无需重启生效。

## 操作审计

所有 admin 写操作（`POST`/`PUT`/`PATCH`/`DELETE`）均进入 `user_activity` 防篡改审计表（issue #833），并在兼容期继续输出结构化 `slog` 事件（`admin_audit`，issue #788 A5）。`user_activity` 是权威审计载体；`admin_audit` 仅保留给既有日志看板迁移使用。

**覆盖范围**：

- **`/admin/*`（端口 9999，Bearer Token）**：在 `AdminAPI.Middleware` 的审计 defer 中统一记录——无论请求最终成功或失败，写方法都会产生一条审计记录；`(method, path)` 映射为稳定 action 枚举，未映射路由回退 `"<method> <path>"`，确保无写操作静默丢失。
- **`/api/admin/*` 与 `/api/workspaces/*`（端口 8888，Cookie）**：在各 handler 的成功/失败路径显式记录；CSRF 同源校验拦截的跨站写请求同样记审计（action `auth.denied`，详见下文「CSRF 同源校验」）。

**审计字段**：

| 字段 | 说明 |
|------|------|
| `actor_user_id` | 操作者标识：Cookie 通道为用户 `uid`；Bearer 通道为 `admin-token`；认证未解析或失败时为 `anonymous` |
| `action` | 稳定动作枚举（见下表），日志看板按此过滤，**不可重命名** |
| `target` | 请求路径（含资源 id） |
| `result` | `ok`（成功）/ `failed`（操作失败）/ `denied`（认证或授权拒绝） |

**action 枚举**：

| 资源域 | action |
|--------|--------|
| 网关 | `gateway.restart` |
| Bot | `bot.create` / `bot.update` / `bot.delete` |
| API Key | `apikey.create` / `apikey.update` / `apikey.delete` |
| Cron | `cron.create` / `cron.update` / `cron.delete` / `cron.trigger` |
| Session | `session.delete` / `session.terminate` / `session.patch` / `session.put` |
| Audit | `audit.identity_link.create` / `audit.identity_link.delete` |
| Config | `config.rollback` / `config.validate` |
| Runtime Fence（#877） | `runtime.fence.action`（middleware slog，decision 无关）<br>`runtime.fence.resolve` / `runtime.fence.abandon`（`user_activity` 行，含 reason/evidence_ref） |
| Runtime Effect | `runtime.effect.action`（middleware slog，decision 无关）<br>`runtime.effect.abandon` / `runtime.effect.mark_delivered` / `runtime.effect.requeue`（`user_activity` 行，含 reason/evidence_ref；永不记录消息正文） |
| 多租户成员/邀请 | `member.status.update` / `invitation.create` / `invitation.delete` |
| 认证拒绝 | `auth.denied` |

## 端点总览

### 健康检查

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/health` | 无需认证 | 综合健康状态（gateway + DB + workers） |
| GET | `/admin/health/workers` | `health:read` | Worker 粒度健康状态 |
| GET | `/admin/health/ready` | 无需认证 | 就绪探针（k8s readiness） |

**GET /admin/health** — 无需认证，适合负载均衡器探活。返回 `status`（healthy/degraded）、`checks`（gateway + database + workers）和 `version`。数据库不可用时降级为 `degraded`，`database.error` 附带错误信息。

**GET /admin/health/workers** — Worker 粒度健康状态，含 `workers[]`（healthy/type/pid）和 `checked_at`。任一 Worker 不健康时返回 `503`。

### 会话管理

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/sessions` | `session:read` | 列出会话（分页） |
| GET | `/admin/sessions/{id}` | `session:read` | 获取单个会话 |
| DELETE | `/admin/sessions/{id}` | `session:delete` | 物理删除会话 |
| POST | `/admin/sessions/{id}/terminate` | `session:write` | 终止会话（状态迁移） |
| GET | `/admin/sessions/{id}/stats` | `session:read` | 会话 Turn 统计 |

**GET /admin/sessions** — 支持 query 参数过滤：

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:9999/admin/sessions?limit=50&offset=0&platform=slack&user_id=U12345"
```

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `limit` | int | 100 | 每页数量（上限 1000） |
| `offset` | int | 0 | 偏移量 |
| `platform` | string | "" | 按平台过滤 |
| `user_id` | string | "" | 按用户过滤 |

**POST /admin/sessions/{id}/terminate** — 将会话状态迁移至 `terminated`（软终止，保留记录）。DELETE 则为物理删除。

> **注意**：会话创建不通过 Admin API，而是通过 Gateway API（`POST /api/sessions`）或 WebSocket `init` 握手完成。Admin API 仅提供只读查询和终止/删除操作。

### Runtime Operations（#877 / #946）

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/executions/fences` | `runtime:read` | 列出阻塞新输入的 fenced executions |
| POST | `/admin/executions/{id}/fence-action` | `runtime:write` | 应用 operator 决策（resolve/abandon），以 fence_version 为条件 |
| GET | `/admin/sessions/{id}/runtime-plan` | `runtime:read` + `session:read` | 会话的 desired-state plan（redacted）与 observed bootstrap 摘要 |
| GET | `/admin/effects` | `runtime:read` | 列出外部交付 effect 及其生命周期状态 |
| GET | `/admin/effects/{id}` | `runtime:read` | 单个 effect 及其逐次发送尝试历史 |
| POST | `/admin/effects/{id}/action` | `runtime:write` | 对不确定交付应用 operator 决策（abandon/mark_delivered/requeue），以 `expected_status` 为条件 |

**GET /admin/executions/fences** — 列出运行时结局不明（runtime unknown）并触发 fence 的 execution。fenced execution 会阻塞同 session 的新输入，直至 operator 决策。

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `session_id` | string | "" | 按 session 过滤 |
| `limit` | int | 100 | 每页数量（上限 500） |
| `offset` | int | 0 | 偏移量 |

返回 `{"fences": [...], "limit": N, "offset": N}`。列表项为刻意收窄的无密投影：`execution_id`、`session_id`、`delivery_status`、`runtime_status`、`fence_reason`、`fence_version`、`fence_created_at`、`updated_at` —— 不含 payload 指纹、错误细节或任何用户内容派生字段。

**POST /admin/executions/{id}/fence-action** — operator 决策入口。请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|:---:|------|
| `decision` | string | ✅ | `resolve`（清除 fence，runtime 保持 unknown）或 `abandon`（清除 fence，runtime 置 failed 并补发 `runtime.execution.failed` 事件） |
| `expected_fence_version` | int | ✅ | 取自 `fences list` 的 `fence_version`；条件更新不匹配时返回 `409 FENCE_CONFLICT` |
| `reason` | string | ✅ | operator 理由，1–512 字符；仅进入审计层，execution store 永不保存 |
| `evidence_ref` | string | - | 工单/run 引用指针，≤256 字符；仅指针，不允许内联内容 |

错误码：`400 BAD_REQUEST`（字段校验）/ `403 INSUFFICIENT_SCOPE` / `404 FENCE_NOT_FOUND` / `409 FENCE_CONFLICT` / `503 SERVICE_UNAVAILABLE`（store 未配置或超时）。收到 409 必须重新 inspect 当前 fence 状态后审慎重试 —— 并发 operator 或 inspect 与 action 之间的网关重启都会触发冲突，服务端不自动重试。`abandon` 成功后 best-effort 通知在线连接终态。

**GET /admin/sessions/{id}/runtime-plan** — 返回会话的 EffectiveRuntimePlan 诊断投影（#946 spec §6.6）：`plan`（redacted view：plan hash、worker_type、permission/sandbox 摘要、env key **名称**、source refs、warnings、blocked codes）与 `observed`（bootstrap 状态：`planned` / `unknown` / `declared` 及 permission ceiling）。投影由会话持久化事实 + 当前 config 按需计算，无 plan 表、无第二份持久化真相；blocked plan 返回 200 并携带 bounded 拦截原因与空 `plan_hash`（blocked 是有效诊断载荷，不是 HTTP 错误）。响应永不包含 prompt、完整命令、model、工具清单或任何值内容。

**外部交付 effect 端点** — 交付 effect 是一次「已向外部渠道投递」的独立事实，与 execution fence 是**两套不相交的 operator 入口**：fence 回答「这次运行还能否接收新输入」，effect 回答「这条消息到底送没送出去」。共用一个端点会让 fence 决策悄悄改写交付历史，或让交付决策解开一个被 fence 的运行。

**GET /admin/effects** — 分页列出交付 effect。参数：`status`（按交付状态过滤）、`limit`（默认 100，上限 500）、`offset`。返回 `{"effects": [...], "limit": N, "offset": N}`。列表项为刻意收窄的无内容投影：`effect_id`、`occurrence_id`、`session_id`、`execution_id`、`delivery_status`、`target_kind`、`target_ref`、`attempt`、`error_code`、`reason`、`provider_ref`、`evidence_ref`、`lease_version`、`created_at`、`updated_at` —— 不含消息正文、payload、凭证或任何由用户内容派生的字段。

**GET /admin/effects/{id}** — 返回 `{"effect": {...}, "attempts": [...]}`，attempts 按发送次序给出每次尝试实际建立了什么：`attempt`、`owner_instance_id`、`lease_version`、`started_at`、`finished_at`、`in_flight`、`outcome`、`rejection_class`、`provider_ref`、`error_code`、`reason`。**lease token 是写凭据，永不投影**；正文与 tool args 同样不返回。`in_flight: true` 表示该次尝试的租约仍有效，不表示消息已送达。

**POST /admin/effects/{id}/action** — 对不确定交付的人工决策入口。请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|:---:|------|
| `decision` | string | ✅ | `abandon`（放弃这笔交付，置 failed）/ `mark_delivered`（operator 依据外部证据确认已送达）/ `requeue`（允许再次发送，产生新的 attempt，**不产生新的 effect**） |
| `expected_status` | string | ✅ | 必须是 `unknown`：只有结局不明的交付才接受人工决策，其余状态一律 `400` |
| `reason` | string | ✅ | operator 理由，1–512 字符；进入审计层与 effect 的 operator 归因字段，绝不冒充 provider 说法 |
| `evidence_ref` | string | - | 工单/run 引用指针，≤256 字符；仅指针，不允许内联消息内容 |

actor 取自鉴权上下文（`ActorFromRequest`），**绝不来自请求体**。错误码：`400 BAD_REQUEST` / `403 INSUFFICIENT_SCOPE` / `404 EFFECT_NOT_FOUND` / `409 EFFECT_CONFLICT` / `503 SERVICE_UNAVAILABLE`（effect store 未配置或超时）。收到 409 必须重新 inspect 后再决定 —— 并发 operator 或后台恢复流程已经改动了这笔交付的状态，服务端不自动重试。

### 监控指标

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/stats` | `stats:read` | 网关聚合统计 |
| GET | `/admin/metrics` | `stats:read` | Prometheus 格式指标 |
| GET | `/admin/sessions/pool` | `stats:read` | Session pool 容量统计 |

**GET /admin/stats** — 返回 `gateway`（uptime/websocket_connections/sessions_active/sessions_total）、`workers`（按 worker_type 分组统计）和 `database`（sessions_count）。

**GET /admin/sessions/pool** — 返回 session pool 容量快照 `{"total":N,"max":N,"users":N}`：`total` 为当前活跃 session 数，`max` 为配置的 pool 上限（`pool.max_size`），`users` 为占用配额的去重用户数。供容量监控与配额告警使用。

### 配置管理

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| POST | `/admin/config/validate` | `config:read` | 校验配置片段 |
| POST | `/admin/config/rollback` | `config:write` | 回滚到历史版本 |

**POST /admin/config/validate** — 校验配置合法性（不应用），请求体最大 1MB：

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"gateway":{"addr":":8888"},"pool":{"max_size":50}}' \
  http://localhost:9999/admin/config/validate
```

支持校验：`gateway`（buffer sizes >= 0）、`db`（path 长度 <= 4096）、`worker`（timeouts）、`pool`（max_size 1-10000）。返回 `{ "valid", "errors[]", "warnings[]" }`。

**POST /admin/config/rollback** — 回滚到指定版本，请求体 `{"version": 3}`，返回 `{ "ok", "rolled_back", "history_index" }`。无 configWatcher 时返回 `503`。

### 日志与调试

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/logs` | `admin:read` | 最近日志（环形缓冲区） |
| GET | `/admin/debug/sessions/{id}` | `admin:read` | 会话调试快照 |

**GET /admin/logs** — 从 100 条环形缓冲区读取，`?limit=N`（最大 1000）。返回 `{ "logs[]", "total", "limit" }`。

**GET /admin/debug/sessions/{id}** — 会话详情 + 调试快照（`debug.available`、`has_worker`、`turn_count`、`last_seq_sent`、`worker_health`）。

### Cron 定时任务

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/cron/jobs` | `admin:read` | 列出所有任务 |
| GET | `/admin/cron/jobs/{id}` | `admin:read` | 获取单个任务 |
| POST | `/admin/cron/jobs` | `admin:write` | 创建任务 |
| PATCH | `/admin/cron/jobs/{id}` | `admin:write` | 更新任务 |
| DELETE | `/admin/cron/jobs/{id}` | `admin:write` | 删除任务 |
| POST | `/admin/cron/jobs/{id}/run` | `admin:write` | 手动触发执行 |
| GET | `/admin/cron/jobs/{id}/runs` | `admin:read` | 执行历史 |

**请求体约定**：

- **POST /admin/cron/jobs** — JSON body 含 `name`、`schedule`（cron:/every:/at: 前缀）、`message`、`bot_id`、`owner_id`、`enabled`。返回 `201 Created`。
- **PATCH /admin/cron/jobs/{id}** — 部分更新，JSON body。返回 `204 No Content`。
- **POST /admin/cron/jobs/{id}/run** — 手动触发（异步），返回 `202 Accepted`。
- **GET /admin/cron/jobs/{id}/runs** — 查询执行历史。

Cron 未启用时返回 `503 Service Unavailable`。

### Bot 管理

Bot 状态查询、配置管理和 Agent 配置文件操作端点。

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/bots` | `admin:read` | 列出所有活跃 bot |
| GET | `/admin/bots/{name}` | `admin:read` | 单个 bot 详情 |
| POST | `/admin/bots` | `admin:write` | 注册新 bot |
| PATCH | `/admin/bots/{name}` | `admin:write` | 更新 bot 配置（部分更新） |
| DELETE | `/admin/bots/{name}` | `admin:write` | 删除 bot 注册 |
| GET | `/admin/bots/config` | `admin:read` | 列出所有 bot 配置 |
| GET | `/admin/bots/{name}/config` | `admin:read` | 单个 bot 完整配置 |
| GET | `/admin/bots/{name}/config/{file}` | `admin:read` | 读取 Agent 配置文件（如 SOUL.md、AGENTS.md） |
| PUT | `/admin/bots/{name}/config/{file}` | `admin:write` | 写入 Agent 配置文件 |
| GET | `/admin/bots/platform/{platform}/config/{file}` | `admin:read` | 读取**平台级 channel 默认**配置文件（webchat 等，绕过 registry 寻址 `dir/{platform}/` 层） |
| PUT | `/admin/bots/platform/{platform}/config/{file}` | `admin:write` | 写入平台级 channel 默认配置文件 |
| GET | `/admin/bots/{name}/preview` | `admin:read` | 预览组装后的系统提示（B+C 通道完整输出） |

**GET /admin/bots** — 返回所有活跃 bot 列表，含 name、platform、worker_type、状态等信息。

**POST /admin/bots** — 注册新 bot，JSON body 含平台凭证和配置。返回 `201 Created`。

**PATCH /admin/bots/{name}** — 部分更新 bot 配置（凭证、worker_type 等）。返回 `204 No Content`。

**GET /admin/bots/{name}/preview** — 返回该 bot 组装后的完整系统提示，包含 B 通道（directives）和 C 通道（context）内容，便于调试 Agent 人格配置。

**PUT /admin/bots/{name}/config/{file}`** — 写入指定 Agent 配置文件（`SOUL.md`、`AGENTS.md`、`TOOLS.md`、`USER.md`、`MEMORY.md`）。请求体为 JSON `{"content":"..."}`。读写端点都只接受这五个规范文件名。

**GET/PUT /admin/bots/platform/{platform}/config/{file}** — 读写**平台级 channel 默认** Agent 配置文件。`{platform}` 为平台标识（如 `webchat`），绕过 messaging registry 直接寻址 `dir/{platform}/` 层，让无 bot 实例的平台（webchat team-default）获得与消息平台 bot 对等的配置编辑能力。规范文件白名单与 bot 级端点一致（`SOUL.md`/`AGENTS.md`/`TOOLS.md`/`USER.md`/`MEMORY.md`），复用 `/admin/*` 中间件鉴权（`admin:read`/`admin:write`）与审计。

Bot 列表与详情的 `agent_configs` 元数据只包含 `soul`、`agents`、`tools`、`user`、`memory`。

### 网关重启

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| POST | `/admin/restart` | `admin:write` | 触发网关重启 |

**POST /admin/restart** — 两阶段异步触发网关重启：先获取全局 restart lease，成功并确认响应已刷新后返回 `{ "status": "restarting" }`，再提交独立 helper 完成重启。已有事务时返回 `409 RESTART_REJECTED`；实例发现、receipt 校验或文件系统等内部准备失败返回 `500 INTERNAL`；未配置 restart handler 时返回 `503`。Admin、CLI 与飞书入口共享同一 lease，不能并发绕过。

### 用户行为审计

`/admin/activity` 系列端点查询 issue #833 的 `user_activity` 表。所有读端点需要 `admin:read`；导出默认脱敏，`include_pii=true` 需要 `admin:write`。每次导出都会写入 `system.audit_export` meta-audit。

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/activity` | `admin:read` | 查询全局活动时间线 |
| GET | `/admin/activity/export` | `admin:read` | 导出全局活动时间线（`format=json|csv`） |
| GET | `/admin/users/{id}/activity` | `admin:read` | 查询单个原生 user_id 的活动 |
| GET | `/admin/users/{id}/activity/export` | `admin:read` | 导出单个原生 user_id 的活动 |
| GET | `/admin/audit/identity-links` | `admin:read` | 列出跨通道身份链接 |
| POST | `/admin/audit/identity-links` | `admin:write` | 创建或更新身份链接 |
| DELETE | `/admin/audit/identity-links/{id}` | `admin:write` | 删除身份链接 |

查询参数：

| 参数 | 说明 |
|------|------|
| `user_id` | 按审计行原始 `user_id` 查询 |
| `principal_user_id` | 按本地 canonical 用户查询；会展开 `audit_identity_links` 中的所有平台原生 subject |
| `action` | 精确匹配 action，如 `tool.call` / `admin.bot.create` |
| `outcome` | `success` / `failure` / `denied` |
| `from` / `to` | RFC3339 时间范围 |
| `limit` / `offset` | 分页 |
| `format` | 导出格式：`json` 或 `csv` |

**POST /admin/audit/identity-links** — JSON body：

```json
{
  "principal_user_id": "local-user-uuid",
  "provider": "feishu",
  "subject": "ou_xxx",
  "subject_type": "platform",
  "display_name": "Alice",
  "email": "alice@example.com"
}
```

`principal_user_id`、`provider`、`subject` 必填；`subject_type` 默认为 `platform`，允许 `registered` / `platform` / `system` / `anonymous`。`UNIQUE(provider, subject)` 保证一个平台原生 ID 只归属一个 principal。写操作通过 middleware 审计为 `admin.audit.identity_link.create` / `admin.audit.identity_link.delete`。

### API Key 用户管理

管理 API Key 到用户身份的映射，用于企业级多用户 Session 隔离。每个 `user_id` 与 API Key 为 **1:1 映射**（一个 user_id 仅能关联一个 API Key），创建或更新时若 user_id 已被其他映射占用则返回 `409 Conflict`。需要数据库支持（SQLite 或 PostgreSQL），未配置 DB resolver 时返回 `501 Not Implemented`。

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/api-keys` | `admin:read` | 列出所有 API Key 映射 |
| POST | `/admin/api-keys` | `admin:write` | 创建 API Key 映射 |
| GET | `/admin/api-keys/{id}` | `admin:read` | 获取单个映射详情 |
| PATCH | `/admin/api-keys/{id}` | `admin:write` | 更新映射 |
| DELETE | `/admin/api-keys/{id}` | `admin:write` | 删除映射 |

**POST /admin/api-keys** — 创建 API Key → UserID 映射。JSON body 含 `user_id`（必填，最长 128 字符）和 `description`（可选，最长 512 字符）。API Key 由系统自动生成（24 字节随机 hex）。返回 `201 Created`；若 `user_id` 已存在则返回 `409 Conflict`。

**GET /admin/api-keys** — 返回所有映射列表，`api_key` 字段自动脱敏（仅显示前 8 + 后 4 位）。

**PATCH /admin/api-keys/{id}** — 更新指定映射的 `user_id` 和 `description`。若新 `user_id` 与其他映射冲突则返回 `409 Conflict`。

**DELETE /admin/api-keys/{id}** — 物理删除映射，同时清除缓存的 resolver 条目。

### Workspace 管理（admin 控制台）

> 🆕 **issue #807** — admin 控制台的 workspace 全局视图。区别于用户自助的 `/api/workspaces/*`（见下文 Gateway API 段）：这两个端点走 admin 双通道鉴权（Bearer + Cookie fallback，强制 `role==admin && status==active`），列出全平台 workspace 并支持 inline 修改 `permission_mode`。

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/workspaces` | `admin:read` | 列出所有 workspace（带 owner 可读标识） |
| PATCH | `/admin/workspaces/{id}` | `admin:write` | 修改单个 workspace 的 `permission_mode` |

**GET /admin/workspaces** — 返回所有 active workspace，`LEFT JOIN users` 带上 owner 可读标识（`owner_display_name` + `owner_username`），admin 无需面对裸 UUID。owner 行缺失时（并发删除窗口）owner 字段回落为空串、workspace 仍返回。响应：`{"workspaces":[{id, owner_user_id, owner_display_name, owner_username, name, work_dir, permission_mode, status, created_at, updated_at, agent_config_overrides, worker_preference}]}`。读操作不审计。

**PATCH /admin/workspaces/{id}** — **仅**接受 `permission_mode` 字段（admin 控制台范围最小化：不暴露 `work_dir`/`name` 修改，避免 admin 误改用户工作目录；这些仍走用户自助 `/api/workspaces/{id}`）。JSON body：`{"permission_mode":"<tier>"}`，`permission_mode` 必须显式提供（缺失返回 `400 BAD_REQUEST`）；值为 4 档之一（`bypass`/`auto-edit`/`workspace`/`read-only`）或空串（清除覆盖，回落 `config.worker.default_permission_mode`，缺省 `workspace`）。返回更新后的 workspace 裸行。

> ⏱️ **生效语义**：写库后**运行中 session 不受影响**（worker 进程已注入权限参数）；新 session（新对话 / `/reset`）初始化时 `resolveWorkspacePermissionMode` 读到新值。UI 标注「对新会话生效」。

> 🛡️ **审计**：PATCH 写操作由 middleware 级 `admin_audit` 统一记录，action = `workspace.permission_mode.update`，actor = uid（Cookie 通道）或 `admin-token`（Bearer），target = `/admin/workspaces/{id}`。

PATCH 错误码：`400 INVALID_PERMISSION_MODE`（档位非法）/ `400 BAD_REQUEST`（body 缺 `permission_mode`）/ `404 WORKSPACE_NOT_FOUND`（id 不存在）/ `409 WORKSPACE_VERSION_MISMATCH`（`updated_at` CAS 冲突；或并发更新下 workspace 恰有活跃会话时的 CAS heuristic 误判——本端点只改 `permission_mode`、不动 `work_dir`，活跃会话不真正阻塞，故两种成因都应重新 GET 最新 `updated_at` 后重试，新值仅对新会话生效）。

### Skill 管理（admin 全局）

Admin 与 WebChat 的 public HTTP Skills catalog 管理和展示真实 Agent Skills；AgentConfig 的
`TOOLS.md` 不属于此 catalog。embedded `hotplex-cli`（runtime）与
`hotplex-operator`（operator）是这些 HTTP read surface 永久可发现的 builtin，发现不依赖 UserHome
projection、`$HOTPLEX_HOME` inventory 或 receipt；Session `/skills` 仍按当前 Worker/filesystem
evidence 决定是否出现，projection/native advertisement 才影响 callable，discoverable 不等于
callable。ACP 没有可推断的 filesystem root。区别于用户自助
的 `/api/workspaces/{wid}/skills/*`（见下文 WebChat 多租户端点段）：admin 端管理真实 global
skill，用户端管理各自 project/workspace skill。

两个 builtin 的 canonical source 是 `internal/skills/builtin/hotplex-cli` 与
`internal/skills/builtin/hotplex-operator`，生成的 `.agents/skills/hotplex-cli` 和
`.agents/skills/hotplex-operator` 是 byte-identical mirror。连同 `hotplex-diagnostics`、
`hotplex-release`、`hotplex-docs-patrol`、`hotplex-stt-tts`，仓库 portfolio 恰好包含六个 Skill。

| 方法 | 路径 | Scope | 说明 |
|------|------|-------|------|
| GET | `/admin/api/skills` | `admin:read` | 列出全局 skill（`.agents`/`.claude`/`.hotplex` 合并，带 `managed` 标注） |
| POST | `/admin/api/skills` | `admin:write` | zip 上传安装全局 skill（`?replace=true` 覆盖同名） |
| GET | `/admin/api/skills/{name}` | `admin:read` | 全局 skill 详情（含 `SKILL.md` 全文 `body` + 包内文件列表 `files`） |
| PUT | `/admin/api/skills/{name}` | `admin:write` | 在线更新 `SKILL.md` 全文（JSON `{"body":"..."}`，区别于 POST 的 zip 上传） |
| DELETE | `/admin/api/skills/{name}` | `admin:write` | 删除全局 skill |

**GET /admin/api/skills** — 合并扫描真实 global/external 项与 embedded builtins；真实项优先遮蔽
同名 builtin，builtin 项 `managed:false`、`builtin:true`，并带可选
`builtin_package_version`。`source` 仍只为 `global` 或 `project`，builtin 身份只由独立的
`builtin` 字段表达。
响应：`{"skills":[{name,description,source,managed,builtin,builtin_package_version}],"total":N}`。
GET 不审计。builtin 的 detail body/files 来自 embedded manifest，不暴露 host path，也不要求
native projection 存在。

**POST /admin/api/skills** — multipart 上传，`file` 字段为 `.zip` 包（body 上限 20MB）。zip 根下须直接有 `SKILL.md`，或单一顶层目录内含 `SKILL.md`（目录名必须等于 frontmatter `name`）。frontmatter 必填 `name`（正则 `^[a-z0-9]+(-[a-z0-9]+)*$`，1-64 字符）与 `description`（1-1024 字符）。文件类型白名单：`.md`/`.json`/`.yaml`/`.yml`/`.txt`/`.py`/`.sh`/`.toml`/图片（`.png`/`.jpg`/`.jpeg`/`.svg`）；拒可执行/二进制。`?replace=true` 覆盖同名，否则同名返回 `409`。安装成功返回 `skills.InstallResult`（含 `name`/`description`/`source:"global"`/`managed:true`/`body`/`files`，以及可能的 `warning`）。

**PUT /admin/api/skills/{name}** — 在线更新已存在真实 skill 的 `SKILL.md` 全文。请求体为 JSON `{"body":"<完整 SKILL.md 文本>"}`（非 multipart、非 zip），仅改写 `SKILL.md`、不动包内其他文件。frontmatter 须合规（`name`/`description` 缺失或 `name` 不匹配正则返回 `400 SKILL_INVALID_FORMAT`），`name` 须为合法 skill 名。若选中的对象是 builtin 且没有同名真实项，返回 `409 SKILL_BUILTIN_READONLY`；同名真实 user/project/global 项优先并照常允许 update。成功返回更新后的 skill detail（同 GET 详情结构）。

> 🛡️ **安全**：zip-slip（`SafePathJoin` + 前缀双保险）、解压炸弹（zip ≤20MB / 解压总 ≤50MB / 单文件 ≤5MB / entry ≤500 / 压缩率 >100× 拒）、恶意 entry（`IsRegular()` 过滤 symlink/device）多维防护（spec §3.3 A）。

> 🛡️ **审计**：POST/PUT/DELETE 写操作由 middleware 级 `admin_audit` 统一记录，action = `skill.create`（POST，`?replace=true` 时映射为 `skill.update`）/ `skill.update`（PUT）/ `skill.delete`（DELETE），actor = uid（Cookie 通道）或 `admin-token`（Bearer）。

**DELETE /admin/api/skills/{name}`** — 删除真实 global skill。builtin-only 对象返回
`409 SKILL_BUILTIN_READONLY`；同名真实项优先，按正常权限删除。创建/安装同名 builtin
override 允许，仍通过 `source:global` 或 `source:project` 表示真实来源。

错误码：`400 SKILL_INVALID_ZIP`（损坏/超限/含恶意 entry）/ `400 SKILL_INVALID_FORMAT`（无 SKILL.md / frontmatter 缺失 / name 不合规 / name≠目录名 / description 超长）/ `400 SKILL_FILE_TYPE_BLOCKED`（类型不在白名单）/ `409 SKILL_ALREADY_EXISTS`（同名且未带 `?replace=true`）/ `409 SKILL_BUILTIN_READONLY`（builtin-only update/delete）/ `404 SKILL_NOT_FOUND`（name 不存在）。

## Gateway API 端点

Gateway API（`/api/sessions`）监听在网关主端口（`8888`），面向客户端 SDK 和 WebSocket 连接，使用 API Key 认证（非 Bearer Token）。

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| GET | `/api/sessions` | API Key | 列出当前用户的会话 |
| POST | `/api/sessions` | API Key | 创建会话 |
| GET | `/api/sessions/{id}` | API Key | 获取单个会话 |
| DELETE | `/api/sessions/{id}` | API Key | 删除会话 |
| POST | `/api/sessions/{id}/cd` | API Key | 切换工作目录 |
| GET | `/api/sessions/{id}/history` | API Key | 获取会话历史 |
| GET | `/api/sessions/{id}/events` | API Key | 获取会话事件流 |
| GET | `/api/workers` | API Key | 列出 Worker 类型及二进制安装状态 |

所有 Gateway API 端点启用 CORS（`Access-Control-Allow-Origin: *`），支持 `GET`、`POST`、`DELETE`、`OPTIONS` 方法。

**GET /api/workers** — 返回所有已注册 Worker 类型（`claude_code` / `opencode_server` / `codex_cli` / `acp`）的二进制安装状态，供客户端（如 WebChat「新建会话」弹窗）动态过滤可选引擎，避免误选未安装的 Worker。命令名取自配置（`worker.<type>.command`），缺失时回落到各类型的默认二进制（`claude` / `opencode` / `codex` / `hermes`），再经 `exec.LookPath` 探测。响应体为 JSON 数组：`[{"type":"<worker_type>","installed":<bool>,"path":"<abs_path, omitempty>"}]`。

### WebChat 多租户端点（spec ① / ④ / ⑥）

WebChat 多租户登录与工作区管理端点，同样监听在网关主端口 `8888`，默认使用 **Cookie 认证**（登录后签发的 HMAC Cookie）；其中 workspace CRUD 还接受 **API Key**（跨通道租户接入，详见下方「Workspace 管理」）。仅在启用 WebChat 多租户（`CookieAuth` + `WorkspaceStore` 就绪）时注册，普通 SDK/WebSocket 集成不涉及。

**账号登录（spec ①）**：

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| POST | `/api/auth/login` | 无 | 内建账号登录（username/password），成功签发 Cookie |
| POST | `/api/auth/logout` | Cookie | 登出并清除 Cookie |
| GET | `/api/auth/me` | Cookie | 当前登录用户信息 |
| POST | `/api/auth/accept-invite` | 无（邀请码） | 凭邀请码（body `code`）注册本地账号并**自动登录**（签发 cookie，返回 `first_login:true`），加入 workspace |
| GET | `/api/auth/bootstrap-status` | 无 | 首次部署引导状态（是否已存在管理员），登录页据此决定是否引导建号 |

**企业 SSO（spec ④，OIDC + PKCE）**：

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| GET | `/api/auth/oauth/providers` | 无 | 列出已配置的 SSO provider（登录页据此渲染按钮） |
| GET | `/api/auth/oauth/{provider}/login` | 无 | 发起 OIDC 登录，重定向到 IdP（name 须匹配 `[a-z0-9-]+`） |
| GET | `/api/auth/oauth/{provider}/callback` | state cookie | OIDC 回调，完成 token exchange + ID Token 验证后签发 Cookie |

`GET /api/auth/oauth/providers` 始终返回稳定 envelope：

```json
{
  "providers": [
    { "name": "keycloak", "display_name": "企业 SSO" }
  ]
}
```

**Workspace 管理（spec ⑥ A1 / ⑦ 跨通道租户）**：

Workspace CRUD 是**跨通道租户锚**：同一个 `users.id` 无论经 **API Key**（header `X-API-Key` 或 query `api_key`，面向第三方集成）还是 **Cookie**（WebChat UI）进入，都能拥有并管理自己的 workspace。鉴权链为 API Key 优先、Cookie 兜底（`AuthenticateRequest`），与 session REST、WS upgrade 对齐。`work_dir` workspace 级可改：PATCH 携带 `work_dir` 时校验 owner 沙箱（`403 WORK_DIR_OUTSIDE_SANDBOX`）且 workspace 必须无活跃会话（`409 WORKSPACE_NOT_EMPTY`，因 work_dir 进 session key 派生）。session 级 `switch-workdir` 对 workspace-bound session 返回 `400 WORK_DIR_IMMUTABLE`（worker 须跟随 workspace）。详见 WebChat-Workspace-Create-WorkDir-Prefix-Spec §5.1.4。

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| POST | `/api/workspaces` | API Key / Cookie | 创建 workspace |
| GET | `/api/workspaces` | API Key / Cookie | 列出当前用户可访问的 workspace |
| GET | `/api/workspaces/{id}` | API Key / Cookie | workspace 详情（含归属校验） |
| PATCH | `/api/workspaces/{id}` | API Key / Cookie | 更新 workspace（乐观并发，见下） |
| DELETE | `/api/workspaces/{id}` | API Key / Cookie | 删除 workspace |

> 🔒 **`permission_mode` 为 admin-only 字段（r3 #804）**：POST `/api/workspaces` 与 PATCH `/api/workspaces/{id}` 仅 admin 可设置/修改 `permission_mode`。非 admin 请求体携带该字段（含空串清空）一律返回 `403 PERMISSION_DENIED`（`message`: `permission_mode can only be configured by admins`）；非 admin 仍可改 `name` / `work_dir` / `agent_config_overrides` / `worker_preference`。未显式配置的 workspace 由 bridge 注入 `config.worker.default_permission_mode`（缺省 `workspace`）。

> **PATCH 乐观并发（CAS）**：更新基于 `updated_at` 做乐观锁——服务端 `UPDATE ... WHERE id = ? AND updated_at = ?`，调用方缓存的 `updated_at` 不再匹配（被并发修改）时影响 0 行，返回 `409 WORKSPACE_VERSION_MISMATCH`（"workspace concurrently modified, please re-fetch and retry"）。客户端无需在 body 显式传版本字段（先 GET 取最新值再 PATCH），收到 409 后应重新 GET 再重试，避免静默 lost update。

> **PATCH body 字段语义**：`name`、`agent_config_overrides` 传空字符串表示**不更新**（无法通过 PATCH 清空这两项）；`worker_preference` 为指针类型以区分三态——**省略**（字段未传）不更新、**空字符串** `""` 显式清除回默认 worker、**非空**设为指定类型（校验失败返回 `400 INVALID_WORKER_TYPE`）；`work_dir` 为 workspace 级可变字段——**省略/空字符串**不更新，**非空**时校验 owner 沙箱（`403 WORK_DIR_OUTSIDE_SANDBOX`）、值变更须 workspace 无活跃会话（`409 WORKSPACE_NOT_EMPTY`，因 work_dir 进 session key 派生）、且未被该 owner 其他 workspace 占用（`409 WORK_DIR_TAKEN`）后更新（见上）。

**Workspace Skill 管理（spec #910）**：

每个 workspace 可管理自己的 skill（安装到 `<work_dir>/.agents/skills`）。鉴权与 workspace CRUD 一致（API Key 优先 / Cookie 兜底），写操作额外校验 owner 归属（`ws.OwnerUserID != uid && !isAdmin` → `403 WORKSPACE_FORBIDDEN`）。另提供脱离 workspace 的合并查询：`/api/skills` 返回 `全局 + 当前用户所有 workspace + 外部只读 + embedded builtin` 的合并列表（每用户视图）；同名真实项优先遮蔽 builtin。

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| GET | `/api/skills` | API Key / Cookie | 合并列表：全局 + 我的 workspace + 外部只读 + builtin（带 `managed`/`builtin`/版本标注） |
| GET | `/api/skills/{name}` | API Key / Cookie | 合并列表中该 skill 的 metadata（真实项优先；不返回 builtin body/files） |
| POST | `/api/workspaces/{wid}/skills` | owner | zip 上传安装 workspace skill（`?replace=true` 覆盖） |
| GET | `/api/workspaces/{wid}/skills/{name}` | owner | workspace scope skill 详情（含 `body` + `files`） |
| DELETE | `/api/workspaces/{wid}/skills/{name}` | owner | 删除 workspace skill |

> ⚠️ **同名遮蔽（spec §3.3 B6）**：workspace 安装与真实 global managed skill 同名时**允许安装但返回 `warning`**（`shadows global skill '<name>'`）——workspace skill 在合并列表中覆盖 global 生效，UI 须显式提示。仅与 builtin 同名时允许创建 override，但当前不会产生该 global-shadow warning；builtin 不注入 workspace-only 管理列表，真实 workspace override 可照常 update/delete。

> 🛡️ **审计**：workspace skill 写操作（POST/DELETE）由 handler 显式写入 tamper-evident `user_activity`（`action` = `skill.install` / `skill.delete`，`resource_type` = `skill`，`platform` = `webchat`），与 `/api/admin/*` 写操作一致；读操作不审计。

zip 格式、文件类型白名单与安全约束同上方「Skill 管理（admin 全局）」；错误码复用 `SKILL_INVALID_ZIP` / `SKILL_INVALID_FORMAT` / `SKILL_FILE_TYPE_BLOCKED` / `SKILL_ALREADY_EXISTS` / `SKILL_NOT_FOUND`，另增 `403 WORKSPACE_FORBIDDEN`（非 owner）。

**Workspace 用户与邀请管理（spec ⑥，admin 维度）**：

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| GET | `/api/admin/users` | Cookie | 列出 workspace 用户 |
| PATCH | `/api/admin/users/{id}` | Cookie | 启用/禁用用户（disable 后 per-request 即时拦截） |
| POST | `/api/admin/users/{id}/password` | Cookie | 重置用户密码 |
| POST | `/api/admin/invitations` | Cookie | 创建邀请码 |
| GET | `/api/admin/invitations` | Cookie | 列出邀请 |
| DELETE | `/api/admin/invitations/{id}` | Cookie | 删除邀请 |

> **GET /api/admin/invitations** — 每条邀请额外返回服务器计算的 `is_expired`（`true` 表示邀请码**未被使用且已过期**），客户端无需自行比对 `expires_at` 与本地时钟，规避时钟漂移。

> **POST /api/admin/users/{id}/password** — 请求体 JSON `{"password":"..."}`，最小长度 6 位（不足返回 `400 BAD_REQUEST`）。经 identity provider 哈希（`idp.HashPassword`）后写入 `users` 表，不回传哈希。需 CSRF 同源校验（同其它 `/api/admin/*` 写方法），成功返回 `200 OK`（无 body），写操作记入 admin 审计（action = `user.password_reset`）。

> ⚠️ **注意区分**：此处的 `/api/admin/*`（端口 `8888`，Cookie 认证，WebChat workspace 维度）与本页上方「认证」章节描述的 Admin API（端口 `9999`，Bearer Token，网关运维维度）是**两套独立端点**，认证模型和作用域完全不同，不要混淆。

> 🔒 **CSRF 同源校验（写方法）**：`/api/admin/*` 与 `/api/workspaces/*` 的状态变更方法（`POST`/`PATCH`/`DELETE`）挂载同源验证中间件，防御针对 SameSite=None session cookie 的跨站写请求；`GET` 一律放行。写方法需提供「同源证明」之一——浏览器 `Sec-Fetch-Site` 取值为 `same-origin`/`same-site`，或 `Origin` 精确命中 CORS `allowedOrigins`（`*` 通配**不**满足：SameSite=None cookie 恰在 allowlist 宽松时跨站送达）。未通过返回 `403 FORBIDDEN`（`message`: `cross-origin write blocked`）并记入 admin 审计日志。嵌入式 WebChat SPA 与网关同源，天然通过；跨域前端需将自身 origin 列入 `allowedOrigins`。

## 错误响应格式

所有错误统一返回 JSON envelope（`Content-Type: application/json`），形状为：

```json
{"error":{"code":"NOT_FOUND","message":"resource not found"}}
```

`code` 为机器可读的大写下划线标识符，`message` 为人类可读的英文描述。客户端应依据 `code` 做分支，而非解析 `message`。此信封由 `web.WriteAppError` 统一生成，Admin API（Bearer）与 `/api/admin/*`（Cookie）共用同一形状。

### 错误码表

| HTTP | code | 典型场景 |
|------|------|----------|
| 400 | `BAD_REQUEST` | JSON 解析失败、参数校验错误、非法枚举值 |
| 400 | `INVALID_CONFIG_JSON` | workspace `agent_config_overrides` 不是合法 JSON 对象 |
| 400 | `UNKNOWN_CONFIG_FILE` | workspace overrides 含未知配置文件键 |
| 400 | `CONFIG_TOO_LARGE` | workspace overrides 超过单文件大小上限 |
| 400 | `INVALID_CONFIG_VALUE` | workspace overrides 值类型/内容非法 |
| 400 | `INVALID_WORK_DIR` | workspace `work_dir` 路径无法展开或不存在 |
| 400 | `INVALID_WORKER_TYPE` | workspace `worker_preference` 非已注册 worker 类型 |
| 401 | `UNAUTHORIZED` | Token 缺失或无效 |
| 401 | `INVALID_CREDENTIALS` | Cookie 会话失效或未登录 |
| 403 | `INSUFFICIENT_SCOPE` | Bearer Token scope 不满足（Admin API） |
| 403 | `FORBIDDEN` | 非管理员访问管理端点，或请求 IP 不在白名单（见「安全中间件」IP Whitelist，两类 403 共用同一 code） |
| 403 | `USER_DISABLED` | 用户已被禁用 |
| 403 | `WORKSPACE_FORBIDDEN` | 非所有者访问他人 workspace（且非 admin） |
| 403 | `WORK_DIR_FORBIDDEN` | workspace `work_dir` 命中安全黑名单（系统目录等） |
| 403 | `WORK_DIR_OUTSIDE_SANDBOX` | workspace `work_dir` 不在 owner 沙箱前缀下 |
| 403 | `PERMISSION_DENIED` | 非 admin 试图在 workspace Create/Update 设置或修改 `permission_mode`（admin-only 字段，r3 #804） |
| 404 | `NOT_FOUND` | Session/Cron Job/Invitation 未找到 |
| 404 | `WORKSPACE_NOT_FOUND` | workspace id 不存在 |
| 404 | `FENCE_NOT_FOUND` | fence-action 目标 execution 不存在 |
| 404 | `EFFECT_NOT_FOUND` | effect 端点目标 effect 不存在 |
| 409 | `CONFLICT` | 资源状态冲突 |
| 409 | `RESTART_REJECTED` | 已有 Gateway 重启事务 |
| 409 | `FENCE_CONFLICT` | fence_version 条件更新失败；重新 inspect 后审慎重试，勿自动重试（#877） |
| 409 | `EFFECT_CONFLICT` | effect 条件更新失败（已不再是 unknown）；重新 inspect 后审慎重试，勿自动重试 |
| 409 | `WORKSPACE_VERSION_MISMATCH` | PATCH workspace 乐观并发冲突（`updated_at` CAS 失败，re-fetch 后重试） |
| 409 | `WORKSPACE_NOT_EMPTY` | workspace 存在活跃会话，拒绝改 `work_dir` / 删除 |
| 409 | `WORK_DIR_TAKEN` | workspace `work_dir` 已被该 owner 的其他 workspace 占用 |
| 429 | `RATE_LIMITED` | 触发 Rate Limit |
| 500 | `INTERNAL` | Handler panic、未分类内部错误 |
| 501 | `NOT_IMPLEMENTED` | 该接口尚未实现 |
| 503 | `SERVICE_UNAVAILABLE` | 数据库故障、Cron 未启用、execution store 未配置或超时 |
| 503 | `NO_IDP` | 未配置身份提供者 |

## 常用操作示例

```bash
# 快速健康检查（无需 Token）
curl http://localhost:9999/admin/health

# 查看活跃会话
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:9999/admin/sessions?limit=10

# 查看 Prometheus 指标
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:9999/admin/metrics

# 终止异常会话
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://localhost:9999/admin/sessions/abc-123/terminate

# 查看最近日志
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:9999/admin/logs?limit=20"

# 调试特定会话
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:9999/admin/debug/sessions/abc-123

# 触发 Cron 任务
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://localhost:9999/admin/cron/jobs/daily-health/run

# 列出 fenced executions（#877）
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:9999/admin/executions/fences

# 决策一条 fence（expected_fence_version 取自上面的列表；冲突时重新 inspect）
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"decision":"abandon","expected_fence_version":3,"reason":"worker host lost, verified via run log","evidence_ref":"OPS-1234"}' \
  http://localhost:9999/admin/executions/exec-abc/fence-action

# 查看会话的 effective runtime plan（#946）
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:9999/admin/sessions/abc-123/runtime-plan
```
