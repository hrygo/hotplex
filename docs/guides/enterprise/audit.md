---
title: 合规与审计指南
weight: 29
description: 用户行为审计系统、不可变存储、跨渠道身份追踪、配置变更审计、凭证管理与合规检查清单
---

# 合规与审计指南

> 面向企业合规与运维团队的 HotPlex 审计系统指南。涵盖用户行为审计（SHA-256 Hash Chain + Append-only 不可变存储）、跨渠道身份追踪、Webhook SIEM 集成、Admin 操作双写审计、配置变更追踪、凭证管理与合规检查清单。

---

## 1. 审计系统概览

HotPlex 内建**用户行为审计子系统**，对用户操作产生不可篡改的审计记录。生命周期 v2 将新事实写入独立的 `user_activity_v2` 链；升级前的 `user_activity` 是 legacy 链。两条链都采用 **SHA-256 Hash Chain + Append-only 存储**。

### 1.1 核心设计

| 特性 | 说明 |
|------|------|
| **Hash Chain** | 每条记录的 `self_hash = SHA256(prev_hash \|\| canonical_json(fields))`，形成密码学链式结构 |
| **Append-only** | 数据库触发器禁止所有 UPDATE 操作，记录一旦写入不可修改 |
| **Zero-loss** | 3 级 Enqueue 回退（内存 channel → WAL Spill → bounded block），确保审计事件不丢失 |
| **双数据库** | SQLite（WriteMu 序列化）+ PostgreSQL（advisory lock 819207），自动选择 |
| **跨渠道身份** | Identity Link 机制将同一用户在不同平台（飞书/Slack/WebChat）的身份关联 |

### 1.2 数据流向

```
                    ┌──────────────┐
                    │  事件产生点   │
                    │ (auth/session│
                    │  message/tool│
                    │  admin/system│
                    └──────┬───────┘
                           │ Enqueue (non-blocking)
                    ┌──────▼───────┐
                    │  Collector   │
                    │  (内存 channel│
                    │   → WAL Spill│
                    │   → bounded) │
                    └──┬───────┬───┘
                       │       │
              ┌────────▼┐  ┌──▼────────┐
              │  Store   │  │   Sinks    │
              │(SQLite/  │  │ (noop/log  │
              │ Postgres)│  │  webhook)  │
              └──────────┘  └───────────┘
```

### 1.3 审计事件分类

| 类别 | Action 前缀 | 说明 |
|------|------------|------|
| 认证 | `auth.login` / `auth.logout` / `auth.apikey_used` / `auth.denied` | 凭证登录/登出、API Key 使用、认证拒绝（`auth.token_validated` 常量保留但不再触发） |
| Session | `session.create` / `session.delete` | 会话创建与删除 |
| 消息 | `message.inbound` | 入站消息事实，不记录消息正文 |
| 交互授权 | `permission.response` / `question.response` / `elicitation.response` | 用户响应工具授权/提问/MCP 引导（只记录关联 ID、决策结果或受限 action） |
| Tool | `tool.call` | Worker 工具调用事实（只记录受限工具名和调用状态） |
| Admin | `admin.*` | 所有管理操作（Bot/ApiKey/Cron/Session 等的 CRUD） |
| 系统 | `system.audit_config_changed` / `system.audit_export` | 审计配置变更、审计数据导出 |

---

## 2. 快速开始

审计系统**默认开启**（`audit.enabled: true`），开箱即用，无需额外配置。

### 2.1 验证审计是否运行

**方式一：检查 Prometheus 指标**

```bash
curl -s http://localhost:9999/admin/metrics | grep 'hotplex_audit_events_total'
```

有输出即表示审计事件正在写入。

**方式二：查看日志**

```bash
grep "audit" ~/.hotplex/logs/gateway.log | head -5
```

启动时应看到 `audit collector started` 相关日志。

**方式三：Admin 活动控制台**

登录 Admin Panel → 左侧导航 → **活动**，查看是否有审计记录。

### 2.2 最低配置示例

如果需要自定义审计参数，在 `config.yaml` 中添加：

```yaml
audit:
  enabled: true
  retention: 26280h       # 3 年（默认值）
  full_content_retention: 2160h  # 90 天兼容字段；不影响 events/turns GC
  collector:
    channel_cap: 4096     # 内存缓冲区
    batch_interval: 1s     # 刷写间隔
    batch_size: 100       # 每批事件数
  sinks:
    - name: log
      type: log           # 输出到结构化日志（推荐用于起步阶段）

lifecycle:
  audit:
    capture_content: false
    facts_retention: 4320h # lifecycle-v2 新审计事实保留 180 天
```

> 更多配置项见 [配置参考](../../reference/configuration.md)。

---

## 3. 审计配置

### 3.1 核心配置

