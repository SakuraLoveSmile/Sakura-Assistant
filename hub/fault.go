package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// applyOutcome 记录一条事件应用的结果，供消息落库与变更广播使用。
type applyOutcome struct {
	msg          *messageRow
	fault        *faultRow // 受影响的故障（独立消息为 nil）
	faultChanged bool
	notify       *notifyBlock // nil = 不产生通知提示
}

// applyEventTx 在写事务内应用一条事件（来源或中枢自产）：
// 事件落库 → 故障状态机迁移 → 消息落库 → 变更行。
// internal=true 表示中枢自产事件（seq 由中枢分配，不参与去重 / 乱序判定）。
func (a *app) applyEventTx(tx *sql.Tx, src *sourceRow, ev *eventPayload, internal bool, outOfOrder bool, changes *[]*changeEntry) error {
	now := nowUTC()

	// 1) 事件行落库（hub 事件同样入 events，便于审计）。
	fk := sql.NullString{}
	if ev.FaultKey != nil && *ev.FaultKey != "" {
		fk = sql.NullString{String: *ev.FaultKey, Valid: true}
	}
	act := sql.NullString{}
	if ev.IncidentAction != nil && *ev.IncidentAction != "" {
		act = sql.NullString{String: *ev.IncidentAction, Valid: true}
	}
	body := sql.NullString{}
	if ev.Body != nil {
		body = sql.NullString{String: *ev.Body, Valid: true}
	}
	ref := sql.NullString{}
	if len(ev.Ref) > 0 && string(ev.Ref) != "null" {
		ref = sql.NullString{String: string(ev.Ref), Valid: true}
	}
	attsJSON := "[]"
	if len(ev.Attachments) > 0 {
		b, _ := json.Marshal(ev.Attachments)
		attsJSON = string(b)
	}
	internalInt := 0
	if internal {
		internalInt = 1
	}
	oooInt := 0
	if outOfOrder {
		oooInt = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO events(source_id,event_id,seq,kind,occurred_at,severity,fault_key,incident_action,
		   title,body,ref_json,attachments_json,out_of_order,internal,received_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		src.ID, ev.EventID, ev.Seq, ev.Kind, ev.OccurredAt, ev.Severity, fk, act,
		ev.Title, body, ref, attsJSON, oooInt, internalInt, fmtTS(now),
	); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}

	// 2) 故障状态机（仅当 faultKey 非空）。
	var f *faultRow
	var incidentApplied *int64
	notifyKind := ""
	faultChanged := false

	if fk.Valid {
		var err error
		f, err = loadFaultTx(tx, src.ID, fk.String)
		if err != nil {
			return err
		}
		action := ""
		if ev.IncidentAction != nil {
			action = *ev.IncidentAction
		}
		switch action {
		case "open", "update":
			pending, err := findPendingResolve(tx, src.ID, fk.String, ev.Seq)
			if err != nil {
				return err
			}
			if f == nil {
				f = newFaultRow(src.ID, fk.String, ev)
				if pending != nil {
					// 迟到的 open：seq 小于已记录 resolve → 落库即 resolved，不开通知。
					f.State = "resolved"
					f.ResolvedAt = sql.NullString{String: pending.occurredAt, Valid: true}
					f.ResolvedBySeq = sql.NullInt64{Int64: pending.seq, Valid: true}
					f.ResolvedBy = sql.NullString{String: pending.eventID, Valid: true}
				} else {
					notifyKind = "incident_open"
				}
				incidentApplied = &f.Incident
				faultChanged = true
			} else if f.State == "open" {
				// 同 faultKey 再 open / update → 合并当前开放轮次。
				mergeFaultEvent(f, ev)
				incidentApplied = &f.Incident
				notifyKind = "incident_update"
				faultChanged = true
			} else { // resolved
				if pending != nil {
					// 迟到 open 归并入已被待定恢复覆盖的轮次：维持 resolved。
					mergeFaultEvent(f, ev)
					incidentApplied = &f.Incident
					faultChanged = true
				} else {
					// 恢复后再发生 → 同一行 incident+1 重新 open。
					f.Incident++
					f.State = "open"
					f.Severity = ev.Severity
					f.Title = ev.Title
					f.Summary = body
					f.OpenedAt = ev.OccurredAt
					f.OpenedBySeq = ev.Seq
					f.LastEventAt = ev.OccurredAt
					f.ResolvedAt = sql.NullString{}
					f.ResolvedBySeq = sql.NullInt64{}
					f.ResolvedBy = sql.NullString{}
					f.EventCount = 1
					f.ReadAt = sql.NullString{} // 新轮次回到未读
					incidentApplied = &f.Incident
					notifyKind = "incident_open"
					faultChanged = true
				}
			}
		case "resolve":
			if f != nil && f.State == "open" && f.OpenedBySeq < ev.Seq {
				f.State = "resolved"
				f.ResolvedAt = sql.NullString{String: ev.OccurredAt, Valid: true}
				f.ResolvedBySeq = sql.NullInt64{Int64: ev.Seq, Valid: true}
				f.ResolvedBy = sql.NullString{String: ev.EventID, Valid: true}
				f.LastEventAt = ev.OccurredAt
				f.EventCount++
				f.Title = ev.Title
				f.Summary = body
				incidentApplied = &f.Incident
				notifyKind = "incident_resolved"
				faultChanged = true
			} else {
				// 先到的 resolve：记录为待定恢复，等迟到的 open。
				if _, err := tx.Exec(
					`INSERT OR IGNORE INTO pending_resolves(source_id,fault_key,seq,occurred_at,title,event_id)
					 VALUES(?,?,?,?,?,?)`,
					src.ID, fk.String, ev.Seq, ev.OccurredAt, ev.Title, ev.EventID,
				); err != nil {
					return err
				}
				// 消息仍关联到故障（若已有故障行），incident 为空。
			}
		default:
			return fmt.Errorf("unknown incidentAction %q", action)
		}
	}

	// 3) 消息行。
	msgKind := ev.Kind
	if !internal && act.Valid && act.String == "open" {
		// api-v1 §2：来源侧 incidentAction=open 的事件产生 fault_open 消息；
		// 中枢自产 open 事件保留自身 kind（fixtures: container_exit）。
		msgKind = "fault_open"
	}
	m := &messageRow{
		ID:          newID("msg_"),
		SourceID:    src.ID,
		Kind:        msgKind,
		Severity:    ev.Severity,
		Title:       ev.Title,
		Body:        body,
		OccurredAt:  ev.OccurredAt,
		ReceivedAt:  fmtTS(now),
		Ref:         ref,
		Attachments: sql.NullString{String: attsJSON, Valid: true},
		EventID:     ev.EventID,
		EventSeq:    ev.Seq,
		FaultKey:    fk,
		OutOfOrder:  outOfOrder,
	}
	if f != nil {
		m.FaultID = sql.NullString{String: f.ID, Valid: true}
	}
	if incidentApplied != nil {
		m.Incident = sql.NullInt64{Int64: *incidentApplied, Valid: true}
	}

	// 4) 变更：先 fault 后 message（客户端先拿到故障再拿消息）。
	srcName := src.Name
	if f != nil && faultChanged {
		fj := toFaultJSON(f, srcName)
		entry, err := insertChangeObj(tx, "fault", f.ID,
			func(seq int64) { fj.ChangeSeq = seq; f.ChangeSeq = seq }, fj, nil)
		if err != nil {
			return err
		}
		*changes = append(*changes, entry)
	}
	if f != nil && faultChanged {
		if err := upsertFaultTx(tx, f); err != nil {
			return err
		}
	}

	// 5) 迟到 open 直接 resolved 时，回填待定恢复期间产生的消息归属。
	if f != nil && f.State == "resolved" && f.ResolvedBySeq.Valid && incidentApplied != nil {
		if _, err := tx.Exec(
			`UPDATE messages SET fault_id=?, incident=?
			 WHERE source_id=? AND fault_key=? AND fault_id IS NULL
			   AND event_seq >= ? AND event_seq <= ?`,
			f.ID, f.Incident, src.ID, fk.String, f.OpenedBySeq, f.ResolvedBySeq.Int64,
		); err != nil {
			return err
		}
	}

	// 独立消息（无故障状态迁移）同样携带 notify{kind:new_message}（events.md §4 通知触发点）。
	// 已读/静音等维护性变更不经此路径，故不会产生误通知。
	if notifyKind == "" {
		notifyKind = "new_message"
	}
	muted := f != nil && f.MutedAt.Valid
	var fid *string
	if f != nil {
		v := f.ID
		fid = &v
	}
	notify := &notifyBlock{Kind: notifyKind, FaultID: fid, Muted: muted}
	mj := toMessageJSON(m, srcName)
	entry, err := insertChangeObj(tx, "message", m.ID,
		func(seq int64) { mj.ChangeSeq = seq; m.ChangeSeq = seq }, mj, notify)
	if err != nil {
		return err
	}
	*changes = append(*changes, entry)

	if _, err := tx.Exec(
		`INSERT INTO messages(`+messageCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.SourceID, m.Kind, m.Severity, m.Title, m.Body, m.OccurredAt, m.ReceivedAt,
		m.ReadAt, m.FaultID, m.Incident, m.Ref, m.Attachments, m.EventID, m.EventSeq,
		m.FaultKey, oooInt, m.ChangeSeq,
	); err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// newFaultRow 以 open/update 事件创建新故障行（incident=1）。
func newFaultRow(sourceID, faultKey string, ev *eventPayload) *faultRow {
	body := sql.NullString{}
	if ev.Body != nil {
		body = sql.NullString{String: *ev.Body, Valid: true}
	}
	return &faultRow{
		ID:          newID("flt_"),
		SourceID:    sourceID,
		FaultKey:    faultKey,
		Severity:    ev.Severity,
		Title:       ev.Title,
		Summary:     body,
		State:       "open",
		Incident:    1,
		OpenedAt:    ev.OccurredAt,
		OpenedBySeq: ev.Seq,
		LastEventAt: ev.OccurredAt,
		EventCount:  1,
	}
}

// mergeFaultEvent 将事件合并进当前开放轮次：eventCount+1、severity 取 max、
// title/summary 刷新为最新事件、lastEventAt 更新。
func mergeFaultEvent(f *faultRow, ev *eventPayload) {
	f.EventCount++
	f.Severity = maxSev(f.Severity, ev.Severity)
	f.Title = ev.Title
	if ev.Body != nil {
		f.Summary = sql.NullString{String: *ev.Body, Valid: true}
	} else {
		f.Summary = sql.NullString{}
	}
	if ev.OccurredAt > f.LastEventAt {
		f.LastEventAt = ev.OccurredAt
	}
}

// upsertFaultTx 写入故障行。
func upsertFaultTx(tx *sql.Tx, f *faultRow) error {
	_, err := tx.Exec(
		`INSERT INTO faults(`+faultCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   severity=excluded.severity, title=excluded.title, summary=excluded.summary,
		   state=excluded.state, incident=excluded.incident, opened_at=excluded.opened_at,
		   opened_by_seq=excluded.opened_by_seq, last_event_at=excluded.last_event_at,
		   resolved_at=excluded.resolved_at, resolved_by_seq=excluded.resolved_by_seq,
		   resolved_by=excluded.resolved_by, event_count=excluded.event_count,
		   read_at=excluded.read_at, muted_at=excluded.muted_at, muted_until=excluded.muted_until,
		   change_seq=excluded.change_seq`,
		f.ID, f.SourceID, f.FaultKey, f.Severity, f.Title, f.Summary, f.State, f.Incident,
		f.OpenedAt, f.OpenedBySeq, f.LastEventAt, f.ResolvedAt, f.ResolvedBySeq, f.ResolvedBy,
		f.EventCount, f.ReadAt, f.MutedAt, f.MutedUntil, f.ChangeSeq,
	)
	return err
}

// pendingResolve 为待定恢复记录。
type pendingResolve struct {
	seq        int64
	occurredAt string
	title      string
	eventID    string
}

// findPendingResolve 查找 seq > openSeq 的最早待定恢复（用于迟到 open 判定）。
func findPendingResolve(tx *sql.Tx, sourceID, faultKey string, openSeq int64) (*pendingResolve, error) {
	var p pendingResolve
	err := tx.QueryRow(
		`SELECT seq, occurred_at, title, event_id FROM pending_resolves
		 WHERE source_id=? AND fault_key=? AND seq > ? ORDER BY seq LIMIT 1`,
		sourceID, faultKey, openSeq,
	).Scan(&p.seq, &p.occurredAt, &p.title, &p.eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// hubSeqBase 为中枢自产事件 seq 的起点（远超来源 seq 实际可达范围）。
// hub_* 事件与来源 seq 空间互斥：sources.last_event_seq 只追踪来源序列，
// 中枢插入不再使后续来源事件被误标 outOfOrder，ack 的 lastSeq 也回到
// 「最近应用的来源 seq」语义。
const hubSeqBase = int64(1) << 62

// nextHubSeqTx 分配全局单调递增的 hub 事件 seq（meta 表承载，免 schema 变更）。
func nextHubSeqTx(tx *sql.Tx) (int64, error) {
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO meta(key,value) VALUES('hub_event_seq',?)`, hubSeqBase); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`UPDATE meta SET value = CAST(value AS INTEGER) + 1 WHERE key='hub_event_seq'`); err != nil {
		return 0, err
	}
	var seq int64
	if err := tx.QueryRow(
		`SELECT CAST(value AS INTEGER) FROM meta WHERE key='hub_event_seq'`).Scan(&seq); err != nil {
		return 0, err
	}
	return seq, nil
}

