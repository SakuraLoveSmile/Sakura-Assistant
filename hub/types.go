package main

import (
	"database/sql"
	"encoding/json"
)

// ---------- 接入载荷（来源 → 中枢） ----------

// ingestMetricsReq 为 POST /api/v1/ingest/metrics 请求体。
type ingestMetricsReq struct {
	Seq          int64             `json:"seq"`
	SentAt       string            `json:"sentAt"`
	Agent        *agentInfo        `json:"agent"`
	Capabilities map[string]string `json:"capabilities"`
	Sample       *samplePayload    `json:"sample"`
}

type agentInfo struct {
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
}

type samplePayload struct {
	TS            string             `json:"ts"`
	CPUPercent    *float64           `json:"cpuPercent"`
	MemPercent    *float64           `json:"memPercent"`
	MemUsedBytes  *int64             `json:"memUsedBytes"`
	MemTotalBytes *int64             `json:"memTotalBytes"`
	Load1         *float64           `json:"load1"`
	UptimeSeconds *int64             `json:"uptimeSeconds"`
	BootTime      *string            `json:"bootTime"`
	Disks         []diskPayload      `json:"disks"`
	Net           *netPayload        `json:"net"`
	Containers    []containerPayload `json:"containers"`
	NAS           *nasPayload        `json:"nas"`
}

type diskPayload struct {
	Mount      string   `json:"mount"`
	Percent    *float64 `json:"percent"`
	UsedBytes  *int64   `json:"usedBytes"`
	TotalBytes *int64   `json:"totalBytes"`
	FSType     string   `json:"fstype"`
}

type netPayload struct {
	RxBps *float64 `json:"rxBps"`
	TxBps *float64 `json:"txBps"`
}

type containerPayload struct {
	Name         string  `json:"name"`
	State        string  `json:"state"`
	ExitCode     *int    `json:"exitCode"`
	StartedAt    *string `json:"startedAt"`
	RestartCount *int    `json:"restartCount"`
}

type nasPayload struct {
	Disks []nasDiskPayload `json:"disks"`
	Pools []nasPoolPayload `json:"pools"`
}

type nasDiskPayload struct {
	Dev   string   `json:"dev"`
	Smart string   `json:"smart"`
	TempC *float64 `json:"tempC"`
}

type nasPoolPayload struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// ingestEventsReq 为 POST /api/v1/ingest/events 请求体。
type ingestEventsReq struct {
	SentAt string         `json:"sentAt"`
	Events []eventPayload `json:"events"`
}

// eventPayload 为来源上报的单条事件；hub 自产事件复用同结构。
type eventPayload struct {
	EventID        string           `json:"eventId"`
	Seq            int64            `json:"seq"`
	Kind           string           `json:"kind"`
	OccurredAt     string           `json:"occurredAt"`
	Severity       string           `json:"severity"`
	FaultKey       *string          `json:"faultKey"`
	IncidentAction *string          `json:"incidentAction"`
	Title          string           `json:"title"`
	Body           *string          `json:"body"`
	Ref            json.RawMessage  `json:"ref"`
	Attachments    []attachmentDesc `json:"attachments"`
}

// attachmentDesc 为附件描述符（字节不经 JSON 传输）。
type attachmentDesc struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
	ByteSize int64  `json:"byteSize"`
	SHA256   string `json:"sha256,omitempty"`
}

// ---------- 规则与设置 ----------

// rule 为单条告警规则。
type rule struct {
	ID                string   `json:"id"`
	Kind              string   `json:"kind"` // threshold | container_exit | smart | pool
	Metric            string   `json:"metric,omitempty"`
	Label             *string  `json:"label"`
	Op                string   `json:"op,omitempty"`
	Value             *float64 `json:"value,omitempty"`
	ForSeconds        int64    `json:"forSeconds,omitempty"`
	RecoverValue      *float64 `json:"recoverValue,omitempty"`
	RecoverForSeconds int64    `json:"recoverForSeconds,omitempty"`
	Match             *string  `json:"match,omitempty"`
	Severity          string   `json:"severity"`
	Enabled           bool     `json:"enabled"`
}

// rulesDoc 为 GET /api/v1/rules 响应 / changes 载荷。
type rulesDoc struct {
	Version          int64  `json:"version"`
	HeartbeatSeconds int64  `json:"heartbeatSeconds"`
	Rules            []rule `json:"rules"`
}

