# Feedback 接入契约 v1.1（冻结）

目标：Feedback 服务（TypeScript，`apps/server`）作为 `kind=feedback` 来源接入中枢 ——
反馈落库、错误、恢复变化时可靠投递事件；截图与日志经**专用只读接口**由中枢按需回取；
v1.1 起新增**管理面**：列表回读与单条生命周期/安全重试操作，由中枢持**独立管理凭证**回连执行。
不改动 Feedback 业务语义、不暴露管理员凭据给 APK、不阻塞反馈主流程。

版本说明：v1.0 即已发布的 Feedback ≤ v0.5.2 接入面（事件 outbox + 只读详情/附件）；
v1.1 管理面自 Feedback v0.6.0 起提供。中枢与 APK 必须对 v1.0 服务端**优雅降级**
（保留事件/附件查看能力，管理功能提示需升级）。

## 1. 新增环境变量（Feedback 服务端，全部可选；未配置 `FEEDBACK_ASSIST_HUB_URL` 即整体关闭接入）

| 变量 | 说明 |
|---|---|
| `FEEDBACK_ASSIST_HUB_URL` | 中枢地址，如 `https://hub.example.com`。缺省 = 不接入（零行为变化） |
| `FEEDBACK_ASSIST_SOURCE_KEY` | 中枢签发的来源密钥 `ask_…`（事件上报凭证） |
| `FEEDBACK_ASSIST_READ_KEY` | 附件只读接口凭证。**与 source key 同一值**（中枢回连时复用同一密钥，见 api-v1 §认证 2）；允许单独设置覆盖 |
| `FEEDBACK_ASSIST_MGMT_KEY` | **v1.1 新增**：管理接口凭证（独立值，如 `amk_…`）。未配置 → 管理路由组不挂载（v1.0 行为）。**必须与生效的只读凭证不同**：取到的值与 READ_KEY/SOURCE_KEY 生效值相同时，服务端拒绝挂载管理组并记警告日志（防止只读凭证意外获得写权限） |
| `FEEDBACK_ASSIST_QUEUE_MAX` | outbox 上限，默认 1000 |
| `FEEDBACK_ASSIST_FLUSH_MS` | 投递循环空闲休眠毫秒，默认 2000 |

凭证分布：SOURCE_KEY/READ_KEY/MGMT_KEY 只出现在 Feedback（env）与中枢（来源记录）两侧；
APK 永远只持有自己到中枢的客户端令牌。三类凭证职责互斥：SOURCE_KEY 只能上报事件，
READ_KEY 只能读详情/附件，MGMT_KEY 只能读列表 + 执行 §6 操作（不能读附件字节、不能上报事件）。

## 2. outbox（同事务可靠投递）

新表（由 Feedback 侧迁移新增，**不得重建既有表**）：

```sql
CREATE TABLE IF NOT EXISTS assist_outbox (
  id           TEXT PRIMARY KEY,          -- eventId：fb_<ulid>
  seq          INTEGER NOT NULL,          -- 来源事件序号：单排行号计数器分配，单调递增
  kind         TEXT NOT NULL,             -- feedback_created | feedback_fault | feedback_recovered
  payload_json TEXT NOT NULL,             -- 完整 event 对象（api-v1 §2 单元素结构）
  state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','sent','dead')),
  attempts     INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT,                   -- 退避后的下次尝试时刻
  created_at   TEXT NOT NULL,
  sent_at      TEXT
);
CREATE INDEX IF NOT EXISTS idx_assist_outbox_pending ON assist_outbox(state, next_attempt_at);
CREATE TABLE IF NOT EXISTS assist_outbox_seq (id INTEGER PRIMARY KEY CHECK (id = 1), value INTEGER NOT NULL);
```

- **写入点（同事务）**：在反馈**首次成功落库**、状态迁入 `failed`/`needs_review`、
  以及从错误态恢复（`retry` 重新入队成功、`resolve`/`recover` 回到正常路径、`archived` 成功）
  的**同一事务**内插 outbox 行。业务写入失败回滚则 outbox 行一并回滚（绝不出现"事件已发但业务没落库"）。
