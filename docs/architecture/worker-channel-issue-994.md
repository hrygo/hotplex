---
title: "Issue #994：传输生命周期审计"
weight: 66
description: "根因跟踪、实际红绿回归证据和交付边界。"
---

# Issue #994 可靠性修复

[Issue #994](https://github.com/hrygo/hotplex/issues/994) 已先保存根因、触发条件和验收标准。独立分支的验证记录保留在本文；当前 `main` 由统一 PR #986 的 `codexTransport` 代际绑定实现该语义，并已在合并时复核。

## 实际验证

[本次隔离 Actions 运行](https://github.com/hrygo/hotplex/actions/runs/34692685711)，工具链 `go version go1.26.8 linux/amd64`。同一回归先在原实现产生测试失败，再在修复后进行五轮 race / shuffle。编译失败不被当作复现。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| red | 0 | 3 | 0 |
| green | 20 | 0 | 0 |
| related | 339 | 0 | 4 |

计数为 go test -json 的终态记录，含父子测试及重复轮次，不是独立 bug 数。red 为预期失败。既有 12 组合 × 8 场景契约矩阵共 96 场景全部通过，无跳过；这是确定性协议 fake，不是生产平台/收费模型联调。

### 原实现失败入口

- `TestDeepAudit994PendingCallsFinishWhenTransportEnds/stdout_eof`
- `TestDeepAudit994PendingCallsFinishWhenTransportEnds/shutdown`
- `TestDeepAudit994PendingCallsFinishWhenTransportEnds`

### 相关测试实际跳过

- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestManager_PerThreadConverterIsolation/concurrent_dispatch_no_data_race`
- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestManagerAcquireStartsProcess`
- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestIntegrationKillImmediatelyTerminatesIdleProcess`
- `github.com/hrygo/hotplex/internal/worker/codexcli` / `TestIntegrationStartSavesSessionAndResetRestarts`

## 边界与合并

独立分支的 `rpcGeneration` 实现未直接叠加到当前树，以避免与 `codexTransport` 形成两套生命周期信号；本报告的回归意图已改写为覆盖现行实现。完整 JSON 日志、补丁和命令输出保存在该运行 artifacts。
