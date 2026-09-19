package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// config 为运行配置（环境变量驱动）。
type config struct {
	bind              string
	dbPath            string
	adminUser         string
	adminPassword     string
	baseURL           string
	rawDays           int
	rollupDays        int
	messagesDays      int
	changesDays       int
	changesMinSeq     int64
	heartbeatInterval time.Duration
	rollupInterval    time.Duration
	retentionInterval time.Duration
}

// app 为中枢应用：存储、广播、规则引擎、HTTP 路由。
type app struct {
	st           *store
	broker       *broker
	re           *ruleEngine
	cfg          config
	adminHash    []byte
	loginLimiter *loginLimiter
	httpClient   *http.Client
}

// routes 注册全部 v1 端点。
func (a *app) routes() http.Handler {
	mux := http.NewServeMux()

	// 认证组
	mux.HandleFunc("POST /api/v1/auth/login", a.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/refresh", a.handleRefresh)
	mux.HandleFunc("GET /api/v1/auth/session", a.requireClient(a.handleSession))
	mux.HandleFunc("POST /api/v1/auth/logout", a.requireClient(a.handleLogout))

	// 来源接入组
	mux.HandleFunc("POST /api/v1/ingest/metrics", a.sourceKeyAuth(a.handleIngestMetrics))
	mux.HandleFunc("POST /api/v1/ingest/events", a.sourceKeyAuth(a.handleIngestEvents))

	// 客户端读取组
	mux.HandleFunc("GET /api/v1/overview", a.requireClient(a.handleOverview))
	mux.HandleFunc("GET /api/v1/sync", a.requireClient(a.handleSync))
	mux.HandleFunc("GET /api/v1/stream", a.requireClient(a.handleStream))
	mux.HandleFunc("GET /api/v1/messages", a.requireClient(a.handleMessages))
	mux.HandleFunc("GET /api/v1/messages/{id}", a.requireClient(a.handleMessageDetail))
	mux.HandleFunc("POST /api/v1/messages/{id}/read", a.requireClient(a.handleMessageRead))
	mux.HandleFunc("POST /api/v1/messages/{id}/unread", a.requireClient(a.handleMessageUnread))
	mux.HandleFunc("POST /api/v1/messages/read-all", a.requireClient(a.handleMessagesReadAll))
	mux.HandleFunc("GET /api/v1/messages/{id}/attachments/{attId...}", a.requireClient(a.handleAttachment))
	mux.HandleFunc("GET /api/v1/faults", a.requireClient(a.handleFaults))
	mux.HandleFunc("POST /api/v1/faults/{id}/read", a.requireClient(a.handleFaultRead))
	mux.HandleFunc("POST /api/v1/faults/{id}/mute", a.requireClient(a.handleFaultMute))
	mux.HandleFunc("POST /api/v1/faults/{id}/unmute", a.requireClient(a.handleFaultUnmute))
	mux.HandleFunc("GET /api/v1/metrics/series", a.requireClient(a.handleMetricsSeries))

	// 管理组
	mux.HandleFunc("GET /api/v1/sources", a.requireClient(a.handleSources))
	mux.HandleFunc("POST /api/v1/sources", a.requireClient(a.handleSourceCreate))
	mux.HandleFunc("GET /api/v1/sources/{id}", a.requireClient(a.handleSourceGet))
	mux.HandleFunc("PATCH /api/v1/sources/{id}", a.requireClient(a.handleSourcePatch))
	mux.HandleFunc("POST /api/v1/sources/{id}/rotate-key", a.requireClient(a.handleSourceRotateKey))
	mux.HandleFunc("DELETE /api/v1/sources/{id}", a.requireClient(a.handleSourceDelete))
	mux.HandleFunc("GET /api/v1/rules", a.requireClient(a.handleRulesGet))
	mux.HandleFunc("PUT /api/v1/rules", a.requireClient(a.handleRulesPut))
	mux.HandleFunc("GET /api/v1/settings", a.requireClient(a.handleSettingsGet))
	mux.HandleFunc("PATCH /api/v1/settings", a.requireClient(a.handleSettingsPatch))

	// 杂项（无需认证）
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "version": hubVersion, "serverTime": fmtTS(nowUTC()),
		})
	})
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"api": 1, "hub": hubVersion})
	})

	return logMiddleware(mux)
}

// statusWriter 记录最终状态码并透传 Flusher/Hijacker 等接口（SSE 依赖 Flush）。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logMiddleware 记录请求日志（不含任何凭证 / 密钥 / 请求体）。
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		logLine("%s %s %d %s", r.Method, r.URL.Path, ww.status, time.Since(start).Round(time.Millisecond))
	})
}

