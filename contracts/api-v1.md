# Assistant 服务契约 v1（冻结）

状态：**已冻结**。冻结后仅允许兼容变更（新增端点、新增可选字段、枚举新增取值、新增事件 kind）。
禁止破坏性变更（改字段类型、删字段、改语义、改认证方式）。确需破坏性变更 → 新版本路径 `/api/v2`，v1 保留。

Base URL：`http(s)://<hub-host>:8795`。所有 JSON 请求 `Content-Type: application/json`；
客户端与接入端点请求体上限 256 KiB（附件不经由 JSON 传输）。
时间一律 RFC3339 UTC（建议毫秒精度，`2026-09-18T06:00:00.123Z`）；`occurredAt` 由来源时钟产生，其余时间戳由中枢产生。
ID 均为带前缀 ULID：`src_`（来源）、`msg_`（消息）、`flt_`（故障）、`tok_`（令牌）、`evt_`（中枢自产事件）。

## 错误信封

所有错误统一：

```jsonc
{ "error": { "code": "<machine_code>", "message": "<人类可读，可安全展示>" } }
```

`message` 措辞不作为判等依据。通用 code：

| code | HTTP | 场景 |
|---|---|---|
| `invalid_request` | 400 | 参数缺失 / 非法 / 超限 |
| `unauthorized` | 401 | 令牌或来源密钥缺失 / 失效 / 未知 |
| `forbidden` | 403 | 凭证有效但无权限（含来源被停用 `source_disabled`） |
| `not_found` | 404 | 对象不存在（不区分"不存在"与"不可见"） |
| `conflict` | 409 | 通用冲突（版本过期时另用 `version_conflict`） |
| `version_conflict` | 409 | 乐观锁版本不匹配，响应携带当前 `version` |
| `too_large` | 413 | 请求体超限 |
| `rate_limited` | 429 | 限流，携带 `Retry-After`（秒） |
| `attachment_unavailable` | 502 | 附件上游（如 Feedback）不可达或返回错误 |
| `internal` | 500 | 未预期错误；绝不回传堆栈 |

限流：`/api/v1/auth/login` 按 IP+用户名滑动窗口（10 次 / 5 分钟）；接入与读取端点 MVP 不限流（独立密钥即凭证）。

## 认证（两类）

1. **客户端令牌**（APK）：`Authorization: Bearer <token>`，由 `POST /api/v1/auth/login` 签发。单账号，账号由部署环境变量 `ASSIST_ADMIN_USER` / `ASSIST_ADMIN_PASSWORD` 建立，不开放注册。
2. **来源密钥**（采集程序 / Feedback 接入层）：`Authorization: Bearer <source-key>`。密钥格式 `ask_<48 位小写十六进制>`，在创建接入项时由中枢生成并**仅此一次明文返回**。中枢按密钥定位来源；未知、吊销或停用一律 `401`/`403`。密钥同时用于中枢回连该来源的附件只读接口（见 `attachmentBaseUrl`），中枢库内保存明文用于回连，接口永不回传（仅回显末 4 位 `keyHint`）。

---

## 1. 认证组（客户端）

### POST /api/v1/auth/login

```jsonc
// 请求
{ "username": "…", "password": "…", "deviceLabel": "Mi 10" }   // deviceLabel 可选 ≤100 字符
// 200
{ "token": "tok_…", "expiresAt": "<iso>", "refreshToken": "rtok_…",
  "user": { "username": "…" }, "serverTime": "<iso>" }
```

- 失败：`401 invalid_credentials`（账号或密码错误，不区分）；`429 rate_limited`。
- `token` 30 天有效；`refreshToken` 90 天有效、一次性轮换（使用后旧值作废）。
- `serverTime` 供客户端校准本地时钟显示（不用于事件排序）。

### POST /api/v1/auth/refresh

```jsonc
{ "refreshToken": "rtok_…" }
// 200 同 login 响应（颁发新 token + 新 refreshToken）
// 401 invalid_refresh（过期 / 已轮换 / 未知）→ 客户端回到登录
```

