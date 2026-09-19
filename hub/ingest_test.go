package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAuthFlow 覆盖 login / session / refresh 一次性轮换 / logout。
func TestAuthFlow(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)

	// 错误密码 → 401 invalid_credentials。
	code, body := doJSON(t, "POST", srv.URL+"/api/v1/auth/login", "",
		map[string]any{"username": "admin", "password": "wrong"})
	if code != http.StatusUnauthorized {
		t.Fatalf("bad login: %d", code)
	}
	if body["error"].(map[string]any)["code"] != "invalid_credentials" {
		t.Fatalf("code: %v", body)
	}

	// 正常登录。
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/auth/login", "",
		map[string]any{"username": "admin", "password": "pw123456", "deviceLabel": "Mi 10"})
	if code != 200 {
		t.Fatalf("login: %d %v", code, body)
	}
	token := body["token"].(string)
	rtok := body["refreshToken"].(string)
	if token == "" || rtok == "" || body["expiresAt"] == nil {
		t.Fatalf("bad login resp: %v", body)
	}

	// session。
	code, body = doJSON(t, "GET", srv.URL+"/api/v1/auth/session", token, nil)
	if code != 200 || body["authenticated"] != true {
		t.Fatalf("session: %d %v", code, body)
	}

	// refresh 轮换：旧 refreshToken 一次性。
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/auth/refresh", "",
		map[string]any{"refreshToken": rtok})
	if code != 200 {
		t.Fatalf("refresh: %d %v", code, body)
	}
	newTok := body["token"].(string)
	newRtok := body["refreshToken"].(string)

	// 旧 refresh 已失效。
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/auth/refresh", "",
		map[string]any{"refreshToken": rtok})
	if code != http.StatusUnauthorized {
		t.Fatalf("reuse refresh: %d", code)
	}
	// 旧 token 也失效（同会话轮换）。
	code, _ = doJSON(t, "GET", srv.URL+"/api/v1/auth/session", token, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("old token after refresh: %d", code)
	}
	// 新 token 可用。
	code, _ = doJSON(t, "GET", srv.URL+"/api/v1/auth/session", newTok, nil)
	if code != 200 {
		t.Fatalf("new token: %d", code)
	}

	// logout → 双令牌失效。
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/auth/logout", newTok, nil)
	if code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	code, _ = doJSON(t, "GET", srv.URL+"/api/v1/auth/session", newTok, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("session after logout: %d", code)
	}
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/auth/refresh", "",
		map[string]any{"refreshToken": newRtok})
	if code != http.StatusUnauthorized {
		t.Fatalf("refresh after logout: %d", code)
	}
}

// TestSourceKeyAuth 未知 / 停用来源密钥。
func TestSourceKeyAuth(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)

	// 无密钥 → 401。
	code, _ := doJSON(t, "POST", srv.URL+"/api/v1/ingest/metrics", "",
		map[string]any{"seq": 1, "sample": map[string]any{"ts": fmtTS(nowUTC())}})
	if code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	// 未知密钥 → 401。
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/ingest/metrics", "ask_"+("0"), nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", code)
	}

	// 停用 → 403 source_disabled。
	code, _ = doJSON(t, "PATCH", srv.URL+"/api/v1/sources/"+srcID, token,
		map[string]any{"enabled": false})
	if code != 200 {
		t.Fatal("patch failed")
	}
	code, body := doJSON(t, "POST", srv.URL+"/api/v1/ingest/metrics", key,
		map[string]any{"seq": 1, "sample": map[string]any{"ts": fmtTS(nowUTC())}})
	if code != http.StatusForbidden {
		t.Fatalf("disabled: %d", code)
	}
	if body["error"].(map[string]any)["code"] != "source_disabled" {
		t.Fatalf("code: %v", body)
	}
}

