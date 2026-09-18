# Feedback 接入契约 v1（冻结）

目标：Feedback 服务（TypeScript，`apps/server`）作为 `kind=feedback` 来源接入中枢 ——
反馈落库、错误、恢复变化时可靠投递事件；截图与日志经**专用只读接口**由中枢按需回取。
不改动 Feedback 业务语义、不暴露管理员凭据给 APK、不阻塞反馈主流程。

## 1. 新增环境变量（Feedback 服务端，全部可选；未配置 `FEEDBACK_ASSIST_HUB_URL` 即整体关闭接入）

| 变量 | 说明 |
|---|---|
| `FEEDBACK_ASSIST_HUB_URL` | 中枢地址，如 `https://hub.example.com`。缺省 = 不接入（零行为变化） |
| `FEEDBACK_ASSIST_SOURCE_KEY` | 中枢签发的来源密钥 `ask_…`（事件上报凭证） |
| `FEEDBACK_ASSIST_READ_KEY` | 附件只读接口凭证。**与 source key 同一值**（中枢回连时复用同一密钥，见 api-v1 §认证 2）；允许单独设置覆盖 |
| `FEEDBACK_ASSIST_QUEUE_MAX` | outbox 上限，默认 1000 |
| `FEEDBACK_ASSIST_FLUSH_MS` | 投递循环空闲休眠毫秒，默认 2000 |

## 2. outbox（同事务可靠投递）

新表（由 Feedback 侧迁移新增，**不得重建既有表**）：

```sql
CREATE TABLE IF NOT EXISTS assist_outbox (
  id           TEXT PRIMARY KEY,          -- eventId：fb_<ulid>
  seq          INTEGER NOT NULL,          -- 来源事件序号：单排行号计数器分配，单调递增
  kind         TEXT NOT NULL,             -- feedback_created | feedback_fault | feedback_recovered
  payload_json TEXT NOT NULL,             -- 完整 event 对象（api-v1 §2 单元素结构）
  state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','sent')),
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
  全部 accepted/duplicates → 标 `sent`（duplicates 也算送达）；含 rejected → 对账后单独标记重试。
  失败退避：1s→2s→5s→30s→5min 封顶（`attempts` 计数，`next_attempt_at` 持久化，重启后继续）。
- **中枢不可达**：网络错 / 5xx / `403` → 留在 pending 按退避重试；`401`（密钥失效）→ 停发并每 5min 探测。
- **溢出**：`pending` 超过 `FEEDBACK_ASSIST_QUEUE_MAX` → 新事件仍入队但**丢弃最旧 pending**
  （每删一批记一条 `queue_overflow` 事件入队，含丢弃数与时间窗）。不阻塞、不占满磁盘、不静默。
- **绝不阻塞业务**：outbox 写入只追加一行；投递失败只影响自身重试。反馈提交、管理操作、worker 均不等待投递。
- 优雅退出：停止 worker → 完成在途批次 → 关库；pending 行下次启动继续。

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
{ "id": "…", "appId": "…", "status": "…", "title": "…", "text": "…",
  "createdAt": "…", "updatedAt": "…", "errorSummary": "…|null",
  "hasScreenshot": true,
  "logs": [ { "id": "…", "filename": "app.log", "byteSize": 123, "sha256": "…", "source": "auto" } ] }
```

- `GET …/attachments/screenshot` → `200 image/png` + `Content-Length` + `ETag: "<sha256>"`；
  无截图 `404`。
- `GET …/attachments/logs/:logId` → `200 application/octet-stream` + `Content-Disposition` 沿用现有
  download 路由同款标头；无此日志或不属于该反馈 `404`。
- 已彻底删除（purge）的反馈 → `404`（读接口不区分 gone，避免泄露）。
- **只读**：本组仅有 GET，不产生任何写；限流 60 次/分/密钥。
- 权限边界：密钥只配置在 Feedback（env）与中枢（来源记录）两侧；APK 拿到的永远是自己到中枢的令牌。

## 4. 中枢侧配套（中枢 agent 实现）

- `kind=feedback` 来源创建时可设 `attachmentBaseUrl`（如 `http://127.0.0.1:8787`，中枢视角可达地址）。
- `GET /api/v1/messages/:id/attachments/:attId`：
  - `attId = "screenshot"` → `{base}/api/assist/feedback/{ref.feedbackId}/attachments/screenshot`；
  - `attId = "logs/<logId>"` → `{base}/api/assist/feedback/{ref.feedbackId}/attachments/logs/{logId}`；
  - 用该来源的 access key 作 Bearer 回连；`ref.feedbackId` 缺失 / base 未配置 → `404`；
    上游网络错 / 5xx → `502 attachment_unavailable`；上游 401 → `502`（配置错误不暴露给 APK 细节）。
- 透传 `Content-Type` / `Content-Length` / `ETag`；中枢不落盘，超时 10s，上限 10 MiB。

## 5. 边界（禁止项）

- 不改 `feedbacks` 表结构与既有状态机；不重排既有迁移；新增迁移号顺延当前最大值。
- 不调用测试反馈 / 不验证 AI、Kaneo 功能（项目业务健康由项目自身判断）。
- 不读取、不记录密码 / 令牌 / API key；`assist_outbox.payload_json` 只含事件字段。
- 保留仓库当前未提交修改：只做**增量**编辑，不执行任何 git 写操作（commit/stash/checkout/revert）。