// dndConfig 为免打扰设置。
type dndConfig struct {
	Enabled  bool   `json:"enabled"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Timezone string `json:"timezone"`
}

// settingsDoc 为 GET /api/v1/settings 响应 / changes 载荷。
type settingsDoc struct {
	Version               int64     `json:"version"`
	DND                   dndConfig `json:"dnd"`
	ReportIntervalSeconds int64     `json:"reportIntervalSeconds"`
}

// ---------- DB 行结构 ----------

type sourceRow struct {
	ID                string
	Name              string
	Kind              string
	Enabled           bool
	KeyHash           string
	KeyPlain          string
	KeyHint           string
	MgmtKeyPlain      string // v1.1：管理面回连凭证明文（仅 feedback 类；不随 API 回传）
	MgmtKeyHint       string // mgmtKey 末 4 位回显
	AttachmentBaseURL sql.NullString
	AgentVersion      sql.NullString
	AgentOS           sql.NullString
	AgentArch         sql.NullString
	Hostname          sql.NullString
	Capabilities      sql.NullString // JSON
	LastSeenAt        sql.NullString
	LastMetricsSeq    int64
	LastEventSeq      int64
	LastBootTime      sql.NullString
	LastSummary       sql.NullString // JSON
	PrevSample        sql.NullString // JSON：上次应用的完整样本（容器/SMART/池迁移判定用）
	DeletedAt         sql.NullString
	CreatedAt         string
	UpdatedAt         string
}

type messageRow struct {
	ID          string
	SourceID    string
	Kind        string
	Severity    string
	Title       string
	Body        sql.NullString
	OccurredAt  string
	ReceivedAt  string
	ReadAt      sql.NullString
	FaultID     sql.NullString
	Incident    sql.NullInt64
	Ref         sql.NullString // JSON
	Attachments sql.NullString // JSON
	EventID     string
	EventSeq    int64
	FaultKey    sql.NullString
	OutOfOrder  bool
	ChangeSeq   int64
}

type faultRow struct {
	ID            string
	SourceID      string
	FaultKey      string
	Severity      string
	Title         string
	Summary       sql.NullString
	State         string
	Incident      int64
	OpenedAt      string
	OpenedBySeq   int64
	LastEventAt   string
	ResolvedAt    sql.NullString
	ResolvedBySeq sql.NullInt64
	ResolvedBy    sql.NullString
	EventCount    int64
	ReadAt        sql.NullString
	MutedAt       sql.NullString
	MutedUntil    sql.NullString
	ChangeSeq     int64
}

// ---------- 对外 JSON 对象 ----------

// notifyBlock 为 SSE message 事件附带的通知提示块。
type notifyBlock struct {
	Kind    string  `json:"kind"` // new_message | incident_open | incident_update | incident_resolved
	FaultID *string `json:"faultId"`
	Muted   bool    `json:"muted"`
}

// messageJSON 为 Message 对象（api-v1 §3）。
type messageJSON struct {
	ID          string           `json:"id"`
	ChangeSeq   int64            `json:"changeSeq"`
	SourceID    string           `json:"sourceId"`
	SourceName  string           `json:"sourceName"`
	Kind        string           `json:"kind"`
	Severity    string           `json:"severity"`
	Title       string           `json:"title"`
	Body        *string          `json:"body"`
	OccurredAt  string           `json:"occurredAt"`
	ReceivedAt  string           `json:"receivedAt"`
	ReadAt      *string          `json:"readAt"`
	FaultID     *string          `json:"faultId"`
	Incident    *int64           `json:"incident"`
	Ref         json.RawMessage  `json:"ref"`
	Attachments []attachmentDesc `json:"attachments"`
	Notify      *notifyBlock     `json:"notify,omitempty"` // 仅 SSE
}

// faultJSON 为 Fault 对象（api-v1 §3）。
type faultJSON struct {
	ID          string  `json:"id"`
	ChangeSeq   int64   `json:"changeSeq"`
	SourceID    string  `json:"sourceId"`
	SourceName  string  `json:"sourceName"`
	FaultKey    string  `json:"faultKey"`
	Severity    string  `json:"severity"`
	Title       string  `json:"title"`
	Summary     *string `json:"summary"`
	State       string  `json:"state"`
	Incident    int64   `json:"incident"`
	OpenedAt    string  `json:"openedAt"`
	LastEventAt string  `json:"lastEventAt"`
	ResolvedAt  *string `json:"resolvedAt"`
	ResolvedBy  *string `json:"resolvedBy"`
	EventCount  int64   `json:"eventCount"`
	ReadAt      *string `json:"readAt"`
	MutedAt     *string `json:"mutedAt"`
	MutedUntil  *string `json:"mutedUntil"`
}

// sourceJSON 为 Source 管理视图 + summary 扩展字段（兼容变更）。
type sourceJSON struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Kind              string            `json:"kind"`
	Enabled           bool              `json:"enabled"`
	Status            string            `json:"status"`
	LastSeenAt        *string           `json:"lastSeenAt"`
	KeyHint           string            `json:"keyHint"`
	MgmtKeyHint       *string           `json:"mgmtKeyHint"` // v1.1：未配置为 null
	AttachmentBaseURL *string           `json:"attachmentBaseUrl"`
	AgentVersion      *string           `json:"agentVersion"`
	Hostname          *string           `json:"hostname"`
	Capabilities      map[string]string `json:"capabilities"`
	InstallHint       any               `json:"installHint"`
	Summary           json.RawMessage   `json:"summary"`
	CreatedAt         string            `json:"createdAt"`
	UpdatedAt         string            `json:"updatedAt"`
}

// changeEntry 为 changes 表行 / sync & SSE 分发单元。
type changeEntry struct {
	Seq    int64
	Type   string // message|fault|source|rules|settings|tombstone
	Data   json.RawMessage
	Notify *notifyBlock // 仅 message 类变更可能携带（SSE 注入用）
}
