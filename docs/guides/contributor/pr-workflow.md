---
title: PR 工作流
weight: 33
description: HotPlex 的分支管理、Commit 规范和 PR 提交流程
---

# PR 工作流

> 阅读本文后，你将掌握 HotPlex 的分支命名、Commit 规范、PR 创建流程和 CI 要求，能够顺利提交贡献。

## 概述

所有代码变更必须通过 PR 合并，不允许直接推送到 main 分支。项目遵循 Conventional Commits 规范，PR 必须通过 CI（lint + test + build）。

## 前提条件

- 已完成[开发环境搭建](development-setup.md)
- 安装 `gh` CLI（`brew install gh`）
- 拥有 GitHub 账号

## 步骤

### 1. 创建功能分支

#### Admin（仓库协作者）

拥有仓库 write/admin 权限的协作者直接在 origin 仓库创建分支：

```bash
# 从最新 main 创建功能分支
git fetch origin main
git checkout -b feat/<feature-name> origin/main
```

#### 外部贡献者

无仓库直接权限的贡献者使用 fork-PR 工作流：

```bash
# 1. 在 GitHub 上 fork 仓库
# 2. 添加远程
git remote add fork https://github.com/<your-username>/hotplex.git

# 3. 创建功能分支（基于上游 main）
git fetch origin main
git checkout -b feat/<feature-name> origin/main
```

### 2. 分支命名规范

| 类型 | 格式 | 示例 |
|------|------|------|
| 新功能 | `feat/<name>` | `feat/cron-scheduler` |
| Bug 修复 | `fix/<name>` | `fix/session-race` |
| 重构 | `refactor/<name>` | `refactor/hub-backpressure` |
| 文档 | `docs/<name>` | `docs/contributor-guides` |
| 杂项 | `chore/<name>` | `chore/update-deps` |
| AI 协作 | `codex/<name>` | `codex/trusted-runtime-delivery` |

`codex/` 前缀供 Codex 桌面端创建分支时使用，变更类型记录在 commit 主题中
（遵循 Conventional Commits），分支名只承载工作区名称。

### 3. 开发与提交

遵循 [Conventional Commits](https://www.conventionalcommits.org/) 规范：

```
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

**类型**：

| 类型 | 用途 |
|------|------|
| `feat` | 新功能 |
| `fix` | Bug 修复 |
| `refactor` | 代码重构（不改变行为） |
| `docs` | 文档变更 |
| `test` | 测试相关 |
| `chore` | 构建、依赖、工具 |
| `perf` | 性能优化 |

**Scope 示例**：`gateway`、`session`、`worker`、`messaging`、`cron`、`config`、`security`、`brain`

**示例**：

```
feat(cron): add lifecycle limits for recurring jobs

Support --max-runs and --expires-at flags to bound job execution.
Jobs auto-disable when limits are reached.

Closes #123
```

### 4. 提交前检查

```bash
# 完整 CI 流程（推荐）
make check
```

`make check` 等效于 CI 流程：`fmt → lint → test → build`。

**检查清单**：

- [ ] `make fmt` 通过
- [ ] `make lint` 无新增 warning
- [ ] `make test` 通过（含 race 检测）
- [ ] `make build` 成功
- [ ] 新功能有对应测试
- [ ] Commit message 遵循 Conventional Commits

> Git pre-push hook 会自动运行格式化、lint、构建和测试。如果未安装，运行 `make hooks`。

### 5. 推送并创建 PR

#### Admin

```bash
# 推送到 origin
git push -u origin feat/<feature-name>

# 创建 PR
gh pr create --title "feat(scope): description"
```

#### 外部贡献者

```bash
# 推送到 fork
git push -u fork feat/<feature-name>

# 创建 PR（从 fork 到上游 main）
gh pr create \
  --base main \
  --head <your-username>:feat/<feature-name> \
  --title "feat(scope): description"
```

### 6. 合并后清理

```bash
# 切回 main 并拉取最新
git checkout main && git pull origin main

# 删除本地分支
git branch -d feat/<feature-name>

# 删除远程分支（Admin）
git push origin --delete feat/<feature-name>

# 外部贡献者删除 fork 上的分支
git push fork --delete feat/<feature-name>
```

## CI 要求

所有 PR 必须通过 CI 检查：

| 阶段 | 命令 | 说明 |
|------|------|------|
| 格式化 | `go fmt` + `goimports` | 代码格式 |
| Lint | `golangci-lint run ./...` | 静态分析 |
| 测试 | `go test -race -timeout 15m ./...` | 含竞态检测 |
| 构建 | `go build` | 编译通过 |

本地验证：`make check` 等效 CI。

## 验证

- PR 在 CI 全部通过后才可合并
- PR 标题使用 Conventional Commits 格式
- 至少一位 reviewer approve 后方可合并

## 下一步

- [测试指南](testing-guide.md) — 确保测试覆盖充分
- [架构概览](architecture.md) — 理解变更影响的模块
- [扩展指南](extending.md) — 新增组件的 PR 参考
