<h1 align="center">HotPlex 网关</h1>

<p align="center">
  <strong>让每一种 AI Coding Agent，都能出现在团队工作的每一个入口。</strong>
</p>

<p align="center">
  HotPlex 是一个可自托管的统一网关，通过稳定一致的生产级接口，<br>
  将 AI Coding Agent 接入 Web、Slack、飞书和企业消息平台。
</p>

<p align="center">
  <strong>简体中文</strong> | <a href="README.md">English</a>
</p>

<p align="center">
  <a href="https://github.com/hrygo/hotplex/actions/workflows/ci.yml"><img src="https://github.com/hrygo/hotplex/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <img src="https://img.shields.io/badge/Version-v1.50.2-10B981?style=flat-square" alt="Version">
  <a href="https://github.com/hrygo/hotplex/blob/main/LICENSE"><img src="https://img.shields.io/badge/License-Apache%202.0-3B82F6?style=flat-square" alt="License"></a>
  <img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat-square&logo=go" alt="Go">
  <img src="https://img.shields.io/badge/Protocol-AEP%20v1-7C3AED?style=flat-square" alt="AEP v1">
  <a href="https://github.com/hrygo/hotplex/stargazers"><img src="https://img.shields.io/github/stars/hrygo/hotplex?style=flat-square" alt="Stars"></a>
</p>

<p align="center">
  <a href="#-快速开始"><strong>快速开始</strong></a> ·
  <a href="#-hotplex-适合哪些场景">使用场景</a> ·
  <a href="docs/index.md">完整文档</a> ·
  <a href="CHANGELOG.md">更新记录</a>
</p>

---

## 把 AI Coding Agent 变成团队共享能力

AI Coding Agent 很强，但它通常被锁在某个终端、某个厂商或某位开发者的工作流里。HotPlex 在 Agent 前提供稳定的统一网关，让同一套能力可以服务浏览器、团队聊天、自动化任务和企业内部平台，而不必为每个入口重新开发一遍 Agent 集成。

| 团队获得的能力 | 带来的价值 |
| :------------- | :--------- |
| **一个网关接入所有 Agent** | Claude Code、Codex CLI、OpenCode Server 和任意 ACP 兼容 Agent，共用统一的 AEP v1 接口。 |
| **在工作的地方直接使用** | 从内置 Web Chat、Slack、飞书或元芯调用 Agent，无需把每个问题搬回开发终端。 |
| **经得起真实工作的连续会话** | 确定性会话可恢复、长任务可流式返回，Agent 忙碌时也能接收用户继续补充的输入。 |
| **完全掌握自己的运行边界** | 在一个 Go 二进制内自托管认证、权限、审计、持久化、指标、追踪和生命周期管理。 |

## 🎯 HotPlex 适合哪些场景

### 团队聊天中的编码助手

让 Slack 或飞书群聊接入真正能完成工作的 Coding Agent。团队成员可以分析代码、继续追问、批准工具调用并接收流式结果，不必共享某位开发者的工作站。

### 随时可继续的远程开发

从内置 Web Chat 或移动端消息应用继续 Agent 会话。确定性 Session ID 和持久化历史让对话在网络中断、重连和服务重启之后仍保持连续。

### Agent 驱动的自动化

把自然语言需求变成一次性提醒、周期巡检或定时工程任务。HotPlex 使用指定 Agent 执行任务，并把结果自动投递回目标平台。

### 企业内部自托管网关

在用户和多种 Agent Runtime 之间建立一条可治理的统一边界，集中管理 API 认证、权限上限、Bot 配置、会话视图、审计记录和运行指标。

## ⚡ 快速开始

### 1. 安装 HotPlex

**macOS / Linux：**

```bash
curl -fsSL https://raw.githubusercontent.com/hrygo/hotplex/main/scripts/install.sh | sudo bash -s -- --latest
```

如需免 `sudo` 安装到当前用户目录，请改用 `--prefix ~/.local`。Windows、源码构建、Docker 和指定版本安装方式见[安装参考](INSTALL.md)。

**Windows（PowerShell 5.1+）：**

```powershell
Invoke-WebRequest -Uri https://raw.githubusercontent.com/hrygo/hotplex/main/scripts/install.ps1 -OutFile install.ps1
.\install.ps1 -Latest
```

### 2. 配置并检查环境

```bash
hotplex onboard
hotplex doctor
```

配置向导会检测本机已有的 Agent、生成本地配置、收集必要密钥，并按需启用 Slack 或飞书。需要调整已有安装时可以安全地再次运行。

### 3. 启动网关

```bash
hotplex gateway start -d
curl http://localhost:8888/health
```

