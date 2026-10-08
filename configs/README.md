# HotPlex Worker Gateway — Configuration Guide

`configs/` 目录包含完整的配置体系。项目遵循 **Convention Over Configuration** 原则——代码内嵌生产就绪的默认值 (`internal/config/config.go:Default()`)，所有 YAML 字段均可选。

---

## 目录结构

```text
configs/
├── config.yaml          # Base — 生产中性默认值，覆盖代码 Default() 的部分字段
├── config-dev.yaml      # Dev  — 继承 base，覆盖开发环境值
├── config-prod.yaml     # Prod — 继承 base，覆盖生产环境值
├── env.example          # 环境变量模板 → cp 到 .env 使用
└── README.md
```

---

## 快速开始

```bash
cp configs/env.example ~/.hotplex/.env   # 填入 HOTPLEX_ADMIN_TOKEN_1
make dev                                # 自动使用 config-dev.yaml
```

---

## 配置优先级（从低到高）

| 优先级 | 来源 | 说明 |
|:------:|:-----|:-----|
| 1 | `Default()` 代码默认值 | `internal/config/config.go` 编译时内嵌 |
| 2 | 父级配置文件 | `inherits` 递归加载，支持多级继承与循环检测 |
| 3 | 当前配置文件 | `-config` 指定的 YAML |
| 4 | 环境变量 `HOTPLEX_*` | Viper AutomaticEnv + `applyMessagingEnv()` 手动映射 |

环境变量映射公式：`HOTPLEX_<SECTION>_<FIELD>`，全大写下划线连接。
例如 `pool.max_size` → `HOTPLEX_POOL_MAX_SIZE`。

编号式环境变量支持令牌轮转：`HOTPLEX_ADMIN_TOKEN_1` ... `HOTPLEX_ADMIN_TOKEN_N`。

---

## 配置继承

子配置通过 `inherits` 指定父文件，只需列出与 base 不同的字段。支持多级继承和循环检测。

```yaml
inherits: config.yaml
log:
  level: "debug"
```

---

### Dev 本机覆盖（config-dev.local.yaml）

`configs/config-dev.local.yaml` 命中 gitignore 规则 `*.local.yaml`，用于本机专属覆盖（如把 dev 栈固定到本地 PostgreSQL），不影响仓库默认行为——文件不存在时一切照旧（SQLite）。

`scripts/dev.sh` 解析顺序：**显式 `$CONFIG` > `config-dev.local.yaml`（存在时）> `config-dev.yaml`**。`./scripts/dev.sh config` 输出最终解析路径。

CLI 需与 dev 网关操作同一数据库时：

```bash
hotplex admin create --config "$(./scripts/dev.sh config)" --username admin
```

---

## 完整字段参考

> **默认值列**为 `config.yaml` 的值（即实际生效的 base 值）。括号内标注代码 `Default()` 的原始值，仅当两者不同时显示。

### gateway — WebSocket 网关

