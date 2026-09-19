# 升级到 Assistant v1.1.0 / Feedback v0.6.0

v1.1.0 新增「反馈管理面」：APK 内提交反馈（截图 + 日志）、反馈列表 / 实时详情 /
单条管理操作（归档 / 取消归档 / 回收站 / 恢复 / 安全重试）。管理面依赖 Feedback ≥ v0.6.0
与中枢 ≥ 1.1.0；任一侧未升级时旧能力（事件、附件、通知）不受影响，管理入口提示需升级。

## 升级顺序（严格按序）

1. **备份 Feedback 数据**（服务侧 SQLite / 数据目录整体拷贝，含 `assist_outbox` 待投递事件）。
2. **升级 Feedback 到 v0.6.0**（沿用既有 compose/镜像流程；迁移 v12 `assist_mgmt_requests` 自动应用）。
3. **配置管理凭证**：
   - 在中枢侧对 feedback 来源执行 `POST /api/v1/sources/:id/rotate-mgmt-key` 取得 `amk_…`（仅此一次明文）；
   - 在 Feedback 服务 env 增加 `FEEDBACK_ASSIST_MGMT_KEY=<同一值>` 并重启。
   - 该凭证必须与 `FEEDBACK_ASSIST_READ_KEY`/`FEEDBACK_ASSIST_SOURCE_KEY` 生效值不同，否则管理组不挂载。
   - 只读链路（`READ_KEY`/`SOURCE_KEY`）不变，已配置的接入不需要重建。
4. **升级中枢到 1.1.0**（三选一：GHCR 镜像 `ghcr.io/sakuralovesmile/assistant-hub:v1.1.0`
   ——固定 digest `sha256:21a7077a21ddbd96a268235f90e3d29c7f9de6a9171d3b54efbfb4047df47c1b`；
   compose 本地构建；或直接替换压缩包二进制。重启即迁移；来源记录新增 mgmtKey 字段，向后兼容）。
5. **安装 APK 1.1.0+2**（签名证书与此前 debug 构建不同：系统会要求先卸载旧包——**卸载前**确认
   本地缓存可丢；服务端数据不受影响）。若手机已装版本即同一签名则直接覆盖升级。

## 版本矩阵（能力降级）

| Feedback 服务 | 中枢 | APK | 效果 |
|---|---|---|---|
| ≥0.6.0 + MGMT_KEY | ≥1.1.0 + 已配 mgmtKey | ≥1.1.0 | 全部能力 |
| ≥0.6.0 未配 MGMT_KEY | ≥1.1.0 | ≥1.1.0 | 事件/附件/详情可看；列表与操作显示「需配置管理凭证」 |
| ≤0.5.2 | ≥1.1.0 | ≥1.1.0 | 事件/附件可看；管理功能显示「需升级 Feedback 服务」 |
| 任意 | ≤0.1.0 | ≥1.1.0 | APK 管理入口提示中枢版本过旧（旧 API 无 §3.1 端点） |

## 回退

- 中枢回退 0.1.0：新列保留在库中不冲突；管理端点消失，APK 回到只读提示。
- Feedback 回退 ≤0.5.2：管理组 404 → APK 显示「需升级」；outbox 中尚未投递的事件保留，
  升回后自动续传。
- APK 回退旧版：需卸载重装（签名不同）；服务端无需动作。