// TestMetricsIngestDedup seq 幂等。
func TestMetricsIngestDedup(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "nas", "device", nil)

	sample := map[string]any{"ts": fmtTS(nowUTC()), "cpuPercent": 10.0}
	code, body := ingestMetrics(t, srv.URL, key, 1, sample)
	if code != http.StatusAccepted || body["accepted"] != true || body["duplicate"] != false {
		t.Fatalf("first: %d %v", code, body)
	}
	if body["reportIntervalSeconds"].(float64) != 30 {
		t.Fatalf("reportInterval: %v", body)
	}

	// 同 seq 重放 → duplicate。
	code, body = ingestMetrics(t, srv.URL, key, 1, sample)
	if code != http.StatusAccepted || body["duplicate"] != true || body["accepted"] != false {
		t.Fatalf("dup: %d %v", code, body)
	}
	// 更小的 seq 也 duplicate。
	code, body = ingestMetrics(t, srv.URL, key, 0, sample)
	if body["duplicate"] != true {
		t.Fatalf("older seq: %v", body)
	}
	// 更大 seq 接受。
	code, body = ingestMetrics(t, srv.URL, key, 2, sample)
	if body["accepted"] != true {
		t.Fatalf("newer seq: %v", body)
	}
}

// TestEventsDedupReplay eventId 去重重放。
func TestEventsDedupReplay(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	ev := mkEvent("fb_01", 1, "feedback_created", map[string]any{"title": "新反馈"})
	code, body := ingestEvents(t, srv.URL, key, []map[string]any{ev})
	if code != 200 {
		t.Fatalf("ingest: %d %v", code, body)
	}
	if len(body["accepted"].([]any)) != 1 {
		t.Fatalf("accepted: %v", body)
	}
	// 重放 → duplicates。
	code, body = ingestEvents(t, srv.URL, key, []map[string]any{ev})
	if len(body["duplicates"].([]any)) != 1 || len(body["accepted"].([]any)) != 0 {
		t.Fatalf("replay: %v", body)
	}
	// 消息只有一条。
	var n int
	if err := a.st.db.QueryRow(`SELECT COUNT(1) FROM messages`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("messages: %d %v", n, err)
	}
}

// TestEventsPartialReject 批次部分失败：合法落库、非法进 rejected，绝不整批 400。
func TestEventsPartialReject(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	good := mkEvent("fb_g", 1, "feedback_created", nil)
	bad := mkEvent("bad id with space!", 2, "feedback_created", nil) // eventId 非法
	badSev := mkEvent("fb_s", 3, "feedback_created", map[string]any{"severity": "fatal"})
	old := mkEvent("fb_old", 4, "feedback_created", map[string]any{
		"occurredAt": fmtTS(nowUTC().Add(-100 * 24 * time.Hour)), // 超 90 天补传窗口
	})
	code, body := ingestEvents(t, srv.URL, key, []map[string]any{good, bad, badSev, old})
	if code != 200 {
		t.Fatalf("batch: %d", code)
	}
	if len(body["accepted"].([]any)) != 1 {
		t.Fatalf("accepted: %v", body)
	}
	rej := body["rejected"].([]any)
	if len(rej) != 3 {
		t.Fatalf("rejected: %v", body)
	}
	for _, r := range rej {
		if r.(map[string]any)["code"] != "invalid_request" {
			t.Fatalf("rej code: %v", r)
		}
	}
}

