# HotPlex Worker Gateway - Development Makefile
# Not for production service management.
#
#   make help        Show commands
#   make quickstart  First-time setup
#   make dev         Start dev environment
#   make check       Quality check (CI)

# ─────────────────────────────────────────────────────────────────────────────
# Configuration
# ─────────────────────────────────────────────────────────────────────────────

BINARY_NAME  := hotplex
BUILD_DIR    := bin
MAIN_PATH    := ./cmd/hotplex
CONFIG_DIR   := configs
LOG_DIR      := logs

GO_VERSION   := $(shell go version | cut -d' ' -f3)
GOOS         := $(shell go env GOOS)
GOARCH       := $(shell go env GOARCH)
GIT_SHA      := $(shell git rev-parse --short=8 HEAD 2>/dev/null || echo "unknown")
BUILD_TIME   := $(shell date '+%Y-%m-%dT%H:%M:%S%z')
VERSION      := v1.51.0
LDFLAGS      := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)
BUILD_OPTS   := -trimpath

GATEWAY_PID   := $(HOME)/.hotplex/.pids/gateway.pid
GATEWAY_LOG   := $(LOG_DIR)/hotplex.log
GATEWAY_ADDR  ?= 127.0.0.1:8888
WEB_CHAT_PID  := $(HOME)/.hotplex/.pids/hotplex-webchat.pid
WEB_CHAT_PORT := 3000
WEB_CHAT_LOG  := $(CURDIR)/$(LOG_DIR)/webchat.log
WEB_CHAT_DIR  := webchat
WEB_CHAT_OUT  := internal/webchat/out
GRACE_PERIOD  := 7

# Derived paths (DRY: used across build targets)
BINARY_PATH   := $(BUILD_DIR)/$(BINARY_NAME)-$(GOOS)-$(GOARCH)
BUILD_HEADER  := $(BOLD)$(CYAN)Build$(RESET)  $(DIM)$(VERSION) · $(GIT_SHA) · $(GOOS)/$(GOARCH)$(RESET)

# ─────────────────────────────────────────────────────────────────────────────
# Color
# ─────────────────────────────────────────────────────────────────────────────

