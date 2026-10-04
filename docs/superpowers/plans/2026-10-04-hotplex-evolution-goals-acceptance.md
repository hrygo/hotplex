---
title: "HotPlex 进化实施方案：实施目标与验收标准"
weight: 10
description: "A–H 全部工作单元的实施目标与可执行验收标准摘要。"
date: 2026-10-04
status: proposed
---

# 实施目标与验收标准

依据 `2026-10-03-hotplex-evolution-luna-guide.md`，覆盖 A–H 全部工作单元。

## 一、实施目标

把 HotPlex 从"统一接入 Coding Agent 的网关"进化为"配置生效可证明、输入去向可解释、结果交付可核验、故障可恢复的运行网关"。保持 Gateway → Session/Execution → Worker 分层，不另建状态机或调度层，按 S0 证据 → S1 运行与交付 → S2 操作投影 → S3 条件产品化串行交付，每个自洽单元独立提交。

A 修 CI 反向依赖闭包筛选；B 让补充输入 ACK 分别表达接收方式、耐久性与派发阶段；C 建立 Cron occurrence → execution → effect → provider 回执的持久交付；D 让同一份 resolved plan 真正驱动启动，公开 hash 与内部 launch 指纹分离；E 提供 strict env 与能力证据，按 backend 实际能力 fail closed；Q 把 `execution_inputs` 扩展为 canonical 持久排队；F 用同一 source SHA 门禁发布并对原生产物做 smoke；G 以有界投影提供执行时间线与服务端推导的安全动作；H 仅在 #870 启动条件满足后做版本化 Recipe。

## 二、验收标准

1. 行为闭环：故障样本无需翻多份日志即可定位阶段；跨 workspace 访问拒绝；stale version 返回 409；中英文一致；无"强制 success"入口。
2. 安全语义：越过 dispatch 边界保持 `unknown` 与 fence；响应丢失、回执写库失败、lease 过期均不产生重复 effect；无 provider 幂等保证时停止自动发送。
3. 单一事实源：诊断、launch、bootstrap evidence 指向同一 plan revision，历史 execution 不被改写。
4. 数据与协议：SQLite/PG 迁移成对；控制事实只存引用与指纹，不存 prompt、secret、metadata 值与原始 Worker 错误；事件变更同步四 SDK 与文档。
5. 门禁可证伪：故意失败必须阻断发布；产物关联同一 SHA 与 lock；缺 `HOTPLEX_TEST_PG_DSN` 或 skip 一律标"未验证"。
6. 验证：`rtk proxy make quality` / `docs-lint` / `build`、`scripts/ci` 单测、`go test -short -count=1 -race -shuffle=on`、`make test-contract-matrix` 退出码 0。
7. 证据：每单元回填 commit 与 Source/Test/Live，未覆盖项如实标注，不以历史计数充当本轮证明。
