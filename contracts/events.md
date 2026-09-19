# 事件与故障状态机契约 v1（冻结）

本文定义事件种类、`faultKey` 约定、故障生命周期、去重 / 乱序规则与规则引擎语义。
消息与故障的字段结构见 `api-v1.md`；本文只定义**语义**。

## 1. 事件种类（kind）

来源经 `POST /api/v1/ingest/events` 上报的事件 kind（可扩展；中枢对未知 kind 按 `custom` 落库为独立消息，severity 照录）：

| kind | 来源 | incidentAction | faultKey | severity | 说明 |
|---|---|---|---|---|---|
| `feedback_created` | feedback | null（独立消息） | null | info | 反馈落库；正文 / `ref.feedbackId` / 附件描述符齐全 |
| `feedback_fault` | feedback | open | `feedback:<feedbackId>` | warning | 反馈进入 `failed` / `needs_review` 等错误态 |
| `feedback_recovered` | feedback | resolve | `feedback:<feedbackId>` | info | 错误态恢复（重新入队 / 归档成功 / 人工解决） |
| `agent_started` | device | null | null | info | 采集进程启动（含版本） |
| `queue_overflow` | 任意 | null（独立消息） | null | warning | 来源持久队列溢出丢弃最旧条目的汇总事件（`body` 含丢弃条数与时间窗，见 §5） |
| `custom` | 任意 | 按声明 | 按声明 | 按声明 | 扩展种类统一归入 |

中枢**自产**事件（不由来源上报，由规则引擎 / 监测器生成，`sourceId` 指向被监测来源）：

| kind | incidentAction | faultKey | 默认 severity | 说明 |
|---|---|---|---|---|
| `heartbeat_lost` | open | `heartbeat` | critical | 连续 `heartbeatSeconds` 无任何接入流量（仅 enabled 的 device 来源，见 §4） |
| `heartbeat_back` | resolve | `heartbeat` | info | 接入恢复 |
| `threshold` | open | `rule:<ruleId>:<labelSel>` | 规则定义 | 指标持续超限 |
| `threshold_recovered` | resolve | 同上 | info | 回落并跨过回差 |
| `container_exit` | open | `container:<name>` | 规则定义（默认 warning） | 容器 running→非 running |
| `container_back` | resolve | `container:<name>` | info | 容器恢复 running |
| `smart_failing` | open | `smart:<dev>` | critical | SMART 判为 failing |
| `smart_back` | resolve | `smart:<dev>` | info | SMART 恢复 ok |
| `pool_error` | open | `pool:<name>` | critical | 存储池 state ≠ ok |
| `pool_back` | resolve | `pool:<name>` | info | 存储池恢复 ok |
| `host_reboot` | null（独立消息） | null | info | `bootTime` 变化 |

每条中枢自产事件也生成对应 Message（kind 同上），`eventId` 形如 `hub_<ulid>`。

## 2. 故障（Fault）生命周期

故障是同一 `(sourceId, faultKey)` 上事件的归并容器；**轮次（incident）** 从 1 起，每次恢复后再发生 +1。

```text
            open(faultKey)                 同 faultKey 再 open / update
  (无故障) ──────────────► open ●──────────────────────► open（合并）
                                │                          │
                                │ resolve(seq 大于本轮 open)│
                                ▼                          ▼
                              resolved ◄───────────────────┘
                                │
                                │ 同 faultKey 再次 open
                                ▼
                       open（incident+1，新一轮）
```

规则：

- **open**：当前无该 faultKey 的开放轮次 → 开启新轮次（`incident = 上轮+1` 或 1），记 `openedAt`、`openedBySeq`；已有开放轮次 → 合并：`eventCount+1`、`lastEventAt` 更新、severity 取 max、title/summary 刷新为最新事件。
- **update**：并入当前开放轮次（不产生新消息类型变化，仍落一条 Message）；无开放轮次时等价 open。
- **resolve**：关闭**其 seq 所覆盖的**轮次 —— 即 `openedBySeq < resolveSeq` 的最近一个开放轮次。记 `resolvedAt` / `resolvedBySeq`，severity 不变，state → resolved。
- **重新发生**：resolved 后同 faultKey 再 open → 同一 Fault 行 `incident+1` 重新 open（历史轮次只在详情内可见）。
- **静音（mute）**：挂在 Fault 上、跨轮次持续。静音故障照常合并 / 恢复 / 记录，只是不产生通知（含 DND 汇总里标记为已静音）。
- **已读**：消息 `readAt` 针对单条消息；Fault `readAt` 针对**当前轮次**（新轮次自动回到未读）。

## 3. 去重与乱序保护

- `(sourceId, eventId)` 唯一约束是**唯一**去重依据；重放入 `duplicates`，事件、消息、故障迁移都只应用一次。
- 排序权威：`(sourceId, seq)` 单调递增（事件流内独立计数，与 metrics 的批次 seq 互不相关）。
  `occurredAt` 仅作展示与告警计时起点，不参与顺序判定。