### GET /api/v1/auth/session

`200 { "authenticated": true, "user": {"username"}, "expiresAt": "<iso>" }`；未认证 `401`。

### POST /api/v1/auth/logout

`204`。撤销当前 token 及其关联 refreshToken。

---

## 2. 来源接入组（来源 → 中枢）

来源分两类：`kind=device`（采集程序，上报指标）与 `kind=feedback`（Feedback 接入层，上报事件）。
两类都可调用两个接入端点；device 来源主要用 metrics，feedback 来源只用 events。

### POST /api/v1/ingest/metrics

```jsonc
{
  "seq": 42,                          // 本来源上报批次序号：单调递增 int64，重启不清零（持久化）
  "sentAt": "<iso>",
  "agent": { "version": "0.1.0", "os": "linux", "arch": "amd64", "hostname": "fn-nas" },
  "capabilities": {                    // 本批次探测能力快照；未知键必须容忍
    "containers": "ok",                // ok | unsupported | failed
    "smart":       "asleep",           // ok | asleep | unsupported | failed（asleep=有休眠盘未探测）
    "storagePool": "ok",               // ok | unsupported | failed
    "network":     "ok"
  },
  "sample": {
    "ts": "<iso>",                     // 采样时刻（来源时钟）
    "cpuPercent": 12.3,                // 0..100；采不到则字段缺席（不是 0）
    "memPercent": 45.6,
    "memUsedBytes": 123, "memTotalBytes": 456,
    "load1": 0.42,
    "uptimeSeconds": 123456,
    "bootTime": "<iso>",               // 本次启动时刻；变化即重启
    "disks": [                          // 块设备/挂载点用量；可多条
      { "mount": "/", "percent": 55.1, "usedBytes": 1, "totalBytes": 2, "fstype": "ext4" }
    ],
    "net": { "rxBps": 12345, "txBps": 6789 },   // 每秒字节速率（来源侧由计数器差值计算）
    "containers": [                     // capabilities.containers=ok 时全量列出受管容器
      { "name": "feedback", "state": "running",   // running|exited|restarting|paused|dead|created
        "exitCode": null, "startedAt": "<iso>", "restartCount": 2 }
    ],
    "nas": {                            // 仅飞牛来源；capabilities 指示支持度
      "disks": [ { "dev": "sda", "smart": "ok", "tempC": 38 } ],   // smart: ok|failing|asleep|unsupported|failed；asleep 时无 tempC
      "pools": [ { "name": "storage1", "state": "ok" } ]           // state: ok|degraded|error|unknown
    }
  }
}
```

响应：`202 { "accepted": true, "duplicate": false, "serverTime": "<iso>", "reportIntervalSeconds": 30 }`

- **幂等**：`seq ≤ 已应用的最大 seq` → `202 {"accepted": false, "duplicate": true}`，丢弃整批（样本按 `(sourceId, ts)` 唯一，重放天然幂等）。
- `reportIntervalSeconds` 回传中枢期望间隔，来源下次按此上报（默认 30s，可经规则接口调整）。
- 缺失指标：字段缺席即"采不到"，中枢/UI 按 capabilities 与缺席显示「不支持/采集失败」，**绝不显示为正常**。
- `sample.bootTime` 变化 → 中枢生成 `host_reboot` 事件；连续 `heartbeatSeconds`（默认 180s）无任何接入流量 → `heartbeat_lost` 故障（见 events.md）。
- `401` 未知密钥；`403 source_disabled` 来源已停用（来源继续排队，恢复后补传）。

### POST /api/v1/ingest/events

