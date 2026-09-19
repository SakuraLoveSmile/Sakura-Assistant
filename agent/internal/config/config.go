// Package config 解析 flags + 环境变量（同名环境变量优先于 flag，见 README）。
package config

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Mode 为运行模式。
type Mode int

const (
	ModeRun     Mode = iota // 正常采集上报
	ModeVersion             // --version
	ModeCheck               // --check：采集一次打印 JSON，不上报
)

// Config 为生效配置。
type Config struct {
	HubURL          string        // ASSIST_HUB_URL，必填
	SourceKey       string        // ASSIST_SOURCE_KEY，必填 ask_<48 hex>
	Interval        time.Duration // ASSIST_INTERVAL，默认 30s
	QueueDir        string        // ASSIST_QUEUE_DIR，默认 /var/lib/assistant-agent（不可写回退 ./data）
	QueueMaxBatches int           // ASSIST_QUEUE_MAX_BATCHES，默认 500
	QueueMaxEvents  int           // ASSIST_QUEUE_MAX_EVENTS，默认 1000
	Hostname        string        // ASSIST_HOSTNAME，可选覆盖
	DockerHost      string        // ASSIST_DOCKER_HOST，默认 unix:///var/run/docker.sock
	Smart           string        // ASSIST_SMART：auto|on|off，默认 auto
	Gzip            bool          // ASSIST_GZIP，默认 false（契约未声明压缩，opt-in）
	LogLevel        string        // ASSIST_LOG_LEVEL：debug|info|warn|error，默认 info
}

// DefaultQueueDir 为默认队列目录。
const DefaultQueueDir = "/var/lib/assistant-agent"

// FallbackQueueDir 为默认目录不可写时的回退目录。
const FallbackQueueDir = "./data"

var sourceKeyRe = regexp.MustCompile(`^ask_[0-9a-f]{48}$`)

// Result 为解析结果。
type Result struct {
	Cfg  Config
	Mode Mode
}