- `seq` 分配：`assist_outbox_seq` 单行 `value+1`，同事务内取新值（写事务串行，无并发问题）。
- **事件构造**：
  - `feedback_created`：`severity=info`，`title=新反馈：<首行或 title 截断>`，`body=text`（≤20000 截断），
    `ref.feedbackId=id`，`attachments` 由截图 / 日志元数据生成描述符
    （截图 `id="screenshot"`；日志 `id="logs/<logId>"`，`kind="log"`，带 `filename`/`byteSize`/`sha256`）。
  - `feedback_fault`：`incidentAction=open`，`faultKey="feedback:<feedbackId>"`，`severity=warning`，
    `body` 含 `error_summary` / `last_error`。同一反馈在开放期间再次恶化（如重试又失败）→ `incidentAction=update`。
  - `feedback_recovered`：`incidentAction=resolve`，同 faultKey，`severity=info`。
- **投递循环**：独立 async worker（不占用请求线程、不被请求等待）。
  取 `state=pending AND next_attempt_at<=now` 按 seq 升序每批 ≤50 条 POST `/api/v1/ingest/events`；
  全部 accepted/duplicates → 标 `sent`（duplicates 也算送达）；含 `rejected` → 该条按同一退避重试，
  `attempts ≥ 10` 仍未被接受 → 标 `dead`（不再投递；行保留可查并记日志，绝不静默丢弃）。
  失败退避：1s→2s→5s→30s→5min 封顶（`attempts` 计数，`next_attempt_at` 持久化，重启后继续）。
- **中枢不可达**：网络错 / 5xx / `403` → 留在 pending 按退避重试；`401`（密钥失效）→ 停发并每 5min 探测。
- **溢出**：`pending` 超过 `FEEDBACK_ASSIST_QUEUE_MAX` → 新事件仍入队但**丢弃最旧 pending**
  （每删一批记一条 `queue_overflow` 事件入队，含丢弃数与时间窗）。不阻塞、不占满磁盘、不静默。
- **绝不阻塞业务**：outbox 写入只追加一行；投递失败只影响自身重试。反馈提交、管理操作、worker 均不等待投递。
- 优雅退出：停止 worker → 完成在途批次 → 关库；pending 行下次启动继续。
- **清理**：worker 启动时及每 24h 删除终态行（`sent` 按 `sent_at`、`dead` 按 `created_at`）
  超过 7 天的记录 —— outbox 不无限增长。

## 3. 只读附件接口（中枢 → Feedback）

新路由组（挂载于现有 server，无 Cookie、无 CORS 需求 —— 仅中枢内网回连调用）：

```
GET /api/assist/feedback/:id
GET /api/assist/feedback/:id/attachments/screenshot
GET /api/assist/feedback/:id/attachments/logs/:logId
```

认证：`Authorization: Bearer <FEEDBACK_ASSIST_READ_KEY>`（缺省回退 `FEEDBACK_ASSIST_SOURCE_KEY`；
两者都未配置 → 路由组不挂载）。统一 401；恒定时间比较。

- `GET /api/assist/feedback/:id` → `200`：

```jsonc
{ "id": "…", "appId": "…", "appName": "…|null", "status": "…", "title": "…", "text": "…",
  "createdAt": "…", "updatedAt": "…", "errorSummary": "…|null",
  "hasScreenshot": true,
  "logs": [ { "id": "…", "filename": "app.log", "byteSize": 123, "sha256": "…", "source": "auto" } ],
  // ---- v1.1 扩展字段（旧版服务端缺席时中枢/客户端按「不支持管理」处理）----
  "mgmtState": "inbox",            // inbox | archived | trash（收件箱生命周期）
  "lifecycleVersion": 3,           // 生命周期乐观锁版本（mgmt_state 维度）
  "revision": 2,                   // 归档恢复数据 revision（retry/recheck 乐观锁维度）
  "issueStatus": "open",           // open | waiting_user | waiting_admin | resolved
  "collectionState": "queued",     // 收集/归档进度语义（waiting_configuration|waiting_source_confirmation|waiting_manual_archive|queued）
  "resumePaused": false,           // 处理已暂停（回收站/恢复副作用）
  "archiveStage": "complete",      // task_pending|task_created|asset_uploading|asset_finalized|comment_pending|complete|null
  "kaneoTaskUrl": "…|null",
  "archivedAt": "…|null", "trashedAt": "…|null",
  "allowedActions": ["archive","trash"],   // 当前允许的管理动作（§6 枚举子集；实时计算）
  "capabilities": { "manage": true }       // 本服务端管理面是否挂载（MGMT_KEY 已配置）
}
```

