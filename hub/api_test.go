package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSyncOrderingAndTombstone sync 按 changeSeq 顺序一致（含 tombstone）。
func TestSyncOrderingAndTombstone(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	ingestEvents(t, srv.URL, key, []map[string]any{
		mkEvent("s1", 1, "feedback_created", nil),
		mkEvent("s2", 2, "feedback_fault", map[string]any{
			"faultKey": "feedback:T", "incidentAction": "open", "severity": "warning"}),
		mkEvent("s3", 3, "feedback_recovered", map[string]any{
			"faultKey": "feedback:T", "incidentAction": "resolve"}),
	})

	cursor, changes := syncAll(t, srv.URL, token, 0)
	if len(changes) == 0 {
		t.Fatal("no changes")
	}
	// 严格按 changeSeq 升序。
	prev := int64(-1)
	for _, c := range changes {
		seq := int64(c["changeSeq"].(float64))
		if seq <= prev {
			t.Fatalf("out of order: %d <= %d", seq, prev)
		}
		prev = seq
	}
	if countChanges(changes, "message") < 3 || countChanges(changes, "fault") < 2 {
		t.Fatalf("changes: %v", changes)
	}
	if cursor != prev {
		t.Fatalf("cursor %d != last seq %d", cursor, prev)
	}

	// 制造超期消息 → 保留任务 → tombstone。
	var msgID string
	if err := a.st.db.QueryRow(`SELECT id FROM messages LIMIT 1`).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	old := fmtTS(nowUTC().Add(-100 * 24 * time.Hour))
	if _, err := a.st.db.Exec(
		`UPDATE messages SET received_at=?, fault_id=NULL WHERE id=?`, old, msgID); err != nil {
		t.Fatal(err)
	}
	if err := a.runRetention(nowUTC()); err != nil {
		t.Fatal(err)
	}
	_, changes = syncAll(t, srv.URL, token, cursor)
	found := false
	for _, c := range changes {
		if c["type"] == "tombstone" {
			data := c["data"].(map[string]any)
			if data["type"] == "message" && data["id"] == msgID {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("tombstone not in sync")
	}
}

// TestSyncIdempotentRead 重复 sync 返回空且 cursor 推进。
func TestSyncIdempotentRead(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)
	ingestEvents(t, srv.URL, key, []map[string]any{mkEvent("x", 1, "custom", nil)})

	cursor, changes := syncAll(t, srv.URL, token, 0)
	if len(changes) == 0 {
		t.Fatal("empty")
	}
	_, changes2 := syncAll(t, srv.URL, token, cursor)
	if len(changes2) != 0 {
		t.Fatalf("should be empty: %v", changes2)
	}
}

// TestSSEStream SSE：重放 + notify 块 + 实时推送。
func TestSSEStream(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	ingestEvents(t, srv.URL, key, []map[string]any{
		mkEvent("m1", 1, "feedback_fault", map[string]any{
			"faultKey": "feedback:S", "incidentAction": "open", "severity": "warning"}),
	})

	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/stream?since=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type: %s", ct)
	}

	type sseFrame struct {
		id    string
		event string
		data  string
	}
	frames := make(chan sseFrame, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data += strings.TrimPrefix(line, "data: ")
			case line == "":
				if f.event != "" {
					frames <- f
				}
				f = sseFrame{}
			}
		}
	}()

	// 读重放帧：应含 fault 与 message 事件，message 带 notify=incident_open。
	var gotNotify bool
	var gotMessage bool
	deadline := time.After(5 * time.Second)
	for !gotNotify || !gotMessage {
		select {
		case f := <-frames:
			if f.event == "message" {
				gotMessage = true
				var m map[string]any
				if json.Unmarshal([]byte(f.data), &m) == nil {
					if nb, ok := m["notify"].(map[string]any); ok {
						if nb["kind"] == "incident_open" && nb["faultId"] != nil {
							gotNotify = true
						}
					}
				}
			}
		case <-deadline:
			t.Fatal("timeout waiting for replayed SSE")
		}
	}

	// 实时推送：新事件到达。
	ingestEvents(t, srv.URL, key, []map[string]any{
		mkEvent("m2", 2, "feedback_recovered", map[string]any{
			"faultKey": "feedback:S", "incidentAction": "resolve"}),
	})
	select {
	case f := <-frames:
		if f.event == "" {
			t.Fatal("empty frame")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no live SSE")
	}
	// 再读几帧确认 message 事件带 incident_resolved notify。
	seenResolved := false
	for i := 0; i < 10 && !seenResolved; i++ {
		select {
		case f := <-frames:
			if f.event == "message" {
				var m map[string]any
				if json.Unmarshal([]byte(f.data), &m) == nil {
					if nb, ok := m["notify"].(map[string]any); ok && nb["kind"] == "incident_resolved" {
						seenResolved = true
					}
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no resolved notify")
		}
	}
	if !seenResolved {
		t.Fatal("incident_resolved notify missing")
	}
}

// TestSSEResync since 落后过多 → resync 帧后关闭。
func TestSSEResync(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	// 制造一条 change 然后把 min seq 推高：直接删旧 changes。
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)
	ingestEvents(t, srv.URL, key, []map[string]any{mkEvent("z", 1, "custom", nil)})
	if _, err := a.st.db.Exec(`DELETE FROM changes WHERE seq < 100`); err != nil {
		t.Fatal(err)
	}
	// 再制造一条 change 使 min 存在但 > since+1。
	ingestEvents(t, srv.URL, key, []map[string]any{mkEvent("z2", 2, "custom", nil)})

	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/stream?since=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "event: resync") {
		t.Fatalf("expected resync: %s", body)
	}
}

