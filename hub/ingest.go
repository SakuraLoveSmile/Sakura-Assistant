package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"time"
	"unicode/utf8"
)

// loadSourceTx 在事务内取来源行（读最新 seq 等）。
func loadSourceTx(tx *sql.Tx, id string) (*sourceRow, error) {
	row := tx.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE id=?`, id)
	return scanSource(row)
}

// emitSourceChangeTx 生成 source 类变更（完整 Source 对象）。
func (a *app) emitSourceChangeTx(tx *sql.Tx, src *sourceRow, changes *[]*changeEntry) error {
	sj := toSourceJSON(src, a.st.heartbeatSeconds())
	entry, err := insertChangeObj(tx, "source", src.ID, func(int64) {}, sj, nil)
	if err != nil {
		return err
	}
	*changes = append(*changes, entry)
	return nil
}

// ---- POST /api/v1/ingest/metrics ----

func (a *app) handleIngestMetrics(w http.ResponseWriter, r *http.Request, src *sourceRow) {
	var req ingestMetricsReq
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Sample == nil || req.Sample.TS == "" {
		errInvalid(w, "sample.ts 必填")
		return
	}
	if _, err := parseTS(req.Sample.TS); err != nil {
		errInvalid(w, "sample.ts 非法")
		return
	}

	arrival := nowUTC()
	var changes []*changeEntry
	duplicate := false

	err := a.st.withTx(r.Context(), func(tx *sql.Tx) error {
		cur, err := loadSourceTx(tx, src.ID)
		if err != nil {
			return err
		}
		// 任何接入调用都刷新 lastSeenAt（并按需恢复 heartbeat 故障），重复批次亦然。
		if err := a.touchSourceSeenTx(tx, cur, arrival, &changes); err != nil {
			return err
		}
		// 幂等：seq ≤ 已应用最大 seq → 整批丢弃（仍算一次接入）。
		if req.Seq <= cur.LastMetricsSeq {
			duplicate = true
			fresh, err := loadSourceTx(tx, cur.ID)
			if err != nil {
				return err
			}
			return a.emitSourceChangeTx(tx, fresh, &changes)
		}

		// 样本落库：(sourceId, ts) 唯一，重放天然幂等。
		sampleJSON, _ := json.Marshal(req.Sample)
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO metrics_samples(source_id, ts, received_at, sample_json)
			 VALUES(?,?,?,?)`,
			cur.ID, req.Sample.TS, fmtTS(arrival), string(sampleJSON),
		); err != nil {
			return err
		}
		if err := insertRawMetrics(tx, cur.ID, req.Sample); err != nil {
			return err
		}

		// bootTime 变化 → host_reboot 独立消息。
		if req.Sample.BootTime != nil && *req.Sample.BootTime != "" {
			if !cur.LastBootTime.Valid || cur.LastBootTime.String != *req.Sample.BootTime {
				if cur.LastBootTime.Valid {
					body := fmt.Sprintf("bootTime %s → %s", cur.LastBootTime.String, *req.Sample.BootTime)
					if err := a.emitHubEventTx(tx, cur, "host_reboot", "info", nil, nil,
						fmt.Sprintf("%s 已重启", cur.Name), &body, arrival, &changes); err != nil {
						return err
					}
				}
				if _, err := tx.Exec(`UPDATE sources SET last_boot_time=? WHERE id=?`,
					*req.Sample.BootTime, cur.ID); err != nil {
					return err
				}
			}
		}

		// agent / capabilities / summary / seq 更新。
		summary := buildSummary(req.Sample)
		capJSON := sql.NullString{}
		if req.Capabilities != nil {
			b, _ := json.Marshal(req.Capabilities)
			capJSON = sql.NullString{String: string(b), Valid: true}
		}
		if _, err := tx.Exec(
			`UPDATE sources SET last_metrics_seq=?, last_summary=?, updated_at=? WHERE id=?`,
			req.Seq, string(summary), fmtTS(arrival), cur.ID); err != nil {
			return err
		}
		if req.Agent != nil {
			if _, err := tx.Exec(
				`UPDATE sources SET agent_version=?, agent_os=?, agent_arch=?, hostname=? WHERE id=?`,
				nullStr(req.Agent.Version), nullStr(req.Agent.OS), nullStr(req.Agent.Arch),
				nullStr(req.Agent.Hostname), cur.ID); err != nil {
				return err
			}
		}
		if capJSON.Valid {
			if _, err := tx.Exec(`UPDATE sources SET capabilities=? WHERE id=?`, capJSON.String, cur.ID); err != nil {
				return err
			}
		}
		cur.LastMetricsSeq = req.Seq
		cur.LastSummary = sql.NullString{String: string(summary), Valid: true}
		if capJSON.Valid {
			cur.Capabilities = capJSON
		}

		// 规则引擎：输入最新样本（prev_sample 提供上一拍）。
		rules, err := a.st.loadRules()
		if err != nil {
			return err
		}
		if err := a.re.evalSample(tx, cur, rules, req.Sample, arrival, &changes); err != nil {
			return err
		}

		// 保存本拍样本供下拍迁移判定。
		if _, err := tx.Exec(`UPDATE sources SET prev_sample=? WHERE id=?`, string(sampleJSON), cur.ID); err != nil {
			return err
		}

		// 来源变更广播。
		fresh, err := loadSourceTx(tx, cur.ID)
		if err != nil {
			return err
		}
		return a.emitSourceChangeTx(tx, fresh, &changes)
	})
	if err != nil {
		errInternal(w)
		return
	}
	a.st.publish(changes)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted":              !duplicate,
		"duplicate":             duplicate,
		"serverTime":            fmtTS(nowUTC()),
		"reportIntervalSeconds": a.st.reportInterval(),
	})
}

