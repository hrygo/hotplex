# HotPlex 项目知识库

**最后更新**: 2026-09-05 · **分支**: main · **版本**: v1.50.2 · **提交**: d618b936

---

## 约定与规范

### 必须遵守

- **Mutex**: 显式 `mu` 字段，不嵌入，不传指针
- **错误**: `Err` 前缀（哨兵）、`Error` 后缀（自定义）、`fmt.Errorf("%w")` 包装
- **日志**: `log/slog` JSON handler（key 一律 snake_case，`sloglint` 强制 `no-mixed-args`）
- **测试**: `testify/require`、table-driven、`t.Parallel()`、单模块 ≤5s（`-count=1 -race -shuffle=on`）、禁止 `time.Sleep` 等待异步结果（改用 `require.Eventually` 或 channel 信号）
- **Worker 注册**: `init()` + `worker.Register()` 模式
- **Worker 接口变更**: 同步更新全部 adapter、测试 mock、`internal/worker/noop` 与 `registry_test`；新增能力须明确其在 CLI、单例和 RPC Worker 上的语义
- **AEP wire contract**: 修改 `pkg/events` 的 Kind、Data 或 JSON tag 时，同步更新 Go SDK、三种示例 SDK、`docs/reference/{aep-protocol,events}.md` 与双向协议测试；新增字段必须向后兼容
- **持久化输入**: Gateway 负责 accept → ACK → Worker 投递，`execution` 只保存 SHA-256 payload 指纹，绝不保存 prompt、metadata 值、凭证或原始 Worker 错误
- **数据库迁移**: SQLite 与 PostgreSQL migration 必须成对新增；同步补 migration 与跨实例/条件更新测试，禁止只修一套方言
- **关闭顺序**: signal → cancel ctx → tracing → hub → bridge → sessionMgr → HTTP
- **服务重启**: 必须使用 `hotplex service restart` 原子指令，禁止手动拆分 `stop && sleep && start`（仅二进制替换场景需手动 stop 等待）
- **非 main 分支 push**: 非 main 分支本地验证通过后直接 commit + push，无需询问用户确认
- **Git Hooks**: clone 后必须安装 hooks（底层目标为 `make hooks`；pre-commit 跑 gofmt + golangci-lint，pre-push 跑完整质量门禁），禁止跳过；`rtk` 可用时执行 `rtk proxy make hooks`，不可用时执行原生 `make hooks` 并说明回退原因
- **RTK 未过滤执行例外**：以下规则仅适用于执行环境中可用 `rtk`（可用 `command -v rtk` 或 `rtk --version` 确认）。`make docs-lint`、`make quality`、`make build`、`make hooks` 存在输出解析失败记录；`rtk` 可用时必须分别通过 `rtk proxy make docs-lint`、`rtk proxy make quality`、`rtk proxy make build`、`rtk proxy make hooks` 执行，以保留原始诊断输出。`rtk` 不可用时，允许执行对应的原生 `make` 命令，但必须说明未经过 RTK 过滤与统计；不得因本规则擅自安装 RTK。

### 反模式（禁止）

- ❌ `sync.Mutex` 嵌入或传指针
- ❌ `math/rand` 用于加密
- ❌ Shell 执行（仅允许 `claude` 二进制）
- ❌ 硬编码路径分隔符
- ❌ 直接使用 POSIX 信号
- ❌ 用 `sed`/`awk` 插入或修改源码行（缩进不可控，必须用 Edit 工具）

### 代码编辑规则

- **Edit 工具优先**：修改源码必须使用 Edit 工具，禁止用 `sed -i` 插入或修改代码行
- **Edit 匹配失败时**：重新 Read 文件获取精确内容，用精确字符串重试 Edit；扩大上下文使其唯一
- **Go 文件 tab 缩进**：Go 项目使用 tab 缩进（gofmt 标准）。使用 Edit 工具时，old_string 必须从 Read 输出中直接复制原文（保留 tab），禁止手敲空格缩进。Edit 匹配失败时先用 `cat -A` 确认实际空白字符
- **`sed` 适用场景**：仅限非代码操作（config 快速替换、日志过滤等简单唯一 token 替换）

### 独特风格

