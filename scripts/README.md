# HotPlex Worker Gateway - Scripts

This directory contains installation and deployment scripts for HotPlex Worker Gateway.

## Overview

| Script | Purpose | Usage |
|--------|---------|-------|
| `install.sh` | Full production installation | `sudo ./scripts/install.sh` |
| `quickstart.sh` | Quick dev environment setup | `./scripts/quickstart.sh` |
| `docker-build.sh` | Build Docker image | `./scripts/docker-build.sh` |
| `uninstall.sh` | Complete uninstallation | `sudo ./scripts/uninstall.sh` |
| `validate-acpx-spec.sh` | Validate ACPX spec via acpx CLI | `./scripts/validate-acpx-spec.sh` |
| `verify_worker_native_commands.py` | Run bounded Live probes for four Worker native command paths | `python scripts/verify_worker_native_commands.py` |
| `ci/select_go_tests.py` | Select the Go packages affected by a change set | `python scripts/ci/select_go_tests.py --full` |
| `ci/cloud_delivery.py` | Deliver an explicit Issue through Codex, validation and a draft PR | `python scripts/ci/cloud_delivery.py run --issue N` |
| `hotplex.service` | Systemd service unit | Install via `install.sh` |

## Installation Scripts

For cloud task delivery, see [the pipeline guide](ci/CLOUD_DELIVERY.md). The
runner reuses this repository's hooks and CI and keeps failed runs inspectable.

### install.sh

**Production installation script** that:

