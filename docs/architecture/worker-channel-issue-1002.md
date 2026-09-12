---
title: "Issue #1002：独立可靠性修复"
weight: 66
description: "根因跟踪、实际红绿回归证据和交付边界。"
---

# Issue #1002 可靠性修复

[Issue #1002](https://github.com/hrygo/hotplex/issues/1002) 已先保存根因、触发条件和验收标准。依赖 PR #986 的固定提交 `fb53e8be5ee8171ccb11e6eefe9fc6625abb98e3`，本分支只包含本问题修复，未计入其他兄弟分支。

## 实际验证

[本次隔离 Actions 运行](https://github.com/hrygo/hotplex/actions/runs/34693473176)，工具链 `go version go1.26.8 linux/amd64`。同一回归先在原实现产生测试失败，再在修复后进行五轮 race / shuffle。编译失败不被当作复现。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| red | 1 | 4 | 0 |
| green | 30 | 0 | 0 |
| related | 529 | 0 | 0 |

计数为 go test -json 的终态记录，含父子测试及重复轮次，不是独立 bug 数。red 为预期失败。既有 12 组合 × 8 场景契约矩阵共 96 场景全部通过，无跳过；这是确定性协议 fake，不是生产平台/收费模型联调。

### 原实现失败入口

- `TestDeepAudit1002CurrentTimeoutStillDeniesOnce`
- `TestDeepAudit1002SameObjectCanBeRegisteredAfterCompletion`
- `TestDeepAudit1002OldTimeoutCannotDenyReplacement/complete`
- `TestDeepAudit1002OldTimeoutCannotDenyReplacement`

### 相关测试实际跳过

无。

## 边界与合并

该独立分支不修改 AEP wire schema、权限默认值或生产数据；其修复已在本次冲突审查中整合到 `main`，并保留了独立回归证据。完整 JSON 日志、补丁和命令输出保存在该运行 artifacts。