控制 WebSocket 连接的底层传输参数。Hub 为每个连接启动独立的 ReadPump / WritePump goroutine，通过带缓冲的 broadcast channel 路由事件。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `addr` | string | `:8888` | ❌ | 网关监听地址。客户端通过此端口建立 WebSocket 连接（AEP v1 协议）。生产环境建议置于反向代理（Nginx/ALB）之后并启用 TLS。**启动后不可变更**——HTTP Server 在启动时绑定端口 |
| `read_buffer_size` | int | `4096` | ❌ | WebSocket 读缓冲区大小（bytes）。`gorilla/websocket.Upgrader` 在 HTTP → WS 升级时分配，影响单连接内存占用。4KB 适合以 JSON 文本为主的消息流。**启动后不可变更**——Upgrader 在 NewHub 时初始化 |
| `write_buffer_size` | int | `4096` | ❌ | WebSocket 写缓冲区大小（bytes）。同上，为写方向预分配的缓冲。**启动后不可变更**——同 `read_buffer_size` |
| `ping_interval` | duration | `54s` | — | 服务端发送 WebSocket Ping 帧的间隔。必须小于 `pong_timeout`，推荐为其 90%。ReadPump goroutine 在此间隔触发 WritePump 发送 Ping，用于检测半开连接（客户端进程崩溃但 TCP 未关闭） |
| `pong_timeout` | duration | `60s` | — | 等待客户端 Pong 响应的超时。超时后 ReadPump 关闭连接，触发 Hub 注销和 session 清理。此值应略大于客户端的 ping-pong 往返延迟 |
| `write_timeout` | duration | `10s` | — | 单次 WebSocket 写操作的截止时间。WritePump 在发送每条消息前设置 `SetWriteDeadline`，超时则判定连接已死。防止慢速客户端阻塞其他连接的写操作 |
| `idle_timeout` | duration | `5m` | — | WebSocket 物理连接空闲超时。如果连接在此时间内没有任何帧（包括 ping/pong），服务端主动断开。用于清理已断开但 TCP 未关闭的僵尸连接 |
| `max_frame_size` | int64 | `32768` | — | 单个 WebSocket 帧最大允许字节数（32KB）。ReadPump 使用此值设置 `SetReadLimit`，超过则立即关闭连接。防止恶意客户端发送超大帧耗尽内存 |
| `broadcast_queue_size` | int | `256` | ❌ | Hub broadcast channel 的缓冲大小。Hub.Run() 从此 channel 消费事件并路由到各 session 连接。增大可缓解瞬时事件突发，但增加内存占用。**启动后不可变更**——Go channel 大小在 make 时确定 |
| `platform_write_buffer` | int | `64` | — | 平台连接（Slack/Feishu）的异步写 channel 容量。每个 platform conn 内部有一个带缓冲 channel 接收待发送事件，WriteCtx 将事件入队后立即返回。64 个槽位在 120ms 合并窗口下可容纳约 8 个批次 |
| `platform_drop_threshold` | int | `56` | — | 平台连接开始丢弃可丢弃事件的水位线（channel 填充度）。当 channel 使用量超过此阈值（87.5%），`message.delta` 和 `raw` 事件被静默丢弃以缓解反压，但 `state`/`done`/`error` 等关键事件永不丢弃 |
| `delta_coalesce_interval` | duration | `120ms` | — | 平台连接 Delta 事件的合并窗口。在此时间窗口内的多个 `message.delta` 事件会被合并为一次 API 调用发送给 Slack/Feishu。120ms 约等于每秒 8.3 次更新，适配飞书 CardKit 的 10 次/秒限制，同时保持首 token 延迟在 200ms 感知阈值内 |
| `delta_coalesce_size` | int | `200` | — | Delta 合并的 rune 数量阈值。当合并窗口内累积的文本超过 200 个 rune（约 40 个 token），立即刷新而非等待定时器。作为突发流量的安全阀，仅在输出高峰时触发 |

### admin — 管理 API

提供 REST 端点用于监控（stats、health）和会话管理（list、get、terminate）。应通过防火墙或网络策略限制访问。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `enabled` | bool | `true` | — | 是否启动 Admin HTTP 服务。设为 `false` 则不监听管理端口，所有 `/admin/*` 端点不可用 |
| `addr` | string | `:9999` | — | Admin API 监听地址。生产环境应绑定到内网 IP（如 `10.0.0.1:9999`）或通过 `allowed_cidrs` 限制访问 |
| `tokens` | []string | `[]` | ✅ | 授权令牌列表。通过 `HOTPLEX_ADMIN_TOKEN_1..N` 编号式环境变量设置。每个请求需携带 `Authorization: Bearer <token>` 头。支持多令牌用于无损轮转 |
| `token_scopes` | map | `{}` | — | 令牌到权限的 RBAC 映射。key 为令牌值，value 为权限列表（如 `["session:read", "session:write"]`）。未映射的令牌使用 `default_scopes` |
| `default_scopes` | []string | `["session:read", "session:write", "session:delete", "stats:read", "health:read", "admin:write"]` | — | 未在 `token_scopes` 中显式映射的令牌的默认权限集。`admin:write` 隐含 `admin:read` → `config:read` |
| `ip_whitelist_enabled` | bool | `false` | — | 启用 CIDR 白名单。开启后仅 `allowed_cidrs` 中的网段可访问 Admin API。Docker/Kubernetes 环境建议使用网络策略替代 |
| `allowed_cidrs` | []string | `127.0.0.0/8, 10.0.0.0/8` | — | 信任的 CIDR 网段列表。仅当 `ip_whitelist_enabled: true` 时生效 |
| `rate_limit_enabled` | bool | `true` | ✅ | 启用基于令牌桶的速率限制。按客户端 IP 独立计数，防止单个客户端滥用管理接口 |
| `requests_per_sec` | int | `10` | ✅ | 令牌桶的持续填充速率（每秒令牌数）。超出此速率的请求返回 429 |
| `burst` | int | `20` | ✅ | 令牌桶的最大容量（突发上限）。允许短时间内的请求突发，如监控面板轮询 |

### db — 数据存储