// ---- overview ----

func (a *app) handleOverview(w http.ResponseWriter, r *http.Request) {
	hbSec := a.st.heartbeatSeconds()
	sources, err := a.st.listSources()
	if err != nil {
		errInternal(w)
		return
	}
	srcJSON := []any{}
	for _, s := range sources {
		srcJSON = append(srcJSON, toSourceJSON(s, hbSec))
	}

	// 开放故障（overview 精简视图）。
	rows, err := a.st.db.Query(
		`SELECT ` + faultCols + ` FROM faults WHERE state='open' ORDER BY last_event_at DESC`)
	if err != nil {
		errInternal(w)
		return
	}
	openFaults := []any{}
	for rows.Next() {
		f, err := scanFault(rows)
		if err != nil {
			rows.Close()
			errInternal(w)
			return
		}
		openFaults = append(openFaults, map[string]any{
			"id":          f.ID,
			"sourceId":    f.SourceID,
			"sourceName":  a.st.sourceName(f.SourceID),
			"faultKey":    f.FaultKey,
			"severity":    f.Severity,
			"title":       f.Title,
			"openedAt":    f.OpenedAt,
			"lastEventAt": f.LastEventAt,
			"incident":    f.Incident,
			"eventCount":  f.EventCount,
			"muted":       f.MutedAt.Valid,
			"read":        f.ReadAt.Valid,
		})
	}
	rows.Close()

	var unread int64
	_ = a.st.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE read_at IS NULL`).Scan(&unread)

	rules, err := a.st.loadRules()
	if err != nil {
		errInternal(w)
		return
	}
	settings, err := a.st.loadSettings()
	if err != nil {
		errInternal(w)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"serverTime":       fmtTS(nowUTC()),
		"hubReachableHint": true,
		"sources":          srcJSON,
		"openFaults":       openFaults,
		"unreadMessages":   unread,
		"rulesVersion":     rules.Version,
		"settingsVersion":  settings.Version,
		"dnd":              settings.DND,
	})
}

// ---- sync ----

func (a *app) handleSync(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since int64
	if s := q.Get("since"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			errInvalid(w, "since 非法")
			return
		}
		since = v
	}
	limit := int64(500)
	if s := q.Get("limit"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 {
			errInvalid(w, "limit 非法")
			return
		}
		if v > 1000 {
			v = 1000
		}
		limit = v
	}

	rows, err := a.st.db.Query(
		`SELECT seq, type, data_json FROM changes WHERE seq > ? ORDER BY seq LIMIT ?`,
		since, limit+1)
	if err != nil {
		errInternal(w)
		return
	}
	defer rows.Close()

	type changeJSON struct {
		ChangeSeq int64           `json:"changeSeq"`
		Type      string          `json:"type"`
		Data      json.RawMessage `json:"data"`
	}
	items := []changeJSON{}
	var lastSeq int64
	for rows.Next() {
		var c changeJSON
		var data string
		if err := rows.Scan(&c.ChangeSeq, &c.Type, &data); err != nil {
			errInternal(w)
			return
		}
		c.Data = json.RawMessage(data)
		if int64(len(items)) >= limit {
			writeJSON(w, http.StatusOK, map[string]any{
				"cursor": lastSeq, "hasMore": true, "changes": items,
			})
			return
		}
		items = append(items, c)
		lastSeq = c.ChangeSeq
	}
	cursor := lastSeq
	if len(items) == 0 {
		if mx := a.st.maxChangeSeq(); mx > since {
			cursor = mx
		} else {
			cursor = since
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cursor": cursor, "hasMore": false, "changes": items,
	})
}

// ---- messages ----

func (a *app) handleMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := q.Get("filter")
	if filter == "" {
		filter = "all"
	}
	if filter != "all" && filter != "unread" && filter != "feedback" && filter != "faults" {
		errInvalid(w, "filter 非法")
		return
	}
	limit := 50
	if s := q.Get("limit"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			errInvalid(w, "limit 非法")
			return
		}
		if v > 200 {
			v = 200
		}
		limit = v
	}
	cursor := q.Get("cursor")
	srcFilter := q.Get("source")

	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT m.` + strings.Join(strings.Split(messageCols, ","), ",m.") +
		` FROM messages m`)
	needJoin := filter == "feedback"
	if needJoin {
		sb.WriteString(` JOIN sources s ON s.id = m.source_id`)
	}
	sb.WriteString(` WHERE 1=1`)
	switch filter {
	case "unread":
		sb.WriteString(` AND m.read_at IS NULL`)
	case "feedback":
		sb.WriteString(` AND s.kind='feedback' AND m.fault_id IS NULL`)
	case "faults":
		sb.WriteString(` AND m.fault_id IS NOT NULL`)
	}
	if srcFilter != "" {
		sb.WriteString(` AND m.source_id = ?`)
		args = append(args, srcFilter)
	}
	if cursor != "" {
		sb.WriteString(` AND m.id < ?`)
		args = append(args, cursor)
	}
	sb.WriteString(` ORDER BY m.id DESC LIMIT ?`)
	args = append(args, limit+1)

	rows, err := a.st.db.Query(sb.String(), args...)
	if err != nil {
		errInternal(w)
		return
	}
	defer rows.Close()

	items := []*messageJSON{}
	var nextCursor *string
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			errInternal(w)
			return
		}
		if len(items) >= limit {
			prev := items[len(items)-1]
			nextCursor = &prev.ID
			break
		}
		items = append(items, toMessageJSON(m, a.st.sourceName(m.SourceID)))
	}
	if items == nil {
		items = []*messageJSON{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "nextCursor": nextCursor,
	})
}

