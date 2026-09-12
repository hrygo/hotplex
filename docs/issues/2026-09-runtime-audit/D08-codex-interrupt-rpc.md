---
title: "D08：Codex停止命令作为单向通知缺乏协议确认"
weight: 8
description: "根因、触发路径、修复与验收证据。"
---

# D08 — Codex停止命令作为单向通知缺乏协议确认

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / new
- 状态：已登记，待修复
- 证据：Source；本批运行证据尚未生成
- 实施PR：待创建
- 位置：`internal/worker/codexcli/manager.go`, `internal/worker/codexcli/worker.go`

## 根因与影响

InterruptTurn使用Notify不带id，也不观察服务端拒绝；官方app-server将turn/interrupt定义为请求并返回空result。

## 复现路径

严格协议fake只接受带id的turn/interrupt，分别返回成功和错误；断言Worker停止标记。

## 验收条件

带id并等待有界ACK；服务端拒绝/取消回滚stopped；最终完成仍看turn/completed；不切换目标turn，不释放共享进程；更新错误fixture而非削弱断言。

## 证据记录

失败回归、通过回归、相关测试、提交与PR按实际结果回填。源码推断不等于Test，fake不等于Live；禁止用编译失败冒充复现，不合并main。

## 上游参考

https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md（2026-09-12核验；运行实例版本需单独记录。）