控制 SQLite 持久化行为。所有写操作通过单写 goroutine 串行化（channel 缓冲 50 条/100ms 批量刷盘），保证 WAL 模式下的写入一致性。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `path` | string | `~/.hotplex/data/hotplex.db` | — | SQLite 数据库文件路径。`~` 自动展开为用户主目录。文件不存在时自动创建。加载时自动转为绝对路径 |
| `events_path` | string | `~/.hotplex/data/events.db` | — | SQLite 事件数据库文件路径。独立于 `path`，存储事件级持久化数据（AEP Envelope）。默认与主数据库同目录 |
| `wal_mode` | bool | `true` | — | 启用 Write-Ahead Logging 模式。WAL 允许并发读写（读不阻塞写），是单写 goroutine 架构的前提。**禁止关闭**，否则写性能急剧下降且并发读不可用 |
| `busy_timeout` | duration | `500ms` | — | SQLite 锁等待超时。当另一个写操作持有时，当前操作在此时间内重试。500ms 在单写 goroutine 模型下足够覆盖一次批量刷盘 |
| `max_open_conns` | int | `1` | — | 最大数据库连接数。SQLite 的并发写入受限于单连接，设为 1 确保所有操作在同一连接上串行化 |

### security — 安全与认证

控制客户端认证、TLS 加密、CORS 跨域策略。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `api_key_header` | string | `X-API-Key` | — | API Key 认证的 HTTP 头名称。客户端通过此头发送 API Key，Hub 在 WebSocket 升级前校验 |
| `api_keys` | []string | `[]` | ✅ | 允许访问的 API 密钥列表。通过 `HOTPLEX_SECURITY_API_KEY_1..N` 编号式环境变量设置。为空时不做 API Key 校验（依赖网络策略保护）。热重载时原子替换整个 key 集合，不影响进行中的请求 |
| `tls_enabled` | bool | `false` | — | 启用 TLS（WSS）。生产环境**必须**设为 `true`。启用后网关使用 `tls_cert_file` 和 `tls_key_file` 加载证书 |
| `tls_cert_file` | string | `/etc/hotplex/tls/server.crt` | — | TLS 证书文件路径。仅当 `tls_enabled: true` 时使用 |
| `tls_key_file` | string | `/etc/hotplex/tls/server.key` | — | TLS 私钥文件路径。仅当 `tls_enabled: true` 时使用 |
| `allowed_origins` | []string | `["*"]` | ✅ | CORS 允许的 Origin 列表。WebSocket 升级时 `Upgrader.CheckOrigin` 校验请求的 Origin 头。`["*"]` 允许所有来源（仅开发用），生产应限制为具体域名。热重载即时生效——每次 WS 升级请求读取最新配置 |
| `cookie_same_site` | string | `lax` | — | WebChat 登录 Session Cookie 的 SameSite 属性。可选值：`lax`、`none`、`strict`、`default`。<br>**Lax 模式 (默认值)**：允许在纯 HTTP 内网/局域网下成功交换 Cookie（不需要 Secure 属性），且具备原生 CSRF 安全防护；但对于“跨域/跨站的前后端分离部署”，浏览器不会在跨站 AJAX 请求中携带此 Cookie，会导致接口调用返回 401。<br>**None 模式**：允许在任何跨站请求下自动携带 Cookie（最宽松模式），但必须配合 `tls_enabled: true` 即 HTTPS 强制启用（因为浏览器强制规定 SameSite=None 必须具有 Secure 属性，否则在非 localhost HTTP 下会被浏览器直接静默丢弃）。 |

### session — 会话生命周期

会话遵循 5 状态机：`CREATED → RUNNING → IDLE → TERMINATED → DELETED`。Manager 持有内存中的活跃会话映射，SQLite 持久化元数据，后台 GC goroutine 按 `gc_scan_interval` 周期扫描过期会话。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `retention_period` | duration | `168h` (7天) | — | 兼容的 Session 运行到期时长；到期后终止运行并释放 Worker。它不是聊天正文或历史保留期 |
| `gc_scan_interval` | duration | `1m` | ✅ | GC 后台扫描间隔。检查僵死 Worker、`expires_at` 运行到期、IDLE 空闲期限和 Legacy TERMINATED 记录期限。v2 归档及数据保留使用独立生命周期时钟 |
| `max_concurrent` | int | `1000` | — | 全局最大并发活跃会话数（CREATED + RUNNING + IDLE）。超出时新会话创建请求被拒绝并返回 `POOL_EXHAUSTED` 错误。根据服务器内存和 Worker 资源需求调整。实际配额由 `pool.max_size` 控制 |
| `event_store_enabled` | bool | `true` | — | 启用事件持久化。Bridge 在每个 `done` 事件时将完整 Envelope 写入 MessageStore，用于会话回放、调试和审计。关闭后不写事件日志，减少 I/O |
| `event_store_type` | string | `sqlite` (代码: `""`) | — | 事件存储后端类型。目前仅支持 `"sqlite"`。空字符串表示未指定，依赖 config.yaml 设置 |

