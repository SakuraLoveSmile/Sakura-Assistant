package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fbUpstream 记录回连请求的 fake Feedback 上游。
type fbUpstream struct {
	srv *httptest.Server

	mu      sync.Mutex
	auths   []string
	paths   []string
	queries []string
	bodies  []string
}

func newFBUpstream(t *testing.T, handler http.HandlerFunc) *fbUpstream {
	t.Helper()
	u := &fbUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		u.paths = append(u.paths, r.URL.Path)
		u.queries = append(u.queries, r.URL.RawQuery)
		u.bodies = append(u.bodies, string(b))
		u.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fbUpstream) hits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.paths)
}
func (u *fbUpstream) lastAuth() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.auths[len(u.auths)-1]
}
func (u *fbUpstream) lastPath() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.paths[len(u.paths)-1]
}
func (u *fbUpstream) lastQuery() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.queries[len(u.queries)-1]
}
func (u *fbUpstream) lastBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[len(u.bodies)-1]
}

// createFeedbackSource 创建 feedback 来源（可带 attachmentBaseUrl / mgmtKey），返回 (srcID, accessKey)。
func createFeedbackSource(t *testing.T, base, token string, baseURL, mgmtKey *string) (string, string) {
	t.Helper()
	req := map[string]any{"name": "fb", "kind": "feedback"}
	if baseURL != nil {
		req["attachmentBaseUrl"] = *baseURL
	}
	if mgmtKey != nil {
		req["mgmtKey"] = *mgmtKey
	}
	code, body := doJSON(t, "POST", base+"/api/v1/sources", token, req)
	if code != 201 {
		t.Fatalf("createFeedbackSource: %d %v", code, body)
	}
	return body["source"].(map[string]any)["id"].(string), body["accessKey"].(string)
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// getRaw 发 GET 请求并返回状态码/标头/原始体（附件等非 JSON 端点用）。
func getRaw(t *testing.T, url, token string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// ---- 来源 mgmtKey 生命周期 ----

// TestFeedbackMgmtKeySourceOps mgmtKey 的创建 / PATCH / 轮换 / 删除清理 / device 拒绝。
func TestFeedbackMgmtKeySourceOps(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	// device 来源传 mgmtKey → 400（含显式 null：字段不适用于 device）。
	code, body := doJSON(t, "POST", srv.URL+"/api/v1/sources", token,
		map[string]any{"name": "d", "kind": "device", "mgmtKey": "amk_x"})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("device mgmtKey: %d %v", code, body)
	}
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/sources", token,
		map[string]any{"name": "d", "kind": "device", "mgmtKey": nil})
	if code != 400 {
		t.Fatalf("device null mgmtKey: %d", code)
	}
	// feedback 创建带 mgmtKey → 201，hint 为末 4 位且不回显明文。
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/sources", token,
		map[string]any{"name": "fb", "kind": "feedback", "mgmtKey": "amk_abcd1234"})
	if code != 201 {
		t.Fatalf("create: %d %v", code, body)
	}
	src := body["source"].(map[string]any)
	srcID := src["id"].(string)
	if src["mgmtKeyHint"] != "1234" {
		t.Fatalf("hint: %v", src["mgmtKeyHint"])
	}
	if _, ok := src["mgmtKey"]; ok {
		t.Fatal("mgmtKey 明文泄露到 source 对象")
	}
	// 超长 / 空串 → 400。
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/sources", token,
		map[string]any{"name": "fb2", "kind": "feedback", "mgmtKey": strings.Repeat("x", 201)})
	if code != 400 {
		t.Fatalf("long mgmtKey: %d", code)
	}
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/sources", token,
		map[string]any{"name": "fb2", "kind": "feedback", "mgmtKey": ""})
	if code != 400 {
		t.Fatalf("empty mgmtKey: %d", code)
	}
	// 未配置 mgmtKey 的 feedback 来源 mgmtKeyHint 为 null。
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/sources", token,
		map[string]any{"name": "fb3", "kind": "feedback"})
	if code != 201 || body["source"].(map[string]any)["mgmtKeyHint"] != nil {
		t.Fatalf("default hint: %d %v", code, body)
	}

	// PATCH 直写 + null 清除 + device 拒绝。
	code, body = doJSON(t, "PATCH", srv.URL+"/api/v1/sources/"+srcID, token,
		map[string]any{"mgmtKey": "amk_zzzz9999"})
	if code != 200 || body["source"].(map[string]any)["mgmtKeyHint"] != "9999" {
		t.Fatalf("patch set: %d %v", code, body)
	}
	code, body = doJSON(t, "PATCH", srv.URL+"/api/v1/sources/"+srcID, token,
		map[string]any{"mgmtKey": nil})
	if code != 200 || body["source"].(map[string]any)["mgmtKeyHint"] != nil {
		t.Fatalf("patch clear: %d %v", code, body)
	}
	code, _ = doJSON(t, "PATCH", srv.URL+"/api/v1/sources/"+srcID, token,
		map[string]any{"mgmtKey": ""})
	if code != 400 {
		t.Fatalf("patch empty: %d", code)
	}
	devID, _ := createSource(t, srv.URL, token, "dev", "device", nil)
	code, _ = doJSON(t, "PATCH", srv.URL+"/api/v1/sources/"+devID, token,
		map[string]any{"mgmtKey": "amk_x"})
	if code != 400 {
		t.Fatalf("device patch: %d", code)
	}

	// rotate-mgmt-key：feedback → 200 amk_<48hex> + install.env；device / 不存在 → 404。
	code, body = doJSON(t, "POST", srv.URL+"/api/v1/sources/"+srcID+"/rotate-mgmt-key", token, nil)
	if code != 200 {
		t.Fatalf("rotate: %d %v", code, body)
	}
	mk := body["mgmtKey"].(string)
	if !strings.HasPrefix(mk, "amk_") || len(mk) != 4+48 {
		t.Fatalf("mgmtKey format: %q", mk)
	}
	env := body["install"].(map[string]any)["env"].(map[string]any)
	if env["FEEDBACK_ASSIST_MGMT_KEY"] != mk {
		t.Fatalf("install env: %v", env)
	}
	if body["install"].(map[string]any)["note"] == "" {
		t.Fatal("install note empty")
	}
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/sources/"+devID+"/rotate-mgmt-key", token, nil)
	if code != 404 {
		t.Fatalf("device rotate: %d", code)
	}
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/sources/src_none/rotate-mgmt-key", token, nil)
	if code != 404 {
		t.Fatalf("missing rotate: %d", code)
	}
	// 轮换后库内明文/hint 更新、source 对象只回显 hint。
	var plain, hint string
	if err := a.st.db.QueryRow(
		`SELECT mgmt_key_plain, mgmt_key_hint FROM sources WHERE id=?`, srcID,
	).Scan(&plain, &hint); err != nil {
		t.Fatal(err)
	}
	if plain != mk || hint != mk[len(mk)-4:] {
		t.Fatalf("db mgmt: %q %q", plain, hint)
	}
	code, body = doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID, token, nil)
	if body["source"].(map[string]any)["mgmtKeyHint"] != hint {
		t.Fatalf("get hint: %v", body)
	}

	// DELETE 清空两列。
	code, _ = doJSON(t, "DELETE", srv.URL+"/api/v1/sources/"+srcID, token, nil)
	if code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if err := a.st.db.QueryRow(
		`SELECT mgmt_key_plain, mgmt_key_hint FROM sources WHERE id=?`, srcID,
	).Scan(&plain, &hint); err != nil {
		t.Fatal(err)
	}
	if plain != "" || hint != "" {
		t.Fatalf("delete not clearing mgmt: %q %q", plain, hint)
	}
	// 已删来源 rotate → 404。
	code, _ = doJSON(t, "POST", srv.URL+"/api/v1/sources/"+srcID+"/rotate-mgmt-key", token, nil)
	if code != 404 {
		t.Fatalf("deleted rotate: %d", code)
	}
}