```jsonc
{
  "sentAt": "<iso>",
  "events": [
    {
      "eventId": "fb_01J…",               // 来源内唯一，[A-Za-z0-9._:-]{1,128}，必填
      "seq": 7,                           // 本来源事件序号：单调递增 int64，事件流内独立计数
      "kind": "feedback_created",         // 见 events.md 种类表；未知 kind 中枢存为 custom 处理
      "occurredAt": "<iso>",              // 事件发生时刻（来源时钟）
      "severity": "info",                 // info | warning | critical
      "faultKey": "feedback:01HX…",       // 可空：非空 → 参与故障合并；空 → 独立消息
      "incidentAction": "open",           // open | update | resolve | null（faultKey 非空时必填，独立消息为 null）
      "title": "新反馈：无法登录",          // ≤200 字符
      "body": "反馈正文…",                 // ≤20000 字符，可空
      "ref": { "feedbackId": "01HX…" },   // 可空，结构化引用（附件解析、详情跳转用）
      "attachments": [                     // 可空；仅描述符，字节经附件接口按需拉取
        { "id": "screenshot", "kind": "screenshot", "filename": "screenshot.png",
          "mime": "image/png", "byteSize": 12345, "sha256": "<64 hex 可空>" }
      ]
    }
  ]
}
```

响应：

```jsonc
{ "accepted": ["fb_01J…"],            // 新落库
  "duplicates": ["fb_01K…"],          // eventId 已存在（重放），不重复处理
  "rejected": [{ "eventId": "…", "code": "invalid_request", "message": "…" }],
  "lastSeq": 7, "serverTime": "<iso>" }
```

- **去重**：`(sourceId, eventId)` 唯一；重复投递进 `duplicates`，不产生新消息、不影响故障。
- **乱序**：中枢按 `(sourceId, seq)` 排序应用故障迁移；先到的 resolve 不关闭比它 seq 新的 open（详见 events.md）。乱序到达的事件仍入库为消息。
- 批次允许部分失败：合法事件正常落库，非法条目进 `rejected`，绝不整批 400。
- 单批 `events` ≤ 100 条，超出 `400 invalid_request`；来源应分批补传。
- `incidentAction=open` 的事件同时产生一条 `fault_open` 消息并入故障；`update` 并入当前轮次；`resolve` 结束当前轮次（语义见 events.md）。

---

## 3. 客户端读取组（APK → 中枢）

全部需要客户端令牌。读取端点兼容变更原则同 Feedback 契约：新增字段必须容忍。

### GET /api/v1/overview

首页一屏数据（避免多请求拼装）：

```jsonc
{
  "serverTime": "<iso>",
  "hubReachableHint": true,
  "sources": [
    { "id": "src_…", "name": "公网服务器", "kind": "device",
      "status": "online",                 // online | offline（>heartbeatSeconds 无接入）
      "lastSeenAt": "<iso>", "agentVersion": "0.1.0", "hostname": "vps",
      "capabilities": { "containers": "ok", "smart": "unsupported", "storagePool": "unsupported" },
      "summary": {                        // 最近一次样本摘要；offline 或从未上报为 null
        "ts": "<iso>", "cpuPercent": 12.3, "memPercent": 45.6,
        "disks": [{ "mount": "/", "percent": 55.1 }],
        "uptimeSeconds": 123456
      }
    },
    { "id": "src_…", "name": "Feedback", "kind": "feedback", "status": "online",
      "lastSeenAt": "<iso>", "summary": null }
  ],
  "openFaults": [
    { "id": "flt_…", "sourceId": "src_…", "sourceName": "飞牛 NAS",
      "faultKey": "container:feedback", "severity": "warning",
      "title": "容器 feedback 退出", "openedAt": "<iso>", "lastEventAt": "<iso>",
      "incident": 1, "eventCount": 3, "muted": false, "read": false }
  ],
  "unreadMessages": 4,                    // 未读消息总数（含独立消息与未读故障轮次）
  "rulesVersion": 3, "settingsVersion": 2,
  "dnd": { "enabled": true, "start": "23:00", "end": "08:00", "timezone": "Asia/Shanghai" }
}
```

### GET /api/v1/sync?since=\<cursor\>\&limit=200

增量同步，客户端重连后追平状态的**唯一入口**。

- `since`：上次返回的 `changeSeq`（缺省 `0` = 全量，首次登录使用）。
- 响应 `{ "cursor": 1234, "hasMore": false, "changes": [ … ] }`；`hasMore=true` 时用新 cursor 继续拉。
- `changes` 按 `changeSeq` 升序；每条 `{ "changeSeq": n, "type": "…", "data": {…} }`：