| 配置项 | 类型 | 默认值 | 环境变量 | 说明 |
|--------|------|--------|----------|------|
| `audit.enabled` | bool | `true` | `HOTPLEX_AUDIT_ENABLED` | 是否启用审计系统 |
| `audit.retention` | duration | `26280h`（3 年） | `HOTPLEX_AUDIT_RETENTION` | legacy `user_activity` 链和升级前旧记录的保留时长 |
| `audit.full_content_retention` | duration | `2160h`（90 天） | `HOTPLEX_AUDIT_FULL_CONTENT_RETENTION` | 兼容配置字段；不影响 event store 或 turns 的留存 |
| `lifecycle.audit.facts_retention` | duration | `4320h`（180 天） | `HOTPLEX_LIFECYCLE_AUDIT_FACTS_RETENTION` | 新写入 lifecycle-v2 `user_activity_v2` 事实的最低保留时长；到期时间随记录写入并纳入哈希，改配置只影响之后的新记录 |

**事件与审计留存相互独立**：event store 与 turns 使用自己的内容期限；legacy 审计链按 `audit.retention` 清理，v2 新事实链在写入时按 `lifecycle.audit.facts_retention` 固定到期时间。新写入的 `message.inbound`、交互响应和 `tool.call` 不含消息正文、答案或工具输入。两条链分别验证、分别使用 checkpoint；Admin 活动查询合并显示并通过 `chain_epoch` 标明来源。`audit.full_content_retention` 为兼容字段，不会延长 event/turn 副本或 `event_ref` 的可用时间。

### 3.2 Collector 调优

| 配置项 | 类型 | 默认值 | 环境变量 | 说明 |
|--------|------|--------|----------|------|
| `audit.collector.channel_cap` | int | `4096` | `HOTPLEX_AUDIT_COLLECTOR_CHANNEL_CAP` | 内存 channel 容量 |
| `audit.collector.batch_interval` | duration | `1s` | `HOTPLEX_AUDIT_COLLECTOR_BATCH_INTERVAL` | 批量刷写间隔 |
| `audit.collector.batch_size` | int | `100` | `HOTPLEX_AUDIT_COLLECTOR_BATCH_SIZE` | 每批最大事件数 |
| `audit.collector.spill_dir` | string | `~/.hotplex/data/audit-spill` | `HOTPLEX_AUDIT_COLLECTOR_SPILL_DIR` | WAL 溢出目录 |

> **调优建议**：高并发场景（>1000 sessions）建议将 `channel_cap` 提升至 `8192`，`batch_size` 提升至 `200`。溢出目录建议使用独立磁盘以减少 I/O 竞争。

### 3.3 Sink 配置

Sink 决定审计事件的投递目标。支持 3 种内置类型：

| Type | 说明 | 适用场景 |
|------|------|----------|
| `noop` | 丢弃所有事件（默认） | 测试环境 |
| `log` | 输出到结构化日志（slog INFO） | 起步阶段、简单审计 |
| `webhook` | HTTP POST + HMAC-SHA256 签名 | SIEM/SOC 集成 |

**Webhook Sink 配置**：

```yaml
audit:
  sinks:
    - name: siem-prod
      type: webhook
      config:
        url: https://siem.example.com/api/audit
        secret: "your-hmac-secret-key"   # 可选，启用签名
        timeout: 5s                       # 可选，默认 5s
        queue_cap: 1024                   # 可选，默认 1024
```

| Webhook 参数 | 必填 | 默认值 | 说明 |
|-------------|------|--------|------|
| `url` | ✅ | — | HTTPS 端点，接收 `POST /application/json` |
| `secret` | ❌ | — | HMAC-SHA256 签名密钥，设置后请求头包含 `X-Audit-Signature: sha256=...` |
| `timeout` | ❌ | `5s` | 单次 POST 超时 |
| `queue_cap` | ❌ | `1024` | 内部队列容量，溢出时丢弃事件 |

### 3.4 热重载 vs 需重启

| 配置项 | 生效方式 | 说明 |
|--------|----------|------|
| `audit.retention` | **热重载** | 立即应用，下一次 GC tick 使用新值 |
| `lifecycle.audit.facts_retention` | **需重启** | 固定之后新写入 v2 事实的到期时间；已有记录期限不变 |
| `audit.collector.batch_size` | **热重载** | 立即应用 |
| `audit.collector.batch_interval` | **热重载**（⚠️ 实际需重启） | 变更会被记录，但刷写器需重启才能生效 |
| `audit.enabled` | **需重启** | 禁止热关闭，防止管理员在审计盲区操作 |
| `audit.full_content_retention` | **需重启** | 兼容字段变更会被审计记录，但不会改变 event/turn GC 留存 |
| `audit.collector.channel_cap` | **需重启** | Channel 容量在创建时确定 |
| `audit.collector.spill_dir` | **需重启** | Spill 路径在启动时确定 |

> **注意**：无论配置是热重载还是需要重启，**所有配置变更都会被 meta-audit 记录**为 `system.audit_config_changed` 事件，确保审计追溯的完整性。

---

## 4. 审计事件详解