// TestMessagesReadOps read/unread/read-all 与 sync 广播。
func TestMessagesReadOps(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	ingestEvents(t, srv.URL, key, []map[string]any{
		mkEvent("r1", 1, "feedback_created", nil),
		mkEvent("r2", 2, "feedback_created", nil),
	})

	code, body := doJSON(t, "GET", srv.URL+"/api/v1/messages", token, nil)
	if code != 200 {
		t.Fatal(code)
	}
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items: %d", len(items))
	}
	mid := items[0].(map[string]any)["id"].(string)

	// read 幂等。
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/messages/"+mid+"/read", token, nil)
	if code != 200 || body["message"].(map[string]any)["readAt"] == nil {
		t.Fatalf("read: %d %v", code, body)
	}
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/messages/"+mid+"/read", token, nil)
	if code != 200 {
		t.Fatalf("read idempotent: %d", code)
	}
	// unread。
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/messages/"+mid+"/unread", token, nil)
	if code != 200 || body["message"].(map[string]any)["readAt"] != nil {
		t.Fatalf("unread: %v", body)
	}
	// read-all。
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/messages/read-all", token, map[string]any{})
	if code != 200 || body["updated"].(float64) != 2 {
		t.Fatalf("read-all: %v", body)
	}
	// filter=unread 应为空。
	code, body = doJSON(t, "GET", srv.URL+"/api/v1/messages?filter=unread", token, nil)
	if len(body["items"].([]any)) != 0 {
		t.Fatal("unread not empty")
	}
}

// TestFaultOps fault read/mute/unmute。
func TestFaultOps(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "fb", "feedback", nil)
	_ = srcID

	ingestEvents(t, srv.URL, key, []map[string]any{
		mkEvent("f1", 1, "feedback_fault", map[string]any{
			"faultKey": "feedback:Q", "incidentAction": "open", "severity": "warning"}),
	})
	code, body := doJSON(t, "GET", srv.URL+"/api/v1/faults?state=open", token, nil)
	if code != 200 {
		t.Fatal(code)
	}
	fid := body["items"].([]any)[0].(map[string]any)["id"].(string)

	code, body = doJSON(t, "POST", srv.URL+"/api/v1/faults/"+fid+"/mute", token, map[string]any{})
	if code != 200 || body["fault"].(map[string]any)["mutedAt"] == nil {
		t.Fatalf("mute: %v", body)
	}
	// 幂等
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/faults/"+fid+"/mute", token, nil)
	if code != 200 {
		t.Fatalf("mute idempotent: %d", code)
	}
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/faults/"+fid+"/unmute", token, nil)
	if code != 200 || body["fault"].(map[string]any)["mutedAt"] != nil {
		t.Fatalf("unmute: %v", body)
	}
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/faults/"+fid+"/read", token, nil)
	if code != 200 || body["fault"].(map[string]any)["readAt"] == nil {
		t.Fatalf("fault read: %v", body)
	}
}

