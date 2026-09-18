# Flutter ↔ Kotlin 桥接契约 v1（冻结）

职责划分：

- **Kotlin 原生前台服务**（Android agent 独占 `app/android/`）：常驻同步（SSE → 断线轮询）、
  本地通知创建与更新、DND 抑制与结束汇总、通知点击投递、权限 / 省电引导跳转、
  原生侧小型持久化（通知去重、待汇总队列、配置副本、lastChangeSeq）。
- **Flutter UI**（Flutter agent 独占 `app/lib/`、`app/test/`、`pubspec.yaml`）：全部页面、
  REST 业务读写（sync / read / mute / 规则 / 接入项 / 设置）、Dart 侧历史缓存。
- 通知**绝不依赖** Dart 侧运行：Flutter 进程被杀，服务仍收 SSE、发通知、点击后可冷启动进入对应详情。

## 1. MethodChannel `assistant/native`

Dart → 原生（均 async 返回）：

| 方法 | 参数 | 返回 | 语义 |
|---|---|---|---|
| `configure` | `{ "hubUrl": string, "token": string, "refreshToken": string, "reportIntervalSeconds": int, "dnd": { "enabled": bool, "start": "HH:mm", "end": "HH:mm", "timezone": "Asia/Shanghai" } }` | `{ "ok": true }` | 写入原生配置副本并立即生效；token/refreshToken 变化重建 SSE；dnd 变化重算抑制状态。**Dart 侧每次登录 / 设置变更 / 冷启动都会调用**（幂等） |
| `startService` | — | `{ "running": true }` | 启动前台服务（含常驻通知）；已运行则幂等返回 |
| `stopService` | — | `{ "running": false }` | 停止前台服务并清除常驻通知（业务通知保留） |
| `getServiceState` | — | `{ "running": bool, "hubReachable": bool, "lastSyncAt": string?, "lastChangeSeq": int, "dndActive": bool, "heldCount": int, "authExpired": bool, "lastError": string? }` | 供首页状态条与设置页显示；`hubReachable` = SSE 通道是否建立（中枢不可达）；手机断网由 Dart 侧 connectivity 自行区分显示；`authExpired` = refresh 已失效、需重新登录（此时 SSE 停摆，见 §3） |
| `getLaunchPayload` | — | `{ "route": "message"|"fault"|"home", "id": string? }` 或 `null` | **冷启动**经通知点开的跳转目标；取一次即清除。Dart 在 `main()` 早期调用 |
| `consumePendingRoute` | — | 同上 | 热启动 / 前台时事件通道未及时送达的兜底拉取 |
| `openNotificationSettings` | — | `bool` | 跳系统通知设置页（本应用） |
| `openBatteryOptimizationSettings` | — | `bool` | 跳「忽略电池优化」授权页 |
| `openAutostartSettings` | — | `bool` | 尽力跳 MIUI 自启管理页（不可达返回 false，UI 退化为图文引导） |
| `getDeviceInfo` | — | `{ "manufacturer": "Xiaomi", "model": "Mi 10", "sdkInt": 34, "miui": "HyperOS 3"? }` | 设置页「权限引导」按此定制文案 |
| `setDebugLogging` | `{ "enabled": bool }` | `{ "ok": true }` | 原生侧诊断日志开关（通知到达计时验证用） |

原生侧不得反向要求 Dart 处理通知数据；Dart 经 REST 自行拉取详情。

## 2. EventChannel `assistant/native_events`

原生 → Dart 流（Dart 存活期间）：

```jsonc
{ "type": "launch",    "route": "message", "id": "msg_…" }     // 热态通知点击
{ "type": "service",   "state": { 同 getServiceState } }        // 服务状态变化主动推送
{ "type": "summary",   "delivered": 5 }                          // DND 结束汇总已发出（供 UI 记录用，可选处理）
```

`launch` 投递保证：服务先把 route 写入原生暂存（pending route），再尝试发事件；Dart 端另外用
`consumePendingRoute` 兜底，因此事件丢失不会让点击失效。

## 3. 通知行为（原生实现细节冻结）

- **通知渠道**（channel id）：`assistant.messages`（新反馈 / 独立消息，importance DEFAULT）、
  `assistant.faults`（故障，importance HIGH）、`assistant.summary`（DND 汇总，DEFAULT）、
  `assistant.service`（常驻通知，MIN）。渠道一经创建不改 importance（系统限制）。
