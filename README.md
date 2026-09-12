<h1 align="center">HotPlex Gateway</h1>

<p align="center">
  <strong>Bring every AI coding agent to every place your team works.</strong>
</p>

<p align="center">
  HotPlex is a self-hosted gateway that connects AI coding agents to Web, Slack,<br>
  Feishu, and enterprise messaging through one consistent, production-ready interface.
</p>

<p align="center">
  <a href="README_zh.md">简体中文</a> | <strong>English</strong>
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
  <a href="#-quick-start"><strong>Quick Start</strong></a> ·
  <a href="#-where-hotplex-fits">Use Cases</a> ·
  <a href="docs/index.md">Documentation</a> ·
  <a href="CHANGELOG.md">Changelog</a>
</p>

---

## Turn AI coding agents into a shared team capability

AI coding agents are powerful, but they usually live inside one terminal, one vendor, or one developer's workflow. HotPlex puts a stable gateway in front of them so the same agents can serve a browser, a team chat, an automation, or an internal platform—without rebuilding the agent integration each time.

| What your team gets | Why it matters |
| :------------------ | :------------- |
| **One gateway for every agent** | Connect Claude Code, Codex CLI, OpenCode Server, or any ACP-compatible agent through the same AEP v1 interface. |
| **Access where work already happens** | Use agents from the embedded Web Chat, Slack, Feishu, or Yuanxin instead of moving every request back to a terminal. |
| **Conversations that survive real work** | Resume deterministic sessions, stream long-running turns, and accept follow-up input even while an agent is busy. |
| **Control you can operate yourself** | Self-host authentication, permissions, audit trails, persistence, metrics, tracing, and lifecycle management in one Go binary. |

## 🎯 Where HotPlex fits

### Team coding assistant

Give a Slack or Feishu channel access to a real coding agent. Teammates can investigate code, ask follow-up questions, approve tool use, and receive streamed results without sharing a developer workstation.

### Remote development access

Continue an agent session from the embedded Web Chat or a mobile messaging client. Deterministic session identity and persisted history keep the conversation connected across network drops and restarts.

### Agent-powered automation

Turn natural-language requests into one-time reminders, recurring checks, or scheduled engineering jobs. HotPlex runs them through the selected agent and delivers the result back to the target platform.

### Self-hosted enterprise gateway

Place one governed boundary between users and multiple agent runtimes. Centralize API authentication, permission ceilings, bot configuration, session visibility, audit records, and operational telemetry.

## ⚡ Quick Start

### 1. Install HotPlex

**macOS / Linux:**

```bash
curl -fsSL https://raw.githubusercontent.com/hrygo/hotplex/main/scripts/install.sh | sudo bash -s -- --latest
```

For a user-local installation without `sudo`, add `--prefix ~/.local` instead. See the [installation reference](INSTALL.md) for Windows, source builds, Docker, and version-pinned installs.

**Windows (PowerShell 5.1+):**

```powershell
Invoke-WebRequest -Uri https://raw.githubusercontent.com/hrygo/hotplex/main/scripts/install.ps1 -OutFile install.ps1
.\install.ps1 -Latest
```

### 2. Configure and verify

```bash
hotplex onboard
hotplex doctor
```

The setup wizard detects installed agents, creates the local configuration, collects required secrets, and optionally enables Slack or Feishu. It is safe to run again when you want to reconfigure an existing installation.

### 3. Start the gateway

```bash
hotplex gateway start -d
curl http://localhost:8888/health
```