| type | data | 说明 |
|---|---|---|
| `message` | 完整 Message 对象（下节） | 新消息或已读状态变化都走此类型（字段为准） |
| `fault` | 完整 Fault 对象 | 开 / 更新 / 恢复 / 静音 / 已读 |
| `source` | 完整 Source 对象 | 状态、能力、摘要变化 |
| `rules` | 完整 Rules 对象 | 告警规则新版本 |
| `settings` | 完整 Settings 对象 | 免打扰等新版本 |
| `tombstone` | `{ "type": "message|fault", "id": "…" }` | 历史清理删除（开放故障永不删除） |

- 一致性保证：同一 `changeSeq` 内对象自洽；客户端按序应用即可得到与中枢一致的本地状态。

### GET /api/v1/stream?since=\<cursor\>（SSE）

常驻连接通道，原生前台服务使用。

- `Content-Type: text/event-stream`；`Authorization: Bearer` 同 REST。
- 事件：`message` / `fault` / `source` / `rules` / `settings`（data 为对应完整对象 JSON，与 sync 同构）、`ping`（每 15s 保活，data 为 `{"serverTime"}`）。
- 每条 SSE 携带 `id: <changeSeq>`；客户端断线后以 `Last-Event-ID` 或 `since` 续传。
- `since` 落后过多（超过服务端保留的变更窗口）→ 服务端发 `event: resync` 后关闭，客户端走全量 sync。
- **notification 提示**：`message` 事件的 data 额外含 `notify` 提示块：
  `{ "notify": { "kind": "new_message|incident_open|incident_update|incident_resolved", "faultId": "flt_…|null", "muted": false } }`，
  供原生服务直接判定通知行为（静音故障不通知；incident_update 只刷新已有通知不重复提醒）。

### Message 对象

```jsonc
{
  "id": "msg_…", "changeSeq": 1234,
  "sourceId": "src_…", "sourceName": "Feedback",
  "kind": "feedback_created",              // 与事件 kind 一致；中枢自产为 fault_open 等
  "severity": "info",
  "title": "新反馈：无法登录", "body": "…",
  "occurredAt": "<iso>", "receivedAt": "<iso>",
  "readAt": null,                          // 非 null = 已读
  "faultId": "flt_…",                      // 属于某故障时非空
  "incident": 2,                           // 所属故障轮次
  "ref": { "feedbackId": "01HX…" },
  "attachments": [ { "id": "screenshot", "kind": "screenshot", "filename": "screenshot.png",
                     "mime": "image/png", "byteSize": 12345 } ]
}
```

### Fault 对象

```jsonc
{
  "id": "flt_…", "changeSeq": 1235,
  "sourceId": "src_…", "sourceName": "飞牛 NAS", "faultKey": "container:feedback",
  "severity": "warning",                    // 当前轮次内事件最高严重度
  "title": "容器 feedback 退出", "summary": "最近事件正文摘要",
  "state": "open",                          // open | resolved
  "incident": 1,                            // 当前轮次（恢复后再发生 +1）
  "openedAt": "<iso>", "lastEventAt": "<iso>",
  "resolvedAt": null, "resolvedBy": null,
  "eventCount": 3,                          // 当前轮次事件数
  "readAt": null,                           // 当前轮次是否已被用户查看
  "mutedAt": null,                          // 非 null = 静音（跨轮次持续，直到取消）
  "mutedUntil": null                        // null=无限期；"<iso>"=到时自动解除（预留，MVP 恒 null）
}
```

### GET /api/v1/messages?cursor=\<c\>\&limit=50\&filter=all|unread|feedback|faults\&source=\<srcId\>

历史查询（倒序分页）。`filter=feedback` 只含 feedback 来源独立消息；`faults` 返回开放与已恢复故障消息。
响应 `{ "items": [Message], "nextCursor": "…|null" }`。