新会话默认使用 `lifecycle.policy: v2`：7 天无输入显示归档标记；会话期限默认 180 天并由有效用户输入续期；每条正文从创建时间起默认保留 180 天。查看历史不会续期，输入也不会延长旧正文期限。逐条正文到期过滤、v2 会话自动退役和持久异步清理均已接线；存量期限变更须先预览，再单独确认 apply。

### pool — 会话池

PoolManager 在内存中追踪全局和每用户的会话配额。每个 Worker 进程按 512MB 估算内存占用。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `min_size` | int | `0` | — | 预热池最小维持数量。大于 0 时启动后立即预创建指定数量的 Worker 进程，减少首次请求延迟。生产环境建议 `>0` 以消除冷启动 |
| `max_size` | int | `100` | ✅ | 全局最大活跃会话数（所有用户合计）。PoolManager.Acquire() 在 `totalCount >= maxSize` 时拒绝新会话。与 `session.max_concurrent` 协同工作 |
| `max_idle_per_user` | int | `5` (代码: `5`) | ✅ | 单个用户（bot_id）允许的最大同时活跃会话数。防止单个用户占用过多资源。0 = 不限 |
| `max_memory_per_user` | int64 | `3221225472` (3GB, 代码: 3GB) | — | 单个用户的总估算内存配额（bytes）。按每 Worker 512MB 估算，3GB 允许约 6 个并发 Worker。超出时拒绝新会话并返回 `MEMORY_EXCEEDED`。Linux 上 Worker 通过 RLIMIT_AS 硬限制为 512MB，macOS 不支持此机制。内存配额检查使用启动时快照 |

---

### 工作目录优先级 (WorkDir Priority)

Worker 进程启动时的工作目录遵循以下优先级覆盖逻辑：

1.  **会话级 (Session)**：客户端在 `session.init` 的 `metadata.work_dir` 中指定的路径（最高优先级）。
2.  **平台级 (Platform)**：在 `messaging.slack.work_dir` 或 `messaging.feishu.work_dir` 中指定的路径。
3.  **全局级 (Global)**：在 `worker.default_work_dir` 中指定的路径（最低优先级，默认兜底）。

---

### worker — Worker 进程

每个会话对应一个独立的 Worker 子进程（如 `claude` CLI），通过 stdin/stdout 的 NDJSON 协议通信。进程使用 PGID 隔离，终止时对整个进程组发送信号。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `max_lifetime` | duration | `24h` | — | **绝对生存周期**。Worker 进程从启动开始计算的强制最大寿命。到期后无论是否活跃均会被终止。旨在通过定期刷新来清除潜在的内存泄漏或内部状态退化。Worker 可通过 resume 机制在下次请求时无缝重启。 |
| `idle_timeout` | duration | `60m` | — | **闲置回收周期**（Idle Recycle）。Worker 在等待新输入（IDLE 状态）时的最大允许时长。每次接收到用户输入后该计时器都会重置。旨在及时释放不活跃会话占用的资源。较短的超时释放资源更快，但可能导致用户体感的冷启动增加。 |
| `execution_timeout` | duration | `30m` | — | **僵死检测周期**（Zombie IO Timeout）。Worker 在执行中（RUNNING 状态）无任何 I/O 输出反馈的最大时长。用于检测并强制清理卡死、陷入无限循环或无响应的 Worker 进程，防止会话永久阻塞。 |
| `turn_timeout` | duration | `30m` | — | 单轮输入到终结的绝对上限。流式输出不会重置期限；v2 的显式 `0` 仍使用 30 分钟默认值 |
| `default_work_dir` | string | `~/.hotplex/workspace` | — | Worker 进程的默认工作目录。当 session 或 platform 未指定 `work_dir` 时使用。`~` 自动展开为用户主目录。目录不存在时自动创建（`mkdir -p`） |
| `pid_dir` | string | `~/.hotplex/.pids/` | — | PID 文件目录。proc.Manager 在启动 Worker 时写入 PID 文件用于孤儿进程清理。网关重启时自动扫描此目录，杀死不再有父进程的孤儿 Worker |
| `env_blocklist` | []string | `[]` | — | 环境变量黑名单。默认所有 `os.Environ()` 变量透传给 Worker，仅黑名单中的变量被过滤。条目以 `_` 结尾时按前缀匹配（如 `HOTPLEX_` 阻止所有 `HOTPLEX_*` 变量）。`HOTPLEX_WORKER_` 前缀的变量会被剥离前缀后注入（如 `HOTPLEX_WORKER_GITHUB_TOKEN` → `GITHUB_TOKEN`） |
| `environment` | []string | `[]` | ✅ | 额外注入 Worker 进程的环境变量列表。每个条目支持 `KEY=VALUE` 或 `KEY=${VAR}` 格式。这是唯一支持 `${VAR}` 展开的配置字段，引用未设置变量（无默认值）的条目会被静默丢弃。优先级最高，覆盖所有其他来源 |