// TestRulesVersionConflict 乐观锁。
func TestRulesVersionConflict(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	code, body := doJSON(t, "GET", srv.URL+"/api/v1/rules", token, nil)
	if code != 200 {
		t.Fatal(code)
	}
	ver := int64(body["version"].(float64))
	if len(body["rules"].([]any)) == 0 {
		t.Fatal("default rules empty")
	}

	// 版本不符 → 409 version_conflict 携带当前 version。
	code, body = doJSON(t, "PUT", srv.URL+"/api/v1/rules", token,
		map[string]any{"expectedVersion": ver + 9, "rules": []any{}})
	if code != http.StatusConflict {
		t.Fatalf("expected 409: %d %v", code, body)
	}
	if body["error"].(map[string]any)["code"] != "version_conflict" {
		t.Fatalf("code: %v", body)
	}
	if int64(body["version"].(float64)) != ver {
		t.Fatalf("version not echoed: %v", body)
	}
	// 版本匹配 → 200，version+1，规则整体替换。
	newRules := []map[string]any{
		{"id": "only", "kind": "pool", "severity": "critical", "enabled": true},
	}
	code, body = doJSON(t, "PUT", srv.URL+"/api/v1/rules", token,
		map[string]any{"expectedVersion": ver, "rules": newRules, "heartbeatSeconds": 120})
	if code != 200 {
		t.Fatalf("put: %d %v", code, body)
	}
	if int64(body["version"].(float64)) != ver+1 || body["heartbeatSeconds"].(float64) != 120 {
		t.Fatalf("version bump: %v", body)
	}
	if len(body["rules"].([]any)) != 1 {
		t.Fatal("rules replaced")
	}
}

// TestSettingsPatch settings 版本锁与校验。
func TestSettingsPatch(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	code, body := doJSON(t, "GET", srv.URL+"/api/v1/settings", token, nil)
	if code != 200 {
		t.Fatal(code)
	}
	ver := int64(body["version"].(float64))

	code, body = doJSON(t, "PATCH", srv.URL+"/api/v1/settings", token,
		map[string]any{"expectedVersion": ver, "reportIntervalSeconds": 5})
	if code != 400 {
		t.Fatalf("interval bounds: %d", code)
	}
	code, body = doJSON(t, "PATCH", srv.URL+"/api/v1/settings", token,
		map[string]any{"expectedVersion": ver,
			"dnd":                   map[string]any{"enabled": true, "start": "23:00", "end": "08:00", "timezone": "Asia/Shanghai"},
			"reportIntervalSeconds": 60})
	if code != 200 {
		t.Fatalf("patch: %d %v", code, body)
	}
	if body["dnd"].(map[string]any)["enabled"] != true ||
		body["reportIntervalSeconds"].(float64) != 60 {
		t.Fatalf("settings: %v", body)
	}
	// reportIntervalSeconds 经 ingest 响应下发。
	_, key := createSource(t, srv.URL, token, "nas", "device", nil)
	_, resp := ingestMetrics(t, srv.URL, key, 1, map[string]any{"ts": fmtTS(nowUTC())})
	if resp["reportIntervalSeconds"].(float64) != 60 {
		t.Fatalf("ingest interval: %v", resp)
	}
}

