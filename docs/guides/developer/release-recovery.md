# 发布恢复：回滚与重发

发布流水线（`.github/workflows/release.yml`）在创建 Release 前校验
checksums、source SHA、每 artifact SBOM、签名与 attestation；
任一失败则发布中断，不产生 Release 页面。

## 回滚

1. 在 Releases 页找到上一个可信版本。
2. 用户侧：用该版本的 artifact 重装；`checksums.txt` + `checksums.txt.sigstore`
   按发布页验证说明核对。
3. 服务侧：`hotplex service restart` 重启网关（原子指令）。

## 重发

1. 修复原因提交到 main。
2. 重新打 tag 触发流水线；`publish` job 依赖
   `resolve、validate、build、offline-bundle、smoke` 全绿，
   任一失败不会覆盖已有 Release。
3. 同一 tag 不可复用：需发新 patch 版本。

## 失败测试挡发布

`validate` 的 `gate` job 要求 lint、build、test、contract-matrix、
webchat、SDK conformance、PostgreSQL 全成功；任一失败则
`publish` 不运行。反向验证：`verify_release_artifacts.py` 在发布前
以发布者视角重跑下载者校验，缺 SBOM/签名/attestation 即失败。