`allowedActions` 为**业务允许 ∩ §6 开放动作集**的交集：
生命周期动作按 `lifecycleAvailableActions` 口径（archive 仅 `status='archived'`、trash 不含回收站内记录等），
`retry` 仅 `status='failed'`（或 `needs_info` 且无 AI 整理结果——与 worker.retry 同口径），
`recheck` 仅 `status='needs_review'`。旧版服务端不返回这些字段。

- `GET …/attachments/screenshot` → `200 image/png` + `Content-Length` + `ETag: "<sha256>"`；
  无截图 `404`。
- `GET …/attachments/logs/:logId` → `200 application/octet-stream` + `Content-Disposition` 沿用现有
  download 路由同款标头；无此日志或不属于该反馈 `404`。
- 已彻底删除（purge）的反馈 → `404`（读接口不区分 gone，避免泄露）。
- **只读**：本组仅有 GET，不产生任何写；限流 60 次/分/密钥。
- 权限边界：密钥只配置在 Feedback（env）与中枢（来源记录）两侧；APK 拿到的永远是自己到中枢的令牌。

## 4. 管理接口组（v1.1，中枢 → Feedback）

新路由组挂载于现有 server（仅中枢内网回连调用）：

```
GET  /api/assist/manage/feedback
POST /api/assist/manage/feedback/:id/action
```

认证：`Authorization: Bearer <FEEDBACK_ASSIST_MGMT_KEY>`（恒定时间比较，统一 401；
**不接受** READ_KEY/SOURCE_KEY）。未配置 `FEEDBACK_ASSIST_MGMT_KEY` → 整组不挂载（404）。
限流 60 次/分/密钥（与只读组各自独立计数）。

### 4.1 列表 `GET /api/assist/manage/feedback`

查询参数：`view=inbox|archived|trash|all`（默认 `inbox`；`all` = inbox+archived，**不含回收站**）、
`cursor`（不透明续页串 `<createdAt>|<id>`）、`limit`（1..100，默认 50）、`q`（≤200 字符，
匹配 title/text/id）。响应：

```jsonc
{ "items": [ {
      "id": "…", "appId": "…", "appName": "…|null", "status": "…",
      "issueStatus": "open", "mgmtState": "inbox", "lifecycleVersion": 3,
      "title": "…|null", "textPreview": "…", "createdAt": "…", "updatedAt": "…",
      "errorSummary": "…|null", "hasScreenshot": true, "logCount": 2,
      "collectionState": "queued", "resumePaused": false,
      "allowedActions": ["trash"]          // 仅生命周期动作子集（轻量，不解析归档数据）
    } ],
  "nextCursor": "…|null",
  "counts": { "inbox": 4, "archived": 12, "trash": 1 }   // 同一 q/筛选下三区计数（view 不计入）
}
```

### 4.2 单条操作 `POST /api/assist/manage/feedback/:id/action`

请求：

```jsonc
{ "requestId": "rop_01J…",               // 必填，[A-Za-z0-9._:-]{1,128}，幂等键（客户端每次操作生成唯一值）
  "action": "archive",                   // archive|unarchive|trash|restore|resume_processing|retry|recheck
  "expectedLifecycleVersion": 3,         // 生命周期动作必填（≥0 整数）
  "expectedRevision": 2 }                // retry/recheck 必填（≥0 整数）
```

- 动作集固定七项，**与详情 `allowedActions` 实时口径一致**；不在允许集中的动作按 `invalid_state` 拒绝。
- 生命周期五动作经 `applyLifecycleInTx`（同一反馈级互斥锁 + 事务 + 审计，actor 固定
  `{id:"assist", username:"Assistant"}`）；`resume_processing` 成功后在锁外 `worker.enqueue`。
- `retry`/`recheck` 经 `worker.retry`/`worker.recheck`（自带锁与 revision 校验）；
  审计 `action` 记为 `retry`/`recheck`，detail 带 `requestId` 与 `via:"assist"`。