### worker.auto_retry — LLM 自动重试

当 Worker（如 Claude Code）遇到 LLM Provider 的临时错误（429 限流、529 过载、网络超时等），Bridge 自动检测并触发重试，对用户透明。

**错误拦截机制**：当 `enabled: true` 时，Worker 发出的 `error` 事件会被 Bridge 暂存而非立即转发。等到 `done` 事件到达时判断是否可重试——如果可重试，原始 error 事件被丢弃，用户只看到 notify 消息（如"🔄 正在自动重试"）；如果不可重试，error 事件在 done 之后放行给客户端。这样用户不会看到原始的 LLM 技术错误信息（如 "429 rate_limit_error"），只看到友好的重试通知。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `enabled` | bool | `true` | ✅ | 启用自动重试。关闭后 Worker 遇到临时错误时直接将错误事件转发给客户端 |
| `max_retries` | int | `9` | ✅ | 单个会话的最大重试次数。每次 Worker 完成一个 turn（`done` 事件），Bridge 检查输出是否匹配可重试错误模式。达到上限后停止重试，返回提示让用户手动操作。9 次重试在 base_delay=5s 指数退避下总耗时约 5+10+20+40+80+120+120+120+120 ≈ 640s（11 分钟），不会超过网关的 `execution_timeout`（默认 30 分钟） |
| `base_delay` | duration | `5s` | ✅ | 首次重试的等待时间。采用指数退避：第 1 次等 `base_delay`，第 2 次等 `2×base_delay`，依此类推，直到 `max_delay` |
| `max_delay` | duration | `120s` | ✅ | 退避延迟上限。即使指数增长超过此值，实际延迟也不会超过 120s |
| `retry_input` | string | `继续` | ✅ | 重试时发送给 Worker 的文本。Bridge 在 Worker 的 stdin 写入此文本，触发 Worker 重新发起 LLM 请求 |
| `notify_user` | bool | `true` | ✅ | 重试期间是否通知用户。启用时在重试前发送一条 `message` 事件（如"🔄 正在自动重试 (1/9)..."），替代原始 LLM 错误信息。关闭后用户看不到任何重试提示，但错误信息仍然会被拦截 |
| `patterns` | []string | `[]` | ✅ | **追加模式 (Append Mode)**。在此定义的正则表达式将追加到内置模式（如 429, 5xx, 网络超时等）之后。自定义模式与内置模式共同生效，不会覆盖内置逻辑。 |

### webchat — 嵌入式 Web Chat

控制网关内嵌的 Web Chat SPA 服务。Next.js 静态导出通过 `go:embed` 编译时嵌入二进制，网关同时托管 API 和前端。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `enabled` | bool | `true` | — | 启用 Web Chat SPA 托管。设为 `false` 则网关仅提供 WebSocket/API 端点，不提供前端静态文件 |
| `addr` | string | `""` | — | Web Chat 地址（信息展示用）。用于启动 Banner 显示，不控制实际监听端口。Web Chat 由 Gateway 的 HTTP Server 在同一端口托管 |

### agent_config — Agent 角色配置 (B/C Channels)

定义 Agent 的“人格（Persona）”与“上下文（Context）”。系统自动将这些 Markdown 文件注入到系统提示词的 XML 标签中。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `enabled` | bool | `true` | — | 是否启用 Agent 配置注入。关闭后 Agent 将仅使用代码内建的通用提示词。 |
| `config_dir` | string | `~/.hotplex/agent-configs` | — | 配置文件根目录。支持 **per-bot 3 级目录 fallback**，按文件独立解析：`<config_dir>/<platform>/<botName>/<file>` → `<config_dir>/<platform>/<file>` → `<config_dir>/<file>`，命中第一个非空文件即停止。有效平台值：`slack`、`feishu`、`webchat`、`""`（仅全局级） |

