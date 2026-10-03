---
title: "Coding Ops Recipes 模板（未启动）"
weight: 10
description: "#870 的两个试点模板草案，以及启动条件为何尚未满足的逐条核对。"
date: 2026-10-04
status: draft
---

# Coding Ops Recipes 模板（未启动）

本文是 [#870](https://github.com/hrygo/hotplex/issues/870) 的**模板草案**，不是产品契约，也不是实现说明。截至 2026-10-04，三条启动条件中有两条不满足，因此仓库里**没有** recipe registry、manifest 校验或 dry-run 引擎。以下内容用于在条件满足时直接实施，而不是描述现状。

## 一、启动条件核对（2026-10-04）

| #870 启动条件 | 当前状态 | 依据 |
| --- | --- | --- |
| Stage 1–3 的 plan divergence、unknown age、reconcile result、delivery loss/duplicate 与数据库容量具有持续运行证据 | **未满足** | Stage 1–3 的 #849、#946、#867、#947、#851、#868 在 GitHub 上仍为 open；相关实现位于未发布分支，没有生产 telemetry可言。用开发环境的读数冒充"持续运行证据"会把观察变成结论。 |
| 至少一个真实业务场景无法由现有单 Agent runtime + tools/context 满足 | **未满足** | 没有任何已登记的场景证据说明单 Agent 路径失败。"模板看起来有用"不构成这个条件。 |
| 新能力不建立平行控制面 | 结构性可满足 | 模板 3、4 节明确复用 Cron/Webhook/Session/Execution/EffectLedger/audit；但前两条不满足时不进入实现。 |

结论：停留在模板。任何 manifest schema、registry、版本校验或 dry-run 执行器都要等前两条有证据之后再写。

## 二、模板一：失败构建诊断（只读）

| 字段 | 值 |
| --- | --- |
| trigger | Webhook（构建平台回调）或 Cron（构建产物落盘后） |
| workspace ref | 触发构建的仓库工作区；必须是调用方已授权的 workspace |
| worker | 只读分析用；不得获得写权限 |
| permission profile | read-only：可读构建证据，不可写工作区、不可改配置 |
| template ref | 内置模板 + 版本号，模板正文不入 manifest |
| timeout | 短于构建平台的 job 超时，避免诊断任务与其竞争 |
| delivery target | 报告发布目标（Issue 评论或消息渠道） |

行为：

- **只读分析已有构建证据**——日志、退出码、失败步骤。不重跑构建，不修改代码，不合并，不发布。
- 输出三段：原因、证据引用（指向具体日志片段）、建议报告（下一步由人决定）。
- delivery 走 EffectLedger：同一逻辑交付只有一个 effect，重试产生新 attempt 而不是新 effect；响应丢失进入 `unknown`，在没有幂等或查询能力时停止自动重发。
- dry-run 只解析（resolve）出 `EffectiveRuntimePlan`：权限、schedule、env/isolation capability、delivery contract 与 plan hash；**不启动 Worker，不发送任何消息**。

## 三、模板二：仓库健康巡检（增量）

| 字段 | 值 |
| --- | --- |
| trigger | Cron（固定周期） |
| workspace ref | 巡检范围内的仓库工作区集合，逐个校验授权 |
| worker | 与仓库既有 Worker 一致，不新增 Worker 类型 |
| permission profile | read-only；健康巡检没有理由写 |
| template ref | 内置模板 + 版本号 |
| timeout | 小于周期长度，避免与下一轮重叠 |
| delivery target | 巡检报告发布目标 |

行为：

- 用 **finding fingerprint** 去重，用 **last published checkpoint** 只输出增量发现；没有增量的周期不产生交付。
- **checkpoint 仅在 effect confirmed 后推进**。交付失败、进入 `unknown` 或被放弃时，checkpoint 保持不动——否则一次失败的交付会永久吞掉这些发现。
- 重复 trigger 稳定：同一份仓库状态重复巡检得到同一组 fingerprint。
- dry-run 零副作用：不写 checkpoint、不发消息、不启动 Worker。

## 四、两个模板共同的 fail-closed 条件

manifest 校验必须拒绝以下输入，且拒绝发生在启动 Worker 之前：

- 未授权的 workspace owner；
- 不存在或越界的 workspace path；
- 未知 Worker 类型；
- 无法满足的 permission profile 或 env/isolation capability；
- 缺失或无效的 delivery target。

不允许的部分回退：任一校验失败时整体拒绝，不允许"跳过这一项继续跑"。

## 五、指标与分母

| 指标 | 分母 / receipt 级别 |
| --- | --- |
| 每 workspace 每周 provider-confirmed 任务数 | 分母是该 workspace 该周的触发次数；分子是 provider 回执确认的完成数 |
| accepted → 完成 | 分母是 accepted 的 execution 数，分子是 runtime 进入终态的数；`unknown` 单列，不计入完成 |
| delivery confirmed | 分母是 planned 的 effect 数，分子是 provider accepted 的数 |
| unknown 数量与年龄 | 分母是全部 effect；年龄是最后一个 observed 事实到观测时刻的距离 |
| queue 等待 | 分母是进入 dispatch 的 execution；等待时长从 accepted 到 claimed |
| 人工恢复时间 | 分母是所有触发过 operator 动作的记录 |

在没有真实数据之前不给生产成功率目标。当前仓库里的数都是开发环境的读数，写成目标会把观察值变成承诺。

## 六、与 workflow / DAG engine 的边界

Recipe 是**一次触发、一个 workspace、一个 Worker、一份 plan、一个交付目标**。它不实现：multi-agent DAG、跨 session 分布式调度、marketplace、worker-private tool protocol，也不引入独立的调度层——触发复用 Cron/Webhook，执行复用 Session/Execution，交付复用 EffectLedger。

若一个需求需要这些能力中的任何一个，它不是 Recipe，应该作为独立提案重新评审。
