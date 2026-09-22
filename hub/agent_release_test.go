package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func testAgentManifest(version string, payload []byte) []byte {
	h := sha256.Sum256(payload)
	return []byte(`{"schemaVersion":1,"version":"` + version + `","assets":[{"os":"linux","arch":"amd64","name":"assistant-agent_` + version + `_linux_amd64","size":` + strconv.Itoa(len(payload)) + `,"sha256":"` + hex.EncodeToString(h[:]) + `"},{"os":"linux","arch":"arm64","name":"assistant-agent_` + version + `_linux_arm64","size":` + strconv.Itoa(len(payload)) + `,"sha256":"` + hex.EncodeToString(h[:]) + `"}]}`)
}

func TestAgentManifestAndAssetValidation(t *testing.T) {
	b := []byte("agent")
	if _, err := parseAgentManifest(testAgentManifest("1.2.0", b), "1.2.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := parseAgentManifest([]byte(`{"schemaVersion":1,"version":"1.2.0","assets":[]}`), "1.2.0"); err == nil {
		t.Fatal("empty manifest accepted")
	}
	if validAgentAssetName("../secret") || validAgentAssetName("assistant-agent_1.2_linux_amd64") {
		t.Fatal("invalid asset accepted")
	}
}

func TestAgentInstallBlockDoesNotLeakKey(t *testing.T) {
	a := &app{cfg: config{baseURL: "https://hub.example"}}
	b := a.installBlock("ask_secret", "device", false)
	if b["command"] == "" || strings.Contains(b["command"].(string), "ask_secret") {
		t.Fatal("key leaked")
	}
	rot := a.installBlock("ask_secret", "device", true)["command"].(string)
	if !strings.Contains(rot, "--reconfigure") {
		t.Fatal("rotation command missing reconfigure")
	}
	if a.installBlock("ask_secret", "feedback", false)["command"] != "" {
		t.Fatal("feedback command generated")
	}
}

func TestAgentStatusAuthNoHeartbeatSideEffect(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	tok := login(t, srv.URL)
	id, key := createSource(t, srv.URL, tok, "dev", "device", nil)
	code, body := doJSON(t, "GET", srv.URL+"/api/v1/agent/status", key, nil)
	if code != 200 || body["sourceId"] != id || body["lastMetricsSeq"].(float64) != 0 {
		t.Fatalf("status: %d %v", code, body)
	}
	var seen string
	_ = a.st.db.QueryRow(`SELECT COALESCE(last_seen_at,'') FROM sources WHERE id=?`, id).Scan(&seen)
	if seen != "" {
		t.Fatal("status changed last_seen_at")
	}
	fbID, fbKey := createSource(t, srv.URL, tok, "fb", "feedback", nil)
	_ = fbID
	code, _ = doJSON(t, "GET", srv.URL+"/api/v1/agent/status", fbKey, nil)
	if code != 403 {
		t.Fatalf("feedback status: %d", code)
	}
}

func TestAgentReleaseBadManifestNotCached(t *testing.T) {
	a := newTestApp(t)
	dir := filepath.Join(t.TempDir(), "cache")
	a.cfg.agentCacheDir = dir
	a.agentCache = newAgentCache(dir, 1<<20)
	var calls atomic.Int32
	a.agentHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := io.NopCloser(strings.NewReader(`{"schemaVersion":1,"version":"1.2.0","assets":[]}`))
		return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
	})}
	if _, _, err := a.openAgentAsset(context.Background(), "1.2.0", "agent-manifest.json"); err == nil {
		t.Fatal("bad manifest accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "1.2.0_agent-manifest.json")); !os.IsNotExist(err) {
		t.Fatal("bad manifest cached")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAgentInstallScriptEmbedded(t *testing.T) {
	a := newTestApp(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/install.sh", nil)
	a.handleAgentInstallScript(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "assistant-agent") {
		t.Fatalf("script: %d", rr.Code)
	}
}
