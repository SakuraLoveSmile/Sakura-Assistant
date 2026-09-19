// Package report 负责向中枢 POST ingest 载荷：指标批次与事件，
// 含失败分类（可重试 / 401 停发探测 / 坏载荷丢弃）与 gzip 可选压缩。
// 契约依据：contracts/api-v1.md §2、events.md §5。
package report

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrKind 为发送失败分类。
type ErrKind int

const (
	// ErrRetryable 中枢不可达 / 5xx / 403 source_disabled / 429 → 入队退避补传。
	ErrRetryable ErrKind = iota
	// ErrUnauthorized 401 密钥失效 → 停发 + 每 5min 探测。
	ErrUnauthorized
	// ErrPoison 400/413 载荷本身被拒 → 该条丢弃（重试无意义），记入日志与事件。
	ErrPoison
)

// SendError 携带失败分类与上下文。
type SendError struct {
	Kind       ErrKind
	Status     int
	RetryAfter time.Duration
	Detail     string
}

func (e *SendError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("ingest status %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("ingest status %d", e.Status)
}

// MetricsAck 为 ingest/metrics 202 响应。
type MetricsAck struct {
	Accepted              bool   `json:"accepted"`
	Duplicate             bool   `json:"duplicate"`
	ServerTime            string `json:"serverTime"`
	ReportIntervalSeconds int    `json:"reportIntervalSeconds"`
}

// RejectedEvent 为 ingest/events 响应中的被拒条目。
type RejectedEvent struct {
	EventID string `json:"eventId"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// EventsAck 为 ingest/events 200 响应。
type EventsAck struct {
	Accepted   []string        `json:"accepted"`
	Duplicates []string        `json:"duplicates"`
	Rejected   []RejectedEvent `json:"rejected"`
	LastSeq    int64           `json:"lastSeq"`
	ServerTime string          `json:"serverTime"`
}

// HubClient 为 ingest 端点客户端。
type HubClient struct {
	base    string
	key     string
	gzip    bool
	hc      *http.Client
	ua      string
	maxBody int
}

// NewHubClient base 形如 http://host:8795（已去尾斜杠）；key 为 ask_… 来源密钥。
func NewHubClient(base, key string, gzipBody bool, timeout time.Duration) *HubClient {
	return &HubClient{
		base:    strings.TrimRight(base, "/"),
		key:     key,
		gzip:    gzipBody,
		hc:      &http.Client{Timeout: timeout},
		maxBody: 2 << 20,
	}
}

// SetUserAgent 设置 User-Agent（版本标识）。
func (c *HubClient) SetUserAgent(ua string) { c.ua = ua }

// PostMetrics POST /api/v1/ingest/metrics，body 为完整载荷 JSON。
func (c *HubClient) PostMetrics(ctx context.Context, body []byte) (*MetricsAck, error) {
	resp, err := c.post(ctx, "/api/v1/ingest/metrics", body)
	if err != nil {
		return nil, err
	}
	var ack MetricsAck
	if err := json.Unmarshal(resp, &ack); err != nil {
		return nil, &SendError{Kind: ErrRetryable, Detail: "bad metrics ack: " + err.Error()}
	}
	return &ack, nil
}

// PostEvents POST /api/v1/ingest/events，body 为完整载荷 JSON。
func (c *HubClient) PostEvents(ctx context.Context, body []byte) (*EventsAck, error) {
	resp, err := c.post(ctx, "/api/v1/ingest/events", body)
	if err != nil {
		return nil, err
	}
	var ack EventsAck
	if err := json.Unmarshal(resp, &ack); err != nil {
		return nil, &SendError{Kind: ErrRetryable, Detail: "bad events ack: " + err.Error()}
	}
	return &ack, nil
}

// post 发送并做状态分类；成功返回响应体。
func (c *HubClient) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	wire := body
	useGzip := c.gzip
	if useGzip {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(body); err == nil {
			if err := gw.Close(); err == nil {
				wire = buf.Bytes()
			} else {
				useGzip = false
			}
		} else {
			useGzip = false
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)
	if c.ua != "" {
		req.Header.Set("User-Agent", c.ua)
	}
	if useGzip {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &SendError{Kind: ErrRetryable, Detail: err.Error()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxBody)))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return respBody, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, &SendError{Kind: ErrUnauthorized, Status: resp.StatusCode, Detail: errDetail(respBody)}
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge:
		return nil, &SendError{Kind: ErrPoison, Status: resp.StatusCode, Detail: errDetail(respBody)}
	default:
		se := &SendError{Kind: ErrRetryable, Status: resp.StatusCode, Detail: errDetail(respBody)}
		if resp.StatusCode == http.StatusTooManyRequests {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if n, err := strconv.Atoi(ra); err == nil {
					se.RetryAfter = time.Duration(n) * time.Second
				}
			}
		}
		return nil, se
	}
}

// errDetail 提取错误信封 message（截断防刷屏）。
func errDetail(body []byte) string {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		msg := env.Error.Code
		if env.Error.Message != "" {
			msg += ": " + env.Error.Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return msg
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// Classify 返回错误分类；非 SendError（如本地构造失败）视为可重试。
func Classify(err error) ErrKind {
	var se *SendError
	if errors.As(err, &se) {
		return se.Kind
	}
	return ErrRetryable
}