// Parse 解析 args（不含 argv0）；env 为环境变量读取函数。
// 同名环境变量优先于 flag。version/check 模式下跳过必填校验。
func Parse(args []string, env func(string) string, stderr io.Writer) (*Result, error) {
	fs := flag.NewFlagSet("assistant-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		hubURL          = fs.String("hub-url", "", "hub base URL (env ASSIST_HUB_URL)")
		sourceKey       = fs.String("source-key", "", "source access key ask_... (env ASSIST_SOURCE_KEY)")
		interval        = fs.String("interval", "30s", "report interval, e.g. 30s (env ASSIST_INTERVAL)")
		queueDir        = fs.String("queue-dir", DefaultQueueDir, "persistent queue dir (env ASSIST_QUEUE_DIR)")
		queueMaxBatches = fs.Int("queue-max-batches", 500, "max queued metrics batches (env ASSIST_QUEUE_MAX_BATCHES)")
		queueMaxEvents  = fs.Int("queue-max-events", 1000, "max queued events (env ASSIST_QUEUE_MAX_EVENTS)")
		hostname        = fs.String("hostname", "", "hostname override (env ASSIST_HOSTNAME)")
		dockerHost      = fs.String("docker-host", "unix:///var/run/docker.sock", "docker endpoint (env ASSIST_DOCKER_HOST)")
		smart           = fs.String("smart", "auto", "SMART probing: auto|on|off (env ASSIST_SMART)")
		gzipBody        = fs.Bool("gzip", false, "gzip request bodies (env ASSIST_GZIP)")
		logLevel        = fs.String("log-level", "info", "log level: debug|info|warn|error (env ASSIST_LOG_LEVEL)")
		showVersion     = fs.Bool("version", false, "print version and exit")
		check           = fs.Bool("check", false, "collect once, print payload JSON, do not report")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// 环境变量优先于 flag（文档化约定）。
	hubURLV := envOr(env, "ASSIST_HUB_URL", *hubURL)
	sourceKeyV := envOr(env, "ASSIST_SOURCE_KEY", *sourceKey)
	intervalV := envOr(env, "ASSIST_INTERVAL", *interval)
	queueDirV := envOr(env, "ASSIST_QUEUE_DIR", *queueDir)
	queueMaxBatchesV := envOr(env, "ASSIST_QUEUE_MAX_BATCHES", strconv.Itoa(*queueMaxBatches))
	queueMaxEventsV := envOr(env, "ASSIST_QUEUE_MAX_EVENTS", strconv.Itoa(*queueMaxEvents))
	hostnameV := envOr(env, "ASSIST_HOSTNAME", *hostname)
	dockerHostV := envOr(env, "ASSIST_DOCKER_HOST", *dockerHost)
	smartV := envOr(env, "ASSIST_SMART", *smart)
	gzipV := envOr(env, "ASSIST_GZIP", strconv.FormatBool(*gzipBody))
	logLevelV := envOr(env, "ASSIST_LOG_LEVEL", *logLevel)

	res := &Result{Mode: ModeRun}
	if *showVersion {
		res.Mode = ModeVersion
		return res, nil
	}
	if *check {
		res.Mode = ModeCheck
	}

	cfg := Config{
		HubURL:     strings.TrimRight(hubURLV, "/"),
		SourceKey:  sourceKeyV,
		QueueDir:   queueDirV,
		Hostname:   hostnameV,
		DockerHost: dockerHostV,
		Smart:      strings.ToLower(smartV),
		LogLevel:   strings.ToLower(logLevelV),
	}

	d, err := parseDurationLoose(intervalV)
	if err != nil {
		return nil, fmt.Errorf("invalid ASSIST_INTERVAL %q: %w", intervalV, err)
	}
	cfg.Interval = d

	if cfg.QueueMaxBatches, err = strconv.Atoi(queueMaxBatchesV); err != nil || cfg.QueueMaxBatches < 1 {
		return nil, fmt.Errorf("invalid ASSIST_QUEUE_MAX_BATCHES %q: need >=1", queueMaxBatchesV)
	}
	if cfg.QueueMaxEvents, err = strconv.Atoi(queueMaxEventsV); err != nil || cfg.QueueMaxEvents < 1 {
		return nil, fmt.Errorf("invalid ASSIST_QUEUE_MAX_EVENTS %q: need >=1", queueMaxEventsV)
	}
	if cfg.Gzip, err = parseBoolLoose(gzipV); err != nil {
		return nil, fmt.Errorf("invalid ASSIST_GZIP %q", gzipV)
	}
	switch cfg.Smart {
	case "auto", "on", "off":
	default:
		return nil, fmt.Errorf("invalid ASSIST_SMART %q: auto|on|off", smartV)
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("invalid ASSIST_LOG_LEVEL %q", logLevelV)
	}

	// --check 不要求 hub/key（只做本地能力探测打印）。
	if res.Mode == ModeCheck {
		res.Cfg = cfg
		return res, nil
	}

	if cfg.HubURL == "" {
		return nil, fmt.Errorf("ASSIST_HUB_URL (or --hub-url) is required")
	}
	u, err := url.Parse(cfg.HubURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid ASSIST_HUB_URL %q: need http(s)://host", hubURLV)
	}
	if !sourceKeyRe.MatchString(cfg.SourceKey) {
		return nil, fmt.Errorf("invalid ASSIST_SOURCE_KEY: must match ask_<48 lowercase hex> (contracts/api-v1.md)")
	}
	res.Cfg = cfg
	return res, nil
}

// envOr 返回环境变量值（非空优先），否则 fallback。
func envOr(env func(string) string, key, fallback string) string {
	if v := env(key); v != "" {
		return v
	}
	return fallback
}

// parseDurationLoose 接受 "30s"/"1m30s" 或裸数字（按秒）。
func parseDurationLoose(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	return 0, fmt.Errorf("need duration like 30s or seconds number")
}

// parseBoolLoose 接受 1/0/true/false/on/off/yes/no。
func parseBoolLoose(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "on", "yes":
		return true, nil
	case "0", "false", "off", "no", "":
		return false, nil
	default:
		return false, fmt.Errorf("need boolean")
	}
}
