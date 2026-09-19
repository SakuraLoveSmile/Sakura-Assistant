package report

import (
	"encoding/json"
	"time"

	"assistant-agent/internal/collect"
)

// AgentInfo 为载荷 agent 块：版本 / 平台 / 主机名。
type AgentInfo struct {
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
}

// metricsPayload 为 POST /api/v1/ingest/metrics 的请求体。
type metricsPayload struct {
	Seq          int64                `json:"seq"`
	SentAt       collect.ISOTime      `json:"sentAt"`
	Agent        AgentInfo            `json:"agent"`
	Capabilities collect.Capabilities `json:"capabilities"`
	Sample       *collect.Sample      `json:"sample"`
}

// eventsPayload 为 POST /api/v1/ingest/events 的请求体。
type eventsPayload struct {
	SentAt collect.ISOTime   `json:"sentAt"`
	Events []json.RawMessage `json:"events"`
}

// BuildMetrics 组装一帧指标批次载荷。
func BuildMetrics(seq int64, info AgentInfo, res *collect.Result, sentAt time.Time) ([]byte, error) {
	return json.Marshal(metricsPayload{
		Seq:          seq,
		SentAt:       collect.NewISOTime(sentAt),
		Agent:        info,
		Capabilities: res.Capabilities,
		Sample:       &res.Sample,
	})
}

// BuildEvents 组装事件批次载荷；events 为已编码的单事件 JSON。
func BuildEvents(events []json.RawMessage, sentAt time.Time) ([]byte, error) {
	return json.Marshal(eventsPayload{
		SentAt: collect.NewISOTime(sentAt),
		Events: events,
	})
}

// MarshalEvent 编码单个事件（入队存储形态）。
func MarshalEvent(e Event) ([]byte, error) {
	return json.Marshal(e)
}