### GET /api/v1/messages/:id

单条详情：`{ "message": Message, "fault": Fault|null }`（属于故障时携带故障全文）。

### GET /api/v1/messages/:id/attachments/:attachmentId

附件字节按需获取。中枢按来源 `attachmentBaseUrl` 回连来源只读接口取字节并透传。

- `200`：`Content-Type` = 描述符 `mime`，`Content-Length`，`Cache-Control: private, max-age=300`，`ETag` = sha256（有则）。
- `404` 附件不存在；`502 attachment_unavailable` 上游不可达 / 未配置回连地址；附件描述符存在但上游 404 同样 `404`。
- UI 先展示占位 / 加载态，失败显示「附件不可用」，绝不崩溃。

### 已读 / 静音（写组）

| 端点 | 语义 |
|---|---|
| `POST /api/v1/messages/:id/read` | 标记已读 → `200 { "message": Message }`；幂等（已读再调仍 200） |
| `POST /api/v1/messages/:id/unread` | 标记未读 → `200 { "message": Message }` |
| `POST /api/v1/faults/:id/read` | 当前轮次标记已读 → `200 { "fault": Fault }` |
| `POST /api/v1/faults/:id/mute` `{ }` | 静音故障 → `200 { "fault": Fault }`；幂等 |
| `POST /api/v1/faults/:id/unmute` | 取消静音 → `200 { "fault": Fault }` |
| `POST /api/v1/messages/read-all` `{ "before": "<iso 可空>" }` | 批量已读 → `200 { "updated": n }` |

已读 / 静音均落中枢并推进 `changeSeq`，经 sync/stream 广播到各端（MVP 仅一台手机验证，但语义按多端一致设计）。

### GET /api/v1/faults?state=open|resolved|all\&cursor=\&limit=50

故障队列历史。响应 `{ "items": [Fault], "nextCursor" }`。

### GET /api/v1/metrics/series?source=\<id\>\&metric=\<kind\>\&from=\<iso\>\&to=\<iso\>\&step=raw|5m|1h\&label=\<sel\>

趋势数据。`metric` ∈ `cpu_percent | mem_percent | disk_percent | net_rx_bps | net_tx_bps`；
`label` 如 `mount=/`（disk_percent 必填，其余忽略）。
`step=raw` 返回原始样本（7 天保留内）；`5m` / `1h` 走汇总表（90 天）。
响应 `{ "series": [{ "ts": "<iso>", "avg": 1.0, "min": 1.0, "max": 1.0 }] }`（raw 时 avg=value）。
区间缺数据返回空缺（不补 0），UI 显示断档。

---

## 4. 接入项与规则管理组（客户端 → 中枢）

### 来源对象（管理视图）

```jsonc
{ "id": "src_…", "name": "飞牛 NAS", "kind": "device",
  "enabled": true, "status": "offline", "lastSeenAt": "<iso>",
  "keyHint": "…a1b2",                     // 仅末 4 位
  "attachmentBaseUrl": "http://nas.local:8787",   // feedback 类来源的附件回连地址；device 类为 null
  "agentVersion": "0.1.0", "hostname": "fn-nas",
  "capabilities": { … },
  "installHint": null,                    // 创建时一次性返回后不再回显
  "createdAt": "<iso>", "updatedAt": "<iso>" }
```

| 端点 | 语义 |
|---|---|
| `GET /api/v1/sources` | `{ "sources": […] }` |
| `POST /api/v1/sources` `{ "name": "飞牛 NAS", "kind": "device", "attachmentBaseUrl": "…可空" }` | `201 { "source": Source, "accessKey": "ask_…", "install": { "env": {"ASSIST_HUB_URL": "…", "ASSIST_SOURCE_KEY": "ask_…"}, "command": "<shell 单行安装命令>", "note": "<markdown 说明>" } }`。`accessKey` **仅此一次明文**；重名不查重（允许多个同名来源） |
| `GET /api/v1/sources/:id` | `{ "source": Source }` |
| `PATCH /api/v1/sources/:id` `{ "name"?, "enabled"?, "attachmentBaseUrl"? }` | `200 { "source": Source }` |
| `POST /api/v1/sources/:id/rotate-key` | `200 { "accessKey": "ask_…", "install": 同创建 }`；旧密钥立即失效，来源事件流不清除 |
| `DELETE /api/v1/sources/:id` | `204`；指标 / 消息 / 故障历史保留，密钥失效 |

