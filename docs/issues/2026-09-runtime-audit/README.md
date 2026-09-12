---
title: "Worker 与消息渠道十项缺陷验收"
weight: 66
description: "D01–D10 的修复提交、复现与统一 PR 验证证据。"
---

# 十项独立缺陷台账

D01–D10 统一通过 [PR #986](https://github.com/hrygo/hotplex/pull/986) 交付，关联 #985、#987；不再要求十个独立 PR。
以下十项均已有指定失败回归、修复提交及通过回归；状态为“已修复并验证、待合并”，不等同于生产验收或所有缺陷均已消除。

| ID | 问题 | 修复提交 | 状态 |
| --- | --- | --- | --- |
| D01 | [Codex 订阅通道多方关闭与发送竞态](D01-codex-subscription-ownership.md) | `3da1511e` | 已修复、Test 通过；PR #986 |
| D02 | [Codex 重置建线程失败导致共享进程引用泄漏](D02-codex-reference-release.md) | `911fd09c` | 已修复、Test 通过；PR #986 |
| D03 | [Codex 排队写入未绑定进程传输快照](D03-codex-transport-generation.md) | `c5645402` | 已修复、Test 通过；PR #986 |
| D04 | [Codex 旧进程退出清理及空闲回收与新 Acquire 交错](D04-codex-process-retirement.md) | `012aafa8` | 已修复、Test 通过；PR #986 |
| D05 | [OpenCode reset 把 HTTP 404 误报为上下文清空成功](D05-opencode-reset-404.md) | `6ad409c2` | 已修复、Test 通过；PR #986 |
| D06 | [飞书 Adapter 关闭时丢失调用方 shutdown 期限](D06-feishu-shutdown-deadline.md) | `695c0ea6` | 已修复、Test 通过；PR #986 |
| D07 | [Codex 传输关闭后 pending RPC 仍等待完整响应超时](D07-codex-pending-on-disconnect.md) | `b6ec8c7d` | 已修复、Test 通过；PR #986 |
| D08 | [Codex turn/interrupt 被当作单向通知导致停止缺乏协议确认](D08-codex-interrupt-rpc.md) | `12e5ef37` | 已修复、Test 通过；PR #986 |
| D09 | [Codex 线程和轮次启动接受缺少原生 ID 的响应](D09-codex-lifecycle-response-validation.md) | `34f93702` | 已修复、Test 通过；PR #986 |
| D10 | [交互旧超时回调可能认领并拒绝同 ID 的新请求](D10-interaction-timeout-generation.md) | `4f3bd9d3` | 已修复、Test 通过；PR #986 |

## 验证结果

| 范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 十项定向回归，5 轮 | 205 | 0 | 0 |
| Worker / Messaging / Gateway | 4795 | 0 | 24 |
| Go SDK | 27 | 0 | 0 |

现有三渠道四 Worker 核心契约：12 组合、96 场景、0 失败、0 跳过。
计数包含父子测试和重复运行，不能当作独立缺陷数量；与先前系列逐项测试的计数不相加。
[逐项红绿回归及原始推送门禁](https://github.com/hrygo/hotplex/actions/runs/34693369860) · [统一复核与交付](https://github.com/hrygo/hotplex/actions/runs/34694026803)

## 明确边界

- D05 修正 404 虚假成功，不虚构上游原地 reset 支持，也不宣称新增会话替换已经实现。
- D06 保留兼容 graceful drain；有期限关闭不会等待不合作任务到无限期，但不能强杀 Go goroutine。
- D08 原生 ACK 只代表接受停止请求，最终结束仍由完成事件确认；ACK 丢失不意味着可以安全自动重试。
- Source / Test / Live 分开；真实平台、生产部署及手工验收不计入本批完成声明。
- PR 常规 CI 与独立验证分开记录；需要批准的工作流不冒充已通过，不更改审批规则。

## 未跳过质量检查

原始 make hooks、pre-commit、pre-push 保留不变。RTK 不可用时使用原生 make。主分支保持不变。