### 4.1 事件类型总览

| Action | 触发场景 | Outcome | 说明 |
|--------|----------|---------|------|
| `auth.login` | 凭证交换（密码登录 / OAuth 回调） | success / failure | 仅在凭证交换边界记录；失败保持匿名（防用户枚举） |
| `auth.logout` | 显式登出（清除 cookie） | success | 携带真实 user_id，cookie 缺失/无效时回退匿名 |
| `auth.apikey_used` | API Key 请求（含禁用用户拒绝） | success / failure | API Key 认证成功或失败 |
| `auth.denied` | 无 / 无效凭证（HTTP 请求 + WS upgrade） | denied | 匿名拒绝（含 IP + UA） |
| `session.create` | 创建新会话 | success | 会话建立 |
| `session.delete` | 删除会话 | success | 会话清理 |
| `message.inbound` | 入站消息 | success | 消息接收事实，不含消息正文 |
| `tool.call` | Worker 工具调用 | success | 受限工具名和调用状态，不含工具输入 |
| `admin.*` | Admin API 写操作 | success / failure | 管理操作（见 §4.6） |
| `system.audit_config_changed` | 审计配置变更 | success | 配置变更 diff 追溯 |
| `system.audit_export` | 导出审计数据 | success / failure | 导出成功或失败 |

### 4.2 认证事件

`auth.*` 事件**只在凭证交换边界**记录：密码登录与 OAuth 回调（`auth.login`）、显式登出（`auth.logout`）、API Key 请求（`auth.apikey_used`）、无/无效凭证（`auth.denied`）。**后续每请求的 cookie 重新校验不再产生审计行**——用户归属由领域动作（`message` / `session` / `tool` / `admin.*`）携带的 `user_id` 完成，避免审计日志被高频 cookie 校验刷屏。各路径的身份标记如下：

| 路径 | Action | Platform | UserIDType | UserID 来源 |
|------|--------|----------|------------|-------------|
| 密码登录 / OAuth（成功） | `auth.login` | `webchat` | `registered` | `users.id`（数据库用户） |
| 登录失败 | `auth.login` | `webchat` | `anonymous` | `"anonymous"` 哨兵值（防枚举） |
| 显式登出 | `auth.logout` | `webchat` | `registered` | `users.id`（cookie 缺失时回退匿名） |
| API Key | `auth.apikey_used` | `api` | `registered` 或 `platform` | `api_key_users` 映射或 `api_user` |
| 无 / 无效凭证 | `auth.denied` | — | `anonymous` | `"anonymous"`（含 IP + UA） |

> 匿名/失败认证**必须包含 IP + User-Agent**，用于追溯未授权访问尝试。`auth.token_validated` 常量保留但不再主动触发——WS upgrade 的无效 token 现统一记为 `auth.denied`，原"API Key 命中禁用用户"的 failure 路径已合并进 `auth.apikey_used`。

### 4.3 Session 事件

| 事件 | 触发时机 | 记录内容 |
|------|----------|----------|
| `session.create` | 会话创建 | session_id, user_id, platform, bot_id |
| `session.delete` | 会话删除 | session_id, user_id, 删除原因 |

### 4.4 消息事件

`message.inbound` 在 Gateway 收到入站消息时触发。所有平台（飞书、Slack、WebChat、Cron）统一记录事件时间、用户、平台、Session 和结果；`detail_json` 固定为空对象 `{}`，不保存消息文本或其摘要。

### 4.5 Tool Call 事件

Worker 每次执行工具调用时触发 `tool.call`。当前只记录受限标识符形式的工具名和调用状态；未知或不符合标识符规则的工具名记作 `unknown`。工具参数、标题、内容摘要和预览都不会写入审计记录。

### 4.6 Admin 操作事件

所有 Admin API 写操作**双写**到两个审计通道：

1. **slog admin_audit**（传统日志管道——在调用时解析 `slog.Default()`，因此与 Gateway 的 JSON / lumberjack 日志管道一致；不会被包初始化时捕获的旧 logger 绕过）
2. **user_activity 表**（不可变审计存储，action 前缀 `admin.*`）

| Admin 操作 | user_activity Action |
|-----------|---------------------|
| 创建 Bot | `admin.bot.create` |
| 更新 Bot | `admin.bot.update` |
| 删除 Bot | `admin.bot.delete` |
| 创建 API Key | `admin.apikey.create` |
| 更新 API Key | `admin.apikey.update` |
| 删除 API Key | `admin.apikey.delete` |
| 创建 Cron 任务 | `admin.cron.create` |
| 更新 Cron 任务 | `admin.cron.update` |
| 删除 Cron 任务 | `admin.cron.delete` |
| 触发 Cron 任务 | `admin.cron.trigger` |
| 删除 Session | `admin.session.delete` |
| 终止 Session | `admin.session.terminate` |
| 配置回滚 | `admin.config.rollback` |
| 创建身份链接 | `admin.audit.identity_link.create` |
| 删除身份链接 | `admin.audit.identity_link.delete` |
| 决策 Fence（resolve，#877） | `admin.runtime.fence.resolve` |
| 决策 Fence（abandon，#877） | `admin.runtime.fence.abandon` |

