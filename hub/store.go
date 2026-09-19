package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// store 封装全部持久化操作。写操作经 mu 串行化（SQLite 单写者模型），
// 每次成功写事务内产生的变更在提交后经 broker 广播给 SSE 订阅者。
type store struct {
	db     *sql.DB
	mu     sync.Mutex // 写事务串行化
	broker *broker
}

func newStore(db *sql.DB, b *broker) *store {
	return &store{db: db, broker: b}
}

// ---- changeSeq 计数与变更写入 ----

// nextChangeSeq 在写事务内分配单调递增 changeSeq。
func nextChangeSeq(tx *sql.Tx) (int64, error) {
	var seq int64
	err := tx.QueryRow(
		`UPDATE meta SET value = CAST(value AS INTEGER) + 1 WHERE key = 'change_seq' RETURNING value`,
	).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRow(
			`INSERT INTO meta(key, value) VALUES('change_seq','1') RETURNING value`,
		).Scan(&seq)
	}
	if err != nil {
		return 0, fmt.Errorf("nextChangeSeq: %w", err)
	}
	return seq, nil
}

// insertChange 在写事务内写一条变更，返回变更条目（含已注入对象 changeSeq 的 data）。
func insertChange(tx *sql.Tx, typ, refID string, data json.RawMessage, notify *notifyBlock) (*changeEntry, error) {
	seq, err := nextChangeSeq(tx)
	if err != nil {
		return nil, err
	}
	var notifyJSON sql.NullString
	if notify != nil {
		b, _ := json.Marshal(notify)
		notifyJSON = sql.NullString{String: string(b), Valid: true}
	}
	_, err = tx.Exec(
		`INSERT INTO changes(seq, type, ref_id, data_json, notify_json, created_at) VALUES(?,?,?,?,?,?)`,
		seq, typ, refID, string(data), notifyJSON, fmtTS(nowUTC()),
	)
	if err != nil {
		return nil, fmt.Errorf("insertChange: %w", err)
	}
	return &changeEntry{Seq: seq, Type: typ, Data: data, Notify: notify}, nil
}

// insertChangeObj 将对象序列化并写变更；对象需已带正确 changeSeq。
// 流程：先占 seq → 回填对象 → 序列化 → 写行。
// 因此使用 insertChangeObjWithSeq。
func insertChangeObj(tx *sql.Tx, typ, refID string, setSeq func(seq int64), obj any, notify *notifyBlock) (*changeEntry, error) {
	seq, err := nextChangeSeq(tx)
	if err != nil {
		return nil, err
	}
	setSeq(seq)
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var notifyJSON sql.NullString
	if notify != nil {
		b, _ := json.Marshal(notify)
		notifyJSON = sql.NullString{String: string(b), Valid: true}
	}
	_, err = tx.Exec(
		`INSERT INTO changes(seq, type, ref_id, data_json, notify_json, created_at) VALUES(?,?,?,?,?,?)`,
		seq, typ, refID, string(data), notifyJSON, fmtTS(nowUTC()),
	)
	if err != nil {
		return nil, fmt.Errorf("insertChangeObj: %w", err)
	}
	return &changeEntry{Seq: seq, Type: typ, Data: data, Notify: notify}, nil
}

// ---- 序列化 ----

// toMessageJSON 将消息行转为契约 Message 对象。
func toMessageJSON(m *messageRow, sourceName string) *messageJSON {
	j := &messageJSON{
		ID:         m.ID,
		ChangeSeq:  m.ChangeSeq,
		SourceID:   m.SourceID,
		SourceName: sourceName,
		Kind:       m.Kind,
		Severity:   m.Severity,
		Title:      m.Title,
		OccurredAt: m.OccurredAt,
		ReceivedAt: m.ReceivedAt,
	}
	if m.Body.Valid {
		j.Body = &m.Body.String
	}
	if m.ReadAt.Valid {
		j.ReadAt = &m.ReadAt.String
	}
	if m.FaultID.Valid {
		j.FaultID = &m.FaultID.String
	}
	if m.Incident.Valid {
		j.Incident = &m.Incident.Int64
	}
	if m.Ref.Valid && m.Ref.String != "" {
		j.Ref = json.RawMessage(m.Ref.String)
	}
	j.Attachments = []attachmentDesc{}
	if m.Attachments.Valid && m.Attachments.String != "" {
		_ = json.Unmarshal([]byte(m.Attachments.String), &j.Attachments)
		if j.Attachments == nil {
			j.Attachments = []attachmentDesc{}
		}
	}
	return j
}