- **先到的 resolve**：resolve 到达时若不存在 `openedBySeq < resolveSeq` 的开放轮次，记录为**待定恢复（pending resolve）**；之后补到的 open 若 seq < 该 resolve 的 seq，落库即直接 resolved（不开通知）。
- **迟到的 open**（seq 小于已记录 resolve）：按上条直接落 resolved；不产生「幽灵开放」。
- **迟到的事件**（seq 小于已应用最大 seq 但 eventId 未见）：仍入库为消息（标记 `outOfOrder` 供调试），按 seq 语义参与故障判定，绝不因迟到而丢弃。
- 中枢自产事件不存在乱序问题（单点产生，seq 由中枢按来源单调分配）。

## 4. 规则引擎（中枢侧）

输入：每次 `ingest/metrics` 的最新样本 + 心跳监测。输出：上表中 `hub_` 事件。
规则判定使用**样本时间戳 `sample.ts`** 与到达时间二者较保守者（防补传旧样本立刻触发）。

- **threshold**：`value > value` 开始计时；在 `forSeconds` 窗口内**所有**样本均超限 → open（faultKey `rule:<id>:<labelSel>`，`labelSel` 如 `mount=/`，无标签为 `-`）。
  恢复：`value < recoverValue` 连续 `recoverForSeconds` → resolve。
  窗口内样本缺席：计时器**保持不重置**（等待下一样本），直至来源 offline（届时由 heartbeat 接管，threshold 计时暂停，来源恢复后以最近样本重新计时）。
- **container_exit**：两次样本间容器 `running` → 其他态 → open；回到 `running` → resolve。
  `restartCount` 增长但 state 恒 running → 仅当该容器已有开放故障时记一条 update，否则忽略（不刷屏）。
  容器从列表消失（被删除）：已有开放故障 → resolve（summary 注记"容器已移除"），无开放故障 → 忽略。
- **smart**：`smart=failing` → open（critical）；`ok` → resolve；`asleep` / `unsupported` / `failed` → **不迁移状态**（不可观测不判好也不判坏，能力缺失经 capabilities 单独呈现）。
- **pool**：`state ∈ {degraded, error}` → open；`ok` → resolve；`unknown` / 缺席 → 不迁移。
- **heartbeat**：仅对 `kind=device` 且 `enabled=true` 的来源判定 —— 距上次**任何**接入调用
  （metrics 或 events）超过 `heartbeatSeconds` → open `heartbeat`（critical）；任何新接入 → resolve。
  同一失联期只开一次；恢复后再次失联开新轮次。
  `kind=feedback` 来源**不做心跳判定**：它只在有事件时投递，无周期上报预期，失联不可观测
  （其可用性经事件投递本身与附件回连体现）。`enabled=false` 的来源同样不判失联。
- **host_reboot**：`sample.bootTime` 相对上次变化 → 独立 info 消息（非故障）。

通知触发点（供 bridge.md 引用）：`incident_open`、`incident_resolved`、独立 `message` 三类需要通知；`incident_update` 只更新已有通知内容（`onlyAlertOnce`）。

v1.1 注记：管理操作（feedback-integration §4.2）引起的 `status` 迁移复用同一 outbox
事件路径（`updateFeedback` 同事务内构造 `feedback_fault`/`feedback_recovered`），
事件种类与 faultKey 语义不变；`mgmt_state` 变化本身不产生事件（除非 `status` 同时迁移）。

## 5. 来源持久队列（对采集 / Feedback 接入层的要求）

- 中枢不可达（连接失败 / 5xx / `403 source_disabled`）→ 事件与指标批次入**本地持久队列**（SQLite 或追加文件），按序补传；绝不阻塞来源自身业务。
- 容量上限：事件队列默认 **1000 条**；指标批次默认 **500 批**。溢出策略：丢弃**最旧**的并记一条 `queue_overflow` 汇总事件（`body` 含丢弃条数与时间窗），恢复联通后随队列一并上报 —— 不静默丢失、也不无限占盘。
- 补传仍走正常 ingest 端点（同一幂等 / 去重 / 乱序语义天然覆盖乱序补传）。
- `401`（密钥失效）→ 暂停上报并每 5 分钟重试一次；密钥轮换后以新 key 恢复。
- 指标补传可降采样：积压超过 100 批时允许只补最近 100 批 + `queue_overflow`，避免淹没中枢。

## 6. 五分钟告警预算

「来源可观测到异常」= `occurredAt` / `sample.ts`。
预算分配（设备联网、权限正常、非 DND 前提下）：上报间隔 ≤30s → 中枢判定 <5s → SSE 下发 <5s → 设备发帖通知 <2s。
采集侧不要把指标攒批超过一个上报周期；事件类（Feedback）要求落库后即时投递（队列非空即发，不做长延迟攒批）。