// TestOutOfOrderPendingResolve 先到 resolve → pending；迟到 open（seq 更小）直接 resolved。
func TestOutOfOrderPendingResolve(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "fb", "feedback", nil)
	fk := "feedback:01TEST"

	// seq=10 resolve 先到（无任何 open）。
	resolve := mkEvent("fb_r10", 10, "feedback_recovered", map[string]any{
		"faultKey": fk, "incidentAction": "resolve", "title": "恢复"})
	code, body := ingestEvents(t, srv.URL, key, []map[string]any{resolve})
	if code != 200 || len(body["accepted"].([]any)) != 1 {
		t.Fatalf("resolve: %d %v", code, body)
	}
	// 尚无故障行。
	if f := queryFault(t, a, srcID, fk); f != nil {
		t.Fatalf("fault should not exist: %+v", f)
	}

	// seq=5 open 迟到 → 落库即 resolved（incident=1），不开通知。
	open := mkEvent("fb_o5", 5, "feedback_fault", map[string]any{
		"faultKey": fk, "incidentAction": "open", "severity": "warning", "title": "故障"})
	code, _ = ingestEvents(t, srv.URL, key, []map[string]any{open})
	if code != 200 {
		t.Fatalf("late open: %d", code)
	}
	f := queryFault(t, a, srcID, fk)
	if f == nil {
		t.Fatal("fault missing")
	}
	if f.State != "resolved" || f.Incident != 1 || f.OpenedBySeq != 5 || f.ResolvedBySeq.Int64 != 10 {
		t.Fatalf("late open state: %+v", f)
	}
	// resolve 消息已回填到该 incident。
	var cnt int
	if err := a.st.db.QueryRow(
		`SELECT COUNT(1) FROM messages WHERE fault_id=? AND incident=1`, f.ID).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 { // open + resolve 两条消息
		t.Fatalf("incident messages: %d", cnt)
	}
}

// TestFaultMergeAndIncident 同 faultKey 合并、severity max、resolve 后新一轮 incident。
func TestFaultMergeAndIncident(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "fb", "feedback", nil)
	fk := "feedback:X"

	open1 := mkEvent("e1", 1, "feedback_fault", map[string]any{
		"faultKey": fk, "incidentAction": "open", "severity": "warning", "title": "t1"})
	open2 := mkEvent("e2", 2, "feedback_fault", map[string]any{
		"faultKey": fk, "incidentAction": "open", "severity": "critical", "title": "t2"})
	upd := mkEvent("e3", 3, "feedback_fault", map[string]any{
		"faultKey": fk, "incidentAction": "update", "severity": "info", "title": "t3"})
	ingestEvents(t, srv.URL, key, []map[string]any{open1, open2, upd})

	f := queryFault(t, a, srcID, fk)
	if f == nil || f.State != "open" || f.Incident != 1 {
		t.Fatalf("fault: %+v", f)
	}
	if f.EventCount != 3 {
		t.Fatalf("eventCount: %d", f.EventCount)
	}
	if f.Severity != "critical" { // max(warning, critical, info)
		t.Fatalf("severity: %s", f.Severity)
	}
	if f.Title != "t3" { // 最新事件标题
		t.Fatalf("title: %s", f.Title)
	}

	// resolve（seq 大于 open）→ resolved。
	res := mkEvent("e4", 4, "feedback_recovered", map[string]any{
		"faultKey": fk, "incidentAction": "resolve", "title": "ok"})
	ingestEvents(t, srv.URL, key, []map[string]any{res})
	f = queryFault(t, a, srcID, fk)
	if f.State != "resolved" || f.ResolvedBySeq.Int64 != 4 {
		t.Fatalf("resolved: %+v", f)
	}

	// 再 open → 同一行 incident=2。
	open3 := mkEvent("e5", 5, "feedback_fault", map[string]any{
		"faultKey": fk, "incidentAction": "open", "severity": "warning", "title": "again"})
	ingestEvents(t, srv.URL, key, []map[string]any{open3})
	f = queryFault(t, a, srcID, fk)
	if f.State != "open" || f.Incident != 2 || f.EventCount != 1 || f.OpenedBySeq != 5 {
		t.Fatalf("reopen: %+v", f)
	}
	if f.ReadAt.Valid || f.ResolvedAt.Valid {
		t.Fatalf("reopen reset: %+v", f)
	}
}