func (a *app) handleMessageDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := a.st.loadMessage(id)
	if errors.Is(err, sql.ErrNoRows) {
		errNotFound(w)
		return
	}
	if err != nil {
		errInternal(w)
		return
	}
	mj := toMessageJSON(m, a.st.sourceName(m.SourceID))
	var fj *faultJSON
	if m.FaultID.Valid {
		if f, err := a.st.loadFault(m.FaultID.String); err == nil && f != nil {
			fj = toFaultJSON(f, a.st.sourceName(f.SourceID))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": mj, "fault": fj})
}

// setMessageRead 更新单条消息已读状态并广播变更。
func (a *app) setMessageRead(w http.ResponseWriter, id string, read bool) {
	var changes []*changeEntry
	var out *messageJSON
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		m, err := loadMessageTx(tx, id)
		if err != nil {
			return err
		}
		if read == m.ReadAt.Valid {
			// 幂等：状态已满足仍返回消息（不落新变更）。
			out = toMessageJSON(m, a.st.sourceName(m.SourceID))
			return nil
		}
		if read {
			m.ReadAt = sql.NullString{String: fmtTS(nowUTC()), Valid: true}
		} else {
			m.ReadAt = sql.NullString{}
		}
		if _, err := tx.Exec(`UPDATE messages SET read_at=? WHERE id=?`, m.ReadAt, m.ID); err != nil {
			return err
		}
		mj := toMessageJSON(m, a.st.sourceName(m.SourceID))
		entry, err := insertChangeObj(tx, "message", m.ID,
			func(seq int64) { mj.ChangeSeq = seq; m.ChangeSeq = seq }, mj, nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE messages SET change_seq=? WHERE id=?`, m.ChangeSeq, m.ID); err != nil {
			return err
		}
		changes = append(changes, entry)
		out = mj
		return nil
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			errNotFound(w)
			return
		}
		errInternal(w)
		return
	}
	a.st.publish(changes)
	writeJSON(w, http.StatusOK, map[string]any{"message": out})
}

func loadMessageTx(tx *sql.Tx, id string) (*messageRow, error) {
	row := tx.QueryRow(`SELECT `+messageCols+` FROM messages WHERE id=?`, id)
	return scanMessage(row)
}

func (a *app) handleMessageRead(w http.ResponseWriter, r *http.Request) {
	a.setMessageRead(w, r.PathValue("id"), true)
}

func (a *app) handleMessageUnread(w http.ResponseWriter, r *http.Request) {
	a.setMessageRead(w, r.PathValue("id"), false)
}

// handleMessagesReadAll 批量已读；每条更新消息各落一条变更。
func (a *app) handleMessagesReadAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Before *string `json:"before"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	var beforeTS *time.Time
	if req.Before != nil && *req.Before != "" {
		t, err := parseTS(*req.Before)
		if err != nil {
			errInvalid(w, "before 非法")
			return
		}
		beforeTS = &t
	}

	var changes []*changeEntry
	updated := 0
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		query := `SELECT id FROM messages WHERE read_at IS NULL`
		args := []any{}
		if beforeTS != nil {
			query += ` AND received_at <= ?`
			args = append(args, fmtTS(*beforeTS))
		}
		query += ` ORDER BY id`
		rows, err := tx.Query(query, args...)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()

		now := fmtTS(nowUTC())
		for _, id := range ids {
			m, err := loadMessageTx(tx, id)
			if err != nil {
				return err
			}
			m.ReadAt = sql.NullString{String: now, Valid: true}
			if _, err := tx.Exec(`UPDATE messages SET read_at=? WHERE id=?`, m.ReadAt, m.ID); err != nil {
				return err
			}
			mj := toMessageJSON(m, a.st.sourceName(m.SourceID))
			entry, err := insertChangeObj(tx, "message", m.ID,
				func(seq int64) { mj.ChangeSeq = seq; m.ChangeSeq = seq }, mj, nil)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE messages SET change_seq=? WHERE id=?`, m.ChangeSeq, m.ID); err != nil {
				return err
			}
			changes = append(changes, entry)
			updated++
		}
		return nil
	})
	if err != nil {
		errInternal(w)
		return
	}
	a.st.publish(changes)
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated})
}

// ---- faults ----

func (a *app) handleFaults(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "all"
	}
	if state != "open" && state != "resolved" && state != "all" {
		errInvalid(w, "state 非法")
		return
	}
	limit := 50
	if s := q.Get("limit"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			errInvalid(w, "limit 非法")
			return
		}
		if v > 200 {
			v = 200
		}
		limit = v
	}

	query := `SELECT ` + faultCols + ` FROM faults WHERE 1=1`
	args := []any{}
	if state != "all" {
		query += ` AND state=?`
		args = append(args, state)
	}
	// 游标：lastEventAt|id 复合（活跃优先倒序）。
	if c := q.Get("cursor"); c != "" {
		parts := strings.SplitN(c, "|", 2)
		if len(parts) != 2 {
			errInvalid(w, "cursor 非法")
			return
		}
		query += ` AND (last_event_at < ? OR (last_event_at = ? AND id < ?))`
		args = append(args, parts[0], parts[0], parts[1])
	}
	query += ` ORDER BY last_event_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := a.st.db.Query(query, args...)
	if err != nil {
		errInternal(w)
		return
	}
	defer rows.Close()

	items := []*faultJSON{}
	var nextCursor *string
	for rows.Next() {
		f, err := scanFault(rows)
		if err != nil {
			errInternal(w)
			return
		}
		if len(items) >= limit {
			prev := items[len(items)-1]
			c := prev.LastEventAt + "|" + prev.ID
			nextCursor = &c
			break
		}
		items = append(items, toFaultJSON(f, a.st.sourceName(f.SourceID)))
	}
	if items == nil {
		items = []*faultJSON{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": nextCursor})
}

// updateFaultFlags 处理 fault read/mute/unmute 三类标记并广播。
func (a *app) updateFaultFlags(w http.ResponseWriter, id, op string) {
	var changes []*changeEntry
	var out *faultJSON
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		row := tx.QueryRow(`SELECT `+faultCols+` FROM faults WHERE id=?`, id)
		f, err := scanFault(row)
		if err != nil {
			return err
		}
		now := fmtTS(nowUTC())
		switch op {
		case "read":
			if !f.ReadAt.Valid {
				f.ReadAt = sql.NullString{String: now, Valid: true}
			} else {
				out = toFaultJSON(f, a.st.sourceName(f.SourceID))
				return nil // 幂等
			}
		case "mute":
			if !f.MutedAt.Valid {
				f.MutedAt = sql.NullString{String: now, Valid: true}
				f.MutedUntil = sql.NullString{}
			} else {
				out = toFaultJSON(f, a.st.sourceName(f.SourceID))
				return nil
			}
		case "unmute":
			if f.MutedAt.Valid {
				f.MutedAt = sql.NullString{}
				f.MutedUntil = sql.NullString{}
			} else {
				out = toFaultJSON(f, a.st.sourceName(f.SourceID))
				return nil
			}
		}
		if _, err := tx.Exec(
			`UPDATE faults SET read_at=?, muted_at=?, muted_until=? WHERE id=?`,
			f.ReadAt, f.MutedAt, f.MutedUntil, f.ID); err != nil {
			return err
		}
		fj := toFaultJSON(f, a.st.sourceName(f.SourceID))
		entry, err := insertChangeObj(tx, "fault", f.ID,
			func(seq int64) { fj.ChangeSeq = seq; f.ChangeSeq = seq }, fj, nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE faults SET change_seq=? WHERE id=?`, f.ChangeSeq, f.ID); err != nil {
			return err
		}
		changes = append(changes, entry)
		out = fj
		return nil
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			errNotFound(w)
			return
		}
		errInternal(w)
		return
	}
	a.st.publish(changes)
	writeJSON(w, http.StatusOK, map[string]any{"fault": out})
}

func (a *app) handleFaultRead(w http.ResponseWriter, r *http.Request) {
	a.updateFaultFlags(w, r.PathValue("id"), "read")
}
func (a *app) handleFaultMute(w http.ResponseWriter, r *http.Request) {
	a.updateFaultFlags(w, r.PathValue("id"), "mute")
}
func (a *app) handleFaultUnmute(w http.ResponseWriter, r *http.Request) {
	a.updateFaultFlags(w, r.PathValue("id"), "unmute")
}

// ---- metrics/series ----

var seriesMetrics = map[string]bool{
	"cpu_percent": true, "mem_percent": true, "disk_percent": true,
	"net_rx_bps": true, "net_tx_bps": true,
}

func (a *app) handleMetricsSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sourceID := q.Get("source")
	metric := q.Get("metric")
	step := q.Get("step")
	if sourceID == "" {
		errInvalid(w, "source 必填")
		return
	}
	if !seriesMetrics[metric] {
		errInvalid(w, "metric 非法")
		return
	}
	label := ""
	if metric == "disk_percent" {
		label = q.Get("label")
		if label == "" {
			errInvalid(w, "disk_percent 需要 label")
			return
		}
	}
	if step == "" {
		step = "raw"
	}
	if step != "raw" && step != "5m" && step != "1h" {
		errInvalid(w, "step 非法")
		return
	}
	var fromTS, toTS string
	if v := q.Get("from"); v != "" {
		t, err := parseTS(v)
		if err != nil {
			errInvalid(w, "from 非法")
			return
		}
		fromTS = fmtTS(t)
	}
	if v := q.Get("to"); v != "" {
		t, err := parseTS(v)
		if err != nil {
			errInvalid(w, "to 非法")
			return
		}
		toTS = fmtTS(t)
	}

	type point struct {
		TS  string  `json:"ts"`
		Avg float64 `json:"avg"`
		Min float64 `json:"min"`
		Max float64 `json:"max"`
	}
	series := []point{}

	switch step {
	case "raw":
		query := `SELECT ts, value FROM metrics_raw WHERE source_id=? AND metric=? AND label=?`
		args := []any{sourceID, metric, label}
		if fromTS != "" {
			query += ` AND ts >= ?`
			args = append(args, fromTS)
		}
		if toTS != "" {
			query += ` AND ts <= ?`
			args = append(args, toTS)
		}
		query += ` ORDER BY ts`
		rows, err := a.st.db.Query(query, args...)
		if err != nil {
			errInternal(w)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var ts string
			var v float64
			if err := rows.Scan(&ts, &v); err != nil {
				errInternal(w)
				return
			}
			series = append(series, point{ts, v, v, v})
		}
	case "5m", "1h":
		query := `SELECT bucket, avg, min, max, n FROM metrics_5m
		  WHERE source_id=? AND metric=? AND label=?`
		args := []any{sourceID, metric, label}
		if fromTS != "" {
			query += ` AND bucket >= ?`
			args = append(args, fromTS)
		}
		if toTS != "" {
			query += ` AND bucket <= ?`
			args = append(args, toTS)
		}
		query += ` ORDER BY bucket`
		rows, err := a.st.db.Query(query, args...)
		if err != nil {
			errInternal(w)
			return
		}
		defer rows.Close()
		type agg struct {
			sumW, min, max float64
			n              int64
		}
		bucketOf := func(ts string) string {
			if step == "5m" {
				return ts
			}
			t, err := parseTS(ts)
			if err != nil {
				return ts
			}
			return fmtTS(t.Truncate(time.Hour))
		}
		order := []string{}
		groups := map[string]*agg{}
		for rows.Next() {
			var b string
			var av, mn, mx float64
			var n int64
			if err := rows.Scan(&b, &av, &mn, &mx, &n); err != nil {
				errInternal(w)
				return
			}
			bk := bucketOf(b)
			g := groups[bk]
			if g == nil {
				g = &agg{min: mn, max: mx}
				groups[bk] = g
				order = append(order, bk)
			}
			g.sumW += av * float64(n)
			g.n += n
			if mn < g.min {
				g.min = mn
			}
			if mx > g.max {
				g.max = mx
			}
		}
		for _, bk := range order {
			g := groups[bk]
			avg := 0.0
			if g.n > 0 {
				avg = g.sumW / float64(g.n)
			}
			series = append(series, point{bk, avg, g.min, g.max})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
}

var _ = fmt.Sprintf // fmt 保留（后续处理器使用）