在浏览器打开 **[http://localhost:8888](http://localhost:8888)**，即可使用内置 Web Chat；之后可以把同一个网关继续接入 Slack、飞书或元芯。

> [!TIP]
> 从源码构建时，克隆仓库后先运行 `make hooks`，再执行 `make quickstart`。完整流程见[贡献者环境搭建](docs/guides/contributor/development-setup.md)。

## 🏗️ 工作方式

HotPlex 把“用户在哪里对话”和“哪个 Agent 执行任务”分离开：

1. **客户端与消息平台**通过 WebSocket 或平台 Adapter 发送 AEP 事件。
2. **HotPlex Gateway**完成用户认证、会话与 Agent 策略解析、生命周期持久化，并通过背压控制和严格递增序号流式转发事件。
3. **Worker Adapter**把统一会话转换为 Claude Code、Codex CLI、OpenCode Server 或 ACP Agent 的原生协议。
4. 执行结果沿同一会话返回最初发起请求的浏览器、群聊或定时任务。

![HotPlex 架构](docs/assets/architecture.svg)

[Worker 与消息渠道可靠性审计](docs/architecture/worker-channel-reliability-audit-20260912.md)记录了生命周期所有权、取消语义、回归证据和仍需联调的能力边界。

## 🔌 集成能力

### 在用户工作的入口提供 Agent

| 渠道 | 使用体验 |
| :--- | :------- |
| **内置 Web Chat** | 内置 Next.js 界面，提供流式对话、工作区控制和管理后台。 |
| **Slack** | Socket Mode 接入，支持流式回复、斜杠命令、交互和文件工具。 |
| **飞书** | WebSocket 接入，支持交互卡片、命令、语音输入和语音摘要。 |
| **元芯** | 基于 Pulsar 的企业消息 Adapter，支持会话路由和定时结果投递。 |

### 为不同任务选择合适的 Agent

| Worker | 适合场景 |
| :----- | :------- |
| **Claude Code** | 完整 Coding Agent 会话、工具交互和执行中的继续追问。 |
| **Codex CLI** | 基于 Codex app-server 的流式会话和执行中的继续追问。 |
| **OpenCode Server** | 由网关托管生命周期的 OpenCode HTTP/SSE 长驻 Runtime。 |
| **ACP** | 通过 JSON-RPC 2.0 stdio 接入任意 Agent Client Protocol 兼容 Runtime。 |

Worker 可以按 Bot 或平台指定，其余场景继承部署级共享默认值。

## ✨ 最新版本：v1.50.2

- **停止语义真实可靠。** Gateway 等待 Worker run、连接和事件转发器完全静默后，才确认 `stopped_by_user`，避免旧输出污染下一轮会话。
- **跨 Worker 生命周期隔离。** ACP、Claude Code、Codex CLI 和 OpenCode Server 的停止、重试与共享单例清理统一经过 run 级屏障和 dispatch gate。
- **失败反馈可操作。** OpenCode 配额/限流失败返回明确错误码，WebChat 显示可执行的重试与凭据检查建议。

完整版本历史见[更新记录](CHANGELOG.md)。

## 🛡️ 为真实运维环境而设计

- **安全：** 时序安全 API Key 校验、权限上限、SSRF 与 DNS 重绑定防护、路径安全和 Worker 环境隔离。
- **持久化：** 确定性 Session ID、事件历史和生命周期数据，默认使用 SQLite，共享部署可切换 PostgreSQL。
- **可观测性：** 结构化 JSON 日志、Prometheus 指标、OpenTelemetry Trace 和 W3C TraceContext 传播。
- **运维：** 一个跨平台二进制提供 14 个顶层命令、27 项诊断检查、配置热更新、系统服务管理和自更新。
- **跨平台：** 支持 Linux、macOS 和 Windows，并提供原生的进程与系统服务生命周期管理。

## 🔗 SDK

| 语言 | 开始使用 |
| :--- | :------- |
| **Go** | [Go Client SDK](client/README.md) |
| **TypeScript** | [TypeScript Client](examples/typescript-client/README.md) |
| **Python** | [Python Client](examples/python-client/README.md) |
| **Java** | [Java Client](examples/java-client/README.md) |

四种客户端共用同一份 AEP v1 契约与一致性测试语料。协议细节见 [AEP 参考](docs/reference/aep-protocol.md)和[事件目录](docs/reference/events.md)。

## 📚 深入了解

| 目标 | 指南 |
| :--- | :--- |
| 五分钟完成首次启动 | [快速上手](docs/getting-started.md) |
| 接入团队聊天 | [Slack 集成](docs/tutorials/slack-integration.md) · [飞书集成](docs/tutorials/feishu-integration.md) |
| 配置 Worker 与平台 | [配置参考](docs/reference/configuration.md) |
| 部署和运维 HotPlex | [企业部署](docs/guides/enterprise/deployment.md) · [可观测性](docs/guides/enterprise/observability.md) |
| 集成自定义客户端 | [WebSocket 集成](docs/guides/developer/websocket-integration.md) · [AEP v1 协议](docs/reference/aep-protocol.md) |
| 自动执行周期任务 | [Cron 定时任务](docs/tutorials/cron-scheduled-tasks.md) |
| 管理网关 | [CLI 参考](docs/reference/cli.md) · [Admin API](docs/reference/admin-api.md) |

HotPlex 还会把中文优先的文档门户直接嵌入二进制；网关启动后可访问 `http://localhost:8888/docs`。

## 👥 参与贡献

欢迎贡献。请从 [CONTRIBUTING.md](CONTRIBUTING.md) 和[开发环境搭建](docs/guides/contributor/development-setup.md)开始，并在修改前运行 `make hooks` 安装仓库 Git Hooks。

## 🛡️ 安全

发现疑似安全漏洞时，请勿创建公开 Issue。请按 [SECURITY.md](SECURITY.md) 中的流程私下报告。

## 📜 开源协议

HotPlex 基于 [Apache License 2.0](LICENSE) 发布。