> Fence 决策同时写入 **slog admin_audit 管道**（decision 无关的 `runtime.fence.action` 一行）与 **user_activity 表**（按决策区分的 `admin.runtime.fence.resolve` / `admin.runtime.fence.abandon`，含 reason 与 evidence_ref）。详见 `reference/admin-api.md` Runtime Operations 章节。

### 4.7 系统事件

**配置变更审计**（`system.audit_config_changed`）：

当 `config.yaml` 中 `audit.*` 相关字段发生变更时，自动写入审计表。记录变更前后的完整 diff，支持 7 个字段追踪：`enabled`、`retention`、`full_content_retention`、`channel_cap`、`batch_interval`、`batch_size`、`spill_dir`。

**导出审计**（`system.audit_export`）：

无论导出成功或失败，都写入审计记录。失败的导出（如攻击者探测、批量 exfil 尝试）同样具有取证价值。

---

## 5. 数据完整性保障

### 5.1 Hash Chain 原理

每条审计记录包含 `prev_hash` 和 `self_hash` 两个字段，形成密码学链式结构：

```
Row 1 (Genesis):
  prev_hash = ""
  self_hash = SHA256("" || canonical_json(fields))

Row 2:
  prev_hash = Row1.self_hash
  self_hash = SHA256(Row1.self_hash || canonical_json(fields))

Row 3:
  prev_hash = Row2.self_hash
  self_hash = SHA256(Row2.self_hash || canonical_json(fields))

  ... 链式延续 ...
```

`canonical_json` 包含除 `id`、`prev_hash`、`self_hash` 外的所有字段。**任何中间行的篡改或删除都会导致后续所有 Hash 不匹配**，后台 Verifier 可自动检测。

### 5.2 不可变性保障

数据库层面通过触发器**禁止所有 UPDATE 与未授权的 DELETE**（migration 023 + 030）：

- **SQLite**：`RAISE(ABORT)` 阻止更新；`trg_ua_no_delete` 阻止未被 Checkpoint 锚定的删除
- **PostgreSQL**：通过 PL/pgSQL 函数 `RAISE EXCEPTION` 阻止更新与未锚定删除

任何试图修改审计记录的操作都会被数据库拒绝并返回错误。

**DELETE 保护原理**（根因修复：历史上手工/脚本删除可在不写 Checkpoint 的情况下静默断链——即 `broken_id=1253` 事件的成因）：

- 只有**被 Checkpoint 锚定覆盖**的删除被允许（即 GC prune 与 `DeleteBefore` 两条路径；两者在**同一事务内先写 Checkpoint 再删除**——这是应用层保证，SQL 触发器无法观测事务身份，放行条件为 `checkpoint.next_id > 被删行 id`）
- 不变量：每次 GC 前缀删除后，所有幸存行 `id >= 最新 checkpoint.next_id`，因此**任何现存行都无法被未锚定删除**
- 违反者返回 `audit: rows are immutable except via checkpoint-anchored GC`

### 5.3 保留与 GC

legacy 审计记录默认保留 **3 年**（`audit.retention: 26280h`）；生命周期 v2 新事实默认保留 **180 天**（`lifecycle.audit.facts_retention: 4320h`）。legacy 链依据当前配置和记录时间计算清理边界；v2 链在写入时保存并哈希保护每条记录的 `expires_at`，GC 只删除已到期的连续前缀。哈希链只能安全地剪除前缀，因此某条未到期记录会暂时挡住后续已到期记录的删除。修改 v2 配置只改变之后写入记录的期限，不会缩短或延长已存在记录。两条链分别运行 GC，不能通过缩短 legacy 保留期来代替 v2 清理。每条链的 GC 都在单个数据库事务中原子执行：

1. legacy 链根据 `audit.retention` 计算 cutoff；v2 链选择 `expires_at` 已到期的记录边界
2. 在写事务中找到该边界之前最后一条记录的 ID
3. **先在事务内写入对应链的 Checkpoint（锚定待删前缀），再删除**——顺序符合该链的删除触发器契约；若表被清空则追加一条 `LastSelfHash=""` 的修正 Checkpoint，使下一条记录成为新创世
4. 提交事务

> **SQLite** 通过 `writeMu` 串行化；**PostgreSQL** 的 legacy 与 v2 链分别使用 `pg_advisory_xact_lock(819207)` 和 `pg_advisory_xact_lock(819208)`，确保各链 GC 与写入不会产生竞态（C1/C2 race 已由单事务收口）。

### 5.4 后台 Verifier

Verifier 每小时自动运行一次，以**流式方式**逐批验证 Hash Chain 完整性（每批 1000 条，O(batchSize) 内存，不 OOM）。

