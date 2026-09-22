package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The transport observes the real fixed GitHub URL; no production origin override.
func assetFixtureApp(t *testing.T, payload []byte, capacity int64) (*app, *atomic.Int32) {
	t.Helper()
	a := &app{cfg: config{agentVersion: "1.2.0"}, agentCache: newAgentCache(t.TempDir(), capacity)}
	calls := new(atomic.Int32)
	manifest := testAgentManifest("1.2.0", payload)
	m, _ := parseAgentManifest(manifest, "1.2.0")
	var sums strings.Builder
	for _, x := range m.Assets {
		sums.WriteString(x.SHA256 + "  " + x.Name + "\n")
	}
	a.agentHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Scheme != "https" || r.URL.Host != "github.com" || !strings.HasPrefix(r.URL.Path, "/"+agentRepo+"/releases/download/v1.2.0/") {
			t.Errorf("unexpected origin %s", r.URL.Host)
			return nil, errors.New("bad URL")
		}
		b := payload
		switch filepath.Base(r.URL.Path) {
		case "agent-manifest.json":
			b = manifest
		case "agent-SHA256SUMS.txt":
			b = []byte(sums.String())
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: int64(len(b)), Body: io.NopCloser(bytes.NewReader(b))}, nil
	})}
	return a, calls
}
func TestAgentCacheConcurrentReadAndOffline(t *testing.T) {
	payload := bytes.Repeat([]byte("executable-fixture"), 8192)
	a, calls := assetFixtureApp(t, payload, 1<<20)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, release, err := a.openAgentAsset(context.Background(), "1.2.0", "assistant-agent_1.2.0_linux_amd64")
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			b, err := io.ReadAll(f)
			if err != nil || !bytes.Equal(b, payload) {
				t.Error("invalid cached bytes")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("wanted one manifest and one binary download; got %d", calls.Load())
	}
	a.agentHTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	f, release, err := a.openAgentAsset(context.Background(), "1.2.0", "assistant-agent_1.2.0_linux_amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	b, _ := io.ReadAll(f)
	if !bytes.Equal(b, payload) {
		t.Fatal("offline cached asset corrupted")
	}
	a.agentCache.mu.Lock()
	refs := len(a.agentCache.refs)
	a.agentCache.mu.Unlock()
	if refs != 1 {
		t.Fatalf("leaked lease entries: %d", refs)
	}
}
func TestAgentCacheCorruptionAndBadDownloads(t *testing.T) {
	for _, mode := range []string{"digest", "short", "long", "metadata", "checksum"} {
		t.Run(mode, func(t *testing.T) {
			payload := []byte("agent-fixture")
			a, _ := assetFixtureApp(t, payload, 1<<20)
			original := a.agentHTTP.Transport
			a.agentHTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp, err := original.RoundTrip(r)
				if err != nil {
					return nil, err
				}
				name := filepath.Base(r.URL.Path)
				if (mode == "metadata" && name == "agent-manifest.json") || (mode == "checksum" && name == "agent-SHA256SUMS.txt") || (mode != "metadata" && mode != "checksum" && strings.HasPrefix(name, "assistant-agent_")) {
					b := bytes.Repeat([]byte("x"), len(payload))
					if mode == "short" {
						b = b[:len(b)-1]
					}
					if mode == "long" {
						b = append(b, 'x')
					}
					resp.Body.Close()
					resp.Body = io.NopCloser(bytes.NewReader(b))
					resp.ContentLength = -1
				}
				return resp, nil
			})
			asset := "assistant-agent_1.2.0_linux_amd64"
			if mode == "checksum" {
				asset = "agent-SHA256SUMS.txt"
			}
			if f, release, err := a.openAgentAsset(context.Background(), "1.2.0", asset); err == nil {
				release()
				t.Fatalf("accepted bad %s: %s", mode, f.Name())
			}
			entries, _ := os.ReadDir(a.agentCache.dir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".agent-download-") || e.Name() == "1.2.0_"+asset {
					t.Fatalf("invalid/partial asset cached: %s", e.Name())
				}
			}
		})
	}
	t.Run("damaged-cache-redownload", func(t *testing.T) {
		a, calls := assetFixtureApp(t, []byte("good"), 1<<20)
		f, release, err := a.openAgentAsset(context.Background(), "1.2.0", "assistant-agent_1.2.0_linux_amd64")
		if err != nil {
			t.Fatal(err)
		}
		p := f.Name()
		release()
		if err = os.WriteFile(p, []byte("evil"), 0600); err != nil {
			t.Fatal(err)
		}
		f, release, err = a.openAgentAsset(context.Background(), "1.2.0", "assistant-agent_1.2.0_linux_amd64")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		b, _ := io.ReadAll(f)
		if string(b) != "good" || calls.Load() != 3 {
			t.Fatalf("did not redownload bad cache: %q calls %d", b, calls.Load())
		}
	})
	t.Run("damaged-manifest-redownload", func(t *testing.T) {
		a, calls := assetFixtureApp(t, []byte("good"), 1<<20)
		if err := os.WriteFile(a.agentCache.path("1.2.0_agent-manifest.json"), []byte("bad"), 0600); err != nil {
			t.Fatal(err)
		}
		_, release, err := a.openAgentAsset(context.Background(), "1.2.0", "agent-manifest.json")
		if err != nil {
			t.Fatal(err)
		}
		release()
		if calls.Load() != 1 {
			t.Fatal("bad metadata was not repaired")
		}
	})
}
func TestAgentCacheCapacityAndLeases(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4096)
	a, _ := assetFixtureApp(t, payload, 5000)
	f, release, err := a.openAgentAsset(context.Background(), "1.2.0", "assistant-agent_1.2.0_linux_amd64")
	if err != nil {
		t.Fatal(err)
	}
	if _, closeOther, err := a.openAgentAsset(context.Background(), "1.2.0", "assistant-agent_1.2.0_linux_arm64"); err == nil {
		closeOther()
		t.Fatal("evicted active asset / exceeded capacity")
	}
	b, _ := io.ReadAll(f)
	if !bytes.Equal(b, payload) {
		t.Fatal("active lease damaged")
	}
	release()
	_, release, err = a.openAgentAsset(context.Background(), "1.2.0", "assistant-agent_1.2.0_linux_arm64")
	if err != nil {
		t.Fatal(err)
	}
	release()
	var total int64
	entries, _ := os.ReadDir(a.agentCache.dir)
	for _, e := range entries {
		info, _ := e.Info()
		total += info.Size()
	}
	if total > a.agentCache.max {
		t.Fatalf("cache capacity exceeded: %d", total)
	}
	a.agentCache.mu.Lock()
	defer a.agentCache.mu.Unlock()
	if len(a.agentCache.refs) != 0 {
		t.Fatal("lease map leaked")
	}
}
func TestAgentCacheCancellationAndStartupCleanup(t *testing.T) {
	a, _ := assetFixtureApp(t, []byte("ok"), 1<<20)
	_ = os.WriteFile(a.agentCache.path(".agent-download-stale"), []byte("partial"), 0600)
	_ = os.WriteFile(a.agentCache.path("user-file"), []byte("preserve"), 0600)
	a.agentCache.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := a.openAgentAsset(ctx, "1.2.0", "agent-manifest.json")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-a.agentCache.gate
	_, release, err := a.openAgentAsset(context.Background(), "1.2.0", "agent-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(a.agentCache.path(".agent-download-stale")); !os.IsNotExist(err) {
		t.Fatal("stale temp not removed")
	}
	if b, _ := os.ReadFile(a.agentCache.path("user-file")); string(b) != "preserve" {
		t.Fatal("unrelated file changed")
	}
}
func TestAgentDownloadRoutesAndRedirects(t *testing.T) {
	a, _ := assetFixtureApp(t, []byte("real-binary-bytes"), 1<<20)
	for _, tc := range []struct {
		version, asset string
		want           int
	}{
		{"1.2.0", "assistant-agent_1.2.0_linux_amd64", 200},
		{"1.2.0", "agent-SHA256SUMS.txt", 200},
		{"1.2.0", "assistant-agent_1.3.0_linux_amd64", 404},
		{"1.2.0", "../../secret", 404}, {"https://evil", "agent-manifest.json", 404},
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "https://hub.test/download", nil)
		req.SetPathValue("version", tc.version)
		req.SetPathValue("asset", tc.asset)
		a.handleAgentAsset(rr, req)
		if rr.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.asset, rr.Code, rr.Body.String())
		}
	}
	for _, tc := range []struct {
		url  string
		good bool
	}{
		{"https://release-assets.githubusercontent.com/x?sig=fixture&se=expiry", true},
		{"https://objects.githubusercontent.com/x?sig=fixture", true},
		{"http://release-assets.githubusercontent.com/x", false},
		{"https://evil.githubusercontent.com/x", false},
		{"https://github.com.evil/x", false}, {"https://user@github.com/x", false},
		{"https://github.com:444/x", false},
	} {
		req, _ := http.NewRequest("GET", tc.url, nil)
		err := agentRedirectAllowed(req, nil)
		if (err == nil) != tc.good {
			t.Fatalf("redirect %s: %v", tc.url, err)
		}
	}
	req, _ := http.NewRequest("GET", "https://github.com/x", nil)
	if agentRedirectAllowed(req, make([]*http.Request, 5)) == nil {
		t.Fatal("redirect loop accepted")
	}
}
func TestAgentStatusIsolationAndDisabledKeys(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	id, key := createSource(t, srv.URL, token, "device-a", "device", nil)
	idB, keyB := createSource(t, srv.URL, token, "device-b", "device", nil)
	code, _ := ingestMetrics(t, srv.URL, key, 10, map[string]any{"ts": fmtTS(time.Now())})
	if code != 202 {
		t.Fatal(code)
	}
	code, b := doJSON(t, "GET", srv.URL+"/api/v1/agent/status?source="+id, keyB, nil)
	if code != 200 || b["sourceId"] != idB || b["lastMetricsSeq"] != float64(0) {
		t.Fatalf("source isolation: %d %v", code, b)
	}
	code, _ = doJSON(t, "GET", srv.URL+"/api/v1/agent/status", token, nil)
	if code != 401 {
		t.Fatal("client token accepted")
	}
	code, _ = doJSON(t, "GET", srv.URL+"/api/v1/agent/status", "ask_invalid", nil)
	if code != 401 {
		t.Fatal(code)
	}
	_, _ = doJSON(t, "PATCH", srv.URL+"/api/v1/sources/"+id, token, map[string]any{"enabled": false})
	code, _ = doJSON(t, "GET", srv.URL+"/api/v1/agent/status", key, nil)
	if code != 403 {
		t.Fatal(code)
	}
	_, _ = doJSON(t, "DELETE", srv.URL+"/api/v1/sources/"+idB, token, nil)
	code, _ = doJSON(t, "GET", srv.URL+"/api/v1/agent/status", keyB, nil)
	if code != 401 {
		t.Fatal(code)
	}
}