RESET  := \033[0m
BOLD   := \033[1m
DIM    := \033[2m
RED    := \033[31m
GREEN  := \033[32m
YELLOW := \033[33m
CYAN   := \033[36m

# ─────────────────────────────────────────────────────────────────────────────
# PHONY
# ─────────────────────────────────────────────────────────────────────────────

.PHONY: all help quickstart hooks check-tools build build-windows build-one run
.PHONY: dev dev-start dev-stop dev-status dev-logs dev-reset
.PHONY: pg-start pg-stop pg-status pg-logs pg-reset dev-pg
.PHONY: gateway-start gateway-stop gateway-status gateway-logs
.PHONY: webchat-dev webchat-stop webchat-embed webchat-rebuild webchat-reset
.PHONY: docs-build docs-clean docs-lint swagger
.PHONY: dev-build coverage test-slack-e2e test-contract-matrix
.PHONY: test test-short lint fmt quality check clean

# ─────────────────────────────────────────────────────────────────────────────
# Default
# ─────────────────────────────────────────────────────────────────────────────

all: help

# ─────────────────────────────────────────────────────────────────────────────
# Setup
# ─────────────────────────────────────────────────────────────────────────────

quickstart: hooks
	@if command -v go > /dev/null 2>&1; then \
		$(MAKE) check-tools build test-short; \
		echo ""; \
		echo "  $(GREEN)✓ Developer setup complete$(RESET)"; \
		echo ""; \
		echo "    make dev      Start dev environment"; \
		echo "    make run      Run gateway"; \
		echo "    make help     Show all commands"; \
		echo ""; \
	else \
		echo ""; \
		echo "  $(GREEN)✓ Quickstart complete$(RESET)"; \
		echo ""; \
		echo "    $(DIM)Go not detected — skipping build & tests.$(RESET)"; \
		echo ""; \
		echo "    $(BOLD)Next steps:$(RESET)"; \
		echo "      1. Download binary from releases"; \
		echo "      2. Run $(CYAN)hotplex onboard$(RESET) to configure"; \
		echo "      3. Run $(CYAN)hotplex gateway start$(RESET) to launch"; \
		echo ""; \
	fi

check-tools:
	@$(call check-tool, go, "Go")
	@$(call check-tool, golangci-lint, "golangci-lint")
	@$(call check-tool, goimports, "goimports")

hooks:
	@echo "$(CYAN)Installing git hooks...$(RESET)"
# Relative core.hooksPath resolves per-worktree root, safe across linked worktrees
	@git config core.hooksPath scripts/git-hooks
	@for hook in scripts/git-hooks/*; do \
		name=$$(basename "$$hook"); \
		echo "  $(GREEN)✓$(RESET) $$name"; \
	done
	@echo "  $(DIM)Pre-push runs: fmt → lint → vet → mod verify → build → test$(RESET)"

define check-tool
	@if command -v $(1) > /dev/null 2>&1; then \
		echo "  $(GREEN)✓$(RESET) $(2)"; \
	else \
		echo "  $(YELLOW)⚠$(RESET) $(2) $(DIM)(missing)$(RESET)"; \
	fi
endef

# ── Reusable build helpers ──────────────────────────────────────────────────

# go-build — compile Go binary, strip duplicated compile+echo across targets.
define go-build
	@echo "  $(CYAN)Compiling$(RESET)$(DIM) Go binary...$(RESET)"
	@CGO_ENABLED=0 go build $(BUILD_OPTS) -ldflags="$(LDFLAGS)" \
		-o $(1) $(MAIN_PATH)
	@echo "  $(GREEN)✓$(RESET) $(1)"
endef

# webchat-build — shared pnpm build + output copy logic.
# NOTE: no @ prefix — $(call ...) inside \ -continued blocks would emit literal @.
define webchat-build
	cd $(WEB_CHAT_DIR) && pnpm build && \
	rm -rf ../$(WEB_CHAT_OUT).tmp && cp -r out ../$(WEB_CHAT_OUT).tmp && \
	rm -rf ../$(WEB_CHAT_OUT) && mv ../$(WEB_CHAT_OUT).tmp ../$(WEB_CHAT_OUT)
endef

# ─────────────────────────────────────────────────────────────────────────────
# Build
# ─────────────────────────────────────────────────────────────────────────────

build:
	@echo "$(BUILD_HEADER)"
	@echo ""
	@$(MAKE) docs-build --no-print-directory
	@$(MAKE) webchat-embed --no-print-directory
	@mkdir -p $(BUILD_DIR) $(LOG_DIR)
	$(call go-build,$(BINARY_PATH))

build-windows:
	@echo "$(CYAN)Cross-compiling for Windows...$(RESET)"
	@mkdir -p $(BUILD_DIR)
	@$(MAKE) build-one GOOS=windows GOARCH=amd64 SUFFIX=.exe --no-print-directory
	@$(MAKE) build-one GOOS=windows GOARCH=arm64 SUFFIX=.exe --no-print-directory

build-one:
	@$(call go-build,$(BUILD_DIR)/$(BINARY_NAME)-$(GOOS)-$(GOARCH)$(SUFFIX))

run: build
	@./$(BINARY_PATH) gateway start -c $(CONFIG_DIR)/config-dev.yaml

# dev-build: 轻量构建，跳过 swagger，仅保证 go:embed 资源存在 + Go 编译。
# 供 `make dev` 每次启动前自动产出最新二进制，避免 dev.sh 兜底提示。
dev-build:
	@echo "$(BUILD_HEADER)"
	@mkdir -p $(BUILD_DIR) $(LOG_DIR)
	@$(MAKE) webchat-embed --no-print-directory
	@if [ ! -f internal/docs/out/index.html ]; then \
		echo "  $(CYAN)Docs$(RESET)$(DIM) building from scratch...$(RESET)"; \
		$(MAKE) docs-build --no-print-directory; \
	else \
		echo "  $(DIM)Docs ✓ cached$(RESET)"; \
	fi
	$(call go-build,$(BINARY_PATH))

# ─────────────────────────────────────────────────────────────────────────────
# Test
# ─────────────────────────────────────────────────────────────────────────────

test:
	@echo "$(CYAN)Testing...$(RESET)"
	@GORACE="history_size=5" go test -race -count=1 -shuffle=on -timeout 15m ./...
	@echo "  $(GREEN)✓ Tests passed$(RESET)"

test-short:
	@echo "$(CYAN)Testing...$(RESET)"
	@GORACE="history_size=5" go test -short -race -count=1 -shuffle=on -timeout 5m ./...
	@echo "  $(GREEN)✓ Tests passed$(RESET)"

test-contract-matrix:
	@./scripts/test-contract-matrix.sh

coverage:
	@echo "$(CYAN)Generating coverage report...$(RESET)"
	@go test -count=1 -timeout=15m -coverprofile=coverage.out -covermode=atomic \
		$$(go list ./... | grep -v -e 'internal/worker/proc' -e 'internal/worker/pi' -e 'cmd/hotplex')
	@echo ""
	@echo "$(BOLD)Per-package coverage:$(RESET)"
	@go tool cover -func=coverage.out | grep -v "^total:" | sort -t: -k3 -n
	@echo ""
	@TOTAL=$$(go tool cover -func=coverage.out | tail -1 | grep -Eo '[0-9]+\.[0-9]+') ; \
		echo "  $(BOLD)Total: $${TOTAL}%$(RESET)"

test-slack-e2e:
	@echo "$(CYAN)Running Slack semi-automated E2E tests...$(RESET)"
	@test -n "$$SLACK_BOT_TOKEN" || (echo "  $(RED)SLACK_BOT_TOKEN required$(RESET)"; exit 1)
	@test -n "$$SLACK_APP_TOKEN" || (echo "  $(RED)SLACK_APP_TOKEN required$(RESET)"; exit 1)
	@go test -v -tags=slack_e2e -timeout 30m ./internal/messaging/slack/...

# ─────────────────────────────────────────────────────────────────────────────
# Quality
# ─────────────────────────────────────────────────────────────────────────────

lint:
	@echo "$(CYAN)Linting...$(RESET)"
	@golangci-lint run ./...

fmt:
	@echo "$(CYAN)Formatting...$(RESET)"
	@go fmt ./...
	@if command -v goimports > /dev/null 2>&1; then goimports -w .; fi

quality: fmt lint test
	@echo ""
	@echo "  $(GREEN)✓ All checks passed$(RESET)"
	@echo ""

check: quality build
	@echo "  $(GREEN)✓ CI check passed$(RESET)"

# ─────────────────────────────────────────────────────────────────────────────
# Dev Environment
# ─────────────────────────────────────────────────────────────────────────────

dev:
	@rm -f "$(GATEWAY_LOG)"
	@$(MAKE) dev-start
	@if curl -sf "http://$(GATEWAY_ADDR)/health" >/dev/null 2>&1; then \
		echo ""; \
		if [ -f "$(GATEWAY_LOG)" ]; then \
			LINE=$$(grep -n 'HOTPLEX.*GATEWAY' "$(GATEWAY_LOG)" 2>/dev/null | head -1 | cut -d: -f1); \
			if [ -n "$$LINE" ]; then \
				START=$$((LINE - 8)); \
				[ $$START -lt 1 ] && START=1; \
				tail -n +$$START "$(GATEWAY_LOG)" | head -40; \
			else \
				head -40 "$(GATEWAY_LOG)"; \
			fi; \
		fi; \
		echo "  $(DIM)────────────────────────────────────────────────$(RESET)"; \
		echo "  $(CYAN)Quick Commands$(RESET)"; \
		printf "    make %-15s %s\n" "dev-logs"   "View logs"; \
		printf "    make %-15s %s\n" "dev-status" "Check status"; \
		printf "    make %-15s %s\n" "dev-stop"   "Stop all"; \
		printf "    make %-15s %s\n" "help"       "Show all commands"; \
		echo "  $(DIM)────────────────────────────────────────────────$(RESET)"; \
		echo ""; \
	else \
		echo ""; \
		echo "  $(RED)✗$(RESET) Gateway is not responding at http://$(GATEWAY_ADDR)"; \
		echo "    Run $(CYAN)make dev-logs$(RESET) to check for errors."; \
		echo ""; \
	fi

dev-start: dev-build
	@$(MAKE) gateway-start
	@$(MAKE) webchat-dev || echo "  $(YELLOW)⚠$(RESET) Webchat skipped (run 'cd webchat && pnpm install' to fix)"

dev-stop: webchat-stop gateway-stop
	@rm -f logs/*.log
	@echo "  $(GREEN)✓ Dev environment stopped$(RESET)"

dev-status:
	@./scripts/dev.sh status all

dev-logs:
	@./scripts/dev.sh logs all

dev-pg: pg-start
	@$(MAKE) gateway-start HOTPLEX_DB_DRIVER=postgres HOTPLEX_DB_POSTGRES_DSN="postgres://$(PG_USER):$${POSTGRES_PASSWORD:-hotplex}@localhost:$(PG_PORT)/$(PG_DB)?sslmode=disable"
	@$(MAKE) webchat-dev || echo "  $(YELLOW)⚠$(RESET) Webchat skipped (run 'cd webchat && pnpm install' to fix)"

dev-reset: dev-stop dev-start

# ─────────────────────────────────────────────────────────────────────────────
# PostgreSQL (dev)
# ─────────────────────────────────────────────────────────────────────────────

PG_USER ?= hotplex
PG_DB   ?= hotplex
PG_PORT ?= 5432

pg-start:
	@echo "$(CYAN)Starting PostgreSQL...$(RESET)"
	@docker compose --profile postgres up -d postgres
	@echo "  $(GREEN)✓$(RESET) PostgreSQL ready  $(DIM)pg://$(PG_USER)@localhost:$(PG_PORT)/$(PG_DB)$(RESET)"

pg-stop:
	@echo "$(CYAN)Stopping PostgreSQL...$(RESET)"
	@docker compose --profile postgres stop postgres
	@echo "  $(GREEN)✓$(RESET) PostgreSQL stopped"

pg-status:
	@docker compose --profile postgres ps postgres 2>/dev/null | grep -q "running" \
		&& echo "  $(GREEN)●$(RESET) PostgreSQL running  $(DIM)localhost:$(PG_PORT)$(RESET)" \
		|| echo "  $(RED)○$(RESET) PostgreSQL stopped"

pg-logs:
	@docker compose --profile postgres logs -f postgres

pg-reset:
	@echo "$(CYAN)Resetting PostgreSQL...$(RESET)"
	@docker compose --profile postgres down -v
	@$(MAKE) pg-start

# ─────────────────────────────────────────────────────────────────────────────
# Gateway
# ─────────────────────────────────────────────────────────────────────────────

gateway-start:
	@./scripts/dev.sh start gateway

gateway-stop:
	@./scripts/dev.sh stop gateway

gateway-status:
	@./scripts/dev.sh status gateway

gateway-logs:
	@./scripts/dev.sh logs gateway

# ─────────────────────────────────────────────────────────────────────────────
# Webchat
# ─────────────────────────────────────────────────────────────────────────────

webchat-dev:
	@./scripts/dev.sh start webchat

webchat-stop:
	@./scripts/dev.sh stop webchat

# webchat-reset — stop webchat, wipe the Turbopack cache (.next), restart.
# Fixes the dev-time CPU spin caused by a bloated/corrupt RocksDB cache
# (.next/dev/cache/turbopack). Run this whenever next-server idles hot.
webchat-reset:
	@./scripts/dev.sh stop webchat
	@rm -rf $(WEB_CHAT_DIR)/.next
	@echo "  $(GREEN)✓$(RESET) Cleared $(WEB_CHAT_DIR)/.next (Turbopack cache)"
	@./scripts/dev.sh start webchat

webchat-embed:
	@if [ ! -d $(WEB_CHAT_OUT)/_next ]; then \
		echo "  $(CYAN)Webchat$(RESET)$(DIM) building from scratch...$(RESET)"; \
		(cd $(WEB_CHAT_DIR) && pnpm install --frozen-lockfile); \
		$(call webchat-build); \
		echo "  $(GREEN)✓$(RESET) Webchat built"; \
	elif find $(WEB_CHAT_DIR)/app $(WEB_CHAT_DIR)/lib $(WEB_CHAT_DIR)/components $(WEB_CHAT_DIR)/public \
		$(WEB_CHAT_DIR)/next.config.mjs $(WEB_CHAT_DIR)/tsconfig.json \
		$(WEB_CHAT_DIR)/postcss.config.mjs $(WEB_CHAT_DIR)/package.json \
		$(WEB_CHAT_DIR)/pnpm-lock.yaml \
		-newer $(WEB_CHAT_OUT)/_next -print 2>/dev/null | head -n 1 | grep -q .; then \
		$(MAKE) webchat-rebuild --no-print-directory; \
	else \
		echo "  $(DIM)Webchat ✓ cached$(RESET)"; \
	fi

webchat-rebuild:
	@echo "$(CYAN)Rebuilding webchat...$(RESET)"
	@$(call webchat-build)
	@echo "  $(GREEN)✓$(RESET) Webchat rebuilt"

# ─────────────────────────────────────────────────────────────────────────────
# Documentation
# ─────────────────────────────────────────────────────────────────────────────

docs-build: swagger
	@if [ ! -f internal/docs/out/index.html ]; then \
		echo "  $(CYAN)Docs$(RESET)$(DIM) building from scratch...$(RESET)"; \
		go run cmd/build-docs/main.go; \
	elif find docs cmd/build-docs -newer internal/docs/out \
		\( -name "*.md" -o -name "*.go" -o -name "*.yaml" -o -name "*.png" -o -name "*.svg" -o -name "*.html" \) \
		! -path "docs/swagger/*" \
		-print 2>/dev/null | head -n 1 | grep -q .; then \
		echo "  $(CYAN)Docs$(RESET)$(DIM) rebuilding (source changed)...$(RESET)"; \
		go run cmd/build-docs/main.go; \
	else \
		echo "  $(DIM)Docs ✓ cached$(RESET)"; \
	fi

docs-clean:
	@rm -rf internal/docs/out
	@echo "  $(GREEN)✓$(RESET) Documentation cleaned"

docs-lint: docs-build
	@echo "$(CYAN)Docs link validation passed$(RESET)"

SWAGGER_DIR     := docs/swagger
SWAG_VERSION    := v1.16.4

swagger:
	@echo "  $(CYAN)Swagger$(RESET)$(DIM) generating API spec...$(RESET)"
	@command -v swag > /dev/null 2>&1 || { \
		echo "  $(RED)swag not found$(RESET)$(DIM) — run: go install github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION)$(RESET)"; \
		exit 1; \
	}
	@mkdir -p $(SWAGGER_DIR)
	@swag init \
		--generalInfo doc.go \
		--dir cmd/hotplex,internal/admin,internal/gateway,internal/agentspec \
		--output $(SWAGGER_DIR) \
		--outputTypes json \
		--parseInternal \
		--quiet
	@printf '\n' >> $(SWAGGER_DIR)/swagger.json
	@echo "  $(GREEN)✓$(RESET) $(SWAGGER_DIR)/swagger.json"

# ─────────────────────────────────────────────────────────────────────────────
# Clean
# ─────────────────────────────────────────────────────────────────────────────

clean:
	@go clean
	@rm -rf $(BUILD_DIR)
	@rm -f coverage.out
	@echo "  $(GREEN)✓$(RESET) Cleaned"

# ─────────────────────────────────────────────────────────────────────────────
# Help
# ─────────────────────────────────────────────────────────────────────────────

help:
	@echo ""
	@echo "  $(CYAN)━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━$(RESET)"
	@echo "  $(CYAN)  ⚡ HotPlex Worker$(RESET)  $(GIT_SHA)  $(GOOS)/$(GOARCH)"
	@echo "  $(CYAN)━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━$(RESET)"
	@echo ""
	@echo "  $(BOLD)⚡ Start"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "dev"         "Start all services (gateway + webchat)"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "dev-start"   "Start individually"
	@echo ""
	@echo "  $(BOLD)⏹  Stop"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "dev-stop"      "Stop all services"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "gateway-stop"   "Stop gateway"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "webchat-stop"  "Stop webchat"
	@echo ""
	@echo "  $(BOLD)🐘 PostgreSQL"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "pg-start"  "Start PG container"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "pg-stop"   "Stop PG container"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "pg-status" "Check PG status"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "pg-logs"   "View PG logs"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "pg-reset"  "Drop data & restart"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "dev-pg"    "PG + gateway + webchat"
	@echo ""
	@echo "  $(BOLD)🔧 Build"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "build"          "Build binary"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "build-windows"  "Cross-compile for Windows"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "run"     "Build and run (foreground)"
	@echo ""
	@echo "  $(BOLD)🧪 Test & Quality"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "test"         "All tests (race, 15m)"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "test-short"   "Short tests (5m)"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "lint"         "Run linter"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "fmt"          "Format code"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "quality"       "fmt + lint + test"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "check"         "quality + build (CI)"
	@echo ""
	@echo "  $(BOLD)📊 Status & Logs"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "dev-status"     "All services"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "gateway-status"  "Gateway"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "dev-logs"      "View all logs"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "gateway-logs"   "Gateway logs"
	@echo ""
	@echo "  $(BOLD)📖 Documentation"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "docs-build"     "Build static HTML docs (includes swagger)"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "docs-clean"     "Remove generated docs"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "swagger"        "Generate docs/swagger/swagger.json"
	@echo ""
	@echo "  $(BOLD)🔄 Workflow"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "dev-reset"   "Restart all services"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "webchat-reset" "Clear .next & restart webchat"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "quickstart"  "First-time setup"
	@echo ""
	@echo "  $(BOLD)🧹 Other"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "clean"        "Clean artifacts"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "check-tools"  "Check dev tools"
	@printf "    $(CYAN)make %-15s$(RESET)  %s\n" "hooks"        "Install git hooks"
	@echo ""
	@echo "  $(DIM)Try:  make dev | make test | make check"
	@echo ""

# Catch-all
%:
	@echo ""
	@echo "  $(RED)Unknown: make $@$(RESET)"
	@echo "    make help  Show commands"
	@echo ""
	@exit 1