**一次校验报告全部断裂点**（非短路）：游标在断裂后照常推进，同一轮即可列出所有断裂行（上限 50 条），避免"修复第一处后才暴露下一处"的掩盖问题。

检测到链断裂时：
- 记录 `hotplex_audit_chain_breaks_total` Prometheus 指标
- 日志按**状态机降噪**输出：新断裂（首次出现）输出 WARN，附带 `rows_checked`、`broken_count` / `broken_ids`（多断点时）、按 `reason` 映射的处置建议（`advice`）与首个断裂行诊断（`broken_at` / `platform` / `action` / `outcome` / `resource_type` / `expected_prev_hash` / `actual_prev_hash`，均为无 PII 字段，可安全外发）；同一断裂持续存在时降为 DEBUG 并累计 `first_seen` / `occurrences`，避免每小时告警风暴；断裂修复后输出 INFO `chain break resolved`
- 也可手动触发：`hotplex audit verify`（只读，输出全部断裂点与 `advice`）

```yaml
# 告警规则示例
- alert: AuditChainBroken
  expr: increase(hotplex_audit_chain_breaks_total[1h]) > 0
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: "审计 Hash Chain 完整性遭到破坏"
```

### 5.5 Zero-loss 保证

Collector 提供三级降级（channel → O_SYNC WAL spill → 有界阻塞），**常规 batch flush 失败时同样不丢事件**：`runWriter` 会把未落库的 batch 重新写入 spill 文件，等待下一次 drain 重放（与 spill-drain 路径的 re-spill 契约一致）。无 spill 配置时失败事件计入 `hotplex_audit_*_dropped_total`，保证丢失可见。

Collector 采用 3 级 Enqueue 策略确保事件不丢失：

| 级别 | 机制 | 说明 |
|------|------|------|
| Level 1 | 内存 Channel（cap 4096） | 非阻塞发送，成功即返回 |
| Level 2 | WAL Spill（O_SYNC） | Channel 满时溢出到磁盘，`fsync` 确保持久化 |
| Level 3 | Bounded Block（5s） | Spill 也失败时阻塞等待，最多 5 秒 |

Spill 文件使用二进制格式（4-byte BE length + JSON payload），崩溃后自动跳过截断记录并恢复。

---

## 6. PII 脱敏与数据安全

### 6.1 查询时 PII 脱敏

通过 API 查询审计记录时，默认对 PII 字段进行脱敏：

| 字段 | 脱敏规则 | 示例 |
|------|----------|------|
| IP（IPv4） | 末段置零 | `192.168.1.100` → `192.168.1.0` |
| IP（IPv6） | /64 后置零 | `2001:db8::abcd:1234` → `2001:db8::` |
| User-Agent | 截断至 50 字符 | `Mozilla/5.0 ...very long...` → `Mozilla/5.0 ...（前 50 字符）...` |

> 获取原始 PII 数据需要 `admin:write` 权限 + `?include_pii=true` 参数。

### 6.2 写入时凭证遮罩

当前 `tool.call` 不写入工具输入，因此新记录没有工具参数需要遮罩。升级前已经存在的旧审计行不会被重写；它们可能包含旧版记录的内容，并按 `audit.retention` 到期清理。

### 6.3 内容存储策略

| 事件类型 | 当前新写入的 detail 内容 | drill-down |
|----------|--------------------------|------------|
| `message.inbound` | `{}`，不含正文、摘要或预览 | `event_ref` 若存在仅关联 events/turns，并只在 `events.retention` 窗口内可用 |
| 交互响应 | 关联 ID、allow/deny 结果或受限 elicitation action | 不保存回答正文或原因 |
| `tool.call` | 受限工具名与调用状态 | 不保存工具参数、摘要或预览 |
| 其他标准事件 | 按各事件的字段白名单记录 | `event_ref` 若存在仅作短期运行事实关联 |

此前写入 legacy 链的旧审计行不会因升级自动脱敏或删除其中的内容；它们仍受 `audit.retention` 管理。新事实进入无正文的 v2 链。`audit.full_content_retention` 只是兼容字段，不提供独立正文清理。

### 6.4 CSV 导出安全

CSV 导出自动防护 **Formula Injection**（OWASP CWE-1236）：

- 以 `=`、`+`、`-`、`@`、Tab、CR 开头的单元格自动添加 `'` 前缀
- 例如 `=cmd|'/C calc'!A0` → `'=cmd|'/C calc'!A0`

---

## 7. 跨渠道身份追踪

### 7.1 问题场景

同一用户在不同平台有不同的标识符：

| 平台 | 标识符格式 | 示例 |
|------|-----------|------|
| WebChat | `users.id`（UUID） | `550e8400-e29b-41d4-a716-446655440000` |
| 飞书 | `open_id` | `ou_xxxxxxxxxxxxxxxx` |
| Slack | `user_id` | `UXXXXXXXX` |
| Cron | `owner_id` | 同 WebChat UUID |

