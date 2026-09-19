package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// newTestApp 建立使用临时库的应用实例（不起后台 goroutine）。
func newTestApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	cfg := config{
		dbPath:            filepath.Join(dir, "test.db"),
		adminUser:         "admin",
		adminPassword:     "pw123456",
		baseURL:           "http://hub.test",
		rawDays:           7,
		rollupDays:        90,
		messagesDays:      90,
		changesDays:       7,
		changesMinSeq:     100000,
		heartbeatInterval: time.Hour, // 测试手动触发 checkHeartbeats
		rollupInterval:    time.Hour,
		retentionInterval: time.Hour,
	}
	a, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { a.st.db.Close() })
	return a
}

// testServer 包装 httptest.Server。
func testServer(t *testing.T, a *app) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(a.routes())
	t.Cleanup(s.Close)
	return s
}

// doJSON 发 JSON 请求，返回状态码与解码体。
func doJSON(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// login 登录并返回 token。
func login(t *testing.T, base string) string {
	t.Helper()
	code, body := doJSON(t, "POST", base+"/api/v1/auth/login", "",
		map[string]any{"username": "admin", "password": "pw123456", "deviceLabel": "test"})
	if code != 200 {
		t.Fatalf("login: %d %v", code, body)
	}
	return body["token"].(string)
}

// createSource 创建来源，返回 (sourceId, accessKey)。
func createSource(t *testing.T, base, token, name, kind string, attBaseURL *string) (string, string) {
	t.Helper()
	req := map[string]any{"name": name, "kind": kind}
	if attBaseURL != nil {
		req["attachmentBaseUrl"] = *attBaseURL
	}
	code, body := doJSON(t, "POST", base+"/api/v1/sources", token, req)
	if code != 201 {
		t.Fatalf("createSource: %d %v", code, body)
	}
	src := body["source"].(map[string]any)
	return src["id"].(string), body["accessKey"].(string)
}

// ingestMetrics 发送一批指标。
func ingestMetrics(t *testing.T, base, key string, seq int64, sample map[string]any) (int, map[string]any) {
	t.Helper()
	return doJSON(t, "POST", base+"/api/v1/ingest/metrics", key, map[string]any{
		"seq":    seq,
		"sentAt": fmtTS(nowUTC()),
		"agent":  map[string]any{"version": "0.1.0", "os": "linux", "arch": "amd64", "hostname": "test-host"},
		"capabilities": map[string]any{
			"containers": "ok", "smart": "ok", "storagePool": "ok", "network": "ok",
		},
		"sample": sample,
	})
}

// ingestEvents 发送一批事件。
func ingestEvents(t *testing.T, base, key string, events []map[string]any) (int, map[string]any) {
	t.Helper()
	return doJSON(t, "POST", base+"/api/v1/ingest/events", key, map[string]any{
		"sentAt": fmtTS(nowUTC()),
		"events": events,
	})
}

// mkEvent 构造事件载荷。
func mkEvent(id string, seq int64, kind string, opts map[string]any) map[string]any {
	ev := map[string]any{
		"eventId":    id,
		"seq":        seq,
		"kind":       kind,
		"occurredAt": fmtTS(nowUTC()),
		"severity":   "info",
		"title":      "t-" + id,
	}
	for k, v := range opts {
		ev[k] = v
	}
	return ev
}

// queryFault 直接读库取故障行。
func queryFault(t *testing.T, a *app, sourceID, faultKey string) *faultRow {
	t.Helper()
	var f *faultRow
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		var err error
		f, err = loadFaultTx(tx, sourceID, faultKey)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// syncAll 拉取 since 之后全部变更。
func syncAll(t *testing.T, base, token string, since int64) (int64, []map[string]any) {
	t.Helper()
	var all []map[string]any
	cursor := since
	for {
		code, body := doJSON(t, "GET",
			fmt.Sprintf("%s/api/v1/sync?since=%d&limit=100", base, cursor), token, nil)
		if code != 200 {
			t.Fatalf("sync: %d %v", code, body)
		}
		for _, c := range body["changes"].([]any) {
			all = append(all, c.(map[string]any))
		}
		cursor = int64(body["cursor"].(float64))
		if !body["hasMore"].(bool) {
			break
		}
	}
	return cursor, all
}

// countChanges 统计变更类型出现次数。
func countChanges(changes []map[string]any, typ string) int {
	n := 0
	for _, c := range changes {
		if c["type"] == typ {
			n++
		}
	}
	return n
}