// insertRawMetrics 将样本展开为时序行（缺席字段不产生行）。
func insertRawMetrics(tx *sql.Tx, sourceID string, s *samplePayload) error {
	ins := func(metric, label string, v float64) error {
		_, err := tx.Exec(
			`INSERT OR REPLACE INTO metrics_raw(source_id, ts, metric, label, value)
			 VALUES(?,?,?,?,?)`, sourceID, s.TS, metric, label, v)
		return err
	}
	if s.CPUPercent != nil {
		if err := ins("cpu_percent", "", *s.CPUPercent); err != nil {
			return err
		}
	}
	if s.MemPercent != nil {
		if err := ins("mem_percent", "", *s.MemPercent); err != nil {
			return err
		}
	}
	for _, d := range s.Disks {
		if d.Percent != nil {
			if err := ins("disk_percent", "mount="+d.Mount, *d.Percent); err != nil {
				return err
			}
		}
	}
	if s.Net != nil {
		if s.Net.RxBps != nil {
			if err := ins("net_rx_bps", "", *s.Net.RxBps); err != nil {
				return err
			}
		}
		if s.Net.TxBps != nil {
			if err := ins("net_tx_bps", "", *s.Net.TxBps); err != nil {
				return err
			}
		}
	}
	return nil
}

// buildSummary 生成 overview 用的最近样本摘要。
func buildSummary(s *samplePayload) []byte {
	type diskSum struct {
		Mount   string  `json:"mount"`
		Percent float64 `json:"percent"`
	}
	sum := map[string]any{"ts": s.TS}
	if s.CPUPercent != nil {
		sum["cpuPercent"] = *s.CPUPercent
	}
	if s.MemPercent != nil {
		sum["memPercent"] = *s.MemPercent
	}
	disks := []diskSum{}
	for _, d := range s.Disks {
		if d.Percent != nil {
			disks = append(disks, diskSum{d.Mount, *d.Percent})
		}
	}
	sum["disks"] = disks
	if s.UptimeSeconds != nil {
		sum["uptimeSeconds"] = *s.UptimeSeconds
	}
	b, _ := json.Marshal(sum)
	return b
}

// ---- POST /api/v1/ingest/events ----

var eventIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

const maxBackfillAge = 90 * 24 * time.Hour // occurredAt 补传窗口

// validateEvent 校验单条事件；返回错误描述（空 = 合法）。
func validateEvent(ev *eventPayload) string {
	if !eventIDRe.MatchString(ev.EventID) {
		return "eventId 缺失或非法（[A-Za-z0-9._:-]{1,128}）"
	}
	if ev.Kind == "" {
		return "kind 必填"
	}
	if _, err := parseTS(ev.OccurredAt); err != nil {
		return "occurredAt 缺失或非法"
	}
	if t, _ := parseTS(ev.OccurredAt); time.Since(t) > maxBackfillAge {
		return "occurredAt 超出 90 天补传窗口"
	}
	switch ev.Severity {
	case "info", "warning", "critical":
	default:
		return "severity 非法"
	}
	if ev.Title == "" || utf8.RuneCountInString(ev.Title) > 200 {
		return "title 缺失或超过 200 字符"
	}
	if ev.Body != nil && utf8.RuneCountInString(*ev.Body) > 20000 {
		return "body 超过 20000 字符"
	}
	hasFK := ev.FaultKey != nil && *ev.FaultKey != ""
	hasAct := ev.IncidentAction != nil && *ev.IncidentAction != ""
	if hasAct {
		switch *ev.IncidentAction {
		case "open", "update", "resolve":
		default:
			return "incidentAction 非法"
		}
	}
	if hasFK && !hasAct {
		return "faultKey 非空时 incidentAction 必填"
	}
	if !hasFK && hasAct {
		return "incidentAction 需要非空 faultKey"
	}
	return ""
}

