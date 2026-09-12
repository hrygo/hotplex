---
title: "Issue #990：独立可靠性修复"
weight: 66
description: "根因跟踪、实际红绿回归证据和交付边界。"
---

# Issue #990 可靠性修复

[Issue #990](https://github.com/hrygo/hotplex/issues/990) 已先保存根因、触发条件和验收标准。依赖 PR #986 的固定提交 `fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3`，本分支只包含本问题修复，未计入其他兄弟分支。

## 实际验证

[本次隔离 Actions 运行](https://github.com/hrygo/hotplex/actions/runs/34692685711)，工具链 `go version go1.26.8 linux/amd64`。同一回归先在原实现产生测试失败，再在修复后进行五轮 race / shuffle。编译失败不被当作复现。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| red | 2 | 1 | 0 |
| green | 15 | 0 | 0 |
| related | 338 | 0 | 4 |

计数为 go test -json 的终态记录，含父子测试及重复轮次，不是独立 bug 数。red 为预期失败。既有 12 组合 × 8 场景契约矩阵共 96 场景全部通过，无跳过；这是确定性协议 fake，不是生产平台/收费模型联调。

### 原实现失败入口

- `TestDeepAudit990ResetFailureReleasesOnlyItsReference`

### 相关测试实际跳过

- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestManagerAcquireStartsProcess`
- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestIntegrationStartSavesSessionAndResetRestarts`
- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestIntegrationKillImmediatelyTerminatesIdleProcess`
- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestManager_PerThreadConverterIsolation/concurrent_dispatch_no_data_race`

## 边界与合并

本变更不修改 AEP wire schema、权限默认值或生产数据，不部署、不合并 main。完整 lint、文档构建、原始 pre-commit / pre-push 是后续发布门禁，最终成功状态以运行与 PR 提交记录为准，不因报告存在就宣称推送成功。独立 PR 依赖 #986；同文件兄弟分支合并后必须复核冲突并运行集成回归。完整 JSON 日志、补丁和命令输出保存在该运行 artifacts。
