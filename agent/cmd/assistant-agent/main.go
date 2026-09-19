// assistant-agent 为设备采集代理：定时采集指标经持久队列上报中枢。
//
// 退出码：0 正常；2 配置错误；1 运行期致命错误。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"assistant-agent/internal/collect"
	"assistant-agent/internal/config"
	"assistant-agent/internal/queue"
	"assistant-agent/internal/report"
)

// version 可由 -ldflags "-X main.version=x.y.z" 覆盖。
var version = "0.1.0"

func main() {
	os.Exit(run())
}

func run() int {
	res, err := config.Parse(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config: "+err.Error())
		return 2
	}
	switch res.Mode {
	case config.ModeVersion:
		fmt.Printf("assistant-agent %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return 0
	case config.ModeCheck:
		return runCheck(res.Cfg)
	}

	log := newLogger(res.Cfg.LogLevel)

	queueDir, err := resolveQueueDir(res.Cfg.QueueDir, log)
	if err != nil {
		log.Error("queue dir unusable", "dir", res.Cfg.QueueDir, "err", err)
		return 1
	}
	q, err := queue.Open(queueDir, res.Cfg.QueueMaxBatches, res.Cfg.QueueMaxEvents)
	if err != nil {
		log.Error("queue open failed", "dir", queueDir, "err", err)
		return 1
	}
	defer q.Close()

	hostname := res.Cfg.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	prov := collect.New(collect.ProviderConfig{
		DockerHost: res.Cfg.DockerHost,
		SmartMode:  res.Cfg.Smart,
	})
	hc := report.NewHubClient(res.Cfg.HubURL, res.Cfg.SourceKey, res.Cfg.Gzip, 15*time.Second)
	hc.SetUserAgent("assistant-agent/" + version)
	info := report.AgentInfo{
		Version:  version,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Hostname: hostname,
	}

	log.Info("assistant-agent starting",
		"version", version, "hub", redactURL(res.Cfg.HubURL),
		"interval", res.Cfg.Interval.String(), "queue_dir", queueDir,
		"smart", res.Cfg.Smart, "hostname", hostname)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rep := report.New(prov, q, hc, info, res.Cfg.Interval, log)
	if err := rep.Run(ctx); err != nil {
		log.Error("reporter exited", "err", err)
		return 1
	}
	log.Info("assistant-agent stopped")
	return 0
}

// runCheck 一次性采集并打印完整 ingest/metrics 载荷（seq 恒 0，不上报、不写队列）。
// 先预热再隔 ~1.1s 正式采样，使速率/利用率差值有意义。
func runCheck(cfg config.Config) int {
	log := newLogger(cfg.LogLevel)
	prov := collect.New(collect.ProviderConfig{
		DockerHost: cfg.DockerHost,
		SmartMode:  cfg.Smart,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := prov.Collect(ctx); err != nil {
		log.Warn("warmup collect failed", "err", err)
	}
	time.Sleep(1100 * time.Millisecond)
	res, err := prov.Collect(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "collect failed: "+err.Error())
		return 1
	}
	hostname := cfg.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	body, err := report.BuildMetrics(0, report.AgentInfo{
		Version:  version,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Hostname: hostname,
	}, res, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal failed: "+err.Error())
		return 1
	}
	var pretty any
	if json.Unmarshal(body, &pretty) == nil {
		out, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Println(string(body))
	}
	return 0
}

// resolveQueueDir 确保队列目录可写；默认目录不可写且未被显式覆盖时回退 ./data。
func resolveQueueDir(dir string, log *slog.Logger) (string, error) {
	if err := probeDir(dir); err != nil {
		if dir == config.DefaultQueueDir {
			fallback := config.FallbackQueueDir
			if ferr := probeDir(fallback); ferr == nil {
				log.Warn("default queue dir not writable, falling back",
					"from", dir, "to", fallback)
				return fallback, nil
			}
		}
		return "", err
	}
	return dir, nil
}

func probeDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// newLogger 结构化 JSON 日志（stderr）；绝不输出密钥。
func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

// redactURL 去掉 URL 中可能的 userinfo/query 再进日志。
func redactURL(raw string) string {
	if i := strings.Index(raw, "@"); i >= 0 {
		if j := strings.Index(raw, "://"); j >= 0 && j < i {
			raw = raw[:j+3] + "***" + raw[i:]
		}
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}