- Checks system dependencies (Go 1.21+, OpenSSL)
- Builds binary with version injection
- Creates directory structure (`/etc/hotplex`, `/var/lib/hotplex`, `/var/log/hotplex`)
- Generates secrets (admin tokens)
- Generates TLS certificates (self-signed or Let's Encrypt integration)
- Creates configuration file
- Installs systemd service (Linux)
- Creates environment file examples

**Usage:**

```bash
# Interactive installation (prompts for configuration)
sudo ./scripts/install.sh

# Non-interactive (uses defaults)
sudo ./scripts/install.sh --non-interactive

# Development mode (self-signed certs, relaxed security)
sudo ./scripts/install.sh --dev

# Custom directories
sudo ./scripts/install.sh \
  --prefix /opt/hotplex \
  --config-dir /opt/hotplex/config \
  --data-dir /data/hotplex

# Install systemd service
sudo ./scripts/install.sh --systemd
```

**What it creates:**

```
/usr/local/bin/hotplex           # Binary
/etc/hotplex/
  ├── config.yaml                       # Main config
  ├── secrets.env                       # Secrets (tokens)
  ├── config.env.example                # Environment template
  └── tls/
      ├── server.crt                    # TLS certificate
      └── server.key                    # TLS private key
/var/lib/hotplex/                       # Data directory (SQLite)
/var/log/hotplex/                       # Log directory
/etc/systemd/system/hotplex.service  # Systemd unit
```

**Post-installation:**

```bash
# Load secrets
source /etc/hotplex/secrets.env

# Start service
systemctl start hotplex

# Check status
systemctl status hotplex

# View logs
journalctl -u hotplex -f

# Test health
curl http://localhost:9999/admin/health
```

### quickstart.sh

**Quick development setup** that:

- Builds binary
- Creates minimal dev config
- Generates dev secrets
- Offers to start gateway immediately

**Usage:**

```bash
./scripts/quickstart.sh
```

**What it creates:**

```
.dev/
  ├── config.yaml          # Dev config
  └── data/
      └── hotplex.db       # SQLite database
```

**Dev mode features:**

- Any API key header value accepted
- TLS disabled
- Admin IP whitelist disabled
- Relaxed security settings

### docker-build.sh

**Build Docker image** with:

- Multi-stage build (minimal final image)
- Version injection (Git SHA, build time)
- Platform-specific builds
- Optional push to registry

**Usage:**

```bash
# Build with default tag
./scripts/docker-build.sh

# Custom tag
./scripts/docker-build.sh hotplex:v1.1.0

# Build and push
./scripts/docker-build.sh hotplex:latest --push

# Build without cache
./scripts/docker-build.sh --no-cache

# Multi-platform build
./scripts/docker-build.sh hotplex:latest --platform linux/amd64
```

**Run container:**

```bash
# Development
docker run -p 8080:8888 -p 9080:9999 \
  hotplex:latest

# With custom config
docker run -p 8080:8888 -p 9080:9999 \
  -v /path/to/config.yaml:/etc/hotplex/config.yaml \
  hotplex:latest

# With TLS
docker run -p 8443:8443 -p 9080:9999 \
  -v /path/to/tls.crt:/etc/hotplex/tls/server.crt \
  -v /path/to/tls.key:/etc/hotplex/tls/server.key \
  hotplex:latest
```

### hotplex.service

**Systemd service unit** for Linux systems.

**Features:**

- Automatic restart on failure
- Security hardening (no new privileges, private tmp, etc.)
- Resource limits (65536 file descriptors)
- Graceful shutdown (30s timeout)
- Watchdog support (30s)
- Journal logging

**Installation:**

```bash
sudo cp scripts/hotplex.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable hotplex
sudo systemctl start hotplex
```

**Management:**

```bash
# Start
sudo systemctl start hotplex

# Stop
sudo systemctl stop hotplex

# Restart
sudo systemctl restart hotplex

# Status
sudo systemctl status hotplex

# Logs
sudo journalctl -u hotplex -f

# Resource usage
systemctl show hotplex -p MemoryCurrent,CPUUsageNSec
```

### ~~validate-acpx-spec.sh~~ (archived — superseded by ACP-Worker-Spec.md)

**ACPX spec validation script** that validated Worker-ACPX-Spec.md against actual acpx CLI behavior.
The Worker-ACPX-Spec.md has been superseded by [ACP-Worker-Spec.md](../docs/specs/ACP-Worker-Spec.md) which uses direct stdio connection instead of acpx middleware.

**Purpose:**

- Validates JSON-RPC 2.0 protocol format
- Checks initialization handshake flow
- Verifies streaming events (agent_thought_chunk, agent_message_chunk, usage_update)
- Tests tool call events (tool_call, tool_call_update)
- Validates named session management
- Tests resume flow with context preservation
- Checks error handling format

**Requirements:**

- acpx CLI installed (`npm install -g acpx`)
- Claude Code CLI configured (`claude` command available)
- Valid Claude API key

**Usage:**

```bash
# Run all validation checks
./scripts/validate-acpx-spec.sh

# Output shows pass/fail for each test
# Summary includes overall validation status
```

### verify_worker_native_commands.py

Runs direct, bounded Live probes against installed Claude Code, OpenCode Server,
Codex CLI, and Hermes ACP. The script creates an isolated temporary Skill,
emits a sanitized JSON report, cleans up all started processes, and distinguishes
product failures from credential or provider blocks.

```bash
python -m unittest scripts/test_verify_worker_native_commands.py -v
python scripts/verify_worker_native_commands.py
python scripts/verify_worker_native_commands.py --worker codex_cli --codex-model gpt-5.6-sol
```

Exit codes are `0` when every selected Worker passes, `1` for a protocol or
assertion failure, and `2` when no failure occurs but an environment block is
present.

### ci/select_go_tests.py

Computes the reverse dependency closure over runtime, internal-test and
external-test imports. The CI Test job feeds NUL-separated `git diff` paths and
uses the result for coverage/sharding; packages previously excluded from that
set are returned separately so they still receive a dedicated test run.

```bash
python -m unittest discover -s scripts/ci -p 'test_*.py' -v
git diff --name-only -z origin/main...HEAD |
  python scripts/ci/select_go_tests.py --repo-root .
```

**What it validates:**

1. **Protocol Format** - JSON-RPC 2.0 structure, Request/Response matching
2. **Initialization Flow** - initialize, session/new, session/prompt
3. **Streaming Events** - Thought/message streams, usage updates
4. **Tool Calls** - Tool call lifecycle, identifiers, input/output
5. **Session Management** - Named sessions, listing, closing
6. **Error Handling** - JSON-RPC error format

**Sample output:**

```
🔍 ACPX Spec 功能快速检查
================================

✅ 检查 1: JSON-RPC 2.0 协议格式
   ✓ JSON-RPC 2.0 格式正确

✅ 检查 2: 初始化流程
   ✓ session/prompt 方法存在

...

================================
✅ 快速检查完成

📊 详细验证报告: docs/specs/ACPX-Validation-Report.md
📄 Spec 文档: docs/specs/Worker-ACPX-Spec.md
🎯 总体置信度: 98%
```

**Validation report:**

After running, see `docs/specs/ACPX-Validation-Report.md` for detailed results.


### ~~validate_opencode_spec.sh~~ (archived — OpenCode CLI adapter removed)

**Purpose:**

- Validates CLI parameter definitions (run, --format, --session, etc.)
- Checks environment variable whitelist
- Compares output format (NDJSON event types)
- Analyzes event type mappings
- Identifies implementation gaps

**Requirements:**

- OpenCode source code at `~/opencode`
- Spec document at `docs/archive/Worker-OpenCode-CLI-Spec.md (archived)`
- Standard Unix tools (grep, jq)

**Usage:**

```bash
# Run full validation

# Output includes:
# - CLI parameter verification (✅ confirmed, ⚠️ pending, ❌ missing)
# - Environment variable check
# - Output format analysis
# - Event type comparison
# - Test command suggestions
```

**What it validates:**

1. **CLI Parameters** - Checks if parameters in spec exist in source code
2. **Environment Variables** - Verifies env whitelist implementation
3. **Output Format** - Analyzes JSON event output structure
4. **Event Types** - Compares spec event types with actual implementation
5. **Implementation Status** - Tracks ✅ confirmed, ⚠️ pending, ❌ missing items

**Sample output:**

```
=== Spec 验证工具 ===

[1/5] 检查文件存在性
✓ Spec 文件存在
✓ 源码存在

[2/5] 验证 CLI 参数
✓ run
? --format json (未在源码中找到)
✓ --session
✓ --continue
? --resume (未在源码中找到)

统计: 3 个参数已确认, 17 个参数待验证

[3/5] 验证环境变量
? OPENAI_API_KEY (未在源码中找到)
? OPENCODE_API_KEY (未在源码中找到)

[4/5] 验证输出格式
✓ JSON 格式输出已实现

源码中定义的事件类型:
  • error
  • reasoning
  • step_finish
  • step_start
  • text
  • tool_use

Spec 中定义的事件类型:
  • step_start
  • message
  • message.part.delta
  • tool_use
  • tool_result
  • step_end
  • error
  • system
  • session_created

=== 验证完成 ===
```

**Analysis report:**



### ~~Output testing script~~ (archived — OpenCode CLI adapter removed)

Script that ran actual CLI commands and captured output for analysis.

**Purpose:**

- Tests actual JSON output format
- Validates event type mappings
- Checks session ID extraction
- Tests tool usage patterns
- Verifies error handling
- Compares JSON vs default format

**Requirements:**

- installed at `~/opencode`
- `bun` runtime
- `jq` for JSON parsing
- Valid API keys (OPENAI_API_KEY or OPENCODE_API_KEY)

**Usage:**

```bash
# Run all tests

# Run specific test

# Output saved to test-output/ directory
```

**Test cases:**

1. **Basic Output** - Simple text response, event type analysis
2. **Tool Usage** - List files command, tool_use event validation
3. **Error Handling** - Invalid file read, error event format
4. **Session Management** - Session ID extraction, step_start analysis
5. **Environment Injection** - HOTPLEX_SESSION_ID injection test
6. **Format Comparison** - JSON vs default output comparison

**Sample output:**

```
=== 输出测试 ===

[Test 1] 基本文本输出
运行: bun run opencode run --format json 'Reply with: Hello, World!'
... (CLI output)

输出已保存到: test-output/basic_output_20260404_123456.jsonl

=== 输出分析 ===
事件类型统计:
      2 error
      1 reasoning
      1 step_finish
      1 step_start
      1 text
      1 tool_use

第一个 step_start 事件:
{
  "type": "step_start",
  "timestamp": 1712234567890,
  "sessionID": "sess_abc123",
  "part": { ... }
}

Session ID:
  sess_abc123
  长度: 12
  格式: UUID-like
```

**Output files:**

```
test-output/
├── basic_output_20260404_123456.jsonl
├── tool_usage_20260404_123457.jsonl
├── error_handling_20260404_123458.jsonl
├── session_test_20260404_123459.jsonl
├── env_test_20260404_123500.jsonl
└── format_json_20260404_123501.jsonl
```

**Analysis commands:**

```bash
# Check event types
jq -r '.type' test-output/basic_output_*.jsonl | sort | uniq -c

# Extract session IDs
jq -r '.sessionID' test-output/*.jsonl | sort -u

# View step_start events
jq 'select(.type == "step_start")' test-output/*.jsonl

# Compare with spec
diff <(jq '.' test-output/basic_output_*.jsonl) expected_output.json
```

## Docker Compose

### docker-compose.yml

**Development compose file** with:

- HotPlex Worker Gateway
- Optional Prometheus (monitoring profile)
- Optional Grafana (monitoring profile)

**Usage:**

```bash
# Start gateway only
docker-compose up -d

# Start with monitoring stack
docker-compose --profile monitoring up -d

# View logs
docker-compose logs -f gateway

# Stop
docker-compose down

# Stop and remove volumes
docker-compose down -v
```

**Environment variables:**

```bash
# Required
export HOTPLEX_ADMIN_TOKEN="your-admin-token"

# Optional
export HOTPLEX_LOG_LEVEL="info"
export TZ="UTC"
```

### docker-compose.prod.yml

**Production override** with:

- TLS enabled
- Traefik reverse proxy
- Let's Encrypt certificates
- Stricter resource limits
- External monitoring

**Usage:**

```bash
# Production deployment
docker-compose -f docker-compose.yml -f docker-compose.prod.yml up -d

# Create Traefik network (first time only)
docker network create traefik-network
```

**Production checklist:**

- [ ] Set strong `HOTPLEX_ADMIN_TOKEN`
- [ ] Configure `GRAFANA_PASSWORD`
- [ ] Update Traefik dashboard host (`traefik.hotplex.dev`)
- [ ] Update gateway hosts (`gateway.hotplex.dev`, `admin.hotplex.dev`)
- [ ] Set up external Prometheus/Grafana (remove monitoring profile)

## Security Best Practices

### Secrets Management

**Development:**

```bash
# Use secrets.env (generated by install.sh)
source /etc/hotplex/secrets.env
```

**Production:**

```bash
# Option 1: Vault
export HOTPLEX_ADMIN_TOKEN=$(vault read -field=admin_token secret/hotplex)

# Option 2: Kubernetes Secrets
envFrom:
  - secretRef:
      name: hotplex-secrets

# Option 3: Docker Swarm Secrets
secrets:
  - hotplex_admin_token
```

### TLS Certificates

**Development (self-signed):**

```bash
./scripts/install.sh --dev
```

**Production (Let's Encrypt):**

```bash
# Use docker-compose.prod.yml with Traefik
# Certificates are automatically managed
```

**Manual:**

```bash
# Generate CSR
openssl req -new -newkey rsa:2048 -nodes \
  -keyout /etc/hotplex/tls/server.key \
  -out /tmp/hotplex.csr \
  -subj "/C=US/ST=State/L=City/O=HotPlex/CN=gateway.hotplex.dev"

# Get certificate from CA
# Then place at /etc/hotplex/tls/server.crt
```

### File Permissions

```bash
# Config files (read-only for service user)
chmod 644 /etc/hotplex/config.yaml

# Secrets (read-write for service user only)
chmod 600 /etc/hotplex/secrets.env
chown hotplex:hotplex /etc/hotplex/secrets.env

# TLS key (read-only for service user)
chmod 600 /etc/hotplex/tls/server.key
chown hotplex:hotplex /etc/hotplex/tls/server.key

# Data directory
chmod 750 /var/lib/hotplex
chown -R hotplex:hotplex /var/lib/hotplex

# Log directory
chmod 750 /var/log/hotplex
chown -R hotplex:hotplex /var/log/hotplex
```

## Troubleshooting

### install.sh fails with "permission denied"

```bash
# Run with sudo
sudo ./scripts/install.sh
```

### Binary not found after install

```bash
# Check PATH
which hotplex

# Add to PATH if needed
export PATH=$PATH:/usr/local/bin
```

### Systemd service won't start

```bash
# Check logs
journalctl -u hotplex -n 50

# Verify secrets file exists
ls -la /etc/hotplex/secrets.env

# Verify config is valid
hotplex -config /etc/hotplex/config.yaml -validate
```

### Docker container unhealthy

```bash
# Check container logs
docker logs hotplex

# Check health check output
docker inspect hotplex | jq '.[0].State.Health'

# Run health check manually
docker exec hotplex curl -f http://localhost:9999/admin/health
```

### Port already in use

```bash
# Find process using port
lsof -i :8888
lsof -i :9999

# Kill process or change ports in config
```

## Additional Resources

- **Quick Start Guide**: `docs/User-Manual.md`
- **Reference Manual**: `docs/Reference-Manual.md`
- **Configuration Guide**: `docs/management/Config-Management.md`
- **Admin API**: `docs/management/Admin-API-Design.md`
- **Security**: `docs/security/Security-Authentication.md`