// TestSourcesCRUD 创建（一次性 accessKey+install）/ 读 / 改 / 轮换 / 删。
func TestSourcesCRUD(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	code, body := doJSON(t, "POST", srv.URL+"/api/v1/sources", token,
		map[string]any{"name": "NAS", "kind": "device"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	src := body["source"].(map[string]any)
	key := body["accessKey"].(string)
	inst := body["install"].(map[string]any)
	if !strings.HasPrefix(key, "ask_") || len(key) != 4+48 {
		t.Fatalf("key format: %q", key)
	}
	if inst["env"].(map[string]any)["ASSIST_HUB_URL"] != "http://hub.test" {
		t.Fatalf("env: %v", inst)
	}
	if inst["command"] == "" || inst["note"] == "" {
		t.Fatal("install incomplete")
	}
	if !strings.Contains(inst["command"].(string), "http://hub.test") {
		t.Fatal("command should use base URL")
	}
	// 列表里 keyHint 只回显末 4 位，绝不明文。
	if src["keyHint"] != key[len(key)-4:] {
		t.Fatalf("keyHint: %v", src["keyHint"])
	}
	if _, leaked := src["accessKey"]; leaked {
		t.Fatal("accessKey leaked in source object")
	}

	srcID := src["id"].(string)
	code, body = doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID, token, nil)
	if code != 200 {
		t.Fatal(code)
	}
	code, body = doJSON(t, "PATCH", srv.URL+"/api/v1/sources/"+srcID, token,
		map[string]any{"name": "NAS2"})
	if code != 200 || body["source"].(map[string]any)["name"] != "NAS2" {
		t.Fatalf("patch: %v", body)
	}
	// 轮换：新 key 生效、旧 key 失效。
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/sources/"+srcID+"/rotate-key", token, nil)
	if code != 200 {
		t.Fatalf("rotate: %d", code)
	}
	newKey := body["accessKey"].(string)
	code, _ = ingestMetrics(t, srv.URL, key, 1, map[string]any{"ts": fmtTS(nowUTC())})
	if code != http.StatusUnauthorized {
		t.Fatalf("old key should fail: %d", code)
	}
	code, _ = ingestMetrics(t, srv.URL, newKey, 1, map[string]any{"ts": fmtTS(nowUTC())})
	if code != http.StatusAccepted {
		t.Fatalf("new key: %d", code)
	}
	// 删除：历史保留、密钥失效。
	code, _ = doJSON(t, "DELETE", srv.URL+"/api/v1/sources/"+srcID, token, nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	code, _ = doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID, token, nil)
	if code != http.StatusNotFound {
		t.Fatalf("deleted get: %d", code)
	}
	code, _ = ingestMetrics(t, srv.URL, newKey, 2, map[string]any{"ts": fmtTS(nowUTC())})
	if code != http.StatusUnauthorized {
		t.Fatalf("deleted key: %d", code)
	}
	// 消息历史仍在。
	var n int
	a.st.db.QueryRow(`SELECT COUNT(1) FROM sources WHERE id=?`, srcID).Scan(&n)
	if n != 1 {
		t.Fatal("history lost")
	}
}

// TestMetricsSeries raw/5m/1h 查询。
func TestMetricsSeries(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)

	base := nowUTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	for i := 0; i < 4; i++ {
		ingestMetrics(t, srv.URL, key, int64(i+1), map[string]any{
			"ts":         fmtTS(base.Add(time.Duration(i) * time.Minute)),
			"cpuPercent": float64(10 * (i + 1)),
			"disks":      []map[string]any{{"mount": "/", "percent": 50.0 + float64(i)}},
		})
	}

	// raw。
	code, body := doJSON(t, "GET",
		fmt.Sprintf("%s/api/v1/metrics/series?source=%s&metric=cpu_percent&step=raw", srv.URL, srcID), token, nil)
	if code != 200 {
		t.Fatalf("raw: %d %v", code, body)
	}
	series := body["series"].([]any)
	if len(series) != 4 {
		t.Fatalf("raw points: %d", len(series))
	}
	p := series[0].(map[string]any)
	if p["avg"].(float64) != 10 || p["min"].(float64) != 10 || p["max"].(float64) != 10 {
		t.Fatalf("raw point: %v", p)
	}

	// disk_percent 无 label → 400。
	code, _ = doJSON(t, "GET",
		fmt.Sprintf("%s/api/v1/metrics/series?source=%s&metric=disk_percent&step=raw", srv.URL, srcID), token, nil)
	if code != 400 {
		t.Fatalf("disk label required: %d", code)
	}
	code, body = doJSON(t, "GET",
		fmt.Sprintf("%s/api/v1/metrics/series?source=%s&metric=disk_percent&step=raw&label=mount=/", srv.URL, srcID), token, nil)
	if code != 200 || len(body["series"].([]any)) != 4 {
		t.Fatalf("disk raw: %v", body)
	}

	// 聚合后查 5m / 1h。
	if err := a.runRollup(nowUTC()); err != nil {
		t.Fatal(err)
	}
	code, body = doJSON(t, "GET",
		fmt.Sprintf("%s/api/v1/metrics/series?source=%s&metric=cpu_percent&step=5m", srv.URL, srcID), token, nil)
	if code != 200 || len(body["series"].([]any)) == 0 {
		t.Fatalf("5m: %v", body)
	}
	code, body = doJSON(t, "GET",
		fmt.Sprintf("%s/api/v1/metrics/series?source=%s&metric=cpu_percent&step=1h", srv.URL, srcID), token, nil)
	if code != 200 {
		t.Fatal(code)
	}
	pts := body["series"].([]any)
	if len(pts) != 1 { // 4 个样本同一小时
		t.Fatalf("1h: %v", body)
	}
	avg := pts[0].(map[string]any)["avg"].(float64)
	if avg < 24.9 || avg > 25.1 { // (10+20+30+40)/4
		t.Fatalf("1h avg: %v", avg)
	}
}