### skills — 技能管理

管理 Agent 可调用的工具和发现逻辑。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `cache_ttl` | duration | `5m` | ✅ | 技能发现结果在内存中的缓存时长。过期后触发重新扫描。设为 `0` 则禁用缓存（不推荐，会增加磁盘 I/O）。 |

### log — 日志

控制网关进程的日志输出。使用 `log/slog` 结构化日志，所有日志带有 `service=hotplex-gateway` 和 `version` 字段。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `level` | string | `info` | ✅ | 最低日志级别。`debug` 输出所有事件流转细节（每个收发的事件），适合开发调试。`info` 仅输出关键生命周期事件（连接注册、会话创建/终止等）。`warn`/`error` 仅输出异常情况。热重载通过 `slog.LevelVar` 即时生效，无需重启 |
| `format` | string | `json` | ❌ | 日志格式。`json` 为结构化 JSON（适合日志聚合系统如 ELK/Loki）。`text` 为人类可读的 key=value 格式（适合终端直接查看）。Makefile `dev-logs` 使用 `tail -f` 展示。**启动后不可变更**——Handler 在初始化时确定格式 |

### messaging — 消息平台全局设置

控制所有消息平台（Slack/飞书）的共享行为。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `turn_summary_enabled` | bool | `true` | ❌ | 是否在每次会话回合完成后发送 turn summary（回合摘要）。关闭后 Slack 不发送 TableBlock 摘要，飞书不发送 CardKit 卡片（但飞书仍保留 `Log.Info` 遥测日志）。通过环境变量 `HOTPLEX_MESSAGING_TURN_SUMMARY_ENABLED` 覆盖。**启动时确定**，运行中变更需重启 |

### messaging.slack — Slack Socket Mode

通过 Slack Socket Mode 建立 WebSocket 连接到 Slack 服务器，实现无需公网入口的消息收发。消息经过 chunker（长消息拆分）→ dedup（TTL 去重）→ format（Markdown 转换）→ rate limiter → send 的流水线处理。Streaming 使用 SlackStreamingWriter，以 150ms 间隔、20 rune 阈值增量更新消息。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `enabled` | bool | `false` | — | 启用 Slack 适配器。启动时通过 Socket Mode 连接到 Slack 服务器，开始监听消息事件 |
| `bot_token` | string | — | — | Slack Bot User OAuth Token（`xoxb-` 前缀）。用于调用 Slack Web API 发送消息、更新卡片。通过环境变量 `HOTPLEX_MESSAGING_SLACK_BOT_TOKEN` 设置 |
| `app_token` | string | — | — | Slack App-Level Token（`xapp-` 前缀）。用于建立 Socket Mode WebSocket 连接。需要在 Slack App 配置中启用 Socket Mode 并生成。通过环境变量 `HOTPLEX_MESSAGING_SLACK_APP_TOKEN` 设置 |
| `socket_mode` | bool | `true` (代码: `false`) | — | 启用 Socket Mode。Socket Mode 通过 WebSocket 与 Slack 服务器通信，无需公网可访问的 HTTP 端点。关闭则需要配置 Events API URL |
| `worker_type` | string | `claude_code` | — | 为 Slack 会话创建的 Worker 类型。`claude_code` = Claude Code CLI，`opencodeserver` = OpenCode Server，`codex_cli` = Codex CLI，`acp` = ACP 兼容 Agent（JSON-RPC 2.0） |
| `work_dir` | string | — | — | Worker 进程的工作目录。为空时使用 `worker.default_work_dir`。可按平台设置不同目录 |
| `dm_policy` | string | `allowlist` | — | 私聊（DM）的访问策略。`open` = 允许所有人，`allowlist` = 仅 `allow_from` + `allow_dm_from` 中的用户，`disabled` = 禁止所有私聊 |
| `group_policy` | string | `allowlist` | — | 频道和群组 DM 的访问策略。选项同 `dm_policy`。`require_mention: true` 时，即使策略允许，也需要 @机器人 才触发 |
| `require_mention` | bool | `true` | — | 群组中是否需要 @机器人 才触发处理。私聊始终触发，不受此限制。`true` 避免群组中的每条消息都创建 Worker 会话 |
| `allow_from` | []string | `[]` | — | 全局白名单（同时授权 DM 和群组）。值为 Slack User ID 或显示名。在 `allowlist` 策略下生效 |
| `allow_dm_from` | []string | `[]` | — | 仅私聊白名单。与 `allow_from` 合并去重 |
| `allow_group_from` | []string | `[]` | — | 仅群组白名单。与 `allow_from` 合并去重 |
| `reconnect_base_delay` | duration | `1s` | — | Socket Mode 连接断开后的首次重连延迟。采用指数退避，每次翻倍直到 `reconnect_max_delay` |
| `reconnect_max_delay` | duration | `60s` | — | 重连延迟上限。避免在网络故障时过于频繁地重试 |
| `assistant_api_enabled` | *bool | `nil` | — | 是否启用 Assistant API 模式。`nil`（未设置）= 自动检测。`true` = 强制使用 Assistant API，`false` = 使用标准对话模式 |

