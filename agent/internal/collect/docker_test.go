package collect

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 起真 unix socket 假 docker API，验证 list+inspect 映射到契约字段。
func TestDockerClientContainers(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"Id": "abc123", "Names": []string{"/feedback"}, "State": "running"},
			{"Id": "def456", "Names": []string{"/kaneo"}, "State": "exited"},
		})
	})
	mux.HandleFunc("/containers/abc123/json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"Name":         "/feedback",
			"RestartCount": 2,
			"State": map[string]any{
				"Status":    "running",
				"ExitCode":  0,
				"StartedAt": "2026-09-17T10:00:00.123456789Z",
			},
		})
	})
	mux.HandleFunc("/containers/def456/json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"Name":         "/kaneo",
			"RestartCount": 1,
			"State": map[string]any{
				"Status":    "exited",
				"ExitCode":  137,
				"StartedAt": "2026-09-17T10:00:05Z",
			},
		})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	c, err := NewDockerClient("unix://" + sock)
	if err != nil {
		t.Fatalf("NewDockerClient: %v", err)
	}
	cs, err := c.Containers(context.Background())
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("containers = %+v; want 2", cs)
	}
	fb := cs[0]
	if fb.Name != "feedback" || fb.State != "running" {
		t.Fatalf("fb = %+v", fb)
	}
	if fb.ExitCode != nil {
		t.Fatalf("running container exitCode = %v; want null", *fb.ExitCode)
	}
	if fb.StartedAt == nil || fb.StartedAt.UTC().Format("2006-01-02T15:04:05") != "2026-09-17T10:00:00" {
		t.Fatalf("fb.startedAt = %v", fb.StartedAt)
	}
	if fb.RestartCount != 2 {
		t.Fatalf("fb.restartCount = %d", fb.RestartCount)
	}
	kn := cs[1]
	if kn.State != "exited" || kn.ExitCode == nil || *kn.ExitCode != 137 {
		t.Fatalf("kaneo = %+v; want exited/137", kn)
	}
}

// socket 不存在 → ErrDockerAbsent（→ unsupported）。
func TestDockerSocketAbsent(t *testing.T) {
	c, err := NewDockerClient("unix:///nonexistent-xyz/docker.sock")
	if err != nil {
		t.Fatalf("ctor err = %v; client expected (stat 在请求时做)", err)
	}
	_, err = c.Containers(context.Background())
	if !errors.Is(err, ErrDockerAbsent) {
		t.Fatalf("err = %v; want ErrDockerAbsent", err)
	}
}

// 空 host / 非法 scheme → ErrDockerAbsent 或解析错（均映射 unsupported）。
func TestDockerBadHost(t *testing.T) {
	if _, err := NewDockerClient(""); !errors.Is(err, ErrDockerAbsent) {
		t.Fatalf("empty host err = %v", err)
	}
	if _, err := NewDockerClient("npipe:////./pipe/docker"); err == nil {
		t.Fatal("unsupported scheme should error")
	}
}

// socket 存在但无服务监听 → 请求错误（→ failed）。
func TestDockerDialFailure(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(sock, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewDockerClient("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Containers(context.Background())
	if err == nil || errors.Is(err, ErrDockerAbsent) {
		t.Fatalf("err = %v; want 连接失败（→failed）", err)
	}
}

// parseDockerTime：零值时间戳 → false（缺席）。
func TestParseDockerTime(t *testing.T) {
	if _, ok := parseDockerTime("0001-01-01T00:00:00Z"); ok {
		t.Fatal("zero time should be absent")
	}
	if _, ok := parseDockerTime(""); ok {
		t.Fatal("empty should be absent")
	}
	tm, ok := parseDockerTime("2026-09-17T10:00:00.123456789Z")
	// ISOTime 按契约归一化为 UTC 毫秒精度（.123456789 → .123）。
	if !ok || !tm.Equal(time.Date(2026, 9, 17, 10, 0, 0, 123*int(time.Millisecond), time.UTC)) {
		t.Fatalf("tm = %v ok=%v", tm, ok)
	}
}
