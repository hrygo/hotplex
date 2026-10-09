---
title: Resource Limits Guide
weight: 25
description: Global and per-user quotas, worker limits, output limits, pool management, and tuning recommendations for HotPlex.
---

# Resource Limits Guide

> 面向企业运维团队的 HotPlex 资源限制配置指南。涵盖全局配额、用户配额、Worker 限制、输出限制和调优建议。

---

## 1. 全局 Session 限制

```yaml
session:
  max_concurrent: 1000       # 状态机层最大并发 Session（默认 1000）
  retention_period: 168h     # 兼容的运行到期时长，不是聊天历史保留期
  gc_scan_interval: 10m      # GC 扫描间隔（默认 10 分钟）

lifecycle:
  policy: v2
  conversation:
    archive_after: 168h                         # 7 天无输入后显示归档标记
    retention_after_last_input: 4320h           # 会话期限：180 天
  content:
    retention: 4320h                            # 每条正文期限：180 天

pool:
  max_size: 100              # 全局最大活跃 Session（默认 100）
  min_size: 0                # 最小保留 Session（默认 0）
```

`session.max_concurrent` 限制状态机层面的并发转换数，`pool.max_size` 限制实际 Worker 进程数。两者独立生效，取较严格者。

---

## 2. Per-User 配额

```yaml
pool:
  max_idle_per_user: 5                 # 每用户最大空闲 Session（默认 5）
  max_memory_per_user: 3221225472      # 每用户最大内存 3GB（默认 3GB）
```

### 配额执行机制

```
Acquire(userID)
  ├─ 全局检查: totalCount < max_size         → 否: PoolExhausted
  ├─ 用户检查: userCount[userID] < max       → 否: UserQuotaExceeded
  └─ 内存检查: userMemory + 512MB < limit    → 否: MemoryExceeded
```

每个 Worker 统一按 **512MB** 估算（匹配 Linux RLIMIT_AS 上限），不区分实际使用量。

**动态调整**：`PoolManager.UpdateLimits()` 运行时修改配额。降低配额不会驱逐已有 session，仅阻止新请求直到自然释放。

---

## 3. Worker 进程限制

```yaml
worker:
  max_lifetime: 24h           # 单 Worker 最长存活时间（默认 24h）
  idle_timeout: 60m           # 空闲超时（默认 60m）
  execution_timeout: 30m      # 单次执行超时（默认 30m）
  turn_timeout: 30m           # 单轮绝对上限（持续输出不会延长）
```

`worker.idle_timeout` 释放运行资源，`lifecycle.conversation.archive_after` 只标记冷会话，`lifecycle.conversation.retention_after_last_input` 与 `lifecycle.content.retention` 管理不同的数据期限。查看历史不会延长任何期限。`session.retention_period` 是兼容运行到期时长，不应用来设置聊天正文保留期。

| 限制 | 触发条件 | 行为 |
|------|----------|------|
| `max_lifetime` | Worker 存活超过 24h | 强制终止 |
| `idle_timeout` | 无 I/O 超过 60m | GC 回收到 TERMINATED |
| `execution_timeout` | 单次运行超过 30m | 标记为 zombie 并终止 |

### 进程内存硬限制（Linux）

Worker 进程启动时设置 `RLIMIT_AS = 512MB`：

```
Linux/POSIX: setrlimit(RLIMIT_AS, {Cur: 512MB, Max: 512MB})
macOS:       不支持（自动跳过）
Windows:     不支持（无 POSIX setrlimit）
```

超出限制时 Worker 进程被 OS 强制终止（OOM），Gateway 检测退出后自动清理。

---

## 4. 输出限制

```go
MaxLineBytes     = 10 * 1024 * 1024   // 10MB — 单行输出上限
MaxSessionBytes  = 20 * 1024 * 1024   // 20MB — 单 Session 累计输出上限
MaxEnvelopeBytes = 1 * 1024 * 1024    // 1MB  — 单个 AEP envelope 上限
```

| 限制 | 默认值 | 触发行为 |
|------|--------|----------|
| `MaxLineBytes` | 10MB | 超出行被截断，记录警告 |
| `MaxSessionBytes` | 20MB | 超出后停止接收输出，Worker 终止 |
| `MaxEnvelopeBytes` | 1MB | 输入验证直接拒绝，返回 `input too large` |

这些限制是代码级常量，不可通过配置修改。

---

## 5. Pool 管理流程

### Acquire / Release 生命周期

```
Session 创建 → PoolManager.Acquire(userID)
                ├─ 成功: totalCount++, userCount[userID]++
                └─ 失败: 返回 PoolError（含 Kind/UserID/Current/Max）

Session 结束 → PoolManager.Release(userID)
                ├─ 正常: totalCount--, userCount[userID]--
                └─ 双重释放: 记录 ERROR 日志 + metrics，尽力清理
```

### 利用率指标

```promql
# Pool 利用率
hotplex_pool_utilization_ratio{instance="gateway:9999"}

# 配额拒绝率
rate(hotplex_pool_acquire_total{result="pool_exhausted"}[5m])
rate(hotplex_pool_acquire_total{result="user_quota_exceeded"}[5m])
rate(hotplex_pool_acquire_total{result="memory_exceeded"}[5m])
```

---

## 6. Cron 调度限制

```yaml
cron:
  max_concurrent_runs: 3     # 最大并发执行（默认 3）
  max_jobs: 50               # 最大任务数（默认 50）
  default_timeout_sec: 300   # 单次执行超时 5 分钟
  tick_interval_sec: 60      # 调度器检查间隔
```

Cron 任务在并发槽内执行，使用 CAS 原子操作控制并发数。超出限制时任务排队等待下一个 tick。

---

## 7. 调优建议

### 按团队规模

| 规模 | max_size | max_idle_per_user | max_memory_per_user | 说明 |
|------|----------|-------------------|---------------------|------|
| 小团队 (<20) | 50 | 3 | 2GB | 默认配置即可 |
| 中团队 (20-100) | 100 | 5 | 3GB | 默认配置 |
| 大团队 (100-500) | 200 | 5 | 5GB | 需要更多并发槽 |
| 企业 (500+) | 500 | 3 | 3GB | 收紧 per-user，提高全局 |

### 按使用场景

| 场景 | max_lifetime | idle_timeout | execution_timeout |
|------|-------------|-------------|-------------------|
| 代码助手 | 24h | 60m | 30m |
| CI/CD Agent | 8h | 30m | 10m |
| 长时间分析 | 48h | 120m | 60m |

### 内存规划

```
总内存需求 ≈ max_size × 512MB + Gateway 开销（~200MB）

示例：
  max_size=100 → 需要约 52GB 内存
  max_size=200 → 需要约 102GB 内存
```

### 关键原则

1. **全局配额 > 单用户配额之和**：确保有足够的空闲槽位
2. **memory_per_user / 512MB >= max_idle_per_user**：配额逻辑一致
3. **execution_timeout < idle_timeout**：避免 zombie 累积
4. **监控 Pool 利用率**：持续 >80% 时考虑扩容
5. **定期审查 Cron 任务数**：接近 max_jobs 上限时评估清理策略
