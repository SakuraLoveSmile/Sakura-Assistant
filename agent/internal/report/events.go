package report

import (
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"

	"assistant-agent/internal/collect"
)

// Event 为 ingest/events 单条事件载荷。
// faultKey / incidentAction 恒输出（null 或值），与契约 fixture 一致。
type Event struct {
	EventID        string          `json:"eventId"`
	Seq            int64           `json:"seq"`
	Kind           string          `json:"kind"`
	OccurredAt     collect.ISOTime `json:"occurredAt"`
	Severity       string          `json:"severity"` // info | warning | critical
	FaultKey       *string         `json:"faultKey"`
	IncidentAction *string         `json:"incidentAction"` // open | update | resolve | null
	Title          string          `json:"title"`
	Body           string          `json:"body,omitempty"`
	Ref            map[string]any  `json:"ref,omitempty"`
	Attachments    []Attachment    `json:"attachments,omitempty"`
}

// Attachment 为附件描述符（采集侧当前不产附件，结构备齐）。
type Attachment struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
	ByteSize int64  `json:"byteSize"`
	SHA256   string `json:"sha256,omitempty"`
}

// newEvent 构造基础事件：eventId 形如 ag_<ulid>，occurredAt 为来源时钟。
func newEvent(kind, severity, title, body string, seq int64, now time.Time) Event {
	return Event{
		EventID:    "ag_" + ulid.Make().String(),
		Seq:        seq,
		Kind:       kind,
		OccurredAt: collect.NewISOTime(now),
		Severity:   severity,
		Title:      truncateRunes(title, 200),
		Body:       truncateRunes(body, 20000),
	}
}

// AgentStartedEvent 进程启动事件（kind=agent_started，独立消息）。
func AgentStartedEvent(seq int64, version, hostname string, now time.Time) Event {
	return newEvent("agent_started", "info",
		"采集代理已启动 v"+version+" @"+hostname, "", seq, now)
}

// QueueOverflowEvent 队列溢出汇总事件（kind=queue_overflow，独立 warning 消息）。
// body 含丢弃条数与时间窗，契约 events.md §5。
func QueueOverflowEvent(seq int64, queueKind string, dropped int64, firstAt, lastAt time.Time, now time.Time) Event {
	e := newEvent("queue_overflow", "warning",
		"本地持久队列溢出：丢弃最旧数据", "", seq, now)
	e.Body = "queue=" + queueKind +
		" dropped=" + strconv.FormatInt(dropped, 10) +
		" window=" + firstAt.UTC().Format(time.RFC3339) +
		".." + lastAt.UTC().Format(time.RFC3339)
	return e
}

// PayloadDroppedEvent 载荷被中枢判为无效而丢弃的通知事件（kind=custom）。
func PayloadDroppedEvent(seq int64, what, reason string, now time.Time) Event {
	return newEvent("custom", "warning",
		"上报载荷被中枢拒绝并已丢弃", what+": "+reason, seq, now)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