- **幂等**：新迁移表 `assist_mgmt_requests(request_id PK, feedback_id, action, http_status,
  outcome_json, created_at)`。同 `requestId`+同目标+同动作 → 返回已存结果（`200`，响应含
  `"replayed": true`）；同 `requestId` 不同参数 → `409 request_id_conflict`。
  执行中的请求先落 `http_status=0` 运行中标记；同 `requestId` 在运行中到达 → `409 request_in_flight`
  （客户端拉详情等待，不得重发）。运行中标记超过 10 分钟视为崩溃残留，惰性终结为
  `{ok:false, code:"outcome_uncertain"}`（客户端显示「结果待确认」并刷新详情，**绝不自动重发**）。
  完成行保留 30 天，每次操作顺带惰性清理过期行。
- 操作引起的 `status` 迁移复用既有 outbox 同事务事件路径（`updateFeedback` 内
  `enqueueStatusTransitionInTx`），不新增事件种类；回收站移动/恢复只动 `mgmt_state`
  不产生事件（除非状态随之变化）。
- 响应 `200`：`{ "ok": true, "action": "…", "replayed": false, "detail": <§3 完整详情对象> }`
  —— detail 为操作后的实时快照，客户端直接替换本地详情。
- 错误：`invalid_request` 400（缺 requestId / action 非法 / 期望版本缺失或非法）；
  `not_found` 404（含已 purge）；`version_conflict` 409 + `lifecycleVersion`；
  `revision_conflict` 409 + `revision`；`invalid_state` 409（reason 可展示）；
  `busy` 409（worker 持锁中）；`request_id_conflict`/`request_in_flight`/`outcome_uncertain` 409。
  `version_conflict`/`revision_conflict`/`invalid_state` 响应同时携带 `detail` 完整对象供刷新。

### 4.3 不含项（明确不做）

彻底删除（purge）、内容编辑/分类保存、管理员回复/对话、force-create / retry_comment /
replace_upload / retry_log 等复杂 Kaneo 恢复。回收站恢复不自动重启处理；
归档仅本地归档区语义（不代表创建 Kaneo 任务）。

## 5. 中枢侧配套（中枢 agent 实现）

- `kind=feedback` 来源创建时可设 `attachmentBaseUrl`（如 `http://127.0.0.1:8787`，中枢视角可达地址）；
  该基址同时作为只读组与管理组的回连基址。来源另可设 `mgmtKey`（管理凭证明文，库内保存用于回连，
  接口仅回显 `mgmtKeyHint` 末 4 位），见 api-v1 §4。
- `GET /api/v1/messages/:id/attachments/:attId`：
  - `attId = "screenshot"` → `{base}/api/assist/feedback/{ref.feedbackId}/attachments/screenshot`；
  - `attId = "logs/<logId>"` → `{base}/api/assist/feedback/{ref.feedbackId}/attachments/logs/{logId}`；
  - 用该来源的 access key 作 Bearer 回连；`ref.feedbackId` 缺失 / base 未配置 → `404`；
    上游网络错 / 5xx → `502 attachment_unavailable`；上游 401 → `502`（配置错误不暴露给 APK 细节）。
- v1.1 新增按来源代理的列表 / 详情 / 操作 / 附件端点（`/api/v1/sources/:id/feedback…`），
  读写凭证分离、错误映射与旧服务端降级规则见 api-v1 §3.1。
- 透传 `Content-Type` / `Content-Length` / `ETag`；中枢不落盘，超时 10s，上限 10 MiB
  （操作类请求超时 30s）。

## 6. 边界（禁止项）

- 不改 `feedbacks` 表结构与既有状态机；不重排既有迁移；新增迁移号顺延当前最大值
  （`assist_mgmt_requests` 为 v12）。
- 不调用测试反馈 / 不验证 AI、Kaneo 功能（项目业务健康由项目自身判断）。
- 不读取、不记录密码 / 令牌 / API key；`assist_outbox.payload_json` 只含事件字段；
  `assist_mgmt_requests.outcome_json` 只含操作结果字段，不含正文/附件字节。
- 保留仓库当前未提交修改：只做**增量**编辑，不执行任何 git 写操作（commit/stash/checkout/revert）。
- 管理面绝不在 APK 落地凭证；APK 离线不排队执行管理操作；操作超时按「结果待确认」处理，
  重发必须复用同一 `requestId`（服务端幂等去重，绝不重复执行）。
