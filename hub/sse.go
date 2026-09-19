package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// broker 为 changeSeq 变更的发布-订阅中心；SSE 客户端各自订阅。
type broker struct {
	mu   sync.Mutex
	subs map[chan *changeEntry]struct{}
}

func newBroker() *broker {
	return &broker{subs: map[chan *changeEntry]struct{}{}}
}

// Subscribe 注册订阅者，返回取消函数。
func (b *broker) Subscribe() (chan *changeEntry, func()) {
	ch := make(chan *changeEntry, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Publish 非阻塞广播；订阅者缓冲满则丢弃（客户端走 resync）。
func (b *broker) Publish(e *changeEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// sseWrite 输出一条 SSE 帧。
func sseWrite(w http.ResponseWriter, event string, id int64, data []byte) {
	if id > 0 {
		fmt.Fprintf(w, "id: %d\n", id)
	}
	fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range splitLines(data) {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	out = append(out, b[start:])
	return out
}

// changeSSEData 生成该变更在 SSE 中的 data：message 类注入 notify 块。
func changeSSEData(e *changeEntry) []byte {
	if e.Type != "message" || e.Notify == nil {
		return e.Data
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(e.Data, &m); err != nil {
		return e.Data
	}
	nb, _ := json.Marshal(e.Notify)
	m["notify"] = nb
	out, err := json.Marshal(m)
	if err != nil {
		return e.Data
	}
	return out
}

// handleStream 实现 GET /api/v1/stream（SSE）。
//
// 语义：since/Last-Event-ID 续传；落后变更窗口 → event: resync 后关闭；
// ping 15s 保活；事件类型 message|fault|source|rules|settings。
func (a *app) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		errInternal(w)
		return
	}

	// since 解析：query since 优先，其次 Last-Event-ID。
	since := int64(0)
	if s := r.URL.Query().Get("since"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			errInvalid(w, "since 非法")
			return
		}
		since = v
	} else if h := r.Header.Get("Last-Event-ID"); h != "" {
		if v, err := strconv.ParseInt(h, 10, 64); err == nil && v >= 0 {
			since = v
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 变更窗口检查：since 落后于保留窗口 → resync。
	if minSeq, okMin := a.st.minChangeSeq(); okMin && since+1 < minSeq {
		sseWrite(w, "resync", 0, []byte(`{"reason":"cursor_expired"}`))
		flusher.Flush()
		return
	}

	// 先订阅再补历史，避免间隙。
	ch, unsub := a.broker.Subscribe()
	defer unsub()

	// 补发 since 之后已保留的变更。
	replayed, err := a.changesSince(since)
	if err != nil {
		sseWrite(w, "resync", 0, []byte(`{"reason":"internal"}`))
		flusher.Flush()
		return
	}
	last := since
	for _, e := range replayed {
		sseWrite(w, e.Type, e.Seq, changeSSEData(e))
		last = e.Seq
	}
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			if e == nil {
				continue
			}
			if e.Seq <= last {
				continue
			}
			sseWrite(w, e.Type, e.Seq, changeSSEData(e))
			last = e.Seq
			flusher.Flush()
		case <-ping.C:
			payload, _ := json.Marshal(map[string]string{"serverTime": fmtTS(nowUTC())})
			sseWrite(w, "ping", 0, payload)
			flusher.Flush()
		}
	}
}

// changesSince 取 seq>since 的全部保留变更（升序）。
func (a *app) changesSince(since int64) ([]*changeEntry, error) {
	rows, err := a.st.db.Query(
		`SELECT seq, type, data_json, notify_json FROM changes WHERE seq > ? ORDER BY seq`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*changeEntry
	for rows.Next() {
		var e changeEntry
		var data string
		var notify *string
		if err := rows.Scan(&e.Seq, &e.Type, &data, &notify); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		if notify != nil && *notify != "" {
			var nb notifyBlock
			if json.Unmarshal([]byte(*notify), &nb) == nil {
				e.Notify = &nb
			}
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
