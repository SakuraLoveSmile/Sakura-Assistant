package report

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"assistant-agent/internal/collect"
	"assistant-agent/internal/queue"
)

// ---------- fakes ----------

type fakeProvider struct {
	res *collect.Result
	err error
}

func (f *fakeProvider) Collect(context.Context) (*collect.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func sampleResult() *collect.Result {
	pct := 12.3
	return &collect.Result{
		Capabilities: collect.Capabilities{
			Containers:  collect.CapUnsupported,
			Smart:       collect.CapUnsupported,
			StoragePool: collect.CapUnsupported,
			Network:     collect.CapOK,
		},
		Sample: collect.Sample{
			TS:         collect.NewISOTime(time.Now()),
			CPUPercent: &pct,
		},
	}
}

// fakeHub 记录到达的指标 seq 与事件。
type fakeHub struct {
	metricsStatus atomic.Int32
	eventsStatus  atomic.Int32
	intervalSecs  int64

	mu           sync.Mutex
	metricsSeqs  []int64
	eventBatches [][]map[string]any
	eventKinds   []string
	metricsHits  int
}

func newFakeHub() *fakeHub {
	h := &fakeHub{intervalSecs: 30}
	h.metricsStatus.Store(202)
	h.eventsStatus.Store(200)
	return h
}

func (h *fakeHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ingest/metrics", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		status := h.metricsStatus.Load()
		if status != 202 {
			w.WriteHeader(int(status))
			w.Write([]byte(`{"error":{"code":"x","message":"m"}}`))
			return
		}
		var p struct {
			Seq int64 `json:"seq"`
		}
		json.Unmarshal(body, &p)
		h.mu.Lock()
		h.metricsSeqs = append(h.metricsSeqs, p.Seq)
		h.metricsHits++
		h.mu.Unlock()
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{
			"accepted": true, "duplicate": false,
			"serverTime":            "2026-09-18T06:30:00.000Z",
			"reportIntervalSeconds": h.intervalSecs,
		})
	})
	mux.HandleFunc("/api/v1/ingest/events", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		status := h.eventsStatus.Load()
		if status != 200 {
			w.WriteHeader(int(status))
			w.Write([]byte(`{"error":{"code":"x","message":"m"}}`))
			return
		}
		var p struct {
			Events []map[string]any `json:"events"`
		}
		json.Unmarshal(body, &p)
		h.mu.Lock()
		h.eventBatches = append(h.eventBatches, p.Events)
		var ids []string
		for _, e := range p.Events {
			if id, ok := e["eventId"].(string); ok {
				ids = append(ids, id)
			}
			if k, ok := e["kind"].(string); ok {
				h.eventKinds = append(h.eventKinds, k)
			}
		}
		h.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"accepted": ids, "duplicates": []string{}, "rejected": []any{},
			"lastSeq": 1, "serverTime": "2026-09-18T06:30:00.000Z",
		})
	})
	return mux
}

func (h *fakeHub) seqs() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int64(nil), h.metricsSeqs...)
}

func (h *fakeHub) batches() [][]map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]map[string]any(nil), h.eventBatches...)
}

func (h *fakeHub) kinds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.eventKinds...)
}

// ---------- helpers ----------

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func newTestReporter(t *testing.T, hubURL func(*httptest.Server) string, q *queue.Queue) (*Reporter, *fakeHub, *httptest.Server) {
	t.Helper()
	hub := newFakeHub()
	srv := httptest.NewServer(hub.handler())
	t.Cleanup(srv.Close)
	hc := NewHubClient(hubURL(srv), "ask_0123456789abcdef0123456789abcdef0123456789abcdef", false, 5*time.Second)
	r := New(&fakeProvider{res: sampleResult()}, q, hc,
		AgentInfo{Version: "0.1.0", OS: "linux", Arch: "amd64", Hostname: "test"},
		30*time.Second, testLogger())
	r.now = time.Now
	r.backoffFn = func(int) time.Duration { return 5 * time.Millisecond }
	r.probeEvery = 60 * time.Millisecond
	return r, hub, srv
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func enqueueMetrics(t *testing.T, q *queue.Queue, n int64) {
	t.Helper()
	for i := int64(1); i <= n; i++ {
		seq, err := q.NextMetricsSeq()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := BuildMetrics(seq, AgentInfo{}, sampleResult(), time.Now())
		if _, err := q.EnqueueMetrics(seq, body); err != nil {
			t.Fatal(err)
		}
	}
}

// ---------- tests ----------

func TestBackoffTable(t *testing.T) {
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second,
	}
	for i, w := range want {
		if got := DefaultBackoff(i); got != w {
			t.Fatalf("backoff(%d) = %v; want %v", i, got, w)
		}
	}
	if got := DefaultBackoff(100); got != 60*time.Second {
		t.Fatalf("backoff(100) = %v; want 60s cap", got)
	}
}

