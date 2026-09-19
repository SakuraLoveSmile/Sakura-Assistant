package main

import (
	"database/sql"
	"encoding/json"
	"time"
)

// runRollup 将 metrics_raw 聚合到 5 分钟桶（metrics_5m）。
// 对近期桶重算并 REPLACE，幂等可重入（覆盖迟到样本的回填窗口）。
func (a *app) runRollup(now time.Time) error {
	earliest := now.Truncate(5 * time.Minute).Add(-24 * time.Hour)
	return a.st.withTx(nil, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT OR REPLACE INTO metrics_5m(source_id, bucket, metric, label, avg, min, max, n)
			 SELECT source_id,
			       strftime('%Y-%m-%dT%H:%M:00.000Z',
			         datetime(ts, '-' || (CAST(strftime('%M', ts) AS INTEGER) % 5) || ' minutes'))
			       AS bucket,
			       metric, label,
			       AVG(value), MIN(value), MAX(value), COUNT(1)
			 FROM metrics_raw
			 WHERE ts >= ?
			 GROUP BY source_id, metric, label, bucket`, fmtTS(earliest))
		return err
	})
}

// runRetention 执行保留策略：
// raw 7d、5m 汇总 90d、已结束消息/已恢复故障 90d（tombstone）、
// 未恢复故障永不清理、changes 窗口 ≥7d 且 ≥100k。
func (a *app) runRetention(now time.Time) error {
	rawCut := fmtTS(now.Add(-time.Duration(a.cfg.rawDays) * 24 * time.Hour))
	rollCut := fmtTS(now.Add(-time.Duration(a.cfg.rollupDays) * 24 * time.Hour))
	msgCut := fmtTS(now.Add(-time.Duration(a.cfg.messagesDays) * 24 * time.Hour))
	chgCut := fmtTS(now.Add(-time.Duration(a.cfg.changesDays) * 24 * time.Hour))

	var changes []*changeEntry
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM metrics_raw WHERE ts < ?`, rawCut); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM metrics_5m WHERE bucket < ?`, rollCut); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM metrics_samples WHERE ts < ?`, rawCut); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM events WHERE received_at < ?`, msgCut); err != nil {
			return err
		}
		// 超期仍未等到 open 的待定恢复（与消息同保留期）：属永久残行，清掉。
		if _, err := tx.Exec(`DELETE FROM pending_resolves WHERE occurred_at < ?`, msgCut); err != nil {
			return err
		}

		// 已恢复且超过保留期的故障 → tombstone + 删除其消息。
		frows, err := tx.Query(
			`SELECT id, source_id, fault_key FROM faults WHERE state='resolved' AND resolved_at < ?`, msgCut)
		if err != nil {
			return err
		}
		type fid struct{ id, src, key string }
		var fids []fid
		for frows.Next() {
			var f fid
			if err := frows.Scan(&f.id, &f.src, &f.key); err != nil {
				frows.Close()
				return err
			}
			fids = append(fids, f)
		}
		frows.Close()
		for _, f := range fids {
			// 该故障的消息 tombstone。
			mrows, err := tx.Query(`SELECT id FROM messages WHERE fault_id=?`, f.id)
			if err != nil {
				return err
			}
			var mids []string
			for mrows.Next() {
				var id string
				if err := mrows.Scan(&id); err != nil {
					mrows.Close()
					return err
				}
				mids = append(mids, id)
			}
			mrows.Close()
			for _, mid := range mids {
				e, err := insertTombstone(tx, "message", mid)
				if err != nil {
					return err
				}
				changes = append(changes, e)
				if _, err := tx.Exec(`DELETE FROM messages WHERE id=?`, mid); err != nil {
					return err
				}
			}
			e, err := insertTombstone(tx, "fault", f.id)
			if err != nil {
				return err
			}
			changes = append(changes, e)
			if _, err := tx.Exec(`DELETE FROM faults WHERE id=?`, f.id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM pending_resolves WHERE source_id=? AND fault_key=?`,
				f.src, f.key); err != nil {
				return err
			}
		}

		// 独立消息（无故障归属）超期 → tombstone + 删除。
		mrows, err := tx.Query(
			`SELECT id FROM messages WHERE fault_id IS NULL AND received_at < ?`, msgCut)
		if err != nil {
			return err
		}
		var mids []string
		for mrows.Next() {
			var id string
			if err := mrows.Scan(&id); err != nil {
				mrows.Close()
				return err
			}
			mids = append(mids, id)
		}
		mrows.Close()
		for _, mid := range mids {
			e, err := insertTombstone(tx, "message", mid)
			if err != nil {
				return err
			}
			changes = append(changes, e)
			if _, err := tx.Exec(`DELETE FROM messages WHERE id=?`, mid); err != nil {
				return err
			}
		}

		// 变更窗口：仅当既超过 7 天又超出最近 100k 时才清除。
		var maxSeq sql.NullInt64
		if err := tx.QueryRow(`SELECT MAX(seq) FROM changes`).Scan(&maxSeq); err != nil {
			return err
		}
		if maxSeq.Valid {
			cutoffSeq := maxSeq.Int64 - a.cfg.changesMinSeq
			if _, err := tx.Exec(
				`DELETE FROM changes WHERE created_at < ? AND seq <= ?`, chgCut, cutoffSeq); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		a.st.publish(changes)
	}
	return err
}

// insertTombstone 写一条 tombstone 变更 {type,id}。
func insertTombstone(tx *sql.Tx, typ, id string) (*changeEntry, error) {
	data, _ := json.Marshal(map[string]string{"type": typ, "id": id})
	return insertChange(tx, "tombstone", id, data, nil)
}

// rollupLoop / retentionLoop 为后台周期任务。
func (a *app) rollupLoop(done <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			if err := a.runRollup(now.UTC()); err != nil {
				logLine("rollup error: %v", err)
			}
		}
	}
}

func (a *app) retentionLoop(done <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			if err := a.runRetention(now.UTC()); err != nil {
				logLine("retention error: %v", err)
			}
		}
	}
}