// toFaultJSON 将故障行转为契约 Fault 对象。
func toFaultJSON(f *faultRow, sourceName string) *faultJSON {
	j := &faultJSON{
		ID:          f.ID,
		ChangeSeq:   f.ChangeSeq,
		SourceID:    f.SourceID,
		SourceName:  sourceName,
		FaultKey:    f.FaultKey,
		Severity:    f.Severity,
		Title:       f.Title,
		State:       f.State,
		Incident:    f.Incident,
		OpenedAt:    f.OpenedAt,
		LastEventAt: f.LastEventAt,
		EventCount:  f.EventCount,
	}
	if f.Summary.Valid {
		j.Summary = &f.Summary.String
	}
	if f.ResolvedAt.Valid {
		j.ResolvedAt = &f.ResolvedAt.String
	}
	if f.ResolvedBy.Valid {
		j.ResolvedBy = &f.ResolvedBy.String
	}
	if f.ReadAt.Valid {
		j.ReadAt = &f.ReadAt.String
	}
	if f.MutedAt.Valid {
		j.MutedAt = &f.MutedAt.String
	}
	if f.MutedUntil.Valid {
		j.MutedUntil = &f.MutedUntil.String
	}
	return j
}

// toSourceJSON 将来源行转为管理视图对象（附 summary 扩展）。
func toSourceJSON(s *sourceRow, heartbeatSeconds int64) *sourceJSON {
	j := &sourceJSON{
		ID:        s.ID,
		Name:      s.Name,
		Kind:      s.Kind,
		Enabled:   s.Enabled,
		KeyHint:   s.KeyHint,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
	// status 语义（api-v1 §3）：feedback 来源无心跳判定，enabled 即 online；
	// device 来源以 heartbeatSeconds 内有接入为 online；任何来源禁用恒 offline。
	j.Status = "offline"
	if s.Enabled && s.Kind == "feedback" {
		j.Status = "online"
	}
	if s.LastSeenAt.Valid {
		j.LastSeenAt = &s.LastSeenAt.String
		if s.Enabled && s.Kind == "device" {
			if t, err := parseTS(s.LastSeenAt.String); err == nil {
				if time.Since(t) <= time.Duration(heartbeatSeconds)*time.Second {
					j.Status = "online"
				}
			}
		}
	}
	if s.AttachmentBaseURL.Valid {
		j.AttachmentBaseURL = &s.AttachmentBaseURL.String
	}
	if s.AgentVersion.Valid {
		j.AgentVersion = &s.AgentVersion.String
	}
	if s.Hostname.Valid {
		j.Hostname = &s.Hostname.String
	}
	j.Capabilities = map[string]string{}
	if s.Capabilities.Valid && s.Capabilities.String != "" {
		_ = json.Unmarshal([]byte(s.Capabilities.String), &j.Capabilities)
		if j.Capabilities == nil {
			j.Capabilities = map[string]string{}
		}
	}
	if s.LastSummary.Valid && s.LastSummary.String != "" {
		j.Summary = json.RawMessage(s.LastSummary.String)
	} else {
		j.Summary = json.RawMessage("null")
	}
	return j
}

// ---- 行加载 ----

const sourceCols = `id,name,kind,enabled,key_hash,key_plain,key_hint,attachment_base_url,
  agent_version,agent_os,agent_arch,hostname,capabilities,last_seen_at,last_metrics_seq,
  last_event_seq,last_boot_time,last_summary,prev_sample,deleted_at,created_at,updated_at`

func scanSource(row interface{ Scan(...any) error }) (*sourceRow, error) {
	var s sourceRow
	var enabled int
	err := row.Scan(&s.ID, &s.Name, &s.Kind, &enabled, &s.KeyHash, &s.KeyPlain, &s.KeyHint,
		&s.AttachmentBaseURL, &s.AgentVersion, &s.AgentOS, &s.AgentArch, &s.Hostname,
		&s.Capabilities, &s.LastSeenAt, &s.LastMetricsSeq, &s.LastEventSeq, &s.LastBootTime,
		&s.LastSummary, &s.PrevSample, &s.DeletedAt, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	s.Enabled = enabled != 0
	return &s, nil
}

func (st *store) loadSource(id string) (*sourceRow, error) {
	row := st.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE id=?`, id)
	return scanSource(row)
}

func (st *store) loadSourceByKeyHash(hash string) (*sourceRow, error) {
	row := st.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE key_hash=? AND deleted_at IS NULL`, hash)
	return scanSource(row)
}

func (st *store) listSources() ([]*sourceRow, error) {
	rows, err := st.db.Query(`SELECT ` + sourceCols + ` FROM sources WHERE deleted_at IS NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*sourceRow
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanMessage(row interface{ Scan(...any) error }) (*messageRow, error) {
	var m messageRow
	var ooo int
	err := row.Scan(&m.ID, &m.SourceID, &m.Kind, &m.Severity, &m.Title, &m.Body,
		&m.OccurredAt, &m.ReceivedAt, &m.ReadAt, &m.FaultID, &m.Incident, &m.Ref,
		&m.Attachments, &m.EventID, &m.EventSeq, &m.FaultKey, &ooo, &m.ChangeSeq)
	if err != nil {
		return nil, err
	}
	m.OutOfOrder = ooo != 0
	return &m, nil
}

const messageCols = `id,source_id,kind,severity,title,body,occurred_at,received_at,read_at,
  fault_id,incident,ref,attachments,event_id,event_seq,fault_key,out_of_order,change_seq`

func (st *store) loadMessage(id string) (*messageRow, error) {
	row := st.db.QueryRow(`SELECT `+messageCols+` FROM messages WHERE id=?`, id)
	return scanMessage(row)
}

func scanFault(row interface{ Scan(...any) error }) (*faultRow, error) {
	var f faultRow
	err := row.Scan(&f.ID, &f.SourceID, &f.FaultKey, &f.Severity, &f.Title, &f.Summary,
		&f.State, &f.Incident, &f.OpenedAt, &f.OpenedBySeq, &f.LastEventAt, &f.ResolvedAt,
		&f.ResolvedBySeq, &f.ResolvedBy, &f.EventCount, &f.ReadAt, &f.MutedAt, &f.MutedUntil,
		&f.ChangeSeq)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

const faultCols = `id,source_id,fault_key,severity,title,summary,state,incident,opened_at,
  opened_by_seq,last_event_at,resolved_at,resolved_by_seq,resolved_by,event_count,
  read_at,muted_at,muted_until,change_seq`

func (st *store) loadFault(id string) (*faultRow, error) {
	row := st.db.QueryRow(`SELECT `+faultCols+` FROM faults WHERE id=?`, id)
	return scanFault(row)
}

// loadFaultTx 在事务内按 (source,faultKey) 取故障行。
func loadFaultTx(tx *sql.Tx, sourceID, faultKey string) (*faultRow, error) {
	row := tx.QueryRow(`SELECT `+faultCols+` FROM faults WHERE source_id=? AND fault_key=?`, sourceID, faultKey)
	f, err := scanFault(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return f, err
}

// sourceName 提供对象序列化时的来源名（容忍已删除来源）。
func (st *store) sourceName(id string) string {
	var name string
	if err := st.db.QueryRow(`SELECT name FROM sources WHERE id=?`, id).Scan(&name); err != nil {
		return ""
	}
	return name
}

// ---- 规则与设置 ----

// defaultRules 为契约示例的默认规则集。
func defaultRules() []rule {
	str := func(s string) *string { return &s }
	f64 := func(v float64) *float64 { return &v }
	return []rule{
		{ID: "cpu_high", Kind: "threshold", Metric: "cpu_percent", Label: nil, Op: "gt",
			Value: f64(90), ForSeconds: 180, RecoverValue: f64(75), RecoverForSeconds: 60,
			Severity: "warning", Enabled: true},
		{ID: "mem_high", Kind: "threshold", Metric: "mem_percent", Label: nil, Op: "gt",
			Value: f64(90), ForSeconds: 180, RecoverValue: f64(80), RecoverForSeconds: 60,
			Severity: "warning", Enabled: true},
		{ID: "disk_high", Kind: "threshold", Metric: "disk_percent", Label: str("*"), Op: "gt",
			Value: f64(90), ForSeconds: 60, RecoverValue: f64(85), RecoverForSeconds: 60,
			Severity: "warning", Enabled: true},
		{ID: "container_exit", Kind: "container_exit", Match: str("*"), Severity: "warning", Enabled: true},
		{ID: "smart_failing", Kind: "smart", Severity: "critical", Enabled: true},
		{ID: "pool_error", Kind: "pool", Severity: "critical", Enabled: true},
	}
}

// loadRules 读取规则文档；库为空时写入默认集（version=1, heartbeat=180）。
func (st *store) loadRules() (*rulesDoc, error) {
	var d rulesDoc
	var js string
	err := st.db.QueryRow(`SELECT version, heartbeat_seconds, rules_json FROM rules WHERE id=1`).
		Scan(&d.Version, &d.HeartbeatSeconds, &js)
	if errors.Is(err, sql.ErrNoRows) {
		d = rulesDoc{Version: 1, HeartbeatSeconds: 180, Rules: defaultRules()}
		b, _ := json.Marshal(d.Rules)
		if _, err := st.db.Exec(
			`INSERT INTO rules(id, version, heartbeat_seconds, rules_json) VALUES(1,?,?,?)`,
			d.Version, d.HeartbeatSeconds, string(b)); err != nil {
			return nil, err
		}
		return &d, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(js), &d.Rules); err != nil {
		return nil, err
	}
	if d.Rules == nil {
		d.Rules = []rule{}
	}
	return &d, nil
}

// loadSettings 读取设置文档；库为空时写入默认值。
func (st *store) loadSettings() (*settingsDoc, error) {
	var d settingsDoc
	var js string
	err := st.db.QueryRow(`SELECT version, dnd_json, report_interval_seconds FROM settings WHERE id=1`).
		Scan(&d.Version, &js, &d.ReportIntervalSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		d = settingsDoc{
			Version:               1,
			DND:                   dndConfig{Enabled: false, Start: "23:00", End: "08:00", Timezone: "Asia/Shanghai"},
			ReportIntervalSeconds: 30,
		}
		b, _ := json.Marshal(d.DND)
		if _, err := st.db.Exec(
			`INSERT INTO settings(id, version, dnd_json, report_interval_seconds) VALUES(1,?,?,?)`,
			d.Version, string(b), d.ReportIntervalSeconds); err != nil {
			return nil, err
		}
		return &d, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(js), &d.DND); err != nil {
		return nil, err
	}
	return &d, nil
}

// heartbeatSeconds 快捷读取（心跳判定 / 来源状态共用）。
func (st *store) heartbeatSeconds() int64 {
	var v int64
	if err := st.db.QueryRow(`SELECT heartbeat_seconds FROM rules WHERE id=1`).Scan(&v); err != nil || v <= 0 {
		return 180
	}
	return v
}

// reportInterval 快捷读取。
func (st *store) reportInterval() int64 {
	var v int64
	if err := st.db.QueryRow(`SELECT report_interval_seconds FROM settings WHERE id=1`).Scan(&v); err != nil || v <= 0 {
		return 30
	}
	return v
}

// maxChangeSeq 返回当前最大 changeSeq（无变更时为 0）。
func (st *store) maxChangeSeq() int64 {
	var v sql.NullInt64
	_ = st.db.QueryRow(`SELECT MAX(seq) FROM changes`).Scan(&v)
	return v.Int64
}

// minChangeSeq 返回当前最小 changeSeq。
func (st *store) minChangeSeq() (int64, bool) {
	var v sql.NullInt64
	_ = st.db.QueryRow(`SELECT MIN(seq) FROM changes`).Scan(&v)
	return v.Int64, v.Valid
}

// publish 在事务提交后广播变更（供 SSE）。
func (st *store) publish(entries []*changeEntry) {
	if st.broker == nil {
		return
	}
	for _, e := range entries {
		st.broker.Publish(e)
	}
}

// withTx 在串行写事务内执行 fn；ctx 可为 nil。
func (st *store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