// TestOverview overview 形态。
func TestOverview(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "NAS", "device", nil)

	ingestMetrics(t, srv.URL, key, 1, map[string]any{
		"ts": fmtTS(nowUTC()), "cpuPercent": 12.3, "memPercent": 45.6,
		"uptimeSeconds": 100,
		"disks":         []map[string]any{{"mount": "/", "percent": 55.1}},
	})
	code, body := doJSON(t, "GET", srv.URL+"/api/v1/overview", token, nil)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	srcs := body["sources"].([]any)
	if len(srcs) != 1 {
		t.Fatal("sources")
	}
	s := srcs[0].(map[string]any)
	if s["status"] != "online" || s["summary"] == nil {
		t.Fatalf("source: %v", s)
	}
	sum := s["summary"].(map[string]any)
	if sum["cpuPercent"].(float64) != 12.3 {
		t.Fatalf("summary: %v", sum)
	}
	if body["unreadMessages"] == nil || body["rulesVersion"] == nil || body["dnd"] == nil {
		t.Fatalf("overview: %v", body)
	}
}

// TestAttachmentProxy 附件代理：透传 / 404 / 502 / 未配置。
func TestAttachmentProxy(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/assist/feedback/FB1/attachments/screenshot":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("ETag", `"abc123"`)
			w.Write([]byte("PNGDATA"))
		case "/api/assist/feedback/FB1/attachments/logs/log9":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("LOGDATA"))
		case "/api/assist/feedback/FB1/attachments/broken":
			w.WriteHeader(500)
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()

	_, key := createSource(t, srv.URL, token, "fb", "feedback", &upstream.URL)
	atts := []map[string]any{
		{"id": "screenshot", "kind": "screenshot", "filename": "s.png", "mime": "image/png", "byteSize": 7, "sha256": "deadbeef"},
		{"id": "logs/log9", "kind": "log", "filename": "a.log", "mime": "text/plain", "byteSize": 7},
		{"id": "broken", "kind": "log", "filename": "b.log", "mime": "text/plain", "byteSize": 3},
	}
	ev := mkEvent("att1", 1, "feedback_created", map[string]any{
		"ref":         map[string]any{"feedbackId": "FB1"},
		"attachments": atts,
	})
	ingestEvents(t, srv.URL, key, []map[string]any{ev})

	var msgID string
	if err := a.st.db.QueryRow(`SELECT id FROM messages`).Scan(&msgID); err != nil {
		t.Fatal(err)
	}

	get := func(att string) (int, http.Header, string) {
		req, _ := http.NewRequest("GET",
			fmt.Sprintf("%s/api/v1/messages/%s/attachments/%s", srv.URL, msgID, att), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}

	// 200 透传：Content-Type / ETag / Cache-Control。
	code, hdr, body := get("screenshot")
	if code != 200 || body != "PNGDATA" {
		t.Fatalf("proxy: %d %q", code, body)
	}
	if hdr.Get("Content-Type") != "image/png" || hdr.Get("ETag") != `"abc123"` {
		t.Fatalf("headers: %v", hdr)
	}
	if hdr.Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("cache: %v", hdr)
	}
	// logs/<id> 透传。
	code, _, body = get("logs/log9")
	if code != 200 || body != "LOGDATA" {
		t.Fatalf("logs: %d %q", code, body)
	}
	// 上游 404 → 404（描述符不存在也是 404）。
	code, _, _ = get("missing")
	if code != 404 {
		t.Fatalf("missing att: %d", code)
	}
	// 描述符存在但 attId 无映射 → 404。
	code, _, _ = get("broken")
	if code != 404 {
		t.Fatalf("unmapped att: %d", code)
	}
	// 无 ref.feedbackId 的消息 → 404。
	ev2 := mkEvent("att2", 2, "feedback_created", map[string]any{
		"attachments": atts[:1],
	})
	ingestEvents(t, srv.URL, key, []map[string]any{ev2})
	var msg2 string
	a.st.db.QueryRow(`SELECT id FROM messages WHERE event_id='att2'`).Scan(&msg2)
	req, _ := http.NewRequest("GET",
		fmt.Sprintf("%s/api/v1/messages/%s/attachments/screenshot", srv.URL, msg2), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Fatalf("no ref: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestAttachmentNoBase 未配置 attachmentBaseUrl → 404。
func TestAttachmentNoBase(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)
	atts := []map[string]any{
		{"id": "screenshot", "kind": "screenshot", "filename": "s.png", "mime": "image/png", "byteSize": 7},
	}
	ev := mkEvent("nb1", 1, "feedback_created", map[string]any{
		"ref": map[string]any{"feedbackId": "FB1"}, "attachments": atts})
	ingestEvents(t, srv.URL, key, []map[string]any{ev})
	var msgID string
	a.st.db.QueryRow(`SELECT id FROM messages`).Scan(&msgID)
	req, _ := http.NewRequest("GET",
		fmt.Sprintf("%s/api/v1/messages/%s/attachments/screenshot", srv.URL, msgID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Fatalf("no base: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestAttachmentUpstreamError 上游 5xx → 502 attachment_unavailable。
func TestAttachmentUpstreamError(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer upstream.Close()
	_, key := createSource(t, srv.URL, token, "fb", "feedback", &upstream.URL)
	atts := []map[string]any{
		{"id": "screenshot", "kind": "screenshot", "filename": "s.png", "mime": "image/png", "byteSize": 7},
	}
	ev := mkEvent("e5", 1, "feedback_created", map[string]any{
		"ref": map[string]any{"feedbackId": "FB1"}, "attachments": atts})
	ingestEvents(t, srv.URL, key, []map[string]any{ev})
	var msgID string
	a.st.db.QueryRow(`SELECT id FROM messages`).Scan(&msgID)

	req, _ := http.NewRequest("GET",
		fmt.Sprintf("%s/api/v1/messages/%s/attachments/screenshot", srv.URL, msgID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("attachment request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("upstream 5xx: %d", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["error"].(map[string]any)["code"] != "attachment_unavailable" {
		t.Fatalf("code: %v", body)
	}
}

// TestHealthVersion 杂项端点无认证。
func TestHealthVersion(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)

	code, body := doJSON(t, "GET", srv.URL+"/api/v1/health", "", nil)
	if code != 200 || body["ok"] != true || body["version"] == nil {
		t.Fatalf("health: %v", body)
	}
	code, body = doJSON(t, "GET", srv.URL+"/api/v1/version", "", nil)
	if code != 200 || body["api"].(float64) != 1 {
		t.Fatalf("version: %v", body)
	}
	// 未认证访问客户端端点 → 401。
	code, _ = doJSON(t, "GET", srv.URL+"/api/v1/overview", "", nil)
	if code != 401 {
		t.Fatalf("unauth: %d", code)
	}
}

// TestSourceDeleteTombstone 删除来源 → sync 必须出现 tombstone{type:source,id}（api-v1 §sync）。
func TestSourceDeleteTombstone(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, _ := createSource(t, srv.URL, token, "nas", "device", nil)

	cursor, _ := syncAll(t, srv.URL, token, 0)
	code, _ := doJSON(t, "DELETE", srv.URL+"/api/v1/sources/"+srcID, token, nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	_, changes := syncAll(t, srv.URL, token, cursor)
	found := false
	for _, c := range changes {
		if c["type"] != "tombstone" {
			continue
		}
		data := c["data"].(map[string]any)
		if data["type"] == "source" && data["id"] == srcID {
			found = true
		}
	}
	if !found {
		t.Fatalf("source tombstone missing: %v", changes)
	}
}

// TestAttachmentInvalidID 含注入字符的 feedbackId / logId 不得发起回连 → 404。
func TestAttachmentInvalidID(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("x"))
	}))
	defer upstream.Close()

	_, key := createSource(t, srv.URL, token, "fb", "feedback", &upstream.URL)
	atts := []map[string]any{
		{"id": "screenshot", "kind": "screenshot", "filename": "s.png", "mime": "image/png", "byteSize": 1},
		{"id": "logs/a.b", "kind": "log", "filename": "x.log", "mime": "text/plain", "byteSize": 1},
	}
	ingestEvents(t, srv.URL, key, []map[string]any{
		mkEvent("bad1", 1, "feedback_created", map[string]any{
			"ref": map[string]any{"feedbackId": "FB1/../admin"}, "attachments": atts}),
		mkEvent("bad2", 2, "feedback_created", map[string]any{
			"ref": map[string]any{"feedbackId": "FB1"}, "attachments": atts}),
	})

	get := func(msgID, att string) int {
		req, _ := http.NewRequest("GET",
			fmt.Sprintf("%s/api/v1/messages/%s/attachments/%s", srv.URL, msgID, att), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	var msg1, msg2 string
	a.st.db.QueryRow(`SELECT id FROM messages WHERE event_id='bad1'`).Scan(&msg1)
	a.st.db.QueryRow(`SELECT id FROM messages WHERE event_id='bad2'`).Scan(&msg2)
	if c := get(msg1, "screenshot"); c != 404 {
		t.Fatalf("injected feedbackId: %d", c)
	}
	if c := get(msg2, "logs/a.b"); c != 404 {
		t.Fatalf("injected logId: %d", c)
	}
	if hits != 0 {
		t.Fatalf("upstream contacted %d times on invalid ids", hits)
	}
}

// TestMessageNotifyMetadata 独立消息变更携带 notify{kind:new_message}；
// 已读变更不得携带通知块（防客户端对已读操作弹通知）。
func TestMessageNotifyMetadata(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	ingestEvents(t, srv.URL, key, []map[string]any{mkEvent("n1", 1, "custom", nil)})
	var msgID, nj string
	if err := a.st.db.QueryRow(
		`SELECT ref_id, COALESCE(notify_json,'') FROM changes WHERE type='message' ORDER BY seq DESC LIMIT 1`,
	).Scan(&msgID, &nj); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nj, "new_message") {
		t.Fatalf("standalone message notify: %q", nj)
	}

	code, _ := doJSON(t, "POST", srv.URL+"/api/v1/messages/"+msgID+"/read", token, nil)
	if code != 200 {
		t.Fatalf("read: %d", code)
	}
	if err := a.st.db.QueryRow(
		`SELECT COALESCE(notify_json,'') FROM changes WHERE type='message' ORDER BY seq DESC LIMIT 1`,
	).Scan(&nj); err != nil {
		t.Fatal(err)
	}
	if nj != "" {
		t.Fatalf("read change must not carry notify: %q", nj)
	}
}

// TestPendingResolveRetention 超期未等到 open 的待定恢复行随消息保留期清理；
// 未超期的保留（仍可能等到迟到的 open）。
func TestPendingResolveRetention(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, _ := createSource(t, srv.URL, token, "fb", "feedback", nil)

	old := fmtTS(nowUTC().Add(-100 * 24 * time.Hour))
	fresh := fmtTS(nowUTC())
	if _, err := a.st.db.Exec(
		`INSERT INTO pending_resolves(source_id,fault_key,seq,occurred_at,title,event_id) VALUES(?,?,?,?,?,?)`,
		srcID, "feedback:X", 9, old, "old", "e_old"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.db.Exec(
		`INSERT INTO pending_resolves(source_id,fault_key,seq,occurred_at,title,event_id) VALUES(?,?,?,?,?,?)`,
		srcID, "feedback:X", 10, fresh, "new", "e_new"); err != nil {
		t.Fatal(err)
	}
	if err := a.runRetention(nowUTC()); err != nil {
		t.Fatal(err)
	}
	var n int
	a.st.db.QueryRow(`SELECT COUNT(1) FROM pending_resolves WHERE event_id='e_old'`).Scan(&n)
	if n != 0 {
		t.Fatal("stale pending_resolve not cleaned")
	}
	a.st.db.QueryRow(`SELECT COUNT(1) FROM pending_resolves WHERE event_id='e_new'`).Scan(&n)
	if n != 1 {
		t.Fatal("fresh pending_resolve wrongly cleaned")
	}
}