### messaging.feishu — 飞书 WebSocket

通过飞书 WebSocket 客户端接收事件（P2 模式），使用流式卡片（CardKit）实现增量更新。消息发送经过 4 层防护：TTL 守卫 → 完整性检查 → 带退避重试 → IM Patch 降级。

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
|:-----|:-----|:-------|:------:|:-----|
| `enabled` | bool | `false` | — | 启用飞书适配器。启动时通过 WebSocket 连接到飞书服务器，监听消息事件 |
| `app_id` | string | — | — | 飞书应用 ID（`cli_` 前缀）。在飞书开放平台创建应用后获取。通过环境变量 `HOTPLEX_MESSAGING_FEISHU_APP_ID` 设置 |
| `app_secret` | string | — | — | 飞书应用密钥。用于获取 tenant_access_token 调用飞书 API。通过环境变量 `HOTPLEX_MESSAGING_FEISHU_APP_SECRET` 设置 |
| `worker_type` | string | `claude_code` | — | 为飞书会话创建的 Worker 类型。同 Slack 的 `worker_type`（`claude_code` / `opencodeserver` / `codex_cli` / `acp`） |
| `work_dir` | string | — | — | Worker 进程工作目录。同 Slack 的 `work_dir` |
| `dm_policy` | string | `allowlist` | — | 单聊访问策略。选项同 Slack |
| `group_policy` | string | `allowlist` | — | 群组和话题群访问策略。选项同 Slack |
| `require_mention` | bool | `true` | — | 群组中是否需要 @机器人。同 Slack |
| `allow_from` | []string | `[]` | — | 全局白名单。值为飞书 User ID 或 Open ID |
| `allow_dm_from` | []string | `[]` | — | 仅单聊白名单 |
| `allow_group_from` | []string | `[]` | — | 仅群组白名单 |
| `stt_provider` | string | `feishu+local` | — | 语音转文字引擎。`feishu` = 飞书 speech_to_text API（需开通权限），`local` = 本地命令行引擎（SenseVoice-Small ONNX），`feishu+local` = 云端优先本地降级（推荐），空 = 禁用 STT |
| `stt_local_cmd` | string | `python3 scripts/stt_server.py` | — | 本地 STT 命令。含 `{file}` → 每次请求 fork 新进程（冷启动约 3-5s）；不含 `{file}` → 常驻子进程 + stdin/stdout JSON 协议（零冷启动，适合高频使用，模型约占 900MB 内存）。详细安装说明见 [STT 安装手册](../docs/STT-Setup.md) |
| `stt_local_idle_ttl` | duration | `15m` | — | 常驻模式空闲超时。STT 子进程在此时间内无转写请求则自动关闭，节省内存。0 = 永不关闭。仅常驻模式生效 |

> **完整的 STT 安装配置说明**见 [STT-Setup.md](../docs/STT-Setup.md) —— 涵盖 Python 依赖安装、模型下载、Ephemeral/Persistent 模式配置、Docker 部署和故障排查。

---

## 环境变量速查

所有环境变量以 `HOTPLEX_` 前缀。编号式变量 (`_1..N`) 支持多值轮转。