- **锁顺序**: `m.mu` → `ms.mu`（防止死锁）
- **背压**: 丢弃 `message.delta`，保留 `state`/`done`/`error`
- **Seq 分配**: Per-session 原子单调计数器
- **Turn 完整性**: turn 计数和 generation 从 event store 恢复；转发器冻结 Worker Conn，避免 `/reset` 或跨平台重连把事件写入过期连接
- **停止当前轮次**: AEP `control.stop` 只中断当前 Worker turn、保留 session；返回 `done.reason="stopped_by_user"`，且不得触发 crash fallback
- **输入 owner lease**: `accepted` 仅能前进至 `delivered` / `unknown` / `failed`；lease 过期须置为 `unknown` 并 fence，晚到的同一 Worker run 完成事件可收敛状态，不能自动重投
- **远端会话清理**: 删除会话时在同一持久化操作中写入 cleanup outbox；由 `CleanupRunner` 租约执行、指数退避，不能以同步远端删除阻塞或回滚本地生命周期
- **进程终止**: 3 层（SIGTERM → 等待 5s → SIGKILL）
- **Detached Restart**: `--detached` fork 独立 PGID helper，60s 冷却期防循环（`pid.go: restartMarker`）
- **Agent 配置**: B/C 双通道注入（通道结构、加载方式、fallback 规则见 [配置参考](#配置参考)）
- **元认知层**: `internal/agentconfig/META-COGNITION.md` 定义 Worker 的身份边界（不管理 Transport/状态/协议）、B/C 通道冲突隔离法则（directives 无条件覆盖 context）、配置替换的"命中即终止"机制、配置修改 SOP（禁止改全局来影响 Bot）
- **XML 安全**: 强制开启 **XML Sanitizer**，对保留标签进行 HTML 转义预防注入
- **Windows 注入**: 强制使用 **临时文件注入**（`--append-system-prompt-file`），严禁使用内联参数防止 cmd.exe 截断

---

## 项目结构

### 顶层目录

```
client/    - Go 客户端 SDK
webchat/   - Next.js Web Chat UI
examples/  - TS/Python/Java 客户端 SDK
docs/      - 自托管中文文档源文件（教程、指南、参考、架构设计）
scripts/   - 构建/校验脚本
configs/   - 配置文件
```

---

## 开发指南

### 新增组件

| 组件类型         | 位置                         | 说明                         |
| ---------------- | ---------------------------- | ---------------------------- |
| 新 AEP 事件类型  | `pkg/events/events.go`       | 添加 Kind 常量 + Data 结构体 |
| 新 Worker 适配器 | `internal/worker/<name>/`    | 嵌入 `base.BaseWorker`       |
| 新消息适配器     | `internal/messaging/<name>/` | 嵌入 `PlatformAdapter`       |
| 新诊断检查       | `internal/cli/checkers/`     | 实现 `Checker` 接口          |
| 新 cobra 子命令  | `cmd/hotplex/<name>.go`      | 在根命令注册                 |

### 修改已有组件

| 组件            | 文件                                                    | 说明                                                                                    |
| --------------- | ------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| Agent 配置      | `internal/agentconfig/loader.go`                        | 文件加载、大小限制                                                                      |
| Session 管理    | `internal/session/manager.go`                           | 状态机、原子操作                                                                        |
| WebSocket 协议  | `internal/gateway/conn.go`                              | ReadPump/WritePump                                                                      |
| LLM 重试        | `internal/gateway/llm_retry.go`                         | 可重试错误检测                                                                          |
| Worker 启动命令 | `configs/config.yaml`                                   | `claude_code.command` / `opencode_server.command` / `codex_cli.command` / `acp.command` |
| 路由注册        | `cmd/hotplex/routes.go`                                 | HTTP 路由                                                                               |
| 多 bot 配置     | `internal/config/config_types.go`                       | `SlackBotConfig`/`FeishuBotConfig`（normalize/propagation 见 `config_defaults.go`）     |
| Bot 状态 API    | `internal/admin/bot_handlers.go`                        | `BotListerProvider` + HTTP handlers                                                     |
| 输入投递可靠性  | `internal/gateway/handler.go` + `internal/execution/`   | durable accept、active gate、owner lease、终态 repair                                   |
| 远端会话清理    | `internal/session/cleanup_outbox.go`                    | 本地删除事务 + 异步 Worker 专属清理                                                     |
| AEP 控制或事件  | `pkg/events/events.go` + `internal/gateway/commands.go` | 兼容性、所有权与客户端/文档同步                                                         |

### 跨平台兼容

**平台分离**：
- 使用 `*_unix.go` / `*_windows.go` build tags
- CI 仅跑 Linux（ubuntu-latest）；macOS/Windows 依赖 release 交叉编译 + build tag 隔离验证（见 `release.yml` 6 矩阵）

### 可靠性变更检查清单

- **发现先落 Issue**：新增审计发现先登记 Issue（含基准提交、触发路径、证据等级与验收条件），再补失败回归和修复；区分 Source / Test / Live，修复提交和验证结果回填 Issue，增量交付到当前实施 PR，不能让发现仅保留在会话或临时环境中。

- **输入生命周期**：覆盖重复 ID、payload 冲突、active gate、Worker 投递失败、lease 过期与晚到 `done` 收敛；不能把 `unknown` 当作可安全重投。
- **会话删除**：验证本地状态变更与 cleanup task 原子入队；远端清理失败只能重试，不能复活或阻塞已删除会话。
- **事件顺序**：所有转发路径使用 per-session Seq；替换 Worker Conn 或 `/reset` 时旧 forwarder 不得处理新连接事件或触发 crash recovery。
- **可观测性**：新计数器/直方图通过访问器以 `sync.Once` 注册，并在 `docs/reference/metrics.md` 记录用户可查询的指标。

### PR Review 修复循环

自动化 review CI 已取消。push 后由开发者自行审查或请求人工 review。

```
push → 自行审查 / 请求人工 review → 一次性修 P0/P1 + 值得的 P2 → push → 重复
终止：review 对当前 HEAD 为 APPROVED 且无新 P0/P1
```

**修复前先核实**（review 可能针对旧 commit 或含误报）：① 发现指向的代码位置在当前 HEAD 仍存在；② 该发现未被后续 commit 修过。一轮 review 的所有发现**一次性修、一次 push**，避免逐条触发新轮次。

```bash
# 最新 review（commit_id 标明针对哪个 commit；先核对它 == 当前 HEAD）
gh api repos/hrygo/hotplex/pulls/{N}/reviews --jq 'max_by(.id) | {state, commit_id, body}'
```

**优先级**：P0/P1 必修 → P2 视价值修或提 Issue → P3 可忽略。

---

## 配置参考

### 配置文件

- Agent 配置目录：`~/.hotplex/agent-configs/`
- **B 通道**（`<directives>`，无条件生效）：
  - `<hotplex>` = `META-COGNITION.md`（go:embed，强制注入首位，不可排除）
  - `<persona>` = `SOUL.md`
  - `<rules>` = `AGENTS.md`
  - `<tool-guidance>` = `TOOLS.md`
- **C 通道**（`<context>`，可被 B 通道覆盖）：
  - `<user>` = `USER.md`
  - `<memory>` = `MEMORY.md`
- 三级 fallback：Bot（slack/<botName>/）→ 平台（slack/）→ 全局，每文件独立解析；缺失才继承，present-empty 显式清空并终止
- AgentConfig 只识别 `SOUL.md` / `AGENTS.md` / `TOOLS.md` / `USER.md` / `MEMORY.md`；Agent Skill 仅以 `.agents/skills/<name>/SKILL.md` 为入口，`TOOLS.md` 只承载常驻环境指导
- 内置 Skill canonical 来源为 `internal/skills/builtin/hotplex-cli` 与 `internal/skills/builtin/hotplex-operator`，生成的 `.agents/skills/hotplex-cli` 与 `.agents/skills/hotplex-operator` 必须 byte-identical；仓库 portfolio 还包含 `hotplex-diagnostics`、`hotplex-release`、`hotplex-docs-patrol`
- 配置热更新：仅在 session 初始化或 `/reset` 时加载，运行中修改不立即生效

### 配置陷阱（高发反直觉点）

- **`.env` 来源**: `make dev` / `scripts/dev.sh:16-18` `source` 的是 **repo-local `<project>/.env`**，**不是** `~/.hotplex/.env`。后者是运行实例的 home 配置（PID/db/agent-configs 父目录），**不被 dev.sh 加载**。如要调整 dev 行为，编辑 `<project>/.env`；`~/.hotplex/.env` 仅供生产/服务安装路径使用。
- **Worker 解析 5 级 fallback**（`internal/config/config_defaults.go:propagatePlatform` + `:210-211`）：
  1. `bots[].worker_type`（per-bot YAML，单 bot 模式下不可用）
  2. `feishu.worker_type`（YAML 平台级，dev → `configs/config-dev.yaml:47`，base → `configs/config.yaml:318`）
  3. `HOTPLEX_MESSAGING_FEISHU_WORKER_TYPE`（env 平台级，`.env:74`，env 覆盖 YAML）
  4. `messaging.worker_type`（YAML 共享默认，`configs/config.yaml:276`）
  5. 编译默认 `claude_code`（`config_defaults.go:127`）
- **`inject_exclude` 边界**：5 个可排除槽位 `SOUL.md` / `AGENTS.md` / `TOOLS.md` / `USER.md` / `MEMORY.md`。`META-COGNITION.md` 是 `go:embed` **强制注入首位，无法被排除**（Worker 身份边界）。3 级 fallback：bot > platform > global；nil 继承父级，`[]string{}` 显式清空。
- **dev YAML vs home YAML**: `configs/config-dev.yaml` 通过 `inherits: config.yaml` 覆盖基础，是 dev-only 覆盖层；`~/.hotplex/config.yaml` 是运行实例配置（影响服务安装路径）。两者**独立**，不互通。
- **dev 数据库真源**: dev 栈 DB 不依赖 repo `.env` 的 `HOTPLEX_DB_*`（该机制仍受支持但本机已停用）。本机 PG 固化在 gitignored `configs/config-dev.local.yaml`；`scripts/dev.sh` 解析顺序 = 显式 `$CONFIG` > `config-dev.local.yaml`（存在时）> `config-dev.yaml`。任何进程要操作 dev 栈数据库必须 `--config "$(./scripts/dev.sh config)"`（admin 系子命令仅支持长格式 `--config`，gateway 支持 `-c`），否则会按默认链路落到 `~/.hotplex/data/hotplex.db`（SQLite）造成双库分裂——症状是 webchat 一直提示创建管理员（bootstrap-status 查的是网关实际连接的库）。`hotplex admin create` 成功输出带 `[db=<driver>]` 可直接核对写入目标。
- **Admin 后台双通道鉴权**（issue #788）：`/admin/*`（Bearer+scope，`AdminAPI.Middleware`）与 `/api/admin/*`（cookie session，`UserAdminHandlers.requireAdmin`）是**两套独立 handler**，不是同一端点的两种认证。`/admin/*` 的 `Middleware` 支持 cookie fallback——无 Bearer 时回落 chat session cookie（校验 `role==admin && status==active`，注入全 scope），使内嵌 webchat admin 免另填 admin token；远程运维仍走 Bearer。SetCookieFallback 在 `routes.go` lap 创建后注入，nil 时回 Bearer-only。Admin 写操作（POST/PUT/PATCH/DELETE）由 middleware 级 `admin_audit` slog 统一记录（动作枚举 `internal/admin/audit.go`，actor=uid 或 `admin-token`）；`/api/admin/*` 写操作在各 handler 成功路径显式调用 `AdminAudit`。
- **`log.file` 文件日志 + 轮转**（`cmd/hotplex/gateway_run.go:buildLogWriter`）：`log.file.enabled` **默认 false**（仅写 stderr，行为不变）。启用后日志经 lumberjack 轮转写入文件（默认 `~/.hotplex/logs/gateway.log`，可 `log.file.path` 覆盖）。前台/TTY 模式同时 tee 到 stderr 便于调试；**daemon/service 模式 stderr 非 TTY 时自动抑制**，避免 daemon 已把 stderr 重定向到同名文件导致双写。`log.file.*` 全部为 **static**（改路径/轮转参数需重启，运行时重建 lumberjack writer 不安全）。`HOTPLEX_HOME` 环境变量覆盖 `config.HotplexHome()` 解析的默认根目录（主要供测试与非标准安装路径）；同时决定默认配置文件路径 `DefaultConfigPath()` = `$HOTPLEX_HOME/config.yaml`（未设置时 `~/.hotplex/config.yaml`）——设置后整个 workspace（配置+数据+日志+PID）整体迁移。

---

## 命令参考

### 命令速查

```bash
# 首次环境配置
cp configs/env.example .env   # 编辑填入 API 密钥
make quickstart               # check-tools + build + test-short
make dev                      # 启动开发环境（gateway + webchat）

# 所有 make 目标（build/test/lint/coverage/dev/gateway 等）的完整列表与说明
make help
```

### Slack / Cron CLI 示例

Slack（send-message / upload-file / bookmark / react 等）与 Cron（create / list / trigger / history 等）的详细命令示例见 skill `hotplex-cli`（`.agents/skills/hotplex-cli/SKILL.md`；Claude 目录为逐项软链接）——按需加载，不常驻上下文。

---

## 附录

### 符号链接

- `CLAUDE.md` ← `AGENTS.md`（只编辑 AGENTS.md）
- `.claude` ← `.agents`

### 重要限制

- 无 `api/` 目录（使用 JSON over WebSocket）
- PostgreSQL 支持已实现（`db.driver: "postgres"`），SQLite 仍为默认
- ACP 适配器已实现（JSON-RPC 2.0 over stdio）
- Windows 自更新不支持（exe 运行时被锁，使用 `scripts/install.ps1` 替代）

### 本批独立Issue文档

用户授权先以 `docs/issues/2026-09-runtime-audit/D*.md` 保存Issue，再修复；该目录是本批发现真相源。每份记录基准、Source/Test/Live、复现、验收、修复提交和PR。用户已确认此批十项独立问题统一通过PR #986增量交付；逐项保留修复和验证记录，不再要求十个PR，不将原已完成问题重复计数。
