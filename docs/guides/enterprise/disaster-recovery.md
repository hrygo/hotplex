---
title: Disaster Recovery Guide
weight: 27
description: RTO/RPO targets, backup strategies, restore procedures, and DR testing for enterprise HotPlex deployments.
---

# Disaster Recovery Guide

> 面向企业运维团队的 HotPlex 灾难恢复指南。涵盖恢复目标、备份策略、恢复流程和 DR 演练。

---

## 1. 恢复目标

| 场景 | RTO | RPO | 恢复方式 |
|------|-----|-----|----------|
| 进程崩溃 | < 1 分钟 | 0（自动重启） | systemd / Docker 自动恢复 |
| 主机故障 | < 5 分钟 | < 1 小时 | 备用主机 + 最近备份 |
| 数据损坏 | < 15 分钟 | < 1 小时 | SQLite 备份还原 |
| 全面灾难 | < 1 小时 | < 24 小时 | 全量重建 + 备份恢复 |

---

## 2. 自动重启机制

### systemd

```ini
[Unit]
After=network.target

[Service]
ExecStart=/usr/local/bin/hotplex gateway start
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

### Docker

```yaml
services:
  gateway:
    image: hotplex/gateway:latest
    restart: unless-stopped
    volumes:
      - hotplex-data:/var/lib/hotplex
```

### LLM 自动重试

Worker 因临时错误（429 Rate Limit、529 Overload、网络超时）崩溃时，Gateway 自动指数退避重试，无需人工干预。默认配置：最多 9 次重试，基础延迟 5s，最大延迟 120s。

---

## 3. SQLite 备份策略

### 自动备份（推荐）

使用 `sqlite3` backup API 进行一致性备份，无需停止服务：

```bash
#!/bin/bash
# /etc/hotplex/backup.sh — 每小时执行
BACKUP_DIR="/var/backups/hotplex"
DB_PATH="/var/lib/hotplex/hotplex.db"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)

# 在线备份（不锁库）
sqlite3 "$DB_PATH" ".backup '${BACKUP_DIR}/hotplex-${TIMESTAMP}.db'"

# 完整性校验
sqlite3 "${BACKUP_DIR}/hotplex-${TIMESTAMP}.db" "PRAGMA integrity_check;"

# 保留 30 天
find "$BACKUP_DIR" -name "*.db" -mtime +30 -delete
```

HotPlex 的会话生命周期清理不会改写已有备份。若恢复的快照包含后来已删除的会话，恢复后这些数据可能重新出现；部署方应为在线及离线备份单独设定保留与销毁期限，并在恢复后按需重新执行清理。

Cron 配置：

```
0 * * * * /etc/hotplex/backup.sh >> /var/log/hotplex-backup.log 2>&1
```

### 手动备份

```bash
# 推荐先停止服务（保证一致性）
systemctl stop hotplex

# 复制数据库
cp /var/lib/hotplex/hotplex.db \
   /backups/hotplex-backup-$(date +%Y%m%d-%H%M%S).db

# 验证完整性
sqlite3 /backups/hotplex-backup-*.db "PRAGMA integrity_check;"
# 期望输出: ok

systemctl start hotplex
```

---

## 4. 恢复流程

### 4.1 进程崩溃（自动恢复）

**症状**：健康检查失败，服务无响应

```bash
# 确认自动恢复
systemctl status hotplex
journalctl -u hotplex -n 50

# 手动干预（如果自动重启失败）
systemctl restart hotplex

# 验证
curl http://localhost:9999/admin/health
```

### 4.2 数据库损坏

**症状**：日志出现 `database disk image is malformed`，健康检查返回 `degraded`

```bash
# 1. 停止服务
systemctl stop hotplex

# 2. 确认损坏
sqlite3 /var/lib/hotplex/hotplex.db "PRAGMA integrity_check;"

# 3. 定位最近有效备份
LATEST=$(ls -t /var/backups/hotplex/*.db | head -1)

# 4. 验证备份完整性
sqlite3 "$LATEST" "PRAGMA integrity_check;"

# 5. 保留损坏文件供分析
mv /var/lib/hotplex/hotplex.db \
   /var/lib/hotplex/hotplex.db.corrupted.$(date +%Y%m%d)

# 6. 恢复备份
cp "$LATEST" /var/lib/hotplex/hotplex.db
chown hotplex:hotplex /var/lib/hotplex/hotplex.db
chmod 644 /var/lib/hotplex/hotplex.db

# 7. 启动服务
systemctl start hotplex

# 8. 健康检查
curl http://localhost:9999/admin/health
```

### 4.3 全面主机恢复

```bash
# 1. 安装依赖
apt-get update && apt-get install -y golang git openssl curl

# 2. 恢复配置和密钥
cp /path/to/backup/config.yaml /etc/hotplex/config.yaml
cp /path/to/backup/.env /etc/hotplex/.env

# 3. 恢复数据库
mkdir -p /var/lib/hotplex/data
cp /path/to/backup/hotplex.db /var/lib/hotplex/data/

# 4. 构建安装
./scripts/install.sh --non-interactive

# 5. 启动并验证
systemctl start hotplex
curl http://localhost:9999/admin/health
```

---

## 5. 密钥轮转流程

### Admin Token 轮转（零停机）

```bash
# 1. 生成新 Token
NEW_TOKEN=$(openssl rand -base64 32 | tr -d '/+=' | head -c 43)

# 2. 更新 _2（保留 _1）
export HOTPLEX_ADMIN_TOKEN_2="$NEW_TOKEN"

# 3. 所有客户端切换到 _2

# 4. 更新 _1 为新 Token，清除旧 _2
```

---

## 6. DR 演练

### 演练计划

| 演练类型 | 频率 | 验证要点 |
|----------|------|----------|
| 备份还原 | 月度 | 备份完整性、恢复时间、数据完整性 |
| 全面灾难恢复 | 季度 | 端到端恢复、密钥轮转、客户端重连 |
| 密钥轮转 | 月度 | Token 更新、服务连续性 |

### 演练检查清单

- [ ] 备份文件可正常还原
- [ ] 还原后 `PRAGMA integrity_check` 返回 `ok`
- [ ] 健康检查返回 `healthy`
- [ ] Session 可正常创建和交互
- [ ] 配置热重载正常工作
- [ ] 日志中无 ERROR 级别异常
- [ ] 监控面板恢复正常指标

### 事后复盘模板

```
演练日期: YYYY-MM-DD
演练类型: [备份还原/全面恢复/密钥轮转]
实际 RTO: ___ 分钟
实际 RPO: ___ 分钟
发现问题: ___
改进措施: ___
下次演练: YYYY-MM-DD
```

---

## 7. 常用诊断命令

```bash
# 健康检查
curl http://localhost:9999/admin/health | jq

# Session 列表
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:9999/admin/sessions | jq

# 数据库统计
sqlite3 /var/lib/hotplex/hotplex.db <<EOF
SELECT 'sessions' as tbl, COUNT(*) FROM sessions
UNION ALL SELECT 'events', COUNT(*) FROM events;
EOF

# 错误统计（24 小时）
journalctl -u hotplex --since "24 hours ago" | grep -c "ERROR"

# 最近备份列表
ls -lh /var/backups/hotplex/*.db | tail -10
```