// emitHubEventTx 生成一条中枢自产事件并应用（写事务内）。
// seq 取自独立的 hub 序列（hubSeqBase 起），eventId 形如 hub_<ulid>。
func (a *app) emitHubEventTx(tx *sql.Tx, src *sourceRow, kind, severity string,
	faultKey *string, action *string, title string, body *string, occurredAt time.Time,
	changes *[]*changeEntry) error {

	seq, err := nextHubSeqTx(tx)
	if err != nil {
		return err
	}
	ev := &eventPayload{
		EventID:        newID("hub_"),
		Seq:            seq,
		Kind:           kind,
		OccurredAt:     fmtTS(occurredAt),
		Severity:       severity,
		FaultKey:       faultKey,
		IncidentAction: action,
		Title:          title,
		Body:           body,
	}
	return a.applyEventTx(tx, src, ev, true, false, changes)
}

// touchSourceSeenTx 刷新来源 lastSeenAt（任何接入流量），并处理心跳恢复：
// 若存在开放的 heartbeat 故障 → 生成 heartbeat_back 事件关闭它。
func (a *app) touchSourceSeenTx(tx *sql.Tx, src *sourceRow, at time.Time, changes *[]*changeEntry) error {
	src.LastSeenAt = sql.NullString{String: fmtTS(at), Valid: true}
	if _, err := tx.Exec(`UPDATE sources SET last_seen_at=? WHERE id=?`, src.LastSeenAt.String, src.ID); err != nil {
		return err
	}
	// 心跳故障恢复（任何接入均恢复）。
	hb, err := loadFaultTx(tx, src.ID, "heartbeat")
	if err != nil {
		return err
	}
	if hb != nil && hb.State == "open" {
		fk := "heartbeat"
		act := "resolve"
		title := fmt.Sprintf("%s 恢复接入", src.Name)
		body := fmt.Sprintf("来源恢复上报（%s）", src.Kind)
		if err := a.emitHubEventTx(tx, src, "heartbeat_back", "info", &fk, &act,
			title, &body, at, changes); err != nil {
			return err
		}
	}
	return nil
}