- **触发点**（依 events.md §4）：SSE `message` 事件的 `notify.kind`：
  - `new_message` → 新通知，group `src_<sourceId>`；
  - `incident_open` → 新通知，id 用 faultId hash，group `flt`；
  - `incident_update` → `setOnlyAlertOnce(true)` 更新同 id 通知内容（无新声音）；
  - `incident_resolved` → 更新同 id 通知（标题追加「已恢复」），`onlyAlertOnce`；
  - `notify.muted=true` 或该 faultId 本地已知静音 → 不通知（仍持久化 lastChangeSeq）。
- **点击**：PendingIntent → MainActivity，`extra: route="message"/"fault", id`。
  汇总通知 → `route="home"`。MainActivity 负责转 `getLaunchPayload` / `consumePendingRoute`。
- **常驻通知**：标题「Assistant 服务运行中」，副标题 lastSyncAt / hubReachable；点击打开首页。
- **DND**：本地按配置副本判定（`HH:mm` 跨零点窗口支持，如 23:00–08:00）。窗口内命中的通知
  **不发声 / 不振动 / 不横幅**：逐条计数并保留最后一条标题；窗口结束发 `assistant.summary`
  一条汇总（`heldCount` 条 / 含各来源计数）。严重故障不绕过。窗口内也**不更新**已有通知。
- **断线策略**：SSE 断开 → 指数退避重连（1s→60s 封顶，网络恢复立即重试）；
  另挂 WorkManager 周期兜底（15min，防止 SSE 僵死与系统误杀后自愈）。
  `hubReachable=false` 期间收到的通知事件在重连后经 `since` 续传补回（中枢保留变更窗口）。
- **401 自愈**：SSE / 轮询遇 `401` → 用本地 `refreshToken` 调 `POST /api/v1/auth/refresh`
  （一次性轮换；成功后更新本地 token/refreshToken 副本并立即重连，不打扰用户）。
  `invalid_refresh` → 置 `authExpired=true`、停止重连，待 Dart 重新登录后 `configure` 恢复。
- **重启 / 升级**：`BOOT_COMPLETED` + `MY_PACKAGE_REPLACED` 接收器在用户曾启动服务时自动拉起前台服务；
  「强行停止」后不承诺拉起（系统限制，设置页明示）。
- **去抖**：同一 faultId 的 incident 通知 60s 内最多更新 3 次（防风暴刷屏；内容始终为最新）。
- **唤醒约束**：原生处理每批事件 < 200ms 主线程外完成；不做轮询忙等；SSE 用 OkHttp 流式读取。

## 4. 原生持久化（内部约定，不跨进程）

`Room`/SQLite 库 `assistant_native.db`（与 Dart 侧 `assistant_cache.db` 互不干扰）：

- `config`：hubUrl、token、refreshToken、reportInterval、dnd 副本、lastChangeSeq；
- `notified`：faultId → (incident, lastNotifiedAt, muted) —— 静音映射随 SSE fault 事件刷新；
- `held`：DND 窗口内被抑制通知的计数与最后标题（窗口结束消费）。

Dart 侧 `sqflite`/`drift` 缓存库 `assistant_cache.db`：同步来的 messages / faults / sources /
settings / rules 快照 + sync cursor —— 历史页离线可读（实现归 Flutter agent）。

## 5. Manifest 基线（Android agent 落地）

- 权限：`POST_NOTIFICATIONS`、`FOREGROUND_SERVICE`、`FOREGROUND_SERVICE_DATA_SYNC`、
  `RECEIVE_BOOT_COMPLETED`、`INTERNET`、`ACCESS_NETWORK_STATE`、`WAKE_LOCK`（仅必要短时）。
- Service：`AssistantSyncService : Service`，`foregroundServiceType="dataSync"`，常驻通知 id=1。
- Receiver：`BootReceiver`（BOOT_COMPLETED / MY_PACKAGE_REPLACED）。
- 网络：生产仅 HTTPS；`networkSecurityConfig` 允许用户配置的 http 开发地址（debug 构建限定，release 移除明文 HTTP 支持）。
- applicationId：`com.sakurasep.assistant`；minSdk 29，target/compileSdk 36。