// 补传顺序：hub 不可达积压 → 恢复后按 seq 升序送达。
func TestDrainPreservesOrder(t *testing.T) {
	q, err := queue.Open(t.TempDir(), 500, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	enqueueMetrics(t, q, 5)

	r, hub, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)
	if err := r.drainOnce(context.Background()); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}
	got := hub.seqs()
	if len(got) != 5 {
		t.Fatalf("received = %v; want 5 batches", got)
	}
	for i, s := range got {
		if s != int64(i+1) {
			t.Fatalf("order = %v; want 1..5", got)
		}
	}
	if n, _ := q.MetricsPending(); n != 0 {
		t.Fatalf("pending = %d after drain", n)
	}
}

// 指标积压 >maxReplay → 只补最近 maxReplay 批 + queue_overflow 事件。
func TestBacklogTrimAndOverflowEvent(t *testing.T) {
	q, err := queue.Open(t.TempDir(), 500, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	enqueueMetrics(t, q, 7)

	r, hub, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)
	r.maxReplay = 3
	if err := r.drainOnce(context.Background()); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}
	got := hub.seqs()
	if len(got) != 3 || got[0] != 5 || got[2] != 7 {
		t.Fatalf("received = %v; want seqs 5,6,7（丢最旧）", got)
	}
	// overflow 事件应随事件流上报。
	found := false
	for _, k := range hub.kinds() {
		if k == "queue_overflow" {
			found = true
		}
	}
	if !found {
		t.Fatalf("queue_overflow event not delivered; kinds=%v", hub.kinds())
	}
	// overflow body 应含丢弃数与时间窗。
	batches := hub.batches()
	var overflowBody string
	for _, b := range batches {
		for _, e := range b {
			if e["kind"] == "queue_overflow" {
				overflowBody, _ = e["body"].(string)
			}
		}
	}
	if overflowBody == "" || !contains(overflowBody, "dropped=4") || !contains(overflowBody, "queue=metrics") {
		t.Fatalf("overflow body = %q; want dropped=4 window", overflowBody)
	}
}

// 事件队列溢出 → queue_overflow（queue=events）。
func TestEventQueueOverflowEmits(t *testing.T) {
	q, err := queue.Open(t.TempDir(), 500, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for i := int64(0); i < 4; i++ {
		seq, _ := q.NextEventSeq()
		b, _ := MarshalEvent(newEvent("custom", "info", "t", "", seq, time.Now()))
		if _, err := q.EnqueueEvent(seq, b); err != nil {
			t.Fatal(err)
		}
	}
	r, hub, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)
	if err := r.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range hub.batches() {
		for _, e := range b {
			if e["kind"] == "queue_overflow" && contains(e["body"].(string), "queue=events") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("events queue_overflow not delivered")
	}
}

// 事件分批 ≤100。
func TestEventBatchCap100(t *testing.T) {
	q, err := queue.Open(t.TempDir(), 500, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for i := int64(0); i < 150; i++ {
		seq, _ := q.NextEventSeq()
		b, _ := MarshalEvent(newEvent("custom", "info", "t", "", seq, time.Now()))
		if _, err := q.EnqueueEvent(seq, b); err != nil {
			t.Fatal(err)
		}
	}
	r, hub, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)
	if err := r.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	batches := hub.batches()
	if len(batches) != 2 || len(batches[0]) != 100 || len(batches[1]) != 50 {
		t.Fatalf("batches = %d sizes %v", len(batches), batchSizes(batches))
	}
}

// 400 → 毒载荷丢弃（不再重试）+ custom 通知事件随流上报。
func TestPoisonMetricsDropped(t *testing.T) {
	q, err := queue.Open(t.TempDir(), 500, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	enqueueMetrics(t, q, 2)
	r, hub, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)
	hub.metricsStatus.Store(400)
	if err := r.drainOnce(context.Background()); err != nil {
		t.Fatalf("poison should be handled internally, got %v", err)
	}
	if n, _ := q.MetricsPending(); n != 0 {
		t.Fatalf("pending = %d; poison rows should be dropped", n)
	}
	// 毒载荷通知事件入队（NoTrim），下一轮送达。
	hub.metricsStatus.Store(202)
	if err := r.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range hub.kinds() {
		if k == "custom" {
			found = true
		}
	}
	if !found {
		t.Fatal("payload-dropped custom event not delivered")
	}
}

// 403 source_disabled → 可重试（入队保留）。
func TestForbiddenIsRetryable(t *testing.T) {
	q, _ := queue.Open(t.TempDir(), 500, 1000)
	defer q.Close()
	enqueueMetrics(t, q, 1)
	r, hub, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)
	hub.metricsStatus.Store(403)
	err := r.drainOnce(context.Background())
	if err == nil || Classify(err) != ErrRetryable {
		t.Fatalf("err = %v; want retryable", err)
	}
	if n, _ := q.MetricsPending(); n != 1 {
		t.Fatalf("pending = %d; batch must stay queued", n)
	}
}

