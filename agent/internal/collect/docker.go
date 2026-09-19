package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrDockerAbsent 表示 docker 端点不存在/未配置 → capabilities.containers=unsupported。
// 端点存在但请求失败 → failed（由调用方映射）。
var ErrDockerAbsent = errors.New("docker endpoint absent")

// DockerClient 通过 docker.sock（unix）或 tcp/http 调用 Docker Engine API。
type DockerClient struct {
	base     string // http(s) 端点（unix 时为占位 host）
	sockPath string // unix socket 路径（非 unix 时为空）
	hc       *http.Client
	stat     func(string) (os.FileInfo, error) // 测试注入
}

// NewDockerClient 解析 ASSIST_DOCKER_HOST：
//   - unix:///path/to/docker.sock（默认）
//   - tcp://host:port / http(s)://host:port
//   - 空 / 无法识别 → ErrDockerAbsent
func NewDockerClient(host string) (*DockerClient, error) {
	if strings.TrimSpace(host) == "" {
		return nil, ErrDockerAbsent
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("docker host %q: %w", host, err)
	}
	c := &DockerClient{stat: os.Stat}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		if sock == "" {
			return nil, ErrDockerAbsent
		}
		c.sockPath = sock
		c.base = "http://docker.sock"
		c.hc = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}}
	case "tcp":
		c.base = "http://" + u.Host
		c.hc = &http.Client{}
	case "http", "https":
		c.base = strings.TrimRight(u.String(), "/")
		c.hc = &http.Client{}
	default:
		return nil, fmt.Errorf("docker host %q: unsupported scheme %q", host, u.Scheme)
	}
	return c, nil
}

// dockerListEntry 为 GET /containers/json?all=1 的单条。
type dockerListEntry struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

// dockerInspect 为 GET /containers/{id}/json 的相关字段。
type dockerInspect struct {
	Name         string `json:"Name"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status    string `json:"Status"`
		ExitCode  int    `json:"ExitCode"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
}

// Containers 列出全部容器（含停止）并逐个 inspect 取 exitCode/startedAt/restartCount。
// inspect 失败的容器保留 list 级字段（name/state），详情缺席。
func (c *DockerClient) Containers(ctx context.Context) ([]Container, error) {
	if c.sockPath != "" {
		if _, err := c.stat(c.sockPath); err != nil {
			return nil, ErrDockerAbsent
		}
	}
	var entries []dockerListEntry
	if err := c.getJSON(ctx, "/containers/json?all=1", &entries); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(entries))
	for _, e := range entries {
		ct := Container{
			Name:  dockerName(e.Names),
			State: e.State,
		}
		var ins dockerInspect
		if err := c.getJSON(ctx, "/containers/"+url.PathEscape(e.ID)+"/json", &ins); err == nil {
			if ins.State.Status != "" {
				ct.State = ins.State.Status
			}
			if ct.Name == "" {
				ct.Name = strings.TrimPrefix(ins.Name, "/")
			}
			ct.RestartCount = ins.RestartCount
			// exitCode 仅对已停止/异常容器有意义；running|created → null。
			switch ct.State {
			case "running", "created":
				ct.ExitCode = nil
			default:
				code := ins.State.ExitCode
				ct.ExitCode = &code
			}
			if t, ok := parseDockerTime(ins.State.StartedAt); ok {
				ct.StartedAt = &t
			}
		}
		out = append(out, ct)
	}
	return out, nil
}

func dockerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

// parseDockerTime 解析 docker 时间戳；"0001-01-01T00:00:00Z"（未启动）返回 false。
func parseDockerTime(s string) (ISOTime, bool) {
	if s == "" {
		return ISOTime{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return ISOTime{}, false
	}
	return NewISOTime(t), true
}

func (c *DockerClient) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("docker api %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
