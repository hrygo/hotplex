---
title: "D08：Codex停止命令作为单向通知缺乏协议确认"
weight: 8
description: "根因、触发路径、修复与验收证据。"
---

# D08 — Codex停止命令作为单向通知缺乏协议确认

- 优先级：P1
- 基准：`68b9f7f0cd11e2dbf603971c4a987fa2de682fb0`
- 来源：Issue #987 / new
- 状态：修复已通过定向与相关测试，未合并
- 证据：Test；源码分析与运行复现已相互印证，非Live
- 实施PR：对应分支 `fix/987-d08-codex-interrupt-rpc`；目标分支 `fix/987-d07-codex-pending-on-disconnect`（堆叠依赖）
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


## 本次验证

父提交：`b6ec8c7d3f07be1473c1b2184216d61a70a81510`。验证运行：https://github.com/hrygo/hotplex/actions/runs/34693369860

| 测试范围 | 通过 | 失败 | 跳过 |
| --- | ---: | ---: | ---: |
| 修复前定向回归 | 0 | 4 | 0 |
| 修复后定向回归，5轮race/shuffle | 20 | 0 | 0 |
| 相关包短测，race/shuffle | 277 | 0 | 4 |

计数包括父子测试及重复轮次，不是独立缺陷数量。缺陷根因由指定失败测试验证；新增防护API专用测试只在修复后运行，未用编译失败充当复现。原始提交hooks继续检查格式与lint；合并系列的完整推送门禁在最后执行，不将最后一项全量结果冒充每个中间提交都执行过全部包。

空result仅确认interrupt被接受，最终turn结束仍由原生完成事件决定。ACK超时不证明远端未执行，不引入盲目重试。既有写入型fixture补上关联ACK，保留原有输出与会话隔离断言。

实际跳过：
- `TestManagerAcquireStartsProcess`
- `TestIntegrationKillImmediatelyTerminatesIdleProcess`
- `TestManager_PerThreadConverterIsolation/concurrent_dispatch_no_data_race`
- `TestIntegrationStartSavesSessionAndResetRestarts`