当需要查询"某用户在所有平台的所有操作"时，需要 **Identity Link** 将这些分散的 ID 关联起来。

### 7.2 Identity Link 机制

Identity Link 通过 `principal_user_id`（主身份）将多个平台的身份绑定：

```
principal_user_id: 550e8400-e29b-41d4-a716-446655440000
  ├─ provider: feishu,    subject: ou_xxxxxxxx
  ├─ provider: slack,     subject: UXXXXXXXX
  └─ provider: webchat,   subject: 550e8400-e29b-41d4-a716-446655440000
```

查询时使用 `?principal_user_id=X`，系统自动展开为所有关联的 `subject` 值，一次性检索跨平台操作记录。

### 7.3 管理 API

```bash
# 列出所有身份链接
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:9999/admin/audit/identity-links

# 查询特定用户的所有链接
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:9999/admin/audit/identity-links?principal_user_id=550e8400-..."

# 创建身份链接
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "principal_user_id": "550e8400-...",
    "provider": "feishu",
    "subject": "ou_xxxxxxxx",
    "subject_type": "platform",
    "display_name": "张三",
    "email": "zhangsan@example.com"
  }' \
  http://localhost:9999/admin/audit/identity-links

# 删除身份链接
curl -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:9999/admin/audit/identity-links/{link_id}
```

> 身份链接的创建和删除操作本身也会被记录为 `admin.audit.identity_link.create` / `admin.audit.identity_link.delete` 审计事件。

### 7.4 跨渠道查询示例

```bash
# 查询某用户在所有平台的操作
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:9999/admin/activity?principal_user_id=550e8400-...&limit=50"
```

响应中包含 `resolved_user_ids` 字段，展示所有被关联的平台 ID：

```json
{
  "principal_user_id": "550e8400-...",
  "resolved_user_ids": ["550e8400-...", "ou_xxxxxxxx", "UXXXXXXXX"],
  "rows": [...],
  "identity_links": {...}
}
```

---

## 8. Admin 活动控制台

### 8.1 活动时间线 UI

Admin Panel（`/admin`）提供可视化活动时间线：

- **左侧导航** → **活动** 进入活动页面
- **筛选栏**：支持按时间范围、Action、Outcome、Platform、User ID、Principal User ID 过滤
- **分页**：支持分页浏览（默认每页 100 条）
- **详情抽屉**：点击行展开详情，包含 JSON 查看器、Platform 徽标、KV 对齐展示
- **导出按钮**：支持 JSON / CSV 格式导出

### 8.2 查询 API

**端点**：`GET /admin/activity`

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `from` | RFC3339 | — | 时间范围起点 |
| `to` | RFC3339 | — | 时间范围终点 |
| `action` | string | — | 精确匹配 action |
| `action_prefix` | string | — | 前缀匹配（如 `tool.` 查所有工具调用） |
| `outcome` | string | — | `success` / `failure` / `denied` |
| `platform` | string | — | `webchat` / `feishu` / `slack` / `admin` / `api` / `cron` |
| `session_id` | string | — | 精确匹配 Session ID |
| `resource_type` | string | — | `session` / `tool` / `bot` 等 |
| `user_id` | string | — | 精确匹配用户 ID（与 principal_user_id 互斥） |
| `principal_user_id` | string | — | 主用户 ID（展开所有关联身份） |
| `limit` | int | `100` | 每页条数（最大 1000） |
| `offset` | int | `0` | 分页偏移 |

```bash
# 查询最近 24 小时的失败认证
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:9999/admin/activity?action=auth.login&outcome=failure&from=$(date -u -d '1 day ago' +%Y-%m-%dT%H:%M:%SZ)"

# 查询所有工具调用（使用 action_prefix）
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:9999/admin/activity?action_prefix=tool.&limit=50"

# 查询特定用户的所有操作（包含 PII）
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:9999/admin/activity?principal_user_id=550e8400-...&include_pii=true"
```

### 8.3 统计 API

**端点**：`GET /admin/activity/stats`

返回聚合统计，用于仪表盘展示：

```bash
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:9999/admin/activity/stats?from=2026-07-01T00:00:00Z"
```

```json
{
  "total": 12345,
  "by_outcome": {
    "success": 12000,
    "failure": 300,
    "denied": 45
  },
  "by_platform": {
    "webchat": 8000,
    "feishu": 4000,
    "slack": 345
  }
}
```

### 8.4 导出 API

**端点**：`GET /admin/activity/export`（全局）/ `GET /admin/users/{id}/activity/export`（按用户）

| 参数 | 说明 |
|------|------|
| `format` | `json`（默认）或 `csv` |
| `from` / `to` | 时间范围过滤 |
| 其他参数 | 与查询 API 相同 |

```bash
# 导出 JSON 格式
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:9999/admin/activity/export?format=json&from=2026-07-01" \
  > audit-export-$(date +%Y%m%d).json

# 导出 CSV 格式
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:9999/admin/activity/export?format=csv" \
  > audit-export-$(date +%Y%m%d).csv
```

