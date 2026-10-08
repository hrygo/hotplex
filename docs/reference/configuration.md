---
title: "配置完整参考"
weight: 3
description: "HotPlex Worker Gateway 所有配置项的权威参考，覆盖配置文件、环境变量、优先级和热重载机制。"
---

# 配置完整参考

本文档是 HotPlex Worker Gateway 配置系统的权威参考。所有字段名、默认值和行为描述均基于源码 `internal/config/config.go:Default()` 和 `configs/config.yaml`。

---

## 目录

1. [配置优先级](#1-配置优先级)
2. [配置文件格式](#2-配置文件格式)
3. [配置项完整参考](#3-配置项完整参考)
   - [gateway — 网络网关](#31-gateway--网络网关)
   - [admin — 管理 API](#32-admin--管理-api)
   - [db — 数据持久化](#33-db--数据持久化)
   - [security — 安全与认证](#34-security--安全与认证)
   - [session — 会话管理](#35-session--会话管理)
   - [pool — 资源池](#36-pool--资源池)
   - [worker — Worker 运行时](#37-worker--worker-运行时)
   - [agent_config — Agent 人格与上下文](#38-agent_config--agent-人格与上下文)
   - [agent_config runtime facts](#381-agent_config-runtime-facts)
   - [skills — Skills 发现](#39-skills--skills-发现)
   - [cron — 定时任务调度器](#310-cron--定时任务调度器)
   - [webhook — GitHub Webhook 接收器](#3101-webhook--github-webhook-接收器)
   - [messaging — 消息平台](#311-messaging--消息平台)
   - [log — 日志](#312-log--日志)
   - [webchat — Web Chat UI](#313-webchat--web-chat-ui)
   - [oauth — WebChat 企业 SSO（OIDC）](#314-oauth--webchat--ssooidc)
   - [inherits — 配置继承](#315-inherits--)
   - [events 和 audit — 事件与审计留存](#316-events-和-audit--事件与审计留存)
   - [execution.queue — 持久输入队列](#317-executionqueue--)
4. [热重载](#4-热重载)
5. [环境变量速查](#5-环境变量速查)

---

## 1. 配置优先级

HotPlex 采用分层覆盖策略，**高优先级覆盖低优先级**：

```
代码默认值 (Default())
  ↓ 被覆盖
配置继承链 (inherits 递归解析，含环检测)
  ↓ 被覆盖
配置文件 (YAML/JSON/TOML)
  ↓ 被覆盖
环境变量 (HOTPLEX_*)
```

**要点**：

- 代码默认值让二进制可以在零配置下启动（敏感字段除外）
- 配置文件是**非敏感值的权威来源**
- 敏感字段（Admin tokens、API keys）**永远不会从配置文件加载**，只能通过环境变量或 Secrets Provider 注入
- 消息平台配置有额外的三级优先级：`platform-level > messaging-level > Default()`

---

## 2. 配置文件格式

### 基本格式

配置文件为 YAML 格式，通过 Viper 解析。支持 YAML、JSON、TOML 三种格式。

### 环境变量展开

配置文件值支持 `${VAR}` 和 `${VAR:-default}` 语法引用环境变量：

```yaml
worker:
  environment:
    - GH_TOKEN=${HOTPLEX_WORKER_GH_TOKEN}
    - GITHUB_TOKEN=${HOTPLEX_WORKER_GITHUB_TOKEN:-}
```

- `${VAR}` — 引用环境变量，**未设置时该条目被排除**（不会传入空字符串）
- `${VAR:-default}` — 引用环境变量，未设置时使用 `default` 作为值
- `${VAR:-}` — 未设置时使用空字符串，**条目保留**

### 路径展开

所有路径字段自动进行以下处理：

- `~` 展开为用户主目录（`$HOME`）
- 相对路径转为绝对路径
- 符号链接解析（防止 TOCTOU 攻击）
- 支持 `${VAR}` 展开

### 加载顺序

环境变量文件加载顺序（后者覆盖前者）：

```
.env → .env.local → shell 环境变量
```

---

## 3. 配置项完整参考

### 3.1 gateway — 网络网关

WebSocket 网关的核心网络参数。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `addr` | string | `localhost:8888` | `HOTPLEX_GATEWAY_ADDR` | 监听地址。默认仅绑定 localhost（安全基线）；反向代理场景设为 `:8888` 监听所有接口 |
| `read_buffer_size` | int | `4096` | — | WS 读缓冲区大小（字节） |
| `write_buffer_size` | int | `4096` | — | WS 写缓冲区大小（字节） |
| `ping_interval` | duration | `54s` | — | Ping 帧发送间隔。必须小于 `pong_timeout` |
| `pong_timeout` | duration | `60s` | — | Pong 响应超时。超时后连接被视为断开 |
| `write_timeout` | duration | `10s` | — | 单次写入操作超时 |
| `idle_timeout` | duration | `5m` | — | 连接空闲超时。超时后服务端关闭连接 |
| `max_frame_size` | int64 | `32768` (32KB) | — | 单个 WS 帧最大大小 |
| `broadcast_queue_size` | int | `256` | — | Hub 广播通道缓冲区大小 |
| `platform_write_buffer` | int | `64` | — | 每连接异步平台写入通道容量。64 个槽位在 120ms 合并窗口下可容纳约 8 批事件 |
| `platform_drop_threshold` | int | `56` | — | 开始丢弃可丢弃事件（delta/raw）的填充水位线。设为 `platform_write_buffer` 的 87.5% 以提供背压缓解 |
| `delta_coalesce_interval` | duration | `120ms` | — | 连续 delta 事件的合并时间窗口。120ms 针对 Feishu CardKit 10 次/秒/卡片 的速率限制（含余量约 8.3 次/秒），同时保持首 token 延迟远低于 200ms 人类感知阈值 |
| `delta_coalesce_size` | int | `200` | — | delta 立即 flush 的 rune 阈值。200 rune ≈ 40 token，仅在突发时触发提前 flush |

---

### 3.2 admin — 管理 API

Admin API 管理端点配置。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `enabled` | bool | `true` | `HOTPLEX_ADMIN_ENABLED` | 是否启用 Admin API |
| `addr` | string | `localhost:9999` | `HOTPLEX_ADMIN_ADDR` | Admin API 监听地址。默认仅绑定 localhost |
| `tokens` | []string | `[]` | `HOTPLEX_ADMIN_TOKEN_1..N` | 认证 token 列表。**只能通过环境变量设置**，支持编号后缀用于轮换 |
| `token_scopes` | map[string][]string | `nil` | — | 每 token 的权限范围映射。可用 scope：`health:read` / `session:read` / `session:write` / `session:delete` / `stats:read` / `config:read` / `config:write` / `admin:read` / `admin:write` / `runtime:read` / `runtime:write`（runtime 二者服务 fence 决策与 runtime-plan 读取，#877/#946） |
| `default_scopes` | []string | `["session:read", "session:write", "session:delete", "stats:read", "health:read", "admin:write"]` | — | 未配置 scopes 的 token 的默认权限范围。`admin:write` 隐含 `admin:read` 与 `runtime:read`/`runtime:write`；`admin:read` 隐含 `config:read` |
| `ip_whitelist_enabled` | bool | `false` | — | 是否启用 IP 白名单 |
| `allowed_cidrs` | []string | `["127.0.0.0/8", "10.0.0.0/8"]` | — | 允许访问的 CIDR 列表 |
| `rate_limit_enabled` | bool | `true` | — | 是否启用速率限制 |
| `requests_per_sec` | int | `10` | — | 每秒允许的请求数 |
| `burst` | int | `20` | — | 突发请求缓冲量 |

---

### 3.3 db — 数据持久化

数据库配置，支持 SQLite（默认）和 PostgreSQL 两种后端。Session、Event Store、Cron 等模块共享同一数据库实例。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `driver` | string | `"sqlite"` | `HOTPLEX_DB_DRIVER` | 数据库驱动。可选 `"sqlite"` / `"postgres"` / `"pg"` / `"postgresql"` |
| `sqlite` | object | — | — | SQLite 子配置（见下方）。当 driver 为 sqlite 时生效 |
| `postgres` | object | — | — | PostgreSQL 子配置（见下方）。当 driver 为 postgres 时生效 |

#### SQLite 配置 (`db.sqlite.*`)

当 `db.driver` 为 `sqlite`（或未设置）时使用。下方遗留字段（`db.path` 等）仍可用于向后兼容，结构化字段优先。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `sqlite.path` | string | `~/.hotplex/data/hotplex.db` | `HOTPLEX_DB_PATH` | 主数据库文件路径。Session 和 Event 共用 |
| `sqlite.wal_mode` | bool | `true` | `HOTPLEX_DB_WAL_MODE` | 启用 WAL (Write-Ahead Logging) 模式。生产环境必须开启 |
| `sqlite.busy_timeout` | duration | `5s` | — | SQLite 忙等待超时。写入冲突时的重试时间 |
| `sqlite.max_open_conns` | int | `3` | — | 最大打开连接数。1 写 + 2 读 |
| `sqlite.vacuum_threshold` | float64 | `0.2` | — | VACUUM 触发阈值。当空闲页占比 ≥ 20% 时自动 VACUUM |
| `sqlite.cache_size_kib` | int | `8192` (8MB) | — | SQLite 页面缓存大小（KiB） |
| `sqlite.mmap_size_mib` | int | `64` (64MB) | — | SQLite mmap 大小（MiB） |
| `sqlite.wal_autocheckpoint` | int | `2000` | — | WAL 自动 checkpoint 页数阈值 |

> **遗留字段**：`db.path`、`db.wal_mode`、`db.busy_timeout`、`db.max_open_conns`、`db.vacuum_threshold`、`db.cache_size_kib`、`db.mmap_size_mib`、`db.wal_autocheckpoint` 仍可使用，等价于 `db.sqlite.*`。结构化字段优先。`db.events_path` 已废弃。

#### PostgreSQL 配置 (`db.postgres.*`)

当 `db.driver` 设为 `"postgres"` 时使用 PostgreSQL 替代 SQLite。适用于生产环境多实例部署和高并发场景。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `postgres.dsn` | string | — | `HOTPLEX_DB_POSTGRES_DSN` | PostgreSQL 连接字符串。格式：`postgres://user:password@host:port/dbname?sslmode=prefer` |
| `postgres.max_open_conns` | int | `25` | `HOTPLEX_DB_POSTGRES_MAX_OPEN_CONNS` | 最大打开连接数 |

```yaml
# PostgreSQL 配置示例
db:
  driver: "postgres"
  postgres:
    dsn: "postgres://hotplex:secret@db.example.com:5432/hotplex?sslmode=require"
    max_open_conns: 25
```

---

### 3.4 security — 安全与认证

安全、认证和输入验证配置。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `api_key_header` | string | `X-API-Key` | `HOTPLEX_SECURITY_API_KEY_HEADER` | 客户端 API Key 的 HTTP 头名称 |
| `api_keys` | []string | `nil` | `HOTPLEX_SECURITY_API_KEY_1..N` | 客户端认证密钥列表。**只能通过环境变量设置**，支持编号后缀 |
| `tls_enabled` | bool | `false` | — | 是否启用 TLS。非 localhost 地址时强烈建议开启 |
| `tls_cert_file` | string | `/etc/hotplex/tls/server.crt` | — | TLS 证书文件路径 |
| `tls_key_file` | string | `/etc/hotplex/tls/server.key` | — | TLS 私钥文件路径 |
| `allowed_origins` | []string | `["*"]` | `HOTPLEX_SECURITY_ALLOWED_ORIGINS` | CORS 允许的 Origin 列表，覆盖 Gateway API、Admin API 和 WebSocket 连接。通配符 `*` 允许所有来源；指定域名时回显匹配的 Origin 并设置 `Vary: Origin` 头 |
| `work_dir_allowed_base_patterns` | []string | `[]` | — | 额外的工作目录白名单模式。支持 `~` 和 `${VAR}` 展开。程序内建默认值：`~/.hotplex/workspace`、`~/workspace`、`~/projects`、`~/work`、`~/dev`、`/var/hotplex/projects` |
| `work_dir_forbidden_dirs` | []string | `[]` | — | 额外的工作目录黑名单。显式禁止的目录列表 |
| `csp` | string | `""` | `HOTPLEX_SECURITY_CSP` | Content-Security-Policy 头覆盖，用于嵌入式 WebChat 和文档门户。空字符串使用内建默认值（适合 localhost）。远程部署时需设置，否则浏览器会阻止 fetch/ws/connect 调用 |
| `security_contact` | string | `""` | `HOTPLEX_SECURITY_SECURITY_CONTACT` | 安全联系 URI，启用 `/.well-known/security.txt`（RFC 9116）。空字符串 = 禁用。示例：`mailto:security@example.com`、`https://example.com/security` |
| `cookie_secret` | string | `""` | `HOTPLEX_SECURITY_COOKIE_SECRET` | WebChat 多租户登录 cookie 的 HMAC-SHA256 密钥。空时由 `crypto/rand` 自动生成并持久化到 `~/.hotplex/data/cookie_secret.key`（权限 `0600`）；非空时取其 SHA256 digest 作密钥并**跳过文件生成**（配置优先，适用于多实例共享固定密钥）。详见 [WebChat 设置 — HMAC Cookie 认证](../guides/developer/webchat-setup.md#authentication) |

---

### 3.5 session — 会话管理

Session 生命周期管理配置。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `retention_period` | duration | `168h` (7天) | `HOTPLEX_SESSION_RETENTION_PERIOD` | 兼容的 Session 运行到期时长；到期后释放/终止运行资源。它不是聊天正文保留期 |
| `term_retention` | duration | `168h` (7天) | `HOTPLEX_SESSION_TERM_RETENTION` | Legacy `TERMINATED` session 根记录保留时长；v2 会话不使用此项提前删除 |
| `cron_term_retention` | duration | `24h` | `HOTPLEX_SESSION_CRON_TERM_RETENTION` | Legacy Cron `TERMINATED` session 根记录保留时长；v2 会话不使用此项提前删除 |
| `gc_scan_interval` | duration | `1m` | — | GC 扫描间隔。定期扫描过期 Session 并清理 |
| `max_concurrent` | int | `1000` | `HOTPLEX_SESSION_MAX_CONCURRENT` | 最大并发 Session 数。达到上限后新请求被拒绝 |

#### lifecycle — 会话与数据期限

这些期限与 Worker 运行资源分开计算。新会话默认使用 `v2` 策略；升级前已存在的记录保留原策略，存量期限不会因改配置被静默缩短。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `policy` | string | `v2` | `HOTPLEX_LIFECYCLE_POLICY` | 新建会话采用的生命周期策略 |
| `conversation.archive_after` | duration | `168h` (7天) | `HOTPLEX_LIFECYCLE_CONVERSATION_ARCHIVE_AFTER` | 自最近有效输入起的归档标记时间；标记后仍可查看和续聊 |
| `conversation.retention_after_last_input` | duration | `4320h` (180天) | `HOTPLEX_LIFECYCLE_CONVERSATION_RETENTION_AFTER_LAST_INPUT` | 每次有效用户输入后向后推进的会话期限。读取历史或编辑标题不会续期 |
| `content.retention` | duration | `4320h` (180天) | `HOTPLEX_LIFECYCLE_CONTENT_RETENTION` | 新写入的每条事件/轮次正文从自身创建时间起计算的期限；既有正文的期限不会被重新计算 |
| `effect_payload.retention_after_settlement` | duration | `168h` (7天) | `HOTPLEX_LIFECYCLE_EFFECT_PAYLOAD_RETENTION_AFTER_SETTLEMENT` | v2 下，所有关联投递都已结算后清除 delivery payload 正文；`unknown` 或未结算记录会继续保留 |
| `facts.retention_after_settlement` | duration | `2160h` (90天) | `HOTPLEX_LIFECYCLE_FACTS_RETENTION_AFTER_SETTLEMENT` | v2 清理已结算投递的详细 attempt，并压缩已结算 execution 的诊断明细；保留输入 ID、payload hash、worker run ID、最终状态及 `effects` / `cron_occurrences` 去重根记录，避免迟到事件或重放造成重复执行；未知、活动租约及缺少结算时钟的记录受保护 |
| `audit.capture_content` | bool | `false` | `HOTPLEX_LIFECYCLE_AUDIT_CAPTURE_CONTENT` | 当前只允许 `false`；设为 `true` 会校验失败，直到独立审计正文存储和期限清理实现 |
| `audit.facts_retention` | duration | `4320h` (180天) | `HOTPLEX_LIFECYCLE_AUDIT_FACTS_RETENTION` | 新写入 lifecycle-v2 审计事实的最低保留期；到期时间随记录写入并纳入哈希，改配置只影响之后的新记录；不改变 legacy `user_activity` 链的 `audit.retention`。哈希链按已到期连续前缀清理，未到期记录可能暂时阻挡后续记录删除 |
| `audit.content_retention` | duration | `4320h` (180天) | `HOTPLEX_LIFECYCLE_AUDIT_CONTENT_RETENTION` | 预留配置；正文采集目前关闭，`capture_content: true` 会校验失败 |
| `media.retention` | duration | `24h` | `HOTPLEX_LIFECYCLE_MEDIA_RETENTION` | Slack 与飞书下载缓存的最长保留时间；后台每 6 小时扫描一次 |
| `trace.retention` | duration | `48h` | `HOTPLEX_LIFECYCLE_TRACE_RETENTION` | v2 仅清理 `~/.hotplex/logs` 中已关闭且过期的 ACP trace 文件及轮转副本；跳过活动 writer 和其他日志 |
| `gc.batch_size` | int | `100` | `HOTPLEX_LIFECYCLE_GC_BATCH_SIZE` | v2 delivery payload、已结算 attempt 与 ACP trace 清理每批上限；存量期限迁移复用该值，单类每批最多 500 条 |
| `gc.interval` | duration | `1m` | `HOTPLEX_LIFECYCLE_GC_INTERVAL` | v2 delivery payload 与 ACP trace 清理扫描间隔；媒体目录仍每 6 小时扫描 |
| `gc.max_lag` | duration | `1h` | `HOTPLEX_LIFECYCLE_GC_MAX_LAG` | 可清理或被义务阻塞的最老到期会话超过此延迟时记录一次告警日志；不强制删除。积压与延迟指标见[指标参考](metrics.md) |

> v2 会话达到 `archive_after` 后仍可查看和续聊；达到会话期限后，GC 会在确认 Worker 已停止且没有未完成/状态未知的输入、执行或清理任务后，将会话退役并写入异步清理任务。正文到期会从读取结果中排除并分批清理。用户主动删除后的异步清理进度可通过 `GET /api/sessions/{id}/cleanup` 查询。升级前的 legacy 会话不会自动切换到 v2；管理员可先调用 `POST /admin/lifecycle/migration/preview` 查看汇总，再通过单独的 `apply` 请求逐批延长期限。迁移不会缩短期限、补造缺失的 `last_input_at` 或恢复已删除记录。上表标为“预留配置”的项目不会因设置参数而启动清理任务。

生命周期清理 Worker 在 Gateway 启动时读取保留期限、扫描间隔和批量大小；调整这些清理参数后需重启 Gateway。每条会话、正文和 lifecycle-v2 审计事实的到期时间在写入时固定，之后修改配置不会追溯改写存量期限；如需调整会话存量期限，使用上述迁移预览与 apply。

---

### 3.6 pool — 资源池

Session 资源池配额管理。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `min_size` | int | `0` | — | 池最小大小（预留） |
| `max_size` | int | `100` | `HOTPLEX_POOL_MAX_SIZE` | 池最大大小。系统级 Session 总量上限 |
| `max_idle_per_user` | int | `5` | `HOTPLEX_POOL_MAX_IDLE_PER_USER` | 每用户最大空闲 Session 数 |
| `max_memory_per_user` | int64 | `3221225472` (3GB) | `HOTPLEX_POOL_MAX_MEMORY_PER_USER` | 每用户最大内存配额（字节）。0 表示无限制 |
| `max_per_workspace` | int | `0` | — | WebChat 多租户每 workspace 最大活跃 Session（v1.29.1+，spec ①）。0 = 无限制；默认禁用，WebChat 部署按需配置 |

---

### 3.7 worker — Worker 运行时

Worker 进程生命周期和环境配置。

#### 3.7.1 基础配置

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `max_lifetime` | duration | `24h` | `HOTPLEX_WORKER_MAX_LIFETIME` | Worker 最大存活时间。超时后强制终止 |
| `idle_timeout` | duration | `60m` | `HOTPLEX_WORKER_IDLE_TIMEOUT` | Worker 空闲超时。无 I/O 时自动回收 |
| `execution_timeout` | duration | `30m` | `HOTPLEX_WORKER_EXECUTION_TIMEOUT` | 单次执行超时。捕获僵尸进程 |
| `turn_timeout` | duration | `30m` | `HOTPLEX_WORKER_TURN_TIMEOUT` | 单轮绝对上限，从 Worker 接收该轮输入时起计算，持续输出不会延长。v2 配置中显式设为 `0` 仍按 30 分钟默认值执行；legacy 策略下 `0` 保持禁用 |
| `default_work_dir` | string | `~/.hotplex/workspace` | `HOTPLEX_WORKER_DEFAULT_WORK_DIR` | Worker 默认工作目录 |
| `pid_dir` | string | `~/.hotplex/.pids` | — | PID 文件存储目录 |
| `env_blocklist` | []string | `[]` | — | 需要从 Worker 环境中屏蔽的变量前缀列表（带尾部 `_`） |
| `environment` | []string | 见下方 | — | 注入到所有 Worker 进程的额外环境变量。支持 `${VAR}` 展开，未设置且无默认值的条目被排除 |
| `default_permission_mode` | string | `workspace` | — | workspace 无显式 override 时 bridge 注入的默认权限模式（r3 #804）。合法值 `read-only`/`workspace`/`auto-edit`/`bypass`/空，空与缺省均归一化为 `workspace`；非法值在启动与热重载时拒绝（防 typo fail-open）。仅 config.yaml 可配置，无 env 绑定 |
| `permission_deny_dedup.enabled` | bool | `true` | `HOTPLEX_WORKER_PERMISSION_DENY_DEDUP_ENABLED` | 用户拒绝权限后，在 `window` 窗口内对相同 owner+fingerprint 的重复权限请求本地自动拒绝，不再转发新卡片给客户端——关闭「拒绝 → Agent 秒级重试同一工具 → 新卡片」循环。详见 `docs/specs/Permission-Deny-Dedup-Spec.md` |
| `permission_deny_dedup.window` | duration | `60s` | `HOTPLEX_WORKER_PERMISSION_DENY_DEDUP_WINDOW` | 拒绝去重窗口长度。`enabled=true` 时必须 > 0，否则启动拒绝 |
| `runtime_plan.mode` | string | `shadow` | — | EffectiveRuntimePlan 在启动路径上的信任级别（#946 D2/D3）。`shadow` = 解析计划、绑定到本次 Worker run 并与实际启动参数做字段级 parity 对比，但**不改变任何启动行为**；`authoritative` = 由计划驱动启动，且仅对下面两个白名单同时命中的「入口 × Worker」组合生效。任何其他值（含缺省）一律按 `shadow` 处理，绝不解读为 authoritative |
| `runtime_plan.authoritative_entries` | string[] | `[]` | — | 允许由计划驱动启动的入口类型：`webchat` / `messaging` / `cron`。为空表示**没有任何入口**处于 authoritative |
| `runtime_plan.authoritative_workers` | string[] | `[]` | — | 允许由计划驱动启动的 Worker 类型：`claude_code` / `codex_cli` / `opencode_server` / `acp`。为空表示**没有任何 Worker**处于 authoritative |
| `env_profile` | string | `compat` | — | Worker 子进程环境的构建方式（#946 E）。`compat` = 沿用现状：继承宿主环境并减去 blocklist（denylist 只能拦住已被枚举的变量）。`strict` = 反转：从**按 OS 的最小系统白名单**开始，只接受显式注入的变量。任何其他取值（含缺省）一律按 `compat` 处理，绝不解读为 strict |
| `env_allow_keys` | string[] | `[]` | — | strict 模式下额外允许从宿主继承的变量名。这是**运维配置，不是客户端输入**：请求不得据此扩大宿主环境向 Worker 的泄漏面。blocklist 优先级更高，显式 block 不会被 allow 覆盖 |
| `codex_cli.env_profile` / `codex_cli.env_allow_keys` | string / string[] | `compat` / `[]` | — | 同上，但作用于 **共享的 codex app-server 进程**。该进程服务多个会话，环境属于**进程**而非会话，因此配置挂在 Worker 自己的段上 |
| `opencode_server.env_profile` / `opencode_server.env_allow_keys` | string / string[] | `compat` / `[]` | — | 同上，作用于共享的 `opencode serve` 进程 |
| `require_isolation.filesystem` | bool | `false` | — | 要求 Worker 在启动前**证明**文件系统隔离，否则拒绝启动（#946 E2）。判定门槛是 `enforced`，不是 `declared`：`declared` 只说明「我们请求过」，`observed` 只说明「后端自述」，`partial` 说明只约束了一部分——都比要求的保证弱 |
| `require_isolation.network` | bool | `false` | — | 同上，针对网络隔离。默认关闭：**不要求隔离时，未知（unknown）不会阻止启动**，因此仅启用 strict env 即可在没有 OS 隔离后端的主机上正常运行 |
> 💡 **两个白名单都必须命中**：`authoritative` 需要「入口」与「Worker」同时在各自白名单内才生效。只配一侧的 rollout 会整体停留在 `shadow`，因此部分上线永远不会自称已完整上线。默认两个白名单都是空的，即使把 `mode` 写成 `authoritative`，也不会有任何组合真正切换。

> ⚠️ **strict 环境不是隔离**。它只收窄**环境变量**，不提供文件系统或网络隔离。`strict` 下传入 `HOME` 也**不**意味着目录被隔离——那只是一个变量，Worker 仍可读取 OS 允许它读取的任何路径。隔离结论必须来自隔离能力报告（`declared` / `observed` / `enforced` / `partial` / `unavailable` / `unknown`），由实际后端证据产生，不能由 permission mode 或 env profile 推断。


> 💡 **未知是合法答案**。Worker 通过可选接口 `IsolationReporter` 上报隔离能力；未实现该接口的 Worker 报告 `unknown`，而不是被推断为安全。空报告等同于「无法回答」，同样按 `unknown` 处理。`scope=process` 表示该边界属于共享进程——同一进程上的所有会话看到的是同一条边界，报告成 per-session 属于编造。

**默认 environment**：

```yaml
environment:
  - GH_TOKEN=${HOTPLEX_WORKER_GH_TOKEN}
  - GITHUB_TOKEN=${HOTPLEX_WORKER_GITHUB_TOKEN}
```

#### 3.7.2 auto_retry — 自动重试

LLM Provider 返回临时错误（429、529、400 等）时的自动重试配置。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `enabled` | bool | `true` | `HOTPLEX_WORKER_AUTO_RETRY_ENABLED` | 是否启用自动重试 |
| `max_retries` | int | `9` | `HOTPLEX_WORKER_AUTO_RETRY_MAX_RETRIES` | 最大重试次数 |
| `base_delay` | duration | `5s` | — | 首次重试延迟。指数退避基数 |
| `max_delay` | duration | `120s` | — | 最大重试延迟。退避上限 |
| `retry_input` | string | `继续` | — | 重试时发送给 Worker 的输入文本（中文字符串，意为"continue"） |
| `notify_user` | bool | `true` | — | 重试时是否通知用户 |
| `patterns` | []string | `[]` | — | 额外的可重试错误正则模式。追加到系统内建模式（429、500 等） |

#### 3.7.3 claude_code — Claude Code Worker

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `command` | string | `claude` | `HOTPLEX_WORKER_CLAUDE_CODE_COMMAND` | Worker 启动命令。支持带子命令，如 `ccr code` |
| `permission_mode` | string | `""` (= `bypass`) | `HOTPLEX_WORKER_CLAUDE_CODE_PERMISSION_MODE` | Claude Code 的 operator 默认/上限权限模式 tier。对 platform/cron 会话（bridge 注入空 sessionMode）直接作为有效模式；对 workspace 会话作为 permissiveness 上限，有效模式取 session 模式与 operator 的较低者（防越权）。合法值 `read-only`/`workspace`/`auto-edit`/`bypass`/空，空 = `bypass`（向后兼容 legacy 默认）。与 `codex_cli.sandbox`+`approval_mode`、`acp.auto_approve` 对应；区别于 `default_permission_mode`（后者是 bridge 为 workspace 会话解析的有效模式，归一化为 `workspace`） |
| `permission_prompt` | bool | `true` | — | 启用 `--permission-prompt-tool` stdio 模式。开启后权限请求会转发到 Slack/飞书交互 UI。config.yaml 默认开启（#920），使 headless Worker 的权限请求默认走交互 UI 而非静默拒绝；字段缺省时 Go 零值为 `false`（行为回退为静默拒绝） |
| `permission_auto_approve` | []string | `["ExitPlanMode"]` | — | 自动批准的工具名称列表（无需转发用户交互 UI） |
| `mcp_servers` | map | `{}` | — | 用户配置的 MCP Server。空值 = 使用默认发现机制 |

##### Claude Code 权限边界与 Doctor

`bypass` 会让 Claude Code 使用跳过常规权限提示的执行路径；它只应出现在受控、短期、已隔离的运维场景，不能作为飞书生产机器人的常规默认值。`messaging.*.allow_from`、`allow_dm_from` 和 `allow_group_from` 仅限制**谁能向机器人发消息**，不能限制获准请求在 Worker 内可调用的工具，也不能替代 Worker 权限模式。

对启用的飞书 `claude_code` Worker，`hotplex doctor` 的 `worker.claude_bypass_mode` 会按运行时同一规则检查两类有效模式：平台会话的空 `sessionMode` 使用 `worker.claude_code.permission_mode`（空值仍是兼容性的 `bypass`）；workspace 会话使用 `worker.default_permission_mode`，再被前者作为 permissiveness ceiling 收紧。发现任一路径为 `bypass` 时只输出 Warn 和收紧建议，**不会修改配置，也没有自动修复**。

`HOTPLEX_WORKER_CLAUDE_CODE_PERMISSION_MODE` 可覆盖 Claude Code operator 模式。相对地，`HOTPLEX_WORKER_DEFAULT_PERMISSION_MODE` 当前**没有**运行时环境变量绑定；该变量不会覆盖 YAML 的 `worker.default_permission_mode`，请在配置文件中设置该字段。收紧到 workspace 的生产基线示例：

```yaml
worker:
  default_permission_mode: workspace
  claude_code:
    permission_mode: workspace
```

#### 3.7.4 opencode_server — OpenCode Server Worker

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `command` | string | `opencode` | `HOTPLEX_WORKER_OPENCODE_SERVER_COMMAND` | Worker 启动命令。支持带子命令，如 `opencode serve` |
| `idle_drain_period` | duration | `30m` | `HOTPLEX_WORKER_OPENCODE_SERVER_IDLE_DRAIN_PERIOD` | 空闲排空周期。单例进程在此期间无任务后关闭 |
| `ready_timeout` | duration | `10s` | `HOTPLEX_WORKER_OPENCODE_SERVER_READY_TIMEOUT` | 进程就绪等待超时 |
| `ready_poll_interval` | duration | `200ms` | `HOTPLEX_WORKER_OPENCODE_SERVER_READY_POLL_INTERVAL` | 就绪状态轮询间隔 |
| `http_timeout` | duration | `30s` | `HOTPLEX_WORKER_OPENCODE_SERVER_HTTP_TIMEOUT` | HTTP 请求超时 |
| `context_window` | int | `200000` | `HOTPLEX_WORKER_OPENCODE_SERVER_CONTEXT_WINDOW` | OCS fallback context window（token 数）。OCS HTTP API 不暴露真实模型 context window，此可配置值喂给 `context_pct`；设 `0` 则不设置 `context_pct`（spec §5 O1，turn summary parity #776） |

#### 3.7.5 codex_cli — Codex CLI Worker

OpenAI Codex CLI Worker，使用 app-server 单例持久进程模式，通过 JSON-RPC 2.0 over stdio 通信。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `command` | string | `codex` | `HOTPLEX_WORKER_CODEX_CLI_COMMAND` | Worker 启动命令。支持带子命令 |
| `model` | string | `""` | `HOTPLEX_WORKER_CODEX_CLI_MODEL` | 模型名称。空值使用 Codex 默认模型（`~/.codex/config.toml`） |
| `sandbox` | string | `danger-full-access` | `HOTPLEX_WORKER_CODEX_CLI_SANDBOX` | 沙箱模式：`read-only`、`workspace-write`、`danger-full-access` |
| `approval_mode` | string | `never` | `HOTPLEX_WORKER_CODEX_CLI_APPROVAL_MODE` | 审批模式：`untrusted`（所有操作需审批）、`on-request`（仅高风险操作）、`never`（全自动） |
| `ephemeral` | bool | `true` | — | 临时会话模式。不持久化到磁盘，Session 结束后数据清除 |
| `personality` | string | `friendly` | — | Agent 人格模式 |
| `startup_timeout` | duration | `30s` | — | 进程启动超时 |
| `call_timeout` | duration | `30s` | — | JSON-RPC 调用超时 |
| `idle_drain_period` | duration | `30m` | — | 空闲排空超时。超时后单例进程关闭 |
| `color` | bool | `false` | — | 启用彩色输出（传递 `--color` 给 Codex CLI） |
| `output_file` | string | `""` | — | 输出最后一条消息到文件（传递 `--output-last-message` 给 Codex CLI） |
| `strict_config` | bool | `false` | — | 严格配置校验（传递 `--strict-config`） |
| `skip_git_repo_check` | bool | `false` | — | 跳过 Git 仓库检查（传递 `--skip-git-repo-check`） |
| `ignore_user_config` | bool | `false` | — | 忽略用户级配置（传递 `--ignore-user-config`） |
| `ignore_rules` | bool | `false` | — | 忽略项目规则（传递 `--ignore-rules`） |
| `local_provider` | bool | `false` | — | 强制使用本地模型提供者（传递 `--local-provider`） |
| `config_profile` | string | `""` | — | Codex 配置 profile（传递 `--profile`） |
| `bypass_hook_trust` | bool | `false` | — | 绕过 hook 信任检查（传递 `--dangerously-bypass-hook-trust`） |

##### 常见问题：`rmcp::transport::worker` fatal / `Transport channel closed`

**症状**：Gateway 日志（DEBUG 级 `proc: stderr`）出现类似：

```
worker quit with fatal: Transport channel closed, when Client(HttpRequest(HttpRequest(
"http/request failed: error sending request for url (https://chatgpt.com/backend-api/ps/mcp)")))
```

**根因**：codex（ChatGPT 登录态）在启动时拉取 ChatGPT 账号配置的 MCP 服务器列表（`https://chatgpt.com/backend-api/ps/mcp`）。该 HTTPS 请求由 codex 进程自身发起，若出口网络无法访问 chatgpt.com（典型场景：国内代理节点出口 IP 被 OpenAI 封禁，OpenAI 边缘节点直接返回 HTTP 451），rmcp 传输层判定失败并 fatal 退出该 MCP 线程。此错误只影响 ChatGPT 账号 MCP/连接器工具，不影响 app-server 主体进程。

**排查**（在运行 Gateway 的机器上）：

```bash
# 1. 直连探测（结果 451 / 超时 / 连接被拒 均为不可达）
curl -s -o /dev/null -w "%{http_code}\n" -m 8 https://chatgpt.com/backend-api/ps/mcp

# 2. 经代理探测（把 <port> 换成你的代理端口；Google 能通但 chatgpt.com 返回 451 = 出口节点被封）
curl -s -x http://127.0.0.1:<port> -o /dev/null -w "%{http_code}\n" -m 8 https://www.google.com
curl -s -x http://127.0.0.1:<port> -o /dev/null -w "%{http_code}\n" -m 8 https://chatgpt.com/backend-api/ps/mcp
```

**修复**：在代理客户端中把 chatgpt.com 的出口节点切换到 OpenAI 支持的地区（美/日/新加坡等；香港、澳门及机房/WARP IP 通常被 451 封禁）。切换后重新执行上面的探测命令，返回非 451（正常为 401/403 等）即恢复；codex 下次会话启动时会自动重试拉取 MCP。

> 提示：codex 进程继承 Gateway 进程的 `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` 环境变量（不受 `env_blocklist` 拦截），但 `worker.environment:` 配置目前不会注入 codex 单例进程；如需给 codex 注入代理，请在启动 Gateway 的 shell / `.env` 中设置 `HTTPS_PROXY` 等变量。

#### 3.7.6 acp — ACP 通用 Worker

ACP (Agent Communication Protocol) 通用 Worker，通过 JSON-RPC 2.0 over stdio 连接任何 ACP 兼容的 AI Agent（如 Hermes Agent）。支持流式响应、工具调用、权限请求和 Session 恢复。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `command` | string | `hermes acp` | `HOTPLEX_WORKER_ACP_COMMAND` | ACP Agent 启动命令。支持带子命令，如 `hermes acp` |
| `auto_approve` | bool | `true` | `HOTPLEX_WORKER_ACP_AUTO_APPROVE` | 自动批准权限请求，无需用户确认 |
| `args` | []string | `[]` | — | 附加到 command 之后的额外参数列表 |
| `debug` | bool | `false` | — | 启用 JSON-RPC 协议 trace 日志（调试用） |

**配置示例**：

```yaml
worker:
  acp:
    command: "hermes acp"    # 或其他 ACP 兼容 Agent
    auto_approve: true       # 自动批准工具调用权限
    args: ["--verbose"]      # 附加命令行参数
    debug: true              # 启用协议 trace 日志
```

**使用方式**：在 messaging 层指定 `worker_type: "acp"` 即可启用：

```yaml
messaging:
  worker_type: "acp"         # 全局默认
  # 或 per-platform / per-bot 指定
```

---

### 3.8 agent_config — Agent 人格与上下文

Agent B/C 通道配置加载器。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `enabled` | bool | `true` | `HOTPLEX_AGENT_CONFIG_ENABLED` | 是否启用 Agent 配置加载（SOUL.md、AGENTS.md 等） |
| `config_dir` | string | `~/.hotplex/agent-configs` | `HOTPLEX_AGENT_CONFIG_DIR` | 配置文件根目录。支持 `~` 和 `${VAR}` 展开 |
| `inject_exclude` | []string | `[]` | — | 全局默认排除列表。列出的配置文件（如 `SOUL.md`、`MEMORY.md`）不会被加载和注入。平台级和 Bot 级可覆盖。`META-COGNITION.md` 始终注入（go:embed），不可排除 |

**B 通道**（`<directives>`）：`META-COGNITION.md`（go:embed，始终首位）+ `SOUL.md` + `AGENTS.md` + `TOOLS.md`。`TOOLS.md` 注入 `<tool-guidance>`，只描述环境工具的使用方式、偏好和边界，不声明工具实际存在。

**C 通道**（`<context>`）：`USER.md` + `MEMORY.md`

**三级 fallback**：解析优先级为 Bot（`slack/{botName}/`）→ 平台（`slack/`）→ 全局，每文件独立、命中即终止。Bot 级目录名使用 YAML 配置中 `bots[].name` 的值。键/文件缺失表示继续继承；存在但正文为空表示显式清空并停止 fallback。

**文件契约**：AgentConfig 只识别 `SOUL.md`、`AGENTS.md`、`TOOLS.md`、`USER.md`、`MEMORY.md`；`inject_exclude` 也只匹配这五个规范槽位。配置根中的其他用户文件不参与加载，HotPlex 不会自动删除或改写它们。

**与 Agent Skills 的边界**：真实 Skills 由 `.agents/skills/<name>/SKILL.md` 等 Skill 根目录发现，
通过 `/admin/api/skills`、WebChat HTTP Skills 页面和 Worker 当前 evidence 管理，不由 `TOOLS.md`
派生，也不会把 Skill catalog 或正文常驻注入 AgentConfig prompt。Session `/skills` 仍按当前
Worker/filesystem evidence 决定是否出现；文件系统发现不代表当前 Session 可调用。

#### 3.8.1 agent_config runtime facts

实时 AgentConfig prompt 的外层版本为 `<agent-configuration schema-version="3">`。Worker 已选定且 Session 信息已解析后，Gateway 可在 `<directives>` 前插入 `<runtime-facts format="application/json" schema-version="1">`；内层 schema 1 是 facts 载荷版本，不能与外层 prompt schema 混淆。新 Session 和 `/reset` 使用同一构建路径，Admin 预览使用 runtime-neutral 的 `BuildSystemPrompt`，不伪造运行时事实。

facts 是受限的**声明**，不是外部 Worker 健康或权限已执行的证明。字段范围仅包括平台、Worker 类型、作用域种类、声明的权限模式、`resume`/`streaming`/`tools`、`skills`/`mcp`/`worker` 查询面、声明的 Skill catalog 所有者，以及 allowlist Gateway 环境键名的存在性。它不包含身份值、Session/频道/线程/团队 ID、工作目录、环境变量值、凭据、动态 catalog、Skill metadata/正文或 MCP 配置。

Skill 的 `name`/`description` progressive disclosure 由原生 Worker 负责，HotPlex 不复制一份 catalog 到 AgentConfig。Session `/skills` 保留 `callable`、`discoverable`、`unavailable` 三种状态；当前 merged catalog 把 filesystem-only Skill 标为 `discoverable`，只有 Worker 权威目录确认的 `callable` 才能执行。短 `/name`（包括 WebChat）、显式 `/worker <name>`、busy replay 和 crash structured replay 共用该判定。

#### 3.8.2 Built-in Skill inventory 与 native projection

HotPlex 发布两个 embedded canonical package：runtime profile 只有 `hotplex-cli`，用于日常
Cron、显式 Slack 和只读诊断；operator profile 累积包含 `hotplex-cli` 与
`hotplex-operator`，用于服务、更新、配置、Admin 和 audit，并要求显式 operator authority。
它们的 canonical source 分别是 `internal/skills/builtin/hotplex-cli` 与
`internal/skills/builtin/hotplex-operator`，生成器产出 byte-identical 的
`.agents/skills/hotplex-cli` 和 `.agents/skills/hotplex-operator` mirror。仓库 portfolio 还包含
`hotplex-diagnostics`、`hotplex-release`、`hotplex-docs-patrol`、`hotplex-stt-tts`，合计六个 Skill。
Public/Admin/WebChat HTTP builtins 永久可发现，不以 projection 或 receipt 存在为前提；Session
`/skills` 的出现由当前 Worker/filesystem evidence 决定，filesystem-only 项是 `discoverable`，
只有 projection/native advertisement 和 adapter-verified activation 才能证明 `callable`。真实
global/project/user Skill 同名时优先遮蔽 builtin，`source`
仍只使用 `global`/`project`，builtin 通过可选 `builtin`、`builtin_package_version` 元数据
表达。builtin-only update/delete 返回 `SKILL_BUILTIN_READONLY`，同名用户 override 可正常
创建和管理。

显式同步命令为 `hotplex skills status|sync|remove`，支持累积 `runtime`/`operator` profile、
重复 `--worker`、`--dry-run` 和 `--json`。Worker native roots 位于 UserHome：Claude 是
`<UserHome>/.claude/skills/<name>` 逐项软链接到 `<UserHome>/.agents/skills/<name>`，Codex/OpenCode
共享 `<UserHome>/.agents/skills`；ACP 没有可推断 filesystem root，typed unsupported 且不写文件。
`.codex/skills` 不属于当前 HotPlex Worker root，不会被隐式修改。immutable inventory 位于
`$HOTPLEX_HOME/skills/builtin/<version>/<name>`，状态和 receipts 也位于
`$HOTPLEX_HOME`，与 UserHome 原生 projection 分离。status/--dry-run 严格零写；同步不会覆盖
未知 user/project Skill，collision、drift、failed item 返回非零；remove 只删除 matching
receipt 且 unchanged-tree 能证明归属的 projection，不删除 inventory。

Gateway startup 的 built-in reconciliation check 与 doctor 的 built-in Skills checker 只读；
onboard/update 只有显式 `--sync-skills`
才同步，修改后在新
Session 或 `/reset` 才进入 Worker 的目录证据边界。未显式传 `--worker` 时，CLI 只解析已启用
messaging platform/bot 的 effective worker targets；empty target 返回 bounded error，不回退到
RegisteredTypes。

---

### 3.9 skills — Skills 发现

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `cache_ttl` | duration | `5m` | `HOTPLEX_SKILLS_CACHE_TTL` | Skills 列表缓存 TTL |

---

### 3.10 cron — 定时任务调度器

AI-native 定时任务引擎：自然语言 prompt 作为 payload，结果投递到创建者的平台（飞书卡片 / Slack 消息）。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `enabled` | bool | `true` | — | 是否启用 Cron 调度器 |
| `max_concurrent_runs` | int | `3` | — | 最大并行 Job 执行数 |
| `max_jobs` | int | `50` | — | 最大注册 Job 数 |
| `default_timeout_sec` | int | `300` (5分钟) | — | 单 Job 执行超时（秒） |
| `default_sandbox` | string | `""` | — | Cron Job 的默认 sandbox 模式覆盖。非空时应用于所有 cron Worker |
| `tick_interval_sec` | int | `60` | — | 调度器 tick 间隔（秒） |
| `yaml_config_path` | string | `""` | — | 外部 YAML 配置文件路径（可选） |
| `jobs` | []map | `[]` | — | 内联 Job 定义（可选） |

**Agent 路由与验证**：处理 Cron 请求时优先使用当前 Session 可用的 `hotplex-cli` Skill；不可用时查询当前二进制的 `hotplex cron --help`，必要时再看 `hotplex cron create --help`。创建后必须用独立读取路径执行 `hotplex cron get <id|name> --json`，核对任务状态、schedule、platform 和 platform key；不能把 Skill 文档或旧示例当作成功证据。

---

### 3.10.1 webhook — GitHub Webhook 接收器

GitHub Webhook 接收器，用于事件驱动的 Cron 任务触发。当收到匹配的 GitHub 事件（如 PR opened/synchronize）时，自动触发指定的 Cron Job。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `enabled` | bool | `false` | `HOTPLEX_WEBHOOK_ENABLED` | 是否启用 Webhook 接收器 |
| `secret` | string | `""` | `HOTPLEX_WEBHOOK_SECRET` | GitHub Webhook Secret（用于 HMAC-SHA256 签名验证）。**必须设置，为空时拒绝启动** |
| `path` | string | `/api/webhook/github` | `HOTPLEX_WEBHOOK_PATH` | HTTP 端点路径 |
| `max_body_size` | int64 | `1048576` (1MB) | `HOTPLEX_WEBHOOK_MAX_BODY_SIZE` | 请求体最大大小（字节） |
| `allowed_repos` | []string | `[]` | — | 允许的仓库列表（空 = 接受所有） |
| `target_job_name` | string | `pr-review-hotplex` | `HOTPLEX_WEBHOOK_TARGET_JOB_NAME` | 匹配事件时触发的 Cron Job 名称 |

**配置示例**：

```yaml
webhook:
  enabled: true
  secret: "${GITHUB_WEBHOOK_SECRET}"   # 通过环境变量注入
  path: /api/webhook/github
  allowed_repos:
    - "owner/repo"
  target_job_name: "pr-review"
```

**安全要求**：`secret` 必须与 GitHub Webhook 设置中的 Secret 一致。未设置 secret 时 Webhook 不会注册（安全基线）。

---

### 3.11 messaging — 消息平台

消息平台适配器配置。采用**共享默认值 + 平台覆盖**模式。

#### 3.11.1 共享配置（顶层 messaging）

这些值被所有平台继承，平台级配置可覆盖。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `turn_summary_enabled` | bool | `true` | `HOTPLEX_MESSAGING_TURN_SUMMARY_ENABLED` | 每次 Turn 完成后发送摘要 |
| `worker_type` | string | `claude_code` | `HOTPLEX_MESSAGING_WORKER_TYPE` | 默认 Worker 类型。平台级可覆盖 |

#### 3.11.2 STT（语音转文字）共享配置

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `stt_provider` | string | `local` | `HOTPLEX_MESSAGING_STT_PROVIDER` | STT 提供者：`local`（外部命令）、`feishu`（云端 API）、`feishu+local`（云端主 + 本地备）、空字符串 = 禁用 |
| `stt_local_cmd` | string | `python3 ~/.hotplex/scripts/stt_server.py` | `HOTPLEX_MESSAGING_STT_LOCAL_CMD` | 本地 STT 命令模板。含 `{file}` → 每次请求 fork 临时进程；不含 → 持久子进程模式（stdin/stdout JSON 协议） |
| `stt_local_idle_ttl` | duration | `1h` | `HOTPLEX_MESSAGING_STT_LOCAL_IDLE_TTL` | 持久子进程自动关闭的空闲 TTL。0 = 禁用自动关闭 |

#### 3.11.3 TTS（文字转语音）共享配置

> **注意**：以下 `tts_*` 字段在配置结构体中通过 mapstructure `,squash` 标签嵌入到平台级配置（`MessagingPlatformConfig`）。这意味着在 YAML 中这些字段直接位于平台键下（如 `messaging.slack.tts_voice`），而非嵌套在 `tts` 子对象中。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `tts_enabled` | bool | `true` | `HOTPLEX_MESSAGING_TTS_ENABLED` | 是否启用语音回复（仅语音触发的 Turn） |
| `tts_provider` | string | `edge+moss` | `HOTPLEX_MESSAGING_TTS_PROVIDER` | TTS 提供者：`edge`（Edge TTS，免费）、`moss`（MOSS-TTS-Nano 本地 CPU）、`edge+moss`（Edge 主 + MOSS 备）、空字符串 = 禁用 |
| `tts_voice` | string | `zh-CN-XiaoxiaoNeural` | `HOTPLEX_MESSAGING_TTS_VOICE` | Edge TTS 语音名称 |
| `tts_max_chars` | int | `150` | `HOTPLEX_MESSAGING_TTS_MAX_CHARS` | TTS 合成前的 LLM 摘要最大长度 |
| `tts_moss_model_dir` | string | `~/.hotplex/models/moss-tts-nano` | `HOTPLEX_MESSAGING_TTS_MOSS_MODEL_DIR` | MOSS-TTS-Nano ONNX 模型目录 |
| `tts_moss_voice` | string | `Xiaoyu` | `HOTPLEX_MESSAGING_TTS_MOSS_VOICE` | MOSS 内置语音预设名称 |
| `tts_moss_port` | int | `18083` | `HOTPLEX_MESSAGING_TTS_MOSS_PORT` | MOSS sidecar 本地端口 |
| `tts_moss_idle_timeout` | duration | `30m` | `HOTPLEX_MESSAGING_TTS_MOSS_IDLE_TIMEOUT` | MOSS sidecar 最后使用后的自动关闭超时 |
| `tts_moss_cpu_threads` | int | `0` | `HOTPLEX_MESSAGING_TTS_MOSS_CPU_THREADS` | ONNX Runtime intra-op 线程数。0 = 自动检测（物理核心数） |

#### 3.11.4 平台通用配置

每个消息平台（Slack / Feishu / Yuanxin）共享以下配置结构。`MessagingPlatformConfig` 字段由共享默认值传播填充，平台级显式设置优先。

| 字段 | 类型 | 默认值 | 环境变量前缀 | 说明 |
|------|------|--------|-------------|------|
| `enabled` | bool | `false` | `HOTPLEX_MESSAGING_{PLATFORM}_ENABLED` | 是否启用该平台 |
| `worker_type` | string | 继承 messaging 级 | `HOTPLEX_MESSAGING_{PLATFORM}_WORKER_TYPE` | 平台级 Worker 类型覆盖 |
| `work_dir` | string | `""` | `HOTPLEX_MESSAGING_{PLATFORM}_WORK_DIR` | 平台级工作目录覆盖 |
| `dm_policy` | string | `allowlist` | `HOTPLEX_MESSAGING_{PLATFORM}_DM_POLICY` | 私聊消息策略：`allowlist` = 仅允许白名单用户 |
| `group_policy` | string | `allowlist` | `HOTPLEX_MESSAGING_{PLATFORM}_GROUP_POLICY` | 群聊消息策略：`allowlist` = 仅允许白名单群组 |
| `require_mention` | bool | `true` | `HOTPLEX_MESSAGING_{PLATFORM}_REQUIRE_MENTION` | 群聊中是否需要 @机器人 才响应 |
| `inject_exclude` | []string | `[]` | — | 平台级默认排除列表。覆盖 `agent_config.inject_exclude`，被 Bot 级覆盖 |
| `allow_from` | []string | `[]` | `HOTPLEX_MESSAGING_{PLATFORM}_ALLOW_FROM` | 全局白名单用户/群组（逗号分隔） |
| `allow_dm_from` | []string | `[]` | `HOTPLEX_MESSAGING_{PLATFORM}_ALLOW_DM_FROM` | 私聊白名单用户（逗号分隔） |
| `allow_group_from` | []string | `[]` | `HOTPLEX_MESSAGING_{PLATFORM}_ALLOW_GROUP_FROM` | 群聊白名单群组（逗号分隔） |

平台还继承 messaging 级的 STT 和 TTS 共享配置，可通过平台级环境变量覆盖（如 `HOTPLEX_MESSAGING_SLACK_TTS_PROVIDER`）。

#### 3.11.5 Slack 专有配置

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `bot_token` | string | `""` | `HOTPLEX_MESSAGING_SLACK_BOT_TOKEN` | Slack Bot Token（`xoxb-...`）（单 bot 模式） |
| `app_token` | string | `""` | `HOTPLEX_MESSAGING_SLACK_APP_TOKEN` | Slack App Token（`xapp-...`），Socket Mode 所需（单 bot 模式） |
| `socket_mode` | bool | `true` | — | 是否使用 Socket Mode（推荐） |
| `assistant_api_enabled` | *bool | `nil` | — | 是否启用 Slack Assistant API。nil = 未设置 |
| `display_name` | string | `""` | — | Assistant 状态和消息中显示的名称（用于品牌化） |
| `icon_emoji` | string | `""` | — | Assistant 状态和消息中显示的 emoji 图标（如 `:robot_face:`） |
| `reconnect_base_delay` | duration | — | — | 断线重连基础延迟 |
| `reconnect_max_delay` | duration | — | — | 断线重连最大延迟 |
| `bots` | []SlackBotConfig | `[]` | — | 多 bot 配置（见 §3.11.8） |

#### 3.11.6 Feishu 专有配置

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `app_id` | string | `""` | `HOTPLEX_MESSAGING_FEISHU_APP_ID` | 飞书 App ID（`cli_xxxx`）（单 bot 模式） |
| `app_secret` | string | `""` | `HOTPLEX_MESSAGING_FEISHU_APP_SECRET` | 飞书 App Secret（单 bot 模式） |
| `gateway_restart_allow_from` | []string | `[]` | `HOTPLEX_MESSAGING_FEISHU_GATEWAY_RESTART_ALLOW_FROM` | Gateway 运维操作者 OpenID 白名单：授权 `/gateway restart`，并接收受控停止及启动就绪私聊；默认无人可重启且不发送运维私聊 |
| `bots` | []FeishuBotConfig | `[]` | — | 多 bot 配置（见 §3.11.8） |

#### 3.11.7 Yuanxin 专有配置

Yuanxin 是基于 Apache Pulsar 的企业消息平台适配器。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `tenant` | string | `""` | `HOTPLEX_MESSAGING_YUANXIN_TENANT` | Pulsar 租户 |
| `namespace` | string | `""` | `HOTPLEX_MESSAGING_YUANXIN_NAMESPACE` | Pulsar 命名空间 |
| `pulsar_url` | string | `""` | `HOTPLEX_MESSAGING_YUANXIN_PULSAR_URL` | Pulsar Broker URL |
| `app_id` | string | `""` | `HOTPLEX_MESSAGING_YUANXIN_APP_ID` | 应用 ID |
| `producer_topic` | string | `""` | `HOTPLEX_MESSAGING_YUANXIN_PRODUCER_TOPIC` | 生产者 Topic |

#### 3.11.8 多 Bot 配置（Multi-Bot）

每个平台支持多个独立 bot 实例，各自拥有独立凭证、STT/TTS 配置。

**SlackBotConfig 字段**：

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | Bot 名称（同一平台内唯一，必填） |
| `bot_token` | string | Slack Bot Token（`xoxb-...`） |
| `app_token` | string | Slack App Token（`xapp-...`） |
| `display_name` | string | 覆盖顶层 Assistant 显示名称（bot 级品牌化） |
| `icon_emoji` | string | 覆盖顶层 Assistant emoji 图标（bot 级品牌化） |
| `worker_type` | string | 覆盖 Worker 类型 |
| `sandbox` | string | 覆盖 CodexCLI sandbox 模式（bot 级） |
| `acp_command` | string | 覆盖 ACP Agent 启动命令（bot 级，仅 `worker_type: acp` 时生效） |
| `inject_exclude` | []string | 覆盖 agent config 排除列表（bot 级，覆盖平台级和全局级） |
| `stt_*` | — | 覆盖 STT 配置（继承平台级 → messaging 级） |
| `tts_*` | — | 覆盖 TTS 配置（继承平台级 → messaging 级） |

**FeishuBotConfig 字段**：

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | Bot 名称（同一平台内唯一，必填） |
| `app_id` | string | 飞书 App ID |
| `app_secret` | string | 飞书 App Secret |
| `worker_type` | string | 覆盖 Worker 类型 |
| `sandbox` | string | 覆盖 CodexCLI sandbox 模式（bot 级） |
| `acp_command` | string | 覆盖 ACP Agent 启动命令（bot 级，仅 `worker_type: acp` 时生效） |
| `gateway_restart_allow_from` | []string | 继承平台级 | — | Gateway 运维操作者 OpenID；显式 `[]` 禁止该 Bot 重启并停用其生命周期私聊 |
| `inject_exclude` | []string | 覆盖 agent config 排除列表（bot 级，覆盖平台级和全局级） |
| `stt_*` | — | 覆盖 STT 配置 |
| `tts_*` | — | 覆盖 TTS 配置 |

**向后兼容**：`normalizeSlackBots()`/`normalizeFeishuBots()` 自动将单 bot 顶层凭证归一化为一个以平台名命名的 Bot（分别为 `slack` / `feishu`），并保留单 Bot 模式语义。`bots[]` 非空时忽略顶层凭证。

**限制**：每平台最多 10 个 bot。Bot 凭证、Worker 类型等结构变更需重启；`gateway_restart_allow_from` 平台级与 bot 级白名单支持热更新。受控关闭（例如服务或开发环境停止）会在退出前私聊每个有效 OpenID；Gateway 启动并监听 HTTP 后会再次私聊就绪消息，即使没有可恢复的聊天 Session。由飞书 `/gateway restart` 发起时，原请求会话使用重启回执链路，不再额外向白名单 OpenID 发送同一轮私聊，避免重复通知。

**启动校验**：`hotplex doctor` 的 `messaging.multi_bot_config` checker 检测重复 name、缺失凭证、超限。

**Bot 状态 API**：

```
GET /admin/bots          → 列出所有活跃 bot
GET /admin/bots/{name}   → 单个 bot 详情
```

#### 3.11.9 配置传播机制

共享配置通过 `propagateMessagingDefaults()` 从 `messaging` 级传播到各平台，再传播到每个 bot：

```
messaging.worker_type  ──→  slack.worker_type (如未设置)  ──→  slack.bots[i].worker_type (如未设置)
messaging.stt_*        ──→  slack.stt_* (如未设置)        ──→  slack.bots[i].stt_* (如未设置)
messaging.tts_*        ──→  slack.tts_* (如未设置)        ──→  slack.bots[i].tts_* (如未设置)
```

`inject_exclude` 使用独立的三级 fallback（通过 `ResolveInjectExclude`）：

```
agent_config.inject_exclude  ──→  messaging.slack.inject_exclude  ──→  slack.bots[i].inject_exclude
```

**优先级**：`bot-level > platform-level (YAML/env) > global-level > Default()`

仅 zero-value 字段被填充，已有值不会被覆盖。

> **worker_type 单一解析入口（#847）**：`config.ResolveWorkerType(platform, botName)` 把上述 `worker_type` 5 级 fallback（per-bot → platform YAML/env → messaging 共享默认 → 编译默认 `claude_code`）收敛为一个函数，供 AgentSpec 归一化层（`internal/agentspec`）消费。**仅适用于 messaging 平台会话**（slack/feishu/yuanxin）；WebChat 会话的 worker_type 是请求驱动的（`body/query → workspace.WorkerPreference → default`，见 Gateway API），不走此 fallback。

---

### 3.12 log — 日志

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `level` | string | `info` | `HOTPLEX_LOG_LEVEL` | 日志级别：`debug`、`info`、`warn`、`error` |
| `format` | string | `json` | `HOTPLEX_LOG_FORMAT` | 日志格式：`json` 或 `text` |

---

### 3.13 webchat — Web Chat UI

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `addr` | string | `""` | — | Webchat 地址（仅用于 banner 展示信息） |
| `enabled` | bool | `true` | — | 是否从 Gateway 提供嵌入式 Webchat SPA（go:embed） |

---

### 3.14 oauth — WebChat 企业 SSO（OIDC）

WebChat 多租户的企业单点登录（SSO）配置（spec ④）。基于标准 OIDC Authorization Code flow + PKCE，一套实现覆盖全部主流 IdP（Keycloak / Okta / Microsoft Entra ID / Google Workspace，以及国内的派拉 / 玉符 / 阿里云 IDaaS / 腾讯云 IDaaS / 宁盾 / Authing / 竹云 / 华为 OneAccess / TOPIAM）。

> 该配置块为 **WebChat 专用**，独立于 Slack/Feishu 的 bot 配置（后者属于 Message Channel 轨道）。仅在启用 WebChat 多租户登录时生效。与内建账号登录（spec ①）并存，均为一等公民。

顶层结构：

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `external_url` | string | `""` | 构造 OAuth callback URL 的基础地址。为空时从请求 Host 头推导（同源部署）；反向代理后公共 URL 与内部不同时必须显式设置 |
| `providers` | []OAuthProvider | `nil` | OIDC 身份提供者列表。多个 provider 在登录页渲染为多个 SSO 按钮（spec ⑥） |

**provider 字段**：

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `name` | string | — | provider 唯一标识。出现在 URL 路径（`/api/auth/oauth/{name}/login`）和 `user_identities.provider`。必须匹配 `[a-z0-9-]+`（URL 安全 + 防路径穿越） |
| `display_name` | string | — | 登录页展示的人类可读标签（spec ⑥） |
| `issuer` | string | — | OIDC issuer URL。discovery 端点自动解析为 `{issuer}/.well-known/openid-configuration` |
| `client_id` | string | — | 在 IdP 注册的 OAuth2 client id |
| `client_secret` | string | — | OAuth2 client secret。支持 `${ENV_VAR}` / `${ENV_VAR:-default}` 展开，生产环境建议通过环境变量或 secret manager 注入 |
| `scopes` | []string | `["openid","profile"]` | 请求的 OIDC scopes |
| `username_claim` | string | `""` | 可选 claim 名覆盖；为空时用 OIDC 标准 claim |
| `display_name_claim` | string | `""` | 同上，显示名 claim |
| `email_claim` | string | `""` | 同上，email claim |

**安全设计**：强制 PKCE（S256），即使 authorization code 被截获也无法 exchange；state cookie HMAC 签名（5min TTL）绑定 provider 防 CSRF；ID Token 经 JWKS 签名 + iss/aud/exp 校验；`(provider, subject)` 首次登录自动建号，**不按 email 自动合并账号**。

**配置示例**：

```yaml
oauth:
  external_url: "https://hotplex.example.com"
  providers:
    - name: "keycloak"
      display_name: "企业 SSO"
      issuer: "https://sso.example.com/realms/main"
      client_id: "${OAUTH_KEYCLOAK_CLIENT_ID}"
      client_secret: "${OAUTH_KEYCLOAK_CLIENT_SECRET}"
      scopes: ["openid", "profile", "email"]
```

企业 IAM 对接步骤、Keycloak / Okta / Microsoft Entra ID / Google Workspace
示例与错误排查见 [企业 OAuth/SSO IAM 对接指南](../guides/enterprise/oauth-sso-iam.md)。

---

### 3.15 inherits — 配置继承

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `inherits` | string | `""` | — | 父配置文件路径。支持相对路径（相对于当前文件目录）。内建环检测防止循环继承 |

**示例**（`config-dev.yaml`）：

```yaml
inherits: config.yaml

admin:
  rate_limit_enabled: false

log:
  level: debug
  format: text
```

继承解析过程：

1. 加载 `Default()` 作为基底
2. 递归加载父配置文件（含环检测）
3. 子配置文件值覆盖父配置值
4. 环境变量覆盖所有文件配置

---

### 3.16 events 和 audit — 事件与审计留存

运行期 event store 与合规 audit store 是职责不同的两类数据副本：前者支持会话恢复和协议重放，后者保存可追溯的用户活动事实。生命周期 v2 将新事实写入独立、无正文的 `user_activity_v2` 哈希链，由 `lifecycle.audit.facts_retention` 控制；升级前的 `user_activity` legacy 链继续按 `audit.retention` 保留，不会被 v2 GC 提前清理。Admin 活动查询会合并两条链，并返回 `chain_epoch` 标识来源。延长任一审计链留存**不会**延长 event store 或 turn 的期限。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `events.retention` | duration | `720h` (30天) | `HOTPLEX_EVENTS_RETENTION` | Event store 和 turns 的运行期留存窗口。到期后由事件 GC 清理。启动时生效，修改后需重启 |
| `audit.retention` | duration | `26280h` (3年) | `HOTPLEX_AUDIT_RETENTION` | legacy `user_activity` 链及升级前可能包含正文的旧记录的留存窗口；与 lifecycle-v2 新链独立 |
| `audit.full_content_retention` | duration | `2160h` (90天) | `HOTPLEX_AUDIT_FULL_CONTENT_RETENTION` | 审计原文的兼容配置字段；不再影响 event store 或 turns 留存 |

> 网关 INFO 日志不会记录消息正文、prompt 或 `Envelope.Event.Data`。为支持关联排障，日志仅包含事件类型、session、seq、`data_size` 与 `data_sha256`（SHA-256 的短指纹）。

### 3.17 execution.queue — 持久输入队列

当一个 session 正在执行时到达的补充输入，可以进入一个**有界持久队列**，等待当前轮次结束后按 FIFO 派发。这个队列兑现的是网关对客户端的承诺："这条输入已经存下来了，稍后会被派发"，而不是"已进入内存暂存，重启可能丢失"。

| 字段 | 类型 | 默认值 | 环境变量 | 说明 |
|------|------|--------|----------|------|
| `execution.queue.enabled` | bool | `false` | `HOTPLEX_EXECUTION_QUEUE_ENABLED` | 队列总开关。**默认关闭**，见下方说明 |
| `execution.queue.per_session` | int | `20` | `HOTPLEX_EXECUTION_QUEUE_PER_SESSION` | 单个 session 允许的未派发输入条数上限 |
| `execution.queue.global` | int | `1000` | `HOTPLEX_EXECUTION_QUEUE_GLOBAL` | 整个实例允许的未派发输入条数上限 |
| `execution.queue.max_payload_bytes` | int | `65536` (64 KiB) | `HOTPLEX_EXECUTION_QUEUE_MAX_PAYLOAD_BYTES` | 单条输入的正文上限；原生命令的 invocation 参数一并计入 |
| `execution.queue.interactive_ttl` | duration | `15m` | `HOTPLEX_EXECUTION_QUEUE_INTERACTIVE_TTL` | 忙碌 Session 接收的交互输入最长等待时间；超时后按 `QUEUE_EXPIRED` 结算 |
| `execution.queue.ttl` | duration | `24h` | `HOTPLEX_EXECUTION_QUEUE_TTL` | 未提供交互专用期限时使用的通用队列期限；当前 Gateway 忙碌输入路径优先采用 `interactive_ttl` |
| `execution.queue.sweep_interval` | duration | `1m` | `HOTPLEX_EXECUTION_QUEUE_SWEEP_INTERVAL` | 过期扫描间隔；设为 `0` 关闭后台扫描 |
| `execution.queue.sweep_batch` | int | `100` | `HOTPLEX_EXECUTION_QUEUE_SWEEP_BATCH` | 单次扫描处理的最大条数，避免大积压长时间占用写锁 |

> **`enabled` 默认为 `false`**：只有派发、取消与过期三条路径都接通之后，接收才成为一件有意义的事。在派发未接通的情况下开启队列，会静默堆积无人发送的输入——这比直接拒绝输入更糟。容量已满、载荷过大等情况下，网关不会挤掉已被ACK 的输入，而是回落到内存暂存并如实标注为 volatile。

队列只覆盖**尚未派发**的输入。一旦某条输入跨过派发边界（进入 pending 并取得 owner lease），它的失败与重试语义与其他 execution 完全一致：响应丢失即 `unknown` + fence，不会自动重投。`/stop` 只停止当前轮次，不影响队列；要撤销一条已入队输入，使用 Admin API `POST /admin/executions/{id}/queue-cancel`，已派发的输入返回 `409`。

---

## 4. 热重载

HotPlex 通过 `fsnotify` 监听配置文件变更，支持运行时热更新。配置字段分为**动态字段**和**静态字段**两类。

### 4.1 动态字段（Hot-Reloadable）

以下字段修改后**立即生效**，无需重启：

| 字段路径 | 说明 |
|----------|------|
| `log.level` | 日志级别 |
| `session.gc_scan_interval` | GC 扫描间隔 |
| `pool.max_size` | 资源池最大大小 |
| `pool.max_idle_per_user` | 每用户最大空闲 Session |
| `security.api_keys` | API Key 列表 |
| `security.allowed_origins` | CORS Origin 列表 |
| `security.security_contact` | security.txt 联系方式 |
| `worker.max_lifetime` | Worker 最大存活时间 |
| `worker.idle_timeout` | Worker 空闲超时 |
| `worker.execution_timeout` | 执行超时 |
| `worker.auto_retry` | 自动重试配置（整体） |
| `admin.requests_per_sec` | Admin API 速率限制 |
| `admin.burst` | Admin API 突发量 |
| `admin.tokens` | Admin Token 列表 |
| `admin.allowed_cidrs` | IP 白名单 CIDR 列表 |
| `oauth.external_url` | OAuth/OIDC callback 公网基础地址 |
| `oauth.providers` | OAuth/OIDC provider 列表、client 配置和 claim 映射 |

### 4.2 静态字段（需重启）

以下字段修改后**仅在重启后生效**，热重载时会记录变更日志但不应用：

| 字段路径 | 说明 |
|----------|------|
| `gateway.addr` | 监听地址 |
| `gateway.broadcast_queue_size` | 广播队列大小 |
| `gateway.read_buffer_size` | 读缓冲区大小 |
| `gateway.write_buffer_size` | 写缓冲区大小 |
| `log.format` | 日志格式 |
| `security.tls_enabled` | TLS 开关 |
| `security.tls_cert_file` | TLS 证书路径 |
| `security.tls_key_file` | TLS 私钥路径 |
| `db.path` | 数据库路径 |
| `db.wal_mode` | WAL 模式 |

### 4.3 热重载行为

- **防抖**：500ms 去抖，避免编辑器保存时的重复触发
- **校验**：重载前执行 `Validate()`，校验失败保留旧配置
- **审计日志**：所有变更记录审计日志（敏感字段脱敏为 `[REDACTED]`）
- **历史快照**：保留最近 64 个配置快照用于回滚
- **回滚**：`Rollback(version)` 恢复内存中的历史快照（不修改磁盘文件）
- **并发控制**：回调信号量限制为 4 个并发，防止快速变更时 goroutine 膨胀

---

## 5. 环境变量速查

### 5.1 命名规则

环境变量统一使用 `HOTPLEX_` 前缀，层级用 `_` 分隔：

```
HOTPLEX_{SECTION}_{FIELD}
```

列表类型使用编号后缀（1-based）：

```
HOTPLEX_ADMIN_TOKEN_1, HOTPLEX_ADMIN_TOKEN_2, ...
HOTPLEX_SECURITY_API_KEY_1, HOTPLEX_SECURITY_API_KEY_2, ...
```

### 5.2 必需变量

| 变量 | 说明 | 示例 |
|------|------|------|
| `HOTPLEX_SECURITY_API_KEY_1` | API Key（至少配置一个） | `openssl rand -base64 32 \| tr -d '/+=' \| head -c 43` |
| `HOTPLEX_ADMIN_TOKEN_1` | Admin API 认证 token | `openssl rand -base64 32 \| tr -d '/+=' \| head -c 43` |

### 5.3 完整环境变量列表

#### 核心覆盖

| 变量 | 对应配置 | 默认值 |
|------|----------|--------|
| `HOTPLEX_HOME` | workspace 根目录（含默认配置文件 `$HOTPLEX_HOME/config.yaml`、数据、日志、PID 等全部状态） | `~/.hotplex` |
| `HOTPLEX_LOG_LEVEL` | `log.level` | `info` |
| `HOTPLEX_LOG_FORMAT` | `log.format` | `json` |
| `HOTPLEX_DB_PATH` | `db.path` | `~/.hotplex/data/hotplex.db` |
| `HOTPLEX_DB_WAL_MODE` | `db.wal_mode` | `true` |
| `HOTPLEX_GATEWAY_ADDR` | `gateway.addr` | `localhost:8888` |
| `HOTPLEX_ADMIN_ENABLED` | `admin.enabled` | `true` |
| `HOTPLEX_ADMIN_ADDR` | `admin.addr` | `localhost:9999` |

#### 资源限制

| 变量 | 对应配置 | 默认值 |
|------|----------|--------|
| `HOTPLEX_SESSION_MAX_CONCURRENT` | `session.max_concurrent` | `1000` |
| `HOTPLEX_SESSION_RETENTION_PERIOD` | `session.retention_period` | `168h` |
| `HOTPLEX_LIFECYCLE_POLICY` | `lifecycle.policy` | `v2` |
| `HOTPLEX_LIFECYCLE_CONVERSATION_ARCHIVE_AFTER` | `lifecycle.conversation.archive_after` | `168h` |
| `HOTPLEX_LIFECYCLE_CONVERSATION_RETENTION_AFTER_LAST_INPUT` | `lifecycle.conversation.retention_after_last_input` | `4320h` |
| `HOTPLEX_LIFECYCLE_CONTENT_RETENTION` | `lifecycle.content.retention` | `4320h` |
| `HOTPLEX_LIFECYCLE_EFFECT_PAYLOAD_RETENTION_AFTER_SETTLEMENT` | `lifecycle.effect_payload.retention_after_settlement` | `168h` |
| `HOTPLEX_LIFECYCLE_FACTS_RETENTION_AFTER_SETTLEMENT` | `lifecycle.facts.retention_after_settlement` | `2160h` |
| `HOTPLEX_LIFECYCLE_AUDIT_CAPTURE_CONTENT` | `lifecycle.audit.capture_content` | `false` |
| `HOTPLEX_LIFECYCLE_AUDIT_FACTS_RETENTION` | `lifecycle.audit.facts_retention` | `4320h` |
| `HOTPLEX_LIFECYCLE_AUDIT_CONTENT_RETENTION` | `lifecycle.audit.content_retention` | `4320h` |
| `HOTPLEX_LIFECYCLE_MEDIA_RETENTION` | `lifecycle.media.retention` | `24h` |
| `HOTPLEX_LIFECYCLE_TRACE_RETENTION` | `lifecycle.trace.retention` | `48h` |
| `HOTPLEX_LIFECYCLE_GC_BATCH_SIZE` | `lifecycle.gc.batch_size` | `100` |
| `HOTPLEX_LIFECYCLE_GC_INTERVAL` | `lifecycle.gc.interval` | `1m` |
| `HOTPLEX_LIFECYCLE_GC_MAX_LAG` | `lifecycle.gc.max_lag` | `1h` |
| `HOTPLEX_POOL_MAX_SIZE` | `pool.max_size` | `100` |
| `HOTPLEX_POOL_MAX_IDLE_PER_USER` | `pool.max_idle_per_user` | `5` |
| `HOTPLEX_POOL_MAX_MEMORY_PER_USER` | `pool.max_memory_per_user` | `3221225472` |

#### Worker

| 变量 | 对应配置 | 默认值 |
|------|----------|--------|
| `HOTPLEX_WORKER_MAX_LIFETIME` | `worker.max_lifetime` | `24h` |
| `HOTPLEX_WORKER_IDLE_TIMEOUT` | `worker.idle_timeout` | `60m` |
| `HOTPLEX_WORKER_EXECUTION_TIMEOUT` | `worker.execution_timeout` | `30m` |
| `HOTPLEX_WORKER_TURN_TIMEOUT` | `worker.turn_timeout` | `30m` |
| `HOTPLEX_WORKER_DEFAULT_WORK_DIR` | `worker.default_work_dir` | `~/.hotplex/workspace` |
| `HOTPLEX_WORKER_CLAUDE_CODE_COMMAND` | `worker.claude_code.command` | `claude` |
| `HOTPLEX_WORKER_OPENCODE_SERVER_COMMAND` | `worker.opencode_server.command` | `opencode` |
| `HOTPLEX_WORKER_OPENCODE_SERVER_IDLE_DRAIN_PERIOD` | `worker.opencode_server.idle_drain_period` | `30m` |
| `HOTPLEX_WORKER_OPENCODE_SERVER_READY_TIMEOUT` | `worker.opencode_server.ready_timeout` | `10s` |
| `HOTPLEX_WORKER_OPENCODE_SERVER_READY_POLL_INTERVAL` | `worker.opencode_server.ready_poll_interval` | `200ms` |
| `HOTPLEX_WORKER_OPENCODE_SERVER_HTTP_TIMEOUT` | `worker.opencode_server.http_timeout` | `30s` |
| `HOTPLEX_WORKER_OPENCODE_SERVER_CONTEXT_WINDOW` | `worker.opencode_server.context_window` | `200000` |
| `HOTPLEX_WORKER_AUTO_RETRY_ENABLED` | `worker.auto_retry.enabled` | `true` |
| `HOTPLEX_WORKER_AUTO_RETRY_MAX_RETRIES` | `worker.auto_retry.max_retries` | `9` |
| `HOTPLEX_WORKER_CODEX_CLI_COMMAND` | `worker.codex_cli.command` | `codex` |
| `HOTPLEX_WORKER_CODEX_CLI_MODEL` | `worker.codex_cli.model` | `""` |
| `HOTPLEX_WORKER_CODEX_CLI_SANDBOX` | `worker.codex_cli.sandbox` | `danger-full-access` |
| `HOTPLEX_WORKER_CODEX_CLI_APPROVAL_MODE` | `worker.codex_cli.approval_mode` | `never` |
| `HOTPLEX_WORKER_ACP_COMMAND` | `worker.acp.command` | `hermes acp` |
| `HOTPLEX_WORKER_ACP_AUTO_APPROVE` | `worker.acp.auto_approve` | `true` |
| `HOTPLEX_WORKER_GH_TOKEN` | 通过 `worker.environment` 注入 | — |
| `HOTPLEX_WORKER_GITHUB_TOKEN` | 通过 `worker.environment` 注入 | — |

#### Webhook

| 变量 | 对应配置 | 默认值 |
|------|----------|--------|
| `HOTPLEX_WEBHOOK_ENABLED` | `webhook.enabled` | `false` |
| `HOTPLEX_WEBHOOK_SECRET` | `webhook.secret` | `""` |
| `HOTPLEX_WEBHOOK_PATH` | `webhook.path` | `/api/webhook/github` |
| `HOTPLEX_WEBHOOK_MAX_BODY_SIZE` | `webhook.max_body_size` | `1048576` |
| `HOTPLEX_WEBHOOK_TARGET_JOB_NAME` | `webhook.target_job_name` | `pr-review-hotplex` |

#### Security

| 变量 | 对应配置 | 说明 |
|------|----------|------|
| `HOTPLEX_ADMIN_TOKEN_1..N` | `admin.tokens` | 编号后缀，支持轮换 |
| `HOTPLEX_SECURITY_API_KEY_1..N` | `security.api_keys` | 编号后缀，支持轮换 |
| `HOTPLEX_SECURITY_API_KEY_HEADER` | `security.api_key_header` | 默认 `X-API-Key` |
| `HOTPLEX_SECURITY_CSP` | `security.csp` | Content-Security-Policy 头覆盖（空 = 内建默认） |
| `HOTPLEX_SECURITY_ALLOWED_ORIGINS` | `security.allowed_origins` | 逗号分隔的 CORS Origin 列表 |
| `HOTPLEX_SECURITY_SECURITY_CONTACT` | `security.security_contact` | 安全联系 URI，启用 `/.well-known/security.txt` |

#### Agent Config

| 变量 | 对应配置 | 默认值 |
|------|----------|--------|
| `HOTPLEX_AGENT_CONFIG_ENABLED` | `agent_config.enabled` | `true` |
| `HOTPLEX_AGENT_CONFIG_DIR` | `agent_config.config_dir` | `~/.hotplex/agent-configs` |
| `HOTPLEX_SKILLS_CACHE_TTL` | `skills.cache_ttl` | `5m` |

#### Messaging 全局

| 变量 | 对应配置 | 默认值 |
|------|----------|--------|
| `HOTPLEX_MESSAGING_TURN_SUMMARY_ENABLED` | `messaging.turn_summary_enabled` | `true` |
| `HOTPLEX_MESSAGING_WORKER_TYPE` | `messaging.worker_type` | `claude_code` |
| `HOTPLEX_MESSAGING_TTS_ENABLED` | `messaging.tts_enabled` | `true` |
| `HOTPLEX_MESSAGING_STT_PROVIDER` | `messaging.stt_provider` | `local` |
| `HOTPLEX_MESSAGING_STT_LOCAL_CMD` | `messaging.stt_local_cmd` | `python3 ~/.hotplex/scripts/stt_server.py` |
| `HOTPLEX_MESSAGING_STT_LOCAL_IDLE_TTL` | `messaging.stt_local_idle_ttl` | `1h` |
| `HOTPLEX_MESSAGING_TTS_PROVIDER` | `messaging.tts_provider` | `edge+moss` |
| `HOTPLEX_MESSAGING_TTS_VOICE` | `messaging.tts_voice` | `zh-CN-XiaoxiaoNeural` |
| `HOTPLEX_MESSAGING_TTS_MAX_CHARS` | `messaging.tts_max_chars` | `150` |
| `HOTPLEX_MESSAGING_TTS_MOSS_MODEL_DIR` | `messaging.tts_moss_model_dir` | `~/.hotplex/models/moss-tts-nano` |
| `HOTPLEX_MESSAGING_TTS_MOSS_VOICE` | `messaging.tts_moss_voice` | `Xiaoyu` |
| `HOTPLEX_MESSAGING_TTS_MOSS_PORT` | `messaging.tts_moss_port` | `18083` |
| `HOTPLEX_MESSAGING_TTS_MOSS_IDLE_TIMEOUT` | `messaging.tts_moss_idle_timeout` | `30m` |
| `HOTPLEX_MESSAGING_TTS_MOSS_CPU_THREADS` | `messaging.tts_moss_cpu_threads` | `0` |

#### Messaging — Slack

| 变量 | 对应配置 |
|------|----------|
| `HOTPLEX_MESSAGING_SLACK_ENABLED` | `messaging.slack.enabled` |
| `HOTPLEX_MESSAGING_SLACK_BOT_TOKEN` | `messaging.slack.bot_token` |
| `HOTPLEX_MESSAGING_SLACK_APP_TOKEN` | `messaging.slack.app_token` |
| `HOTPLEX_MESSAGING_SLACK_WORKER_TYPE` | `messaging.slack.worker_type` |
| `HOTPLEX_MESSAGING_SLACK_WORK_DIR` | `messaging.slack.work_dir` |
| `HOTPLEX_MESSAGING_SLACK_REQUIRE_MENTION` | `messaging.slack.require_mention` |
| `HOTPLEX_MESSAGING_SLACK_DM_POLICY` | `messaging.slack.dm_policy` |
| `HOTPLEX_MESSAGING_SLACK_GROUP_POLICY` | `messaging.slack.group_policy` |
| `HOTPLEX_MESSAGING_SLACK_ALLOW_FROM` | `messaging.slack.allow_from` |
| `HOTPLEX_MESSAGING_SLACK_ALLOW_DM_FROM` | `messaging.slack.allow_dm_from` |
| `HOTPLEX_MESSAGING_SLACK_ALLOW_GROUP_FROM` | `messaging.slack.allow_group_from` |
| `HOTPLEX_MESSAGING_SLACK_STT_PROVIDER` | `messaging.slack.stt_provider` |
| `HOTPLEX_MESSAGING_SLACK_STT_LOCAL_CMD` | `messaging.slack.stt_local_cmd` |
| `HOTPLEX_MESSAGING_SLACK_STT_LOCAL_IDLE_TTL` | `messaging.slack.stt_local_idle_ttl` |
| `HOTPLEX_MESSAGING_SLACK_TTS_ENABLED` | `messaging.slack.tts_enabled` |
| `HOTPLEX_MESSAGING_SLACK_TTS_PROVIDER` | `messaging.slack.tts_provider` |
| `HOTPLEX_MESSAGING_SLACK_TTS_VOICE` | `messaging.slack.tts_voice` |
| `HOTPLEX_MESSAGING_SLACK_TTS_MAX_CHARS` | `messaging.slack.tts_max_chars` |
| `HOTPLEX_MESSAGING_SLACK_TTS_MOSS_MODEL_DIR` | `messaging.slack.tts_moss_model_dir` |
| `HOTPLEX_MESSAGING_SLACK_TTS_MOSS_VOICE` | `messaging.slack.tts_moss_voice` |
| `HOTPLEX_MESSAGING_SLACK_TTS_MOSS_PORT` | `messaging.slack.tts_moss_port` |
| `HOTPLEX_MESSAGING_SLACK_TTS_MOSS_IDLE_TIMEOUT` | `messaging.slack.tts_moss_idle_timeout` |
| `HOTPLEX_MESSAGING_SLACK_TTS_MOSS_CPU_THREADS` | `messaging.slack.tts_moss_cpu_threads` |

#### Messaging — Feishu

| 变量 | 对应配置 |
|------|----------|
| `HOTPLEX_MESSAGING_FEISHU_ENABLED` | `messaging.feishu.enabled` |
| `HOTPLEX_MESSAGING_FEISHU_APP_ID` | `messaging.feishu.app_id` |
| `HOTPLEX_MESSAGING_FEISHU_APP_SECRET` | `messaging.feishu.app_secret` |
| `HOTPLEX_MESSAGING_FEISHU_WORKER_TYPE` | `messaging.feishu.worker_type` |
| `HOTPLEX_MESSAGING_FEISHU_WORK_DIR` | `messaging.feishu.work_dir` |
| `HOTPLEX_MESSAGING_FEISHU_REQUIRE_MENTION` | `messaging.feishu.require_mention` |
| `HOTPLEX_MESSAGING_FEISHU_DM_POLICY` | `messaging.feishu.dm_policy` |
| `HOTPLEX_MESSAGING_FEISHU_GROUP_POLICY` | `messaging.feishu.group_policy` |
| `HOTPLEX_MESSAGING_FEISHU_ALLOW_FROM` | `messaging.feishu.allow_from` |
| `HOTPLEX_MESSAGING_FEISHU_ALLOW_DM_FROM` | `messaging.feishu.allow_dm_from` |
| `HOTPLEX_MESSAGING_FEISHU_ALLOW_GROUP_FROM` | `messaging.feishu.allow_group_from` |
| `HOTPLEX_MESSAGING_FEISHU_GATEWAY_RESTART_ALLOW_FROM` | `messaging.feishu.gateway_restart_allow_from` |
| `HOTPLEX_MESSAGING_FEISHU_STT_PROVIDER` | `messaging.feishu.stt_provider` |
| `HOTPLEX_MESSAGING_FEISHU_STT_LOCAL_CMD` | `messaging.feishu.stt_local_cmd` |
| `HOTPLEX_MESSAGING_FEISHU_STT_LOCAL_IDLE_TTL` | `messaging.feishu.stt_local_idle_ttl` |
| `HOTPLEX_MESSAGING_FEISHU_TTS_ENABLED` | `messaging.feishu.tts_enabled` |
| `HOTPLEX_MESSAGING_FEISHU_TTS_PROVIDER` | `messaging.feishu.tts_provider` |
| `HOTPLEX_MESSAGING_FEISHU_TTS_VOICE` | `messaging.feishu.tts_voice` |
| `HOTPLEX_MESSAGING_FEISHU_TTS_MAX_CHARS` | `messaging.feishu.tts_max_chars` |
| `HOTPLEX_MESSAGING_FEISHU_TTS_MOSS_MODEL_DIR` | `messaging.feishu.tts_moss_model_dir` |
| `HOTPLEX_MESSAGING_FEISHU_TTS_MOSS_VOICE` | `messaging.feishu.tts_moss_voice` |
| `HOTPLEX_MESSAGING_FEISHU_TTS_MOSS_PORT` | `messaging.feishu.tts_moss_port` |
| `HOTPLEX_MESSAGING_FEISHU_TTS_MOSS_IDLE_TIMEOUT` | `messaging.feishu.tts_moss_idle_timeout` |
| `HOTPLEX_MESSAGING_FEISHU_TTS_MOSS_CPU_THREADS` | `messaging.feishu.tts_moss_cpu_threads` |

#### Messaging — Yuanxin

| 变量 | 对应配置 |
|------|----------|
| `HOTPLEX_MESSAGING_YUANXIN_ENABLED` | `messaging.yuanxin.enabled` |
| `HOTPLEX_MESSAGING_YUANXIN_APP_ID` | `messaging.yuanxin.app_id` |
| `HOTPLEX_MESSAGING_YUANXIN_PULSAR_URL` | `messaging.yuanxin.pulsar_url` |
| `HOTPLEX_MESSAGING_YUANXIN_TENANT` | `messaging.yuanxin.tenant` |
| `HOTPLEX_MESSAGING_YUANXIN_NAMESPACE` | `messaging.yuanxin.namespace` |
| `HOTPLEX_MESSAGING_YUANXIN_PRODUCER_TOPIC` | `messaging.yuanxin.producer_topic` |
| `HOTPLEX_MESSAGING_YUANXIN_WORKER_TYPE` | `messaging.yuanxin.worker_type` |
| `HOTPLEX_MESSAGING_YUANXIN_WORK_DIR` | `messaging.yuanxin.work_dir` |
| `HOTPLEX_MESSAGING_YUANXIN_DM_POLICY` | `messaging.yuanxin.dm_policy` |
| `HOTPLEX_MESSAGING_YUANXIN_GROUP_POLICY` | `messaging.yuanxin.group_policy` |
| `HOTPLEX_MESSAGING_YUANXIN_ALLOW_FROM` | `messaging.yuanxin.allow_from` |
| `HOTPLEX_MESSAGING_YUANXIN_ALLOW_DM_FROM` | `messaging.yuanxin.allow_dm_from` |
| `HOTPLEX_MESSAGING_YUANXIN_ALLOW_GROUP_FROM` | `messaging.yuanxin.allow_group_from` |

#### Observability

| 变量 | 说明 |
|------|------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OpenTelemetry Collector 端点 |
| `OTEL_SERVICE_NAME` | 服务名称（默认 `hotplex-gateway`） |