| 变量 | 必填 | 说明 |
|:-----|:----:|:-----|
| `HOTPLEX_ADMIN_TOKEN_1` | **是** | 主管理端令牌 |
| `HOTPLEX_ADMIN_TOKEN_2..N` | 否 | 备用管理端令牌（轮转用） |
| `HOTPLEX_SECURITY_API_KEY_1..N` | 否 | 客户端 API 密钥 |
| `HOTPLEX_LOG_LEVEL` | 否 | 覆盖 `log.level` |
| `HOTPLEX_LOG_FORMAT` | 否 | 覆盖 `log.format` |
| `HOTPLEX_DB_PATH` | 否 | 覆盖 `db.path` |
| `HOTPLEX_GATEWAY_ADDR` | 否 | 覆盖 `gateway.addr` |
| `HOTPLEX_ADMIN_ADDR` | 否 | 覆盖 `admin.addr` |
| `HOTPLEX_SESSION_MAX_CONCURRENT` | 否 | 覆盖 `session.max_concurrent` |
| `HOTPLEX_POOL_MAX_SIZE` | 否 | 覆盖 `pool.max_size` |
| `HOTPLEX_POOL_MAX_MEMORY_PER_USER` | 否 | 覆盖 `pool.max_memory_per_user` |
| `HOTPLEX_MESSAGING_SLACK_*` | 否 | 覆盖 `messaging.slack.*` 全部字段 |
| `HOTPLEX_MESSAGING_FEISHU_*` | 否 | 覆盖 `messaging.feishu.*` 全部字段 |
| `HOTPLEX_MESSAGING_TURN_SUMMARY_ENABLED` | 否 | 覆盖 `messaging.turn_summary_enabled` |

---

## 热重载

Watcher 监听 `-config` 指定文件的变更（500ms 防抖），通过反射逐字段比较检测变化，验证新配置后原子替换 `ConfigStore` 并通知各组件的 observer。

### 可热重载字段（即时生效，无需重启）

| 模块 | 字段 | 生效机制 |
|:-----|:-----|:---------|
| log | `level` | `slog.LevelVar` 动态切换 |
| security | `api_keys` | Authenticator 原子替换 key map |
| security | `allowed_origins` | 每次 WS 升级请求读取最新值 |
| session | `gc_scan_interval` | Channel 信号重置 GC ticker |
| pool | `max_size` | PoolManager 加锁更新 |
| pool | `max_idle_per_user` | 同上 |
| worker.auto_retry | `enabled` / `max_retries` / `base_delay` / `max_delay` / `retry_input` / `notify_user` / `patterns` | LLMRetryController 原子替换配置和编译后的正则 |
| admin | `requests_per_sec` / `burst` | 令牌桶动态调整填充速率和容量 |
| admin | `tokens` | Admin API token 列表热更新 |

### 不可热重载字段（变更需重启）

以下字段涉及**启动时一次性创建的资源**，运行中无法变更：

| 模块 | 字段 | 原因 |
|:-----|:-----|:-----|
| gateway | `addr` | HTTP Server 启动时绑定端口 |
| gateway | `read_buffer_size` / `write_buffer_size` | WebSocket Upgrader 在 NewHub 时初始化 |
| gateway | `broadcast_queue_size` | Go channel 大小在 make 时确定 |
| log | `format` | slog Handler 在初始化时确定格式 |
| db | `path` / `wal_mode` | SQLite 连接在启动时建立 |
| security | `tls_*` | TLS 证书在启动时加载 |

> 变更不可热重载字段时，Watcher 会记录日志 `config: static field changed, restart required`，新值存入 ConfigStore 但不产生实际效果，需重启网关才能生效。

支持历史快照和回滚（最多 64 个版本），回滚操作同样通过 ConfigStore 原子传播到所有 observer。

---

## Dev vs Prod 差异

| 字段 | config-dev.yaml | config-prod.yaml |
|:-----|:----------------|:-----------------|
| `admin.rate_limit_enabled` | `false` | (base: `true`) |
| `session.retention_period` | `24h` | (base: `168h`) |
| `session.max_concurrent` | `100` | (base: `1000`) |
| `pool.max_size` | `50` | `500` |
| `pool.min_size` | (base: `0`) | `10`（预热） |
| `pool.max_memory_per_user` | 3GB | 3GB |
| `worker.idle_timeout` | `30m` | `30m` |
| `worker.execution_timeout` | `30m` | (base: `30m`) |
| `log.level` | `debug` | (base: `info`) |
| `log.format` | `text` | (base: `json`) |
| `security.tls_enabled` | (base: `false`) | `true` |
| `security.allowed_origins` | (base: `*`) | 具体域名 |
| `messaging.*.enabled` | `true` | (base: `false`) |
| `messaging.feishu.*_policy` | (base: `allowlist`) | `open` |

---

## 生产安全清单

- `admin.tokens` 仅通过 `HOTPLEX_ADMIN_TOKEN_1..N` 设置
- 生产必须 `tls_enabled: true`
- `admin.addr` 绑定内网或通过 `allowed_cidrs` 限制访问
- 敏感凭据使用编号式环境变量支持轮转
- `db.wal_mode` 禁止关闭（`true`）