// TestResolveDoesNotCoverNewerOpen resolve 不关闭 seq 更新的开放轮次。
func TestResolveDoesNotCoverNewerOpen(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "fb", "feedback", nil)
	fk := "feedback:Y"

	// open seq=8 先应用；resolve seq=5 后到 → 不关闭（pending）。
	open := mkEvent("o8", 8, "feedback_fault", map[string]any{
		"faultKey": fk, "incidentAction": "open", "severity": "warning"})
	res := mkEvent("r5", 5, "feedback_recovered", map[string]any{
		"faultKey": fk, "incidentAction": "resolve"})
	ingestEvents(t, srv.URL, key, []map[string]any{open, res})
	f := queryFault(t, a, srcID, fk)
	if f == nil || f.State != "open" {
		t.Fatalf("should stay open: %+v", f)
	}
	// resolve seq=12 覆盖 → resolved。
	res2 := mkEvent("r12", 12, "feedback_recovered", map[string]any{
		"faultKey": fk, "incidentAction": "resolve"})
	ingestEvents(t, srv.URL, key, []map[string]any{res2})
	f = queryFault(t, a, srcID, fk)
	if f.State != "resolved" {
		t.Fatalf("should resolve: %+v", f)
	}
}

// TestOutOfOrderFlag 迟到事件标记 outOfOrder 但仍入库。
func TestOutOfOrderFlag(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	ingestEvents(t, srv.URL, key, []map[string]any{mkEvent("a", 10, "custom", nil)})
	ingestEvents(t, srv.URL, key, []map[string]any{mkEvent("b", 3, "custom", nil)})
	var ooo int
	if err := a.st.db.QueryRow(
		`SELECT out_of_order FROM events WHERE event_id='b'`).Scan(&ooo); err != nil {
		t.Fatal(err)
	}
	if ooo != 1 {
		t.Fatal("expected out_of_order=1")
	}
	var n int
	a.st.db.QueryRow(`SELECT COUNT(1) FROM messages`).Scan(&n)
	if n != 2 {
		t.Fatalf("late event dropped: %d", n)
	}
}

// TestIndependentMessageKinds 独立消息与 custom kind。
func TestIndependentMessageKinds(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	ev := mkEvent("u1", 1, "some_new_kind", map[string]any{"severity": "warning"})
	code, body := ingestEvents(t, srv.URL, key, []map[string]any{ev})
	if code != 200 || len(body["accepted"].([]any)) != 1 {
		t.Fatalf("%d %v", code, body)
	}
	m, err := a.st.db.Query(`SELECT kind, severity, fault_id FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var kind, sev string
	var fid *string
	for m.Next() {
		m.Scan(&kind, &sev, &fid)
	}
	if kind != "some_new_kind" || sev != "warning" || fid != nil {
		t.Fatalf("custom: %v %v %v", kind, sev, fid)
	}
}

// TestOpenMessageKind 来源 open 事件消息 kind=fault_open（api-v1 §2）。
func TestOpenMessageKind(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	ev := mkEvent("k1", 1, "feedback_fault", map[string]any{
		"faultKey": "feedback:Z", "incidentAction": "open", "severity": "warning"})
	ingestEvents(t, srv.URL, key, []map[string]any{ev})
	var kind string
	if err := a.st.db.QueryRow(`SELECT kind FROM messages`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "fault_open" {
		t.Fatalf("kind: %s", kind)
	}
}

// TestHostReboot bootTime 变化 → host_reboot 独立消息。
func TestHostReboot(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "nas", "device", nil)

	s1 := map[string]any{"ts": fmtTS(nowUTC()), "bootTime": "2026-01-01T00:00:00.000Z"}
	ingestMetrics(t, srv.URL, key, 1, s1)
	var n int
	a.st.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE kind='host_reboot'`).Scan(&n)
	if n != 0 {
		t.Fatalf("first boot no event: %d", n)
	}
	s2 := map[string]any{"ts": fmtTS(nowUTC()), "bootTime": "2026-02-01T00:00:00.000Z"}
	ingestMetrics(t, srv.URL, key, 2, s2)
	a.st.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE kind='host_reboot'`).Scan(&n)
	if n != 1 {
		t.Fatalf("reboot event: %d", n)
	}
	// 相同 bootTime 不再产生。
	ingestMetrics(t, srv.URL, key, 3, s2)
	a.st.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE kind='host_reboot'`).Scan(&n)
	if n != 1 {
		t.Fatalf("reboot dup: %d", n)
	}
}