// 401 → ErrUnauthorized；probe 成功恢复并送达该条。
func TestUnauthorizedProbeRecovery(t *testing.T) {
	q, _ := queue.Open(t.TempDir(), 500, 1000)
	defer q.Close()
	enqueueMetrics(t, q, 2)
	r, hub, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)

	hub.metricsStatus.Store(401)
	err := r.drainOnce(context.Background())
	if err == nil || Classify(err) != ErrUnauthorized {
		t.Fatalf("err = %v; want unauthorized", err)
	}
	if n, _ := q.MetricsPending(); n != 2 {
		t.Fatalf("pending = %d; nothing delivered", n)
	}

	// 密钥恢复（hub 重新 202）→ probe 应送达队首并恢复。
	hub.metricsStatus.Store(202)
	attempt, unauthorized := 0, true
	var nextTry time.Time
	r.probeUnauthorized(context.Background(), &attempt, &unauthorized, &nextTry)
	if unauthorized {
		t.Fatal("still unauthorized after successful probe")
	}
	if n, _ := q.MetricsPending(); n != 1 {
		t.Fatalf("pending = %d; probed batch should be delivered", n)
	}
	if got := hub.seqs(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("hub received %v; want seq 1", got)
	}
}

// reportIntervalSeconds 响应 → 调整间隔。
func TestIntervalAdjusted(t *testing.T) {
	q, _ := queue.Open(t.TempDir(), 500, 1000)
	defer q.Close()
	enqueueMetrics(t, q, 1)
	r, hub, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)
	hub.intervalSecs = 45
	if err := r.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Interval() != 45*time.Second {
		t.Fatalf("interval = %v; want 45s", r.Interval())
	}
}

// 端到端：Run → agent_started 先发 → 指标按序 → 断连积压 → 恢复补传。
func TestEndToEnd(t *testing.T) {
	q, _ := queue.Open(t.TempDir(), 500, 1000)
	defer q.Close()
	r, hub, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)
	// 不让 hub ack 覆盖采集间隔，否则 30s 内不会有新样本，积压判据无法满足。
	hub.intervalSecs = 0

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	// 加快采集循环。
	r.interval.Store(int64(60 * time.Millisecond))

	// agent_started 事件应最先到达。
	waitFor(t, "agent_started", 3*time.Second, func() bool {
		for _, k := range hub.kinds() {
			if k == "agent_started" {
				return true
			}
		}
		return false
	})

	// 指标批次到达。
	waitFor(t, "metrics batches", 3*time.Second, func() bool {
		return len(hub.seqs()) >= 2
	})

	// hub 断连 → 继续入队（seq 单调）。
	hub.metricsStatus.Store(503)
	time.Sleep(200 * time.Millisecond)
	before := len(hub.seqs())

	// 恢复 → 按序补传全部积压。
	hub.metricsStatus.Store(202)
	waitFor(t, "backlog replay", 5*time.Second, func() bool {
		n, _ := q.MetricsPending()
		return n == 0 && len(hub.seqs()) > before
	})
	got := hub.seqs()
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("seq order broken: %v", got)
		}
	}

	// 优雅退出：cancel → flush → Run 返回。
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// 401 端到端：停发 + 周期性探测，恢复后继续。
func TestEndToEndUnauthorized(t *testing.T) {
	q, _ := queue.Open(t.TempDir(), 500, 1000)
	defer q.Close()
	r, hub, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)
	hub.metricsStatus.Store(401)
	hub.eventsStatus.Store(401)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	r.interval.Store(int64(50 * time.Millisecond))

	// 401 期间：请求应有（探测）但队列只增不减。
	time.Sleep(300 * time.Millisecond)
	if n, _ := q.MetricsPending(); n == 0 {
		t.Fatal("metrics should be queued during 401")
	}

	// 密钥恢复 → 队列清空。
	hub.metricsStatus.Store(202)
	hub.eventsStatus.Store(200)
	waitFor(t, "resume after key rotation", 5*time.Second, func() bool {
		n, _ := q.MetricsPending()
		return n == 0
	})
	cancel()
	<-done
}

// 采集失败 → 不入队（绝不伪造数据）。
func TestCollectFailureSkipsEnqueue(t *testing.T) {
	q, _ := queue.Open(t.TempDir(), 500, 1000)
	defer q.Close()
	r, _, _ := newTestReporter(t, func(s *httptest.Server) string { return s.URL }, q)
	r.prov = &fakeProvider{err: errors.New("collect boom")}
	r.collectOnce(context.Background())
	if n, _ := q.MetricsPending(); n != 0 {
		t.Fatalf("pending = %d; failed collect must not enqueue", n)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func batchSizes(b [][]map[string]any) []int {
	out := make([]int, len(b))
	for i, x := range b {
		out[i] = len(x)
	}
	return out
}
