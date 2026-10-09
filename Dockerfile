# HotPlex Worker Gateway - Production Dockerfile
#
# Multi-stage build inspired by hotplex-legacy/docker/Dockerfile.base patterns:
#   - Build stage with full Go toolchain
#   - AI Tools stage for pre-installed Claude Code / OpenCode binaries
#   - Runtime stage with debian:bookworm-slim
#   - Proxy/mirror support for Chinese users
#   - Non-root user, health check, security hardening

# ─────────────────────────────────────────────────────────────────────────────
# Stage 1: Build
# ─────────────────────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG GIT_SHA
ARG BUILD_TIME
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

RUN apk add --no-cache git make ca-certificates tzdata

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w \
    -X main.version=${GIT_SHA} \
    -X main.buildTime=${BUILD_TIME}" \
    -o /build/bin/hotplex \
    ./cmd/hotplex

# ─────────────────────────────────────────────────────────────────────────────
# Stage 2: AI Tools Collector
# ─────────────────────────────────────────────────────────────────────────────
FROM alpine:3.21 AS ai-tools-collector

ARG GITHUB_PROXY

RUN apk add --no-cache curl ca-certificates

RUN arch=$(uname -m) && \
    case "$arch" in \
        x86_64) cl_platform="linux-x64"; op_arch="x86_64" ;; \
        aarch64) cl_platform="linux-arm64"; op_arch="arm64" ;; \
    esac && \
    GCS_BUCKET="https://storage.googleapis.com/claude-code-dist-86c565f3-f756-42ad-8dfa-d59b1c096819/claude-code-releases" && \
    VERSION=$(curl -fsSL "$GCS_BUCKET/latest") && \
    curl -fsSL -o /usr/local/bin/claude "$GCS_BUCKET/$VERSION/$cl_platform/claude" && \
    chmod +x /usr/local/bin/claude && \
    (curl -sSL "${GITHUB_PROXY}https://github.com/opencode-ai/opencode/releases/latest/download/opencode-linux-$op_arch.tar.gz" || \
     curl -sSL "https://github.com/opencode-ai/opencode/releases/latest/download/opencode-linux-$op_arch.tar.gz") | tar xz -C /usr/local/bin opencode && \
    chmod +x /usr/local/bin/opencode

# ─────────────────────────────────────────────────────────────────────────────
# Stage 3: Runtime
# ─────────────────────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

ARG GIT_SHA
ARG BUILD_TIME
ARG HOST_UID=1000
ARG GITHUB_PROXY
ARG DEBIAN_MIRROR

LABEL org.opencontainers.image.title="HotPlex Worker Gateway"
LABEL org.opencontainers.image.description="AI Coding Agent access layer"
LABEL org.opencontainers.image.version="1.52.0"
LABEL org.opencontainers.image.source="https://github.com/hotplex/hotplex"
LABEL org.opencontainers.image.revision="${GIT_SHA}"
LABEL org.opencontainers.image.created="${BUILD_TIME}"
LABEL org.opencontainers.image.vendor="HotPlex Team"
LABEL org.opencontainers.image.authors="support@hotplex.dev"

# 1. OS baseline with mirror support
RUN if [ "$DEBIAN_MIRROR" != "deb.debian.org" ] && [ -n "$DEBIAN_MIRROR" ]; then \
        sed -i "s/deb.debian.org/$DEBIAN_MIRROR/g" /etc/apt/sources.list.d/debian.sources; \
    fi && \
    apt-get update && apt-get install -y --no-install-recommends \
    curl wget ca-certificates git make bash jq gettext-base \
    procps openssl unzip gnupg \
    python3 python3-pip python3-venv \
    sqlite3 postgresql-client \
    && rm -rf /var/lib/apt/lists/*

# Node.js from official image (avoids deprecated NodeSource setup script)
COPY --from=node:24-bookworm-slim /usr/local/bin/node /usr/local/bin/node
COPY --from=node:24-bookworm-slim /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm

# 2. Multi-architecture dev tools (gh)
RUN bash -c 'set -o pipefail && \
    arch=$(uname -m) && \
    case "$arch" in \
        x86_64) gh_arch="amd64" ;; \
        aarch64) gh_arch="arm64" ;; \
    esac && \
    gh_version="2.63.0" && \
    gh_url="https://github.com/cli/cli/releases/download/v${gh_version}/gh_${gh_version}_linux_${gh_arch}.tar.gz" && \
    (curl -sSL "${GITHUB_PROXY}${gh_url}" || curl -sSL "${gh_url}") | tar xz -C /tmp && \
    mv /tmp/gh_${gh_version}_linux_${gh_arch}/bin/gh /usr/local/bin/ && \
    rm -rf /tmp/gh_${gh_version}_linux_${gh_arch}'

# 3. Copy AI tools from collector
COPY --from=ai-tools-collector /usr/local/bin/claude /usr/local/bin/claude
COPY --from=ai-tools-collector /usr/local/bin/opencode /usr/local/bin/opencode

# 4. HotPlex user & directories
RUN useradd -m -u ${HOST_UID} -s /bin/bash hotplex && \
    mkdir -p /etc/hotplex \
    /var/lib/hotplex/data \
    /var/lib/hotplex/tls \
    /var/log/hotplex \
    /home/hotplex/.claude \
    /home/hotplex/projects \
    && chown -R hotplex:hotplex /etc/hotplex /var/lib/hotplex /var/log/hotplex /home/hotplex

# 5. Copy binary and configs
COPY --from=builder /build/bin/hotplex /usr/local/bin/hotplex
COPY --chown=hotplex:hotplex configs/ /etc/hotplex/

# 6. Copy entrypoint
COPY --chmod=755 docker/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

WORKDIR /home/hotplex

EXPOSE 8888 9999

ENV HOTPLEX_CONFIG=/etc/hotplex/config.yaml
ENV HOTPLEX_DATA_DIR=/var/lib/hotplex/data
ENV HOTPLEX_LOG_DIR=/var/log/hotplex
ENV PATH="/usr/local/bin:/home/hotplex/.npm-global/bin:${PATH}"

USER hotplex:hotplex

HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
    CMD ["curl", "-f", "http://localhost:9999/admin/health/ready"]

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]

CMD ["hotplex", "gateway", "start", "--config", "/etc/hotplex/config.yaml"]
