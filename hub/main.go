package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func logLine(format string, args ...any) {
	log.Printf(format, args...)
}

// getenv 读取环境变量，带默认值。
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func getenvInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// loadConfig 从环境变量装配配置。
func loadConfig() config {
	return config{
		bind:              getenv("ASSIST_BIND", ":8795"),
		dbPath:            getenv("ASSIST_DB_PATH", "assistant-hub.db"),
		adminUser:         getenv("ASSIST_ADMIN_USER", "admin"),
		adminPassword:     os.Getenv("ASSIST_ADMIN_PASSWORD"),
		baseURL:           getenv("ASSIST_BASE_URL", "http://localhost:8795"),
		rawDays:           getenvInt("ASSIST_RETENTION_RAW_DAYS", 7),
		rollupDays:        getenvInt("ASSIST_RETENTION_ROLLUP_DAYS", 90),
		messagesDays:      getenvInt("ASSIST_RETENTION_MESSAGES_DAYS", 90),
		changesDays:       getenvInt("ASSIST_RETENTION_CHANGES_DAYS", 7),
		changesMinSeq:     getenvInt64("ASSIST_RETENTION_CHANGES_MIN_SEQ", 100000),
		heartbeatInterval: 2 * time.Second,
		rollupInterval:    time.Minute,
		retentionInterval: time.Hour,
	}
}

// newApp 装配应用：库、存储、广播、规则引擎、路由。
func newApp(cfg config) (*app, error) {
	db, err := openDB(cfg.dbPath)
	if err != nil {
		return nil, err
	}
	b := newBroker()
	st := newStore(db, b)
	a := &app{
		st:           st,
		broker:       b,
		cfg:          cfg,
		loginLimiter: newLoginLimiter(),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
	a.re = newRuleEngine(a)

	// 初始化规则 / 设置文档（首启写默认集）。
	if _, err := st.loadRules(); err != nil {
		return nil, fmt.Errorf("init rules: %w", err)
	}
	if _, err := st.loadSettings(); err != nil {
		return nil, fmt.Errorf("init settings: %w", err)
	}

	// 管理密码哈希（bcrypt）。未配置密码时生成随机密码（仅记日志提示，不输出明文）。
	pw := cfg.adminPassword
	if pw == "" {
		pw = randToken("admin_")
		logLine("WARN: ASSIST_ADMIN_PASSWORD 未设置，已生成随机管理密码（登录不可用，请配置环境变量）")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	a.adminHash = hash
	return a, nil
}

func main() {
	cfg := loadConfig()
	a, err := newApp(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	defer a.st.db.Close()

	done := make(chan struct{})
	defer close(done)
	go a.heartbeatLoop(done, cfg.heartbeatInterval)
	go a.rollupLoop(done, cfg.rollupInterval)
	go a.retentionLoop(done, cfg.retentionInterval)

	srv := &http.Server{
		Addr:    cfg.bind,
		Handler: a.routes(),
		// ReadHeaderTimeout 防慢速头部；ReadTimeout 覆盖整个请求读取（含 body），
		// 防 Slowloris。WriteTimeout 不能设：SSE 长连接会持续写。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logLine("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	logLine("assistant-hub %s listening on %s (db %s)", hubVersion, cfg.bind, cfg.dbPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}
