---
title: "Issue #995：平台终态回执一次性完成"
weight: 66
description: "独立根因、回归证据和交付边界。"
---

# 平台终态回执一次性完成

根因、触发路径与验收约束见 [Issue #995](https://github.com/hrygo/hotplex/issues/995)。本变更以 PR #986 的 `68b9f7f0cd11e2dbf603971c4a987fa2de682fb0` 为基准，只处理本 Issue，不把其他并行分支的修复计入本次交付。

## 实际验证

[隔离 Actions 运行](https://github.com/hrygo/hotplex/actions/runs/34688992818) 使用 `go version go1.26.8 linux/amd64`。先加入最终回归验证原实现失败，再应用产品修复，使用 race detector 和随机顺序重复五轮。

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| red | 0 | 3 | 0 |
| green | 15 | 0 | 0 |
| related | 1081 | 0 | 2 |

red 为预期失败。计数包含父子测试与重复轮次，不等于独立 bug 数量。既有三渠道四 Worker 契约矩阵：12 个组合、96 个核心场景，0 失败、0 跳过。它是确定性外部协议 fake，不是生产平台或收费模型联调。

### 原实现失败用例

- `TestDeepAudit995SuccessfulReceiptNeverBecomesTimeout`
- `TestDeepAudit995TerminalWithoutReceiptKeepsLiveContext`
- `TestDeepAudit995LateWriteCannotOverwriteTimeout`

### 相关测试实际跳过

- `github.com/hrygo/hotplex/internal/gateway` / `TestWSPingPong`
- `github.com/hrygo/hotplex/internal/gateway` / `TestLogin_FirstLoginFlag`

## 变更文件

```text
internal/gateway/deep_audit_995_test.go
internal/gateway/platform_write_completion.go
internal/gateway/platform_writer.go
```

## 交付边界

不变更 AEP wire schema、权限默认值或生产数据；没有部署或合并 main。完整 lint、文档构建和原始 git commit / git push hooks 是后续发布门禁，最终发布状态以本运行与 PR 提交记录为准，不能仅以本文件存在推断已推送。独立 PR 依赖尚未合并的 #986；共同修改文件的兄弟 PR 合并后需重新核验冲突与集成测试。