### 告警规则

```jsonc
// GET /api/v1/rules →
{ "version": 3,
  "heartbeatSeconds": 180,                 // 失联判定，可改（60..3600）
  "rules": [
    { "id": "cpu_high", "kind": "threshold", "metric": "cpu_percent", "label": null,
      "op": "gt", "value": 90, "forSeconds": 180,
      "recoverValue": 75, "recoverForSeconds": 60,
      "severity": "warning", "enabled": true },
    { "id": "mem_high",  "kind": "threshold", "metric": "mem_percent", "value": 90,
      "forSeconds": 180, "recoverValue": 80, "recoverForSeconds": 60, "severity": "warning", "enabled": true },
    { "id": "disk_high", "kind": "threshold", "metric": "disk_percent", "label": "*",
      "value": 90, "forSeconds": 60, "recoverValue": 85, "recoverForSeconds": 60,
      "severity": "warning", "enabled": true },
    { "id": "container_exit", "kind": "container_exit", "match": "*", "severity": "warning", "enabled": true },
    { "id": "smart_failing", "kind": "smart", "severity": "critical", "enabled": true },
    { "id": "pool_error",    "kind": "pool",  "severity": "critical", "enabled": true }
  ]
}
```

- `PUT /api/v1/rules` `{ "expectedVersion": 3, "heartbeatSeconds"?, "rules": […] }` → `200 Rules`；
  `expectedVersion` 不符 → `409 version_conflict`（携带当前 version）。整体替换：缺省的 rule 视为删除。
- 规则语义（阈值窗口、回差恢复、容器迁移、SMART、存储池、心跳）见 `events.md` §规则引擎。
- `label` / `match`：`null` 或 `"*"` 匹配全部；`"mount=/data"` 之类选择器限定单标签集。MVP 支持 `*` 与精确值两种。
- severity 只允许 `warning` / `critical`（规则告警不产生 info）。

### 设置（免打扰等）

```jsonc
// GET /api/v1/settings →
{ "version": 2,
  "dnd": { "enabled": true, "start": "23:00", "end": "08:00", "timezone": "Asia/Shanghai" },
  "reportIntervalSeconds": 30 }
```

- `PATCH /api/v1/settings` `{ "expectedVersion": 2, "dnd": {…}, "reportIntervalSeconds"?: 30 }` → `200 Settings`；版本不符 `409 version_conflict`。
- `reportIntervalSeconds`（10..600）经 ingest 响应下发给来源。
- DND 语义：窗口内中枢照常记录；**设备端**抑制声音 / 振动 / 横幅并累积，窗口结束发汇总通知；严重故障不绕过（MVP 无例外通道）。手机本地以此设置为准（sync 持久化，离线仍按本地副本执行）。

### 杂项

- `GET /api/v1/health` → `200 { "ok": true, "version": "0.1.0", "serverTime": "…" }`（无需认证，部署探活）。
- `GET /api/v1/version` → `200 { "api": 1, "hub": "0.1.0" }`（无需认证）。

---

## 5. 保留与容量

- 原始指标：保留 **7 天**；5 分钟汇总：保留 **90 天**。
- 独立消息与已恢复故障：保留 **90 天**（tombstone 经 sync 下发）；**未恢复故障永不清理**。
- 变更窗口（sync 可回溯范围）：≥ 7 天或 ≥ 100k changeSeq；超出后客户端需全量重同步。
- 事件 `occurredAt` 允许补传窗口：90 天；更早的事件进 `rejected`（`code=invalid_request`）。
- 附件不经中枢落盘：按需回源拉取，中枢只透传（可内存中转，不写磁盘）。