Open **[http://localhost:8888](http://localhost:8888)** for the embedded Web Chat. The same gateway can then be connected to Slack, Feishu, or Yuanxin.

> [!TIP]
> Building from source? Clone the repository, run `make hooks`, then `make quickstart`. The full workflow is in the [contributor setup guide](docs/guides/contributor/development-setup.md).

## 🏗️ How it works

HotPlex separates the place where a user talks from the agent that performs the work:

1. **Clients and messaging platforms** send AEP events through WebSocket or a platform adapter.
2. **HotPlex Gateway** authenticates the user, resolves the session and agent policy, persists lifecycle state, and streams events with backpressure and ordered sequence numbers.
3. **Worker adapters** translate the unified session into the native protocol of Claude Code, Codex CLI, OpenCode Server, or an ACP agent.
4. Results flow back through the same session to the originating browser, chat, or scheduled task.

![HotPlex Architecture](docs/assets/architecture.svg)

See the [worker and channel reliability audit](docs/architecture/worker-channel-reliability-audit-20260912.md) for lifecycle ownership, cancellation semantics, regression evidence, and explicit integration limits.

## 🔌 Integrations

### Meet users where they work

| Channel | Experience |
| :------ | :--------- |
| **Embedded Web Chat** | Built-in Next.js interface with streaming conversations, workspace controls, and an admin console. |
| **Slack** | Socket Mode adapter with streamed replies, slash commands, interactions, and file tooling. |
| **Feishu** | WebSocket adapter with interactive cards, commands, voice input, and voice summaries. |
| **Yuanxin** | Pulsar-backed enterprise messaging adapter with session routing and scheduled-result delivery. |

### Choose the right agent for each workload

| Worker | Best fit |
| :----- | :------- |
| **Claude Code** | Full coding-agent sessions with tool interactions and mid-turn follow-up injection. |
| **Codex CLI** | Codex app-server sessions with streaming events and mid-turn follow-ups. |
| **OpenCode Server** | Long-lived OpenCode HTTP/SSE runtime managed behind the gateway. |
| **ACP** | Any Agent Client Protocol-compatible runtime over JSON-RPC 2.0 stdio. |

Worker choice can be set per bot or platform, with shared defaults for the rest of the deployment.

## ✨ Latest release: v1.50.2

- **真实停止语义。** Gateway 现在等待 Worker run、连接和事件转发器完全静默后，才确认 `stopped_by_user`，避免旧输出污染下一轮会话。
- **跨 Worker 生命周期隔离。** ACP、Claude Code、Codex CLI 和 OpenCode Server 的停止、重试与共享单例清理统一经过 run 级屏障和 dispatch gate。
- **可操作的失败反馈。** OpenCode 配额/限流失败会返回明确错误码，WebChat 显示可执行的重试与凭据检查建议。

See the [changelog](CHANGELOG.md) for the complete release history.

## 🛡️ Built for teams that run it

- **Security:** timing-safe API key checks, permission ceilings, SSRF and DNS-rebinding protection, path safety, and isolated worker environments.
- **Persistence:** deterministic session identity, event history, and lifecycle storage on SQLite by default or PostgreSQL for shared deployments.
- **Observability:** structured JSON logs, Prometheus metrics, OpenTelemetry traces, and W3C TraceContext propagation.
- **Operations:** one cross-platform binary with 14 top-level commands, 27 diagnostic checks, hot-reloadable configuration, service management, and self-update support.
- **Cross-platform:** Linux, macOS, and Windows support with native process and service lifecycle handling.

## 🔗 SDKs

| Language | Start here |
| :------- | :--------- |
| **Go** | [Go client SDK](client/README.md) |
| **TypeScript** | [TypeScript client](examples/typescript-client/README.md) |
| **Python** | [Python client](examples/python-client/README.md) |
| **Java** | [Java client](examples/java-client/README.md) |

All four clients consume the same AEP v1 contract and conformance corpus. Protocol details are available in the [AEP reference](docs/reference/aep-protocol.md) and [event catalog](docs/reference/events.md).

## 📚 Go deeper

| Goal | Guide |
| :--- | :---- |
| Get running in five minutes | [Getting Started](docs/getting-started.md) |
| Connect a team chat | [Slack Integration](docs/tutorials/slack-integration.md) · [Feishu Integration](docs/tutorials/feishu-integration.md) |
| Configure workers and platforms | [Configuration Reference](docs/reference/configuration.md) |
| Deploy and operate HotPlex | [Enterprise Deployment](docs/guides/enterprise/deployment.md) · [Observability](docs/guides/enterprise/observability.md) |
| Integrate a custom client | [WebSocket Integration](docs/guides/developer/websocket-integration.md) · [AEP v1 Protocol](docs/reference/aep-protocol.md) |
| Automate recurring work | [Cron Scheduled Tasks](docs/tutorials/cron-scheduled-tasks.md) |
| Manage the gateway | [CLI Reference](docs/reference/cli.md) · [Admin API](docs/reference/admin-api.md) |

HotPlex also serves its Chinese-first documentation portal directly from the binary at `http://localhost:8888/docs`.

## 👥 Contributing

Contributions are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md) and the [development setup guide](docs/guides/contributor/development-setup.md). Install the repository hooks with `make hooks` before making changes.

## 🛡️ Security

Do not open a public issue for a suspected vulnerability. Follow the private reporting process in [SECURITY.md](SECURITY.md).

## 📜 License

HotPlex is distributed under the [Apache License 2.0](LICENSE).

[Worker / 消息渠道独立缺陷台账](docs/issues/2026-09-runtime-audit/README.md)