---

## 9. 外部集成（SIEM / SOC）

### 9.1 Webhook Sink

Webhook Sink 将审计事件实时推送到外部 SIEM/SOC 系统：

```yaml
audit:
  sinks:
    - name: splunk-siem
      type: webhook
      config:
        url: https://splunk.example.com/services/collector/event
        secret: "your-hmac-secret"
```

**签名验证**（接收端实现）：

```python
import hmac, hashlib

def verify_signature(body: bytes, signature_header: str, secret: str) -> bool:
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    received = signature_header.replace("sha256=", "")
    return hmac.compare_digest(expected, received)
```

请求头：
- `X-Audit-Event-Source: hotplex` — 标识来源
- `X-Audit-Signature: sha256=...` — HMAC-SHA256 签名（仅当配置了 secret 时）

### 9.2 Payload 格式

```json
{
  "event_id": "01912345-6789-7abc-def0-123456789abc",
  "timestamp": "2026-07-07T06:00:00.000Z",
  "action": "auth.login",
  "user_id": "550e8400-...",
  "user_id_type": "registered",
  "platform": "webchat",
  "session_id": "...",
  "resource_type": "",
  "resource_id": "",
  "outcome": "success",
  "detail_json": {},
  "event_ref": "",
  "ip": "192.168.1.0",
  "user_agent": "Mozilla/5.0 ..."
}
```

### 9.3 自定义 Sink 开发

第三方系统可通过 Go `init()` 注册自定义 Sink：

```go
package mysink

import (
    "context"
    "log/slog"
    "github.com/hrygo/hotplex/internal/audit/sinks"
)

func init() {
    sinks.Register("my_corp_syslog", func(cfg map[string]any, log *slog.Logger) (sinks.Sink, error) {
        address := cfg["address"].(string)
        return &SyslogSink{address: address, log: log}, nil
    })
}

type SyslogSink struct {
    address string
    log     *slog.Logger
}

func (s *SyslogSink) OnAuditEvent(_ context.Context, event sinks.AlertEvent) error {
    // 发送到 Syslog
    return nil
}
```

**扩展契约**：
- 非阻塞：处理超时后丢弃事件，不阻塞 Collector
- Panic 安全：Sink 内部的 panic 会被 Collector 恢复并记录
- 错误非致命：单次投递失败不中断审计写入链路

然后在配置中引用：

```yaml
audit:
  sinks:
    - name: corp-syslog
      type: my_corp_syslog
      config:
        address: "tcp://syslog.example.com:514"
```

### 9.4 Delivery 语义

| 特性 | 说明 |
|------|------|
| 投递保证 | At-most-once（事件已在 DB 持久化，Sink 失败不影响存储） |
| 顺序保证 | 同一 Sink 内按事件顺序投递 |
| 重试策略 | 3 次重试，指数退避（1s / 2s / 4s） |
| 溢出策略 | 队列满时丢弃 + `hotplex_audit_sink_failures_total` 指标递增 |

---

## 10. 配置变更审计

Config Watcher 自动记录所有运行时配置变更，包含完整的 diff 信息。

### 10.1 审计日志

```
ConfigChange{
  Timestamp: 2026-07-07T08:30:00Z,
  Field:     "pool.max_size",
  OldValue:  "100",
  NewValue:  "200",
  Hot:       true,              // 是否立即生效
}
```

- 审计日志上限 **256 条**，超出后 FIFO 裁剪
- 敏感字段（`security.api_keys`）自动脱敏为 `[REDACTED]`

### 10.2 配置历史与回滚

Watcher 维护 **64 个版本**的完整配置快照：

```bash
# 回滚到上一个版本
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:9999/admin/config/rollback?version=1
```

| 特性 | 说明 |
|------|------|
| 版本语义 | `version=1` 回退一步，`version=2` 回退两步 |
| 恢复方式 | 从内存快照恢复，不依赖磁盘文件 |
| 传播机制 | 回滚后通过 ConfigStore 原子传播给所有观察者 |

### 10.3 Hot vs Static 字段

| 类别 | 字段示例 | 生效方式 |
|------|----------|----------|
| Hot（立即生效） | `log.level`, `pool.*`, `worker.timeout`, `admin.tokens` | fsnotify + 500ms debounce |
| Static（需重启） | `gateway.addr`, `db.path`, `tls.*`, `audit.enabled` | 仅记录，下次重启生效 |

---

## 11. 凭证管理

### 11.1 原则：凭证不进配置文件

所有敏感凭证通过**环境变量**注入，Config struct 中敏感字段标记 `mapstructure:"-"`，永不从 YAML 读取：