// TestSourcesMigrationV11 老库（无 mgmt 列）经 openDB 幂等补列；新库重复打开幂等。
func TestSourcesMigrationV11(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "old.db")

	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sources(
	  id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL,
	  enabled INTEGER NOT NULL DEFAULT 1, key_hash TEXT NOT NULL,
	  key_plain TEXT NOT NULL DEFAULT '', key_hint TEXT NOT NULL DEFAULT '',
	  created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO sources(id,name,kind,enabled,key_hash,created_at,updated_at)
		 VALUES('src_old','old','feedback',1,'h','t','t')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := openDB(p)
	if err != nil {
		t.Fatalf("openDB migrate: %v", err)
	}
	var n int
	if err := db2.QueryRow(
		`SELECT COUNT(1) FROM pragma_table_info('sources') WHERE name LIKE 'mgmt_key_%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("mgmt cols: %d", n)
	}
	// 既有行默认空串；新列可写可读。
	var plain string
	if err := db2.QueryRow(`SELECT mgmt_key_plain FROM sources WHERE id='src_old'`).Scan(&plain); err != nil {
		t.Fatal(err)
	}
	if plain != "" {
		t.Fatalf("default: %q", plain)
	}
	if _, err := db2.Exec(`UPDATE sources SET mgmt_key_plain='amk_x', mgmt_key_hint='k_x' WHERE id='src_old'`); err != nil {
		t.Fatal(err)
	}
	db2.Close()

	// 再次打开幂等不报错、数据保留。
	db3, err := openDB(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db3.Close()
	if err := db3.QueryRow(`SELECT mgmt_key_plain FROM sources WHERE id='src_old'`).Scan(&plain); err != nil {
		t.Fatal(err)
	}
	if plain != "amk_x" {
		t.Fatalf("reopen lost: %q", plain)
	}
}

// ---- §3.1 前置条件矩阵 ----

// TestFeedbackPreconditions 来源不存在 / device / 缺 base / 缺 mgmtKey 的 404/409 判定。
func TestFeedbackPreconditions(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	up := newFBUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"FB1","capabilities":{"manage":true}}`)
	})

	devID, _ := createSource(t, srv.URL, token, "dev", "device", nil)
	noBaseID, _ := createFeedbackSource(t, srv.URL, token, nil, nil)
	noMgmtID, accessKey := createFeedbackSource(t, srv.URL, token, &up.srv.URL, nil)

	type tc struct {
		name     string
		method   string
		path     string
		wantCode int
		wantErr  string
	}
	cases := []tc{
		{"missing list", "GET", "/api/v1/sources/src_none/feedback", 404, "not_found"},
		{"missing detail", "GET", "/api/v1/sources/src_none/feedback/FB1", 404, "not_found"},
		{"missing action", "POST", "/api/v1/sources/src_none/feedback/FB1/action", 404, "not_found"},
		{"missing att", "GET", "/api/v1/sources/src_none/feedback/FB1/attachments/screenshot", 404, "not_found"},
		{"device list", "GET", "/api/v1/sources/" + devID + "/feedback", 404, "not_found"},
		{"device detail", "GET", "/api/v1/sources/" + devID + "/feedback/FB1", 404, "not_found"},
		{"device action", "POST", "/api/v1/sources/" + devID + "/feedback/FB1/action", 404, "not_found"},
		{"device att", "GET", "/api/v1/sources/" + devID + "/feedback/FB1/attachments/screenshot", 404, "not_found"},
		{"nobase list", "GET", "/api/v1/sources/" + noBaseID + "/feedback", 409, "feedback_upstream_not_configured"},
		{"nobase detail", "GET", "/api/v1/sources/" + noBaseID + "/feedback/FB1", 409, "feedback_upstream_not_configured"},
		{"nobase action", "POST", "/api/v1/sources/" + noBaseID + "/feedback/FB1/action", 409, "feedback_upstream_not_configured"},
		{"nobase att", "GET", "/api/v1/sources/" + noBaseID + "/feedback/FB1/attachments/screenshot", 409, "feedback_upstream_not_configured"},
		{"nomgmt list", "GET", "/api/v1/sources/" + noMgmtID + "/feedback", 409, "feedback_mgmt_not_configured"},
		{"nomgmt action", "POST", "/api/v1/sources/" + noMgmtID + "/feedback/FB1/action", 409, "feedback_mgmt_not_configured"},
	}
	for _, c := range cases {
		code, body := doJSON(t, c.method, srv.URL+c.path, token,
			map[string]any{"requestId": "r1", "action": "archive", "expectedLifecycleVersion": 0})
		if code != c.wantCode || errCode(body) != c.wantErr {
			t.Fatalf("%s: got %d %v, want %d %s", c.name, code, body, c.wantCode, c.wantErr)
		}
	}
	// 无 mgmtKey 时 detail 仍可用（只读凭证）——上游 manage=true × 无 mgmtKey → false。
	code, body := doJSON(t, "GET", srv.URL+"/api/v1/sources/"+noMgmtID+"/feedback/FB1", token, nil)
	if code != 200 {
		t.Fatalf("nomgmt detail: %d %v", code, body)
	}
	if up.lastAuth() != "Bearer "+accessKey {
		t.Fatalf("detail auth: %q", up.lastAuth())
	}
	caps := body["capabilities"].(map[string]any)
	if caps["manage"] != false {
		t.Fatalf("manage should be false: %v", caps)
	}
	if up.hits() == 0 {
		t.Fatal("no upstream hit")
	}
}