func (a *app) handleIngestEvents(w http.ResponseWriter, r *http.Request, src *sourceRow) {
	var req ingestEventsReq
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Events) > 100 {
		errInvalid(w, "单批 events 超过 100 条")
		return
	}

	arrival := nowUTC()
	var changes []*changeEntry
	var lastSeq int64

	type item struct {
		idx int
		ev  *eventPayload
	}
	var valid []*item
	type rej struct {
		EventID string `json:"eventId"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	rejected := []rej{}
	acceptedIDs := map[int]bool{}
	dupIDs := map[int]bool{}

	err := a.st.withTx(r.Context(), func(tx *sql.Tx) error {
		cur, err := loadSourceTx(tx, src.ID)
		if err != nil {
			return err
		}
		if err := a.touchSourceSeenTx(tx, cur, arrival, &changes); err != nil {
			return err
		}

		// 先校验 + 去重，再按 seq 排序应用。
		seen := map[string]bool{}
		for i := range req.Events {
			ev := &req.Events[i]
			if msg := validateEvent(ev); msg != "" {
				rejected = append(rejected, rej{ev.EventID, "invalid_request", msg})
				continue
			}
			if seen[ev.EventID] {
				dupIDs[i] = true // 批内重复 eventId：首条已收，其余算重复投递
				continue
			}
			var exists int
			if err := tx.QueryRow(
				`SELECT COUNT(1) FROM events WHERE source_id=? AND event_id=?`,
				cur.ID, ev.EventID).Scan(&exists); err != nil {
				return err
			}
			if exists > 0 {
				dupIDs[i] = true
				continue
			}
			seen[ev.EventID] = true
			valid = append(valid, &item{i, ev})
		}
		sort.Slice(valid, func(i, j int) bool { return valid[i].ev.Seq < valid[j].ev.Seq })

		for _, it := range valid {
			outOfOrder := it.ev.Seq <= cur.LastEventSeq
			if err := a.applyEventTx(tx, cur, it.ev, false, outOfOrder, &changes); err != nil {
				return fmt.Errorf("apply event %s: %w", it.ev.EventID, err)
			}
			if it.ev.Seq > cur.LastEventSeq {
				cur.LastEventSeq = it.ev.Seq
			}
			acceptedIDs[it.idx] = true
		}
		if _, err := tx.Exec(`UPDATE sources SET last_event_seq=? WHERE id=?`,
			cur.LastEventSeq, cur.ID); err != nil {
			return err
		}
		lastSeq = cur.LastEventSeq

		fresh, err := loadSourceTx(tx, cur.ID)
		if err != nil {
			return err
		}
		return a.emitSourceChangeTx(tx, fresh, &changes)
	})
	if err != nil {
		errInternal(w)
		return
	}
	a.st.publish(changes)

	accepted := []string{}
	duplicates := []string{}
	for i, ev := range req.Events {
		switch {
		case acceptedIDs[i]:
			accepted = append(accepted, ev.EventID)
		case dupIDs[i]:
			duplicates = append(duplicates, ev.EventID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":   accepted,
		"duplicates": duplicates,
		"rejected":   rejected,
		"lastSeq":    lastSeq,
		"serverTime": fmtTS(nowUTC()),
	})
}

// nullStr 辅助：空串 → NULL。
func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// faultOpen 读取来源某 faultKey 是否有开放轮次（事务外便捷版）。
func (st *store) faultOpen(sourceID, faultKey string) (bool, error) {
	var n int
	err := st.db.QueryRow(
		`SELECT COUNT(1) FROM faults WHERE source_id=? AND fault_key=? AND state='open'`,
		sourceID, faultKey).Scan(&n)
	return n > 0, err
}