// TestBatchLimit 单批 >100 条 → 400。
func TestBatchLimit(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	var evs []map[string]any
	for i := 0; i < 101; i++ {
		evs = append(evs, mkEvent(fmt.Sprintf("e%d", i), int64(i+1), "custom", nil))
	}
	code, _ := ingestEvents(t, srv.URL, key, evs)
	if code != http.StatusBadRequest {
		t.Fatalf("over limit: %d", code)
	}
}

// TestGzipIngest Content-Encoding: gzip 正常解码（agent -gzip 选项）；
// 解压后超 256KiB → 413；未知编码 → 415；坏 gzip → 400。
func TestGzipIngest(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "nas", "device", nil)

	payload := func() map[string]any {
		return map[string]any{
			"seq":    1,
			"sentAt": fmtTS(nowUTC()),
			"agent":  map[string]any{"version": "0.1.0", "os": "linux", "arch": "amd64", "hostname": "t"},
			"sample": map[string]any{"ts": fmtTS(nowUTC()), "cpuPercent": 1.0},
		}
	}
	gzipOf := func(v any) []byte {
		raw, _ := json.Marshal(v)
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(raw)
		_ = w.Close()
		return buf.Bytes()
	}
	post := func(body []byte, encoding string) int {
		req, err := http.NewRequest("POST", srv.URL+"/api/v1/ingest/metrics", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		if encoding != "" {
			req.Header.Set("Content-Encoding", encoding)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if code := post(gzipOf(payload()), "gzip"); code != http.StatusAccepted {
		t.Fatalf("gzip ingest: %d", code)
	}
	// 解压后超 256KiB → 413（压缩再小也拦）。
	big := payload()
	big["pad"] = strings.Repeat("x", 300*1024)
	if code := post(gzipOf(big), "gzip"); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d", code)
	}
	// 未支持的编码 → 415。
	if code := post(gzipOf(payload()), "br"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("encoding: %d", code)
	}
	// 坏 gzip → 400。
	if code := post([]byte("not a gzip stream"), "gzip"); code != http.StatusBadRequest {
		t.Fatalf("bad gzip: %d", code)
	}
}

// TestHubEventSeqIsolation 中枢自产事件走独立 seq 空间：不占用来源序列、
// 不误标后续来源事件 out_of_order，ack.lastSeq 只反映来源 seq。
func TestHubEventSeqIsolation(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	_, key := createSource(t, srv.URL, token, "nas", "device", nil)

	// bootTime 变化 → 中枢自产 host_reboot 事件（旧实现会消耗来源 seq=1）。
	ingestMetrics(t, srv.URL, key, 1, map[string]any{
		"ts": fmtTS(nowUTC()), "bootTime": "2026-01-01T00:00:00.000Z"})
	ingestMetrics(t, srv.URL, key, 2, map[string]any{
		"ts": fmtTS(nowUTC()), "bootTime": "2026-02-01T00:00:00.000Z"})

	var hubSeq int64
	if err := a.st.db.QueryRow(
		`SELECT seq FROM events WHERE kind='host_reboot'`).Scan(&hubSeq); err != nil {
		t.Fatal(err)
	}
	if hubSeq < hubSeqBase {
		t.Fatalf("hub event should use disjoint seq space: %d", hubSeq)
	}

	// 来源事件 seq=1 应按序应用（旧实现此处会误标 out_of_order）。
	code, body := ingestEvents(t, srv.URL, key, []map[string]any{mkEvent("iso1", 1, "custom", nil)})
	if code != 200 {
		t.Fatalf("ingest: %d", code)
	}
	if int64(body["lastSeq"].(float64)) != 1 {
		t.Fatalf("lastSeq should be source seq 1: %v", body["lastSeq"])
	}
	var ooo int
	if err := a.st.db.QueryRow(`SELECT out_of_order FROM events WHERE event_id='iso1'`).Scan(&ooo); err != nil {
		t.Fatal(err)
	}
	if ooo != 0 {
		t.Fatal("source event wrongly marked out_of_order")
	}
}