// ---- 列表 ----

// TestFeedbackListProxy 列表透传：mgmt 凭证、query 白名单、sourceId/sourceName 注入。
func TestFeedbackListProxy(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	up := newFBUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"items":[{"id":"fb1","title":"t"}],"nextCursor":"c2","counts":{"inbox":1}}`)
	})
	mgmt := "amk_mgmtAAA"
	srcID, accessKey := createFeedbackSource(t, srv.URL, token, &up.srv.URL, &mgmt)

	code, body := doJSON(t, "GET",
		srv.URL+"/api/v1/sources/"+srcID+"/feedback?view=inbox&limit=10&q=hello&cursor=cur1&bogus=1",
		token, nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	if up.lastAuth() != "Bearer "+mgmt {
		t.Fatalf("list auth=%q want mgmt（access=%q 不得混用）", up.lastAuth(), accessKey)
	}
	if up.lastPath() != "/api/assist/manage/feedback" {
		t.Fatalf("path: %q", up.lastPath())
	}
	q := up.lastQuery()
	for _, want := range []string{"view=inbox", "limit=10", "q=hello", "cursor=cur1"} {
		if !strings.Contains(q, want) {
			t.Fatalf("query %q missing %q", q, want)
		}
	}
	if strings.Contains(q, "bogus") {
		t.Fatalf("非白名单参数被透传: %q", q)
	}
	if body["sourceId"] != srcID || body["sourceName"] != "fb" {
		t.Fatalf("inject: %v", body)
	}
	if len(body["items"].([]any)) != 1 || body["nextCursor"] != "c2" {
		t.Fatalf("list body: %v", body)
	}
}

// TestFeedbackListUpstream404 列表上游 404 → 409 feedback_mgmt_unsupported。
func TestFeedbackListUpstream404(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	up := newFBUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	mgmt := "amk_x"
	srcID, _ := createFeedbackSource(t, srv.URL, token, &up.srv.URL, &mgmt)
	code, body := doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID+"/feedback", token, nil)
	if code != 409 || errCode(body) != "feedback_mgmt_unsupported" {
		t.Fatalf("list 404: %d %v", code, body)
	}
}

// ---- 详情 ----

// TestFeedbackDetailProxy 详情透传：只读凭证 + capabilities.manage 重算 + fbId 校验。
func TestFeedbackDetailProxy(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	up := newFBUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/assist/feedback/FB1":
			io.WriteString(w, `{"id":"FB1","status":"archived","capabilities":{"manage":true,"other":1}}`)
		case "/api/assist/feedback/FB2":
			io.WriteString(w, `{"id":"FB2"}`) // 旧版服务端：无 capabilities
		case "/api/assist/feedback/FB3":
			io.WriteString(w, `{"id":"FB3","capabilities":{"manage":false}}`)
		default:
			w.WriteHeader(404)
		}
	})
	mgmt := "amk_mgmtX"
	srcID, accessKey := createFeedbackSource(t, srv.URL, token, &up.srv.URL, &mgmt)

	// 上游 manage=true × 本地有 mgmtKey → true。
	code, body := doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID+"/feedback/FB1", token, nil)
	if code != 200 {
		t.Fatalf("detail: %d %v", code, body)
	}
	if up.lastAuth() != "Bearer "+accessKey {
		t.Fatalf("detail auth=%q want access key", up.lastAuth())
	}
	if body["sourceId"] != srcID || body["sourceName"] != "fb" {
		t.Fatalf("inject: %v", body)
	}
	caps := body["capabilities"].(map[string]any)
	if caps["manage"] != true || caps["other"].(float64) != 1 {
		t.Fatalf("caps: %v", caps)
	}
	// 字段缺席 → manage=false。
	code, body = doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID+"/feedback/FB2", token, nil)
	if code != 200 || body["capabilities"].(map[string]any)["manage"] != false {
		t.Fatalf("absent caps: %d %v", code, body)
	}
	// 上游 false → false。
	code, body = doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID+"/feedback/FB3", token, nil)
	if code != 200 || body["capabilities"].(map[string]any)["manage"] != false {
		t.Fatalf("false caps: %d %v", code, body)
	}
	// PATCH 清掉 mgmtKey → 上游 true 也被重算为 false。
	code, _ = doJSON(t, "PATCH", srv.URL+"/api/v1/sources/"+srcID, token,
		map[string]any{"mgmtKey": nil})
	if code != 200 {
		t.Fatal(code)
	}
	code, body = doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID+"/feedback/FB1", token, nil)
	if code != 200 || body["capabilities"].(map[string]any)["manage"] != false {
		t.Fatalf("no-mgmt caps: %d %v", code, body)
	}
	// 非法 fbId → 404 且不回连。
	before := up.hits()
	for _, bad := range []string{"fb$id", "fb..id", strings.Repeat("x", 129)} {
		code, _ = doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID+"/feedback/"+bad, token, nil)
		if code != 404 {
			t.Fatalf("bad fbId %q: %d", bad, code)
		}
	}
	if up.hits() != before {
		t.Fatal("非法 fbId 触发回连")
	}
	// 上游 404 → 404 not_found。
	code, body = doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID+"/feedback/GONE", token, nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("upstream 404: %d %v", code, body)
	}
}

// ---- 操作 ----

// TestFeedbackActionValidation 请求体形状校验失败不发上游。
func TestFeedbackActionValidation(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	up := newFBUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	})
	mgmt := "amk_m"
	srcID, _ := createFeedbackSource(t, srv.URL, token, &up.srv.URL, &mgmt)
	url := srv.URL + "/api/v1/sources/" + srcID + "/feedback/FB1/action"

	bad := []map[string]any{
		{}, // 缺 requestId
		{"requestId": "", "action": "archive", "expectedLifecycleVersion": 0},
		{"requestId": "bad id!", "action": "archive", "expectedLifecycleVersion": 0},
		{"requestId": strings.Repeat("r", 129), "action": "archive", "expectedLifecycleVersion": 0},
		{"requestId": "r1"},                      // 缺 action
		{"requestId": "r1", "action": "purge"},   // 非法 action
		{"requestId": "r1", "action": "archive"}, // 缺 expectedLifecycleVersion
		{"requestId": "r1", "action": "archive", "expectedLifecycleVersion": -1},
		{"requestId": "r1", "action": "archive", "expectedLifecycleVersion": "3"},
		{"requestId": "r1", "action": "trash", "expectedLifecycleVersion": 1.5},
		{"requestId": "r1", "action": "retry"}, // 缺 expectedRevision
		{"requestId": "r1", "action": "retry", "expectedRevision": -2},
		{"requestId": "r1", "action": "recheck", "expectedLifecycleVersion": 0},
	}
	for i, payload := range bad {
		code, body := doJSON(t, "POST", url, token, payload)
		if code != 400 || errCode(body) != "invalid_request" {
			t.Fatalf("case %d: %d %v", i, code, body)
		}
	}
	if up.hits() != 0 {
		t.Fatalf("非法请求触发回连 %d 次", up.hits())
	}
	// 非法 fbId → 404 不发上游。
	code, _ := doJSON(t, "POST",
		srv.URL+"/api/v1/sources/"+srcID+"/feedback/fb$id/action", token,
		map[string]any{"requestId": "r1", "action": "archive", "expectedLifecycleVersion": 0})
	if code != 404 || up.hits() != 0 {
		t.Fatalf("bad fbId action: %d hits=%d", code, up.hits())
	}
}

// TestFeedbackActionProxy 操作透传：mgmt 凭证 + 成功/冲突/回探 404/限流。
func TestFeedbackActionProxy(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	up := newFBUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/assist/manage/feedback/OK1/action":
			io.WriteString(w, `{"ok":true,"action":"archive","replayed":false,"detail":{"id":"OK1","mgmtState":"archived"}}`)
		case "/api/assist/manage/feedback/VC1/action":
			w.WriteHeader(409)
			io.WriteString(w, `{"error":{"code":"version_conflict","message":"v mismatch"},"detail":{"lifecycleVersion":3}}`)
		case "/api/assist/manage/feedback/BUSY1/action":
			w.WriteHeader(409)
			io.WriteString(w, `{"error":{"code":"busy","message":"processing"}}`)
		case "/api/assist/manage/feedback/RID1/action":
			w.WriteHeader(409)
			io.WriteString(w, `{"error":{"code":"request_id_conflict","message":"dup"}}`)
		case "/api/assist/manage/feedback/OLD1/action", "/api/assist/manage/feedback/GONE1/action", "/api/assist/manage/feedback/WEIRD1/action":
			w.WriteHeader(404)
		case "/api/assist/feedback/OLD1":
			io.WriteString(w, `{"id":"OLD1","status":"failed"}`) // 详情在但无 manage → unsupported
		case "/api/assist/feedback/WEIRD1":
			io.WriteString(w, `{"id":"WEIRD1","capabilities":{"manage":true}}`) // manage=true 仍 404 → not_found
		case "/api/assist/feedback/GONE1":
			w.WriteHeader(404)
		case "/api/assist/manage/feedback/E401/action":
			w.WriteHeader(401)
		case "/api/assist/manage/feedback/E500/action":
			w.WriteHeader(500)
		case "/api/assist/manage/feedback/R429/action":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
		case "/api/assist/manage/feedback/E400UNK/action":
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"weird_code","message":"?"}}`)
		default:
			w.WriteHeader(404)
		}
	})
	mgmt := "amk_mgmtSECRET"
	srcID, accessKey := createFeedbackSource(t, srv.URL, token, &up.srv.URL, &mgmt)
	post := func(fb string, payload map[string]any) (int, map[string]any) {
		if payload == nil {
			payload = map[string]any{
				"requestId": "rop_t1", "action": "archive", "expectedLifecycleVersion": 0}
		}
		return doJSON(t, "POST",
			srv.URL+"/api/v1/sources/"+srcID+"/feedback/"+fb+"/action", token, payload)
	}

	// 200 透传 + mgmt 凭证。
	code, body := post("OK1", nil)
	if code != 200 || body["ok"] != true || body["action"] != "archive" {
		t.Fatalf("ok: %d %v", code, body)
	}
	if up.lastAuth() != "Bearer "+mgmt {
		t.Fatalf("action auth=%q（access=%q 不得混用）", up.lastAuth(), accessKey)
	}
	if up.lastPath() != "/api/assist/manage/feedback/OK1/action" {
		t.Fatalf("path: %q", up.lastPath())
	}
	// 请求体透传（含 requestId）。
	var sent map[string]any
	json.Unmarshal([]byte(up.lastBody()), &sent)
	if sent["requestId"] != "rop_t1" || sent["action"] != "archive" {
		t.Fatalf("forwarded body: %s", up.lastBody())
	}
	// retry + expectedRevision 合法路径。
	code, _ = post("OK1", map[string]any{
		"requestId": "rop_t2", "action": "retry", "expectedRevision": 2})
	if code != 200 {
		t.Fatalf("retry: %d", code)
	}
	// 冲突类 code 原样透传（含 detail）。
	code, body = post("VC1", nil)
	if code != 409 || errCode(body) != "version_conflict" {
		t.Fatalf("version_conflict: %d %v", code, body)
	}
	if body["detail"].(map[string]any)["lifecycleVersion"].(float64) != 3 {
		t.Fatalf("detail 未透传: %v", body)
	}
	for fb, want := range map[string]string{
		"BUSY1": "busy", "RID1": "request_id_conflict",
	} {
		code, body = post(fb, nil)
		if code != 409 || errCode(body) != want {
			t.Fatalf("%s: %d %v", want, code, body)
		}
	}
	// 上游 404 → 回探详情三态。
	code, body = post("GONE1", nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("gone: %d %v", code, body)
	}
	if up.lastAuth() != "Bearer "+accessKey || up.lastPath() != "/api/assist/feedback/GONE1" {
		t.Fatalf("probe should use access key: %q %q", up.lastAuth(), up.lastPath())
	}
	code, body = post("OLD1", nil)
	if code != 409 || errCode(body) != "feedback_mgmt_unsupported" {
		t.Fatalf("old server: %d %v", code, body)
	}
	code, body = post("WEIRD1", nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("weird: %d %v", code, body)
	}
	// 401 / 500 / 未知 4xx → 502 feedback_unavailable。
	for _, fb := range []string{"E401", "E500", "E400UNK"} {
		code, body = post(fb, nil)
		if code != 502 || errCode(body) != "feedback_unavailable" {
			t.Fatalf("%s: %d %v", fb, code, body)
		}
	}
	// 429 → rate_limited + Retry-After 透传。
	req, _ := http.NewRequest("POST",
		srv.URL+"/api/v1/sources/"+srcID+"/feedback/R429/action",
		strings.NewReader(`{"requestId":"r1","action":"archive","expectedLifecycleVersion":0}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "7" {
		t.Fatalf("429: %d ra=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	var rb map[string]any
	json.NewDecoder(resp.Body).Decode(&rb)
	if errCode(rb) != "rate_limited" {
		t.Fatalf("429 body: %v", rb)
	}
}

// TestFeedbackUpstreamUnreachable 上游不可达 / 超时 → 502 feedback_unavailable。
func TestFeedbackUpstreamUnreachable(t *testing.T) {
	a := newTestApp(t)
	a.httpClientOp = &http.Client{Timeout: 50 * time.Millisecond}
	srv := testServer(t, a)
	token := login(t, srv.URL)

	// 已关闭的上游 → 网络错误。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	mgmt := "amk_x"
	srcID, _ := createFeedbackSource(t, srv.URL, token, &deadURL, &mgmt)
	code, body := doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID+"/feedback", token, nil)
	if code != 502 || errCode(body) != "feedback_unavailable" {
		t.Fatalf("dead upstream: %d %v", code, body)
	}

	// 慢上游 → 超时。
	slow := newFBUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, `{"items":[]}`)
	})
	srcID2, _ := createFeedbackSource(t, srv.URL, token, &slow.srv.URL, &mgmt)
	code, body = doJSON(t, "GET", srv.URL+"/api/v1/sources/"+srcID2+"/feedback", token, nil)
	if code != 502 || errCode(body) != "feedback_unavailable" {
		t.Fatalf("slow upstream: %d %v", code, body)
	}
}

// ---- 附件 ----

// TestFeedbackAttachmentProxy 附件透传：只读凭证、标头、attId 白名单、错误映射。
func TestFeedbackAttachmentProxy(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	up := newFBUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/assist/feedback/FB9/attachments/screenshot":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("ETag", `"sha1"`)
			w.Write([]byte("PNGDATA"))
		case "/api/assist/feedback/FB9/attachments/logs/L1":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="a.log"`)
			w.Write([]byte("LOGDATA"))
		case "/api/assist/feedback/FB9/attachments/logs/E500":
			w.WriteHeader(500)
		default:
			w.WriteHeader(404)
		}
	})
	srcID, accessKey := createFeedbackSource(t, srv.URL, token, &up.srv.URL, nil)
	att := func(fb, attID string) (int, http.Header, []byte) {
		return getRaw(t, fmt.Sprintf("%s/api/v1/sources/%s/feedback/%s/attachments/%s",
			srv.URL, srcID, fb, attID), token)
	}

	code, hdr, body := att("FB9", "screenshot")
	if code != 200 || string(body) != "PNGDATA" {
		t.Fatalf("screenshot: %d %q", code, body)
	}
	if up.lastAuth() != "Bearer "+accessKey {
		t.Fatalf("att auth=%q want access key", up.lastAuth())
	}
	if hdr.Get("Content-Type") != "image/png" || hdr.Get("ETag") != `"sha1"` ||
		hdr.Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("headers: %v", hdr)
	}
	code, hdr, body = att("FB9", "logs/L1")
	if code != 200 || string(body) != "LOGDATA" ||
		hdr.Get("Content-Disposition") != `attachment; filename="a.log"` {
		t.Fatalf("logs: %d %q %v", code, body, hdr)
	}
	// 非法 attId / fbId → 404 不回连。
	before := up.hits()
	for _, bad := range []string{"nope", "logs/a.b", "logs/", "screenshot/x"} {
		if c, _, _ := att("FB9", bad); c != 404 {
			t.Fatalf("bad attId %q: %d", bad, c)
		}
	}
	if c, _, _ := att("fb$id", "screenshot"); c != 404 {
		t.Fatalf("bad fbId att: %d", c)
	}
	if up.hits() != before {
		t.Fatal("非法 attId/fbId 触发回连")
	}
	// 上游 404 → 404；5xx → 502 attachment_unavailable。
	code, _, body = att("FB9", "logs/MISSING")
	if code != 404 {
		t.Fatalf("upstream 404: %d", code)
	}
	code, _, body = att("FB9", "logs/E500")
	if code != 502 {
		t.Fatalf("upstream 500: %d", code)
	}
	var m map[string]any
	json.Unmarshal(body, &m)
	if errCode(m) != "attachment_unavailable" {
		t.Fatalf("att err: %s", body)
	}
}

