---
title: "Worker 与消息渠道独立缺陷台账"
weight: 66
description: "十份独立Issue文档与对应修复证据。"
---

# 十项独立缺陷台账

用户要求十项独立问题先以源码文档保存，再对应十个修复PR；保留既有PR作为基线，不重复计入已修复A02/A07，不合并main。所有条目创建时为Source；本轮测试、提交和PR按实际结果更新。

| ID | 问题 | 状态 |
| --- | --- | --- |
| D01 | [Codex订阅通道多方关闭与发送竞态](D01-codex-subscription-ownership.md) | Test，修复通过；`fix/987-d01-codex-subscription-ownership` |
| D02 | [Codex重置建线程失败泄漏共享进程引用](D02-codex-reference-release.md) | Test，修复通过；`fix/987-d02-codex-reference-release` |
| D03 | [Codex排队写入未绑定进程传输快照](D03-codex-transport-generation.md) | Source，已登记 |
| D04 | [Codex旧进程清理和空闲回收与新Acquire交错](D04-codex-process-retirement.md) | Source，已登记 |
| D05 | [OpenCode reset把HTTP404误报为上下文清空成功](D05-opencode-reset-404.md) | Source，已登记 |
| D06 | [飞书Adapter关闭时丢失shutdown期限](D06-feishu-shutdown-deadline.md) | Source，已登记 |
| D07 | [Codex传输关闭后pending RPC仍等待完整超时](D07-codex-pending-on-disconnect.md) | Source，已登记 |
| D08 | [Codex停止命令作为单向通知缺乏协议确认](D08-codex-interrupt-rpc.md) | Source，已登记 |
| D09 | [Codex启动接受缺少原生线程或轮次ID的响应](D09-codex-lifecycle-response-validation.md) | Source，已登记 |
| D10 | [交互旧超时回调可能拒绝同ID的新请求](D10-interaction-timeout-generation.md) | Source，已登记 |

本台账文档先于实现提交。每个修复PR仅处理一个独立根因；有依赖时明确base，不重复计算PR #986已修复的问题。RTK不可用，使用原生make hooks。