| 凭证 | 环境变量 | 注入方式 |
|------|----------|----------|
| Admin Token | `HOTPLEX_ADMIN_TOKEN_1` / `_2` | 编号聚合 |
| API Key | `HOTPLEX_SECURITY_API_KEY_1` | 编号聚合 |
| Slack Token | `HOTPLEX_MESSAGING_SLACK_BOT_TOKEN` | 环境覆盖 |
| 飞书 Secret | `HOTPLEX_MESSAGING_FEISHU_APP_SECRET` | 环境覆盖 |

### 11.2 Worker 环境隔离

Worker 进程的 Environment 列表支持 `${VAR:-default}` 模板展开：

- 引用未设置且无默认值的变量 → 该条目自动排除（不注入空值）
- `Sensitive` 检测自动脱敏 `AWS_*`、`ANTHROPIC_*`、`SLACK_*` 等前缀变量

---

## 12. Admin Token 轮转

使用 `_1` / `_2` 后缀实现**零停机双 Token 轮转**：

```bash
# .env 文件
HOTPLEX_ADMIN_TOKEN_1=tk_current_xxx    # 当前活跃
HOTPLEX_ADMIN_TOKEN_2=tk_previous_yyy   # 前一代（过渡期）
```

**轮转步骤**：

1. 生成新 Token，设置到 `_2`
2. 所有客户端切换到 `_2` 的值
3. 将 `_1` 更新为新 Token
4. 清理旧 `_2` 值

两个 Token 同时有效，确保轮转期间无请求失败。

---

## 13. Prometheus 审计指标

审计系统暴露 5 个专属 Prometheus 指标（前缀 `hotplex_audit_`），详见 [Metrics 参考](../../reference/metrics.md)：

| 指标 | 类型 | 属性 | 说明 |
|------|------|------|------|
| `hotplex_audit_events_total` | Counter | `action`, `outcome` | 审计事件写入总数 |
| `hotplex_audit_chain_breaks_total` | Counter | `reason` | Hash Chain 完整性违规检测 |
| `hotplex_audit_spill_total` | Counter | `action` (spill_ok / spill_failed) | Spill-to-disk 事件 |
| `hotplex_audit_write_failures_total` | Counter | `action` (begin_tx / append / commit) | DB 写入失败 |
| `hotplex_audit_sink_failures_total` | Counter | `sink` (sink 类型名) | Sink 投递失败 |

### 告警规则示例

```yaml
groups:
  - name: hotplex-audit
    rules:
      # Hash Chain 完整性告警
      - alert: AuditChainBroken
        expr: increase(hotplex_audit_chain_breaks_total[1h]) > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "审计 Hash Chain 完整性遭到破坏，可能存在数据篡改"

      # Spill 频率过高（内存缓冲不足）
      - alert: AuditSpillRateHigh
        expr: rate(hotplex_audit_spill_total{action="spill_ok"}[5m]) > 10
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "审计 Spill 频率过高，建议增大 channel_cap"

      # DB 写入失败
      - alert: AuditWriteFailures
        expr: increase(hotplex_audit_write_failures_total[10m]) > 5
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "审计数据库写入持续失败，审计事件可能丢失"

      # Sink 投递失败
      - alert: AuditSinkDeliveryFailures
        expr: increase(hotplex_audit_sink_failures_total[10m]) > 50
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "审计 Sink 投递失败率过高"
```

---

## 14. 合规检查清单

- [ ] **审计系统**：确认 `audit.enabled: true`（默认开启），生产环境**禁止关闭**
- [ ] **Hash Chain 告警**：配置 `AuditChainBroken` 告警规则，确保链断裂在 5 分钟内通知
- [ ] **Spill 监控**：配置 `AuditSpillRateHigh` 告警，高并发场景适当调大 `channel_cap`
- [ ] **保留期**：根据合规要求分别调整 `audit.retention`（legacy 链默认 3 年）、`lifecycle.audit.facts_retention`（新链默认 180 天）与 `events.retention`（默认 30 天）；`audit.full_content_retention`（默认 90 天）仅为兼容字段
- [ ] **身份链接**：为跨平台用户创建 Identity Link，确保跨渠道审计查询完整
- [ ] **Sink 投递**：生产环境配置 Webhook Sink 接入 SIEM，不要使用 noop 默认值
- [ ] **导出归档**：定期导出审计数据（JSON/CSV）并归档，满足合规审查要求
- [ ] **PII 保护**：默认查询已自动脱敏，`include_pii=true` 需 `admin:write` 权限，审查权限分配
- [ ] **凭证安全**：所有敏感凭证通过环境变量注入，不在 `config.yaml` 中明文存储
- [ ] **Admin Token**：使用双 Token 轮转模式，建议月度轮转
- [ ] **Admin API 加固**：启用 IP 白名单 + Rate Limit，非本地地址启用 TLS
- [ ] **配置变更**：定期检查 Config Watcher 审计日志，关注非预期的配置修改
- [ ] **版本完整性**：定期运行 Verifier 检查（每小时自动运行，关注 chain_breaks 指标）
- [ ] **数据库迁移**：升级时确认审计相关迁移（023_user_activity、024_audit_identity_links）已执行