// ---- 跨来源隔离 ----

// TestFeedbackCrossSourceIsolation 来源 B 的请求绝不回连来源 A 的上游。
func TestFeedbackCrossSourceIsolation(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)

	upA := newFBUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"SHARED"}`)
	})
	upB := newFBUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	srcA, keyA := createFeedbackSource(t, srv.URL, token, &upA.srv.URL, nil)
	srcB, _ := createFeedbackSource(t, srv.URL, token, &upB.srv.URL, nil)

	// 经 B 访问 → 只打 B 的上游（同 fbId 在 B 侧 404 → 中枢 404）。
	code, _ := doJSON(t, "GET",
		srv.URL+"/api/v1/sources/"+srcB+"/feedback/SHARED", token, nil)
	if code != 404 {
		t.Fatalf("via B: %d", code)
	}
	if upB.hits() != 1 || upA.hits() != 0 {
		t.Fatalf("isolation: A=%d B=%d", upA.hits(), upB.hits())
	}
	if upB.lastAuth() == "Bearer "+keyA {
		t.Fatal("B 请求用了 A 的凭证")
	}
	// 经 A 访问 → 只打 A。
	code, body := doJSON(t, "GET",
		srv.URL+"/api/v1/sources/"+srcA+"/feedback/SHARED", token, nil)
	if code != 200 || body["id"] != "SHARED" || body["sourceId"] != srcA {
		t.Fatalf("via A: %d %v", code, body)
	}
	if upA.hits() != 1 || upB.hits() != 1 {
		t.Fatalf("isolation2: A=%d B=%d", upA.hits(), upB.hits())
	}
	if upA.lastAuth() != "Bearer "+keyA {
		t.Fatalf("A auth: %q", upA.lastAuth())
	}
}
