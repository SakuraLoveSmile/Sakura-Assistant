// Package queue 提供 SQLite 持久队列：指标批次与事件分开排队，
// seq 单调持久化（重启续号），溢出丢最旧并累计 overflow 记录，
// 进程重启不丢队列。契约依据：contracts/events.md §5、api-v1.md §2。
package queue

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	dbFileName = "agent-queue.db"

	// KindMetrics / KindEvents 为 overflow 记录与日志中的队列名。
	KindMetrics = "metrics"
	KindEvents  = "events"
)

// Queue 为持久队列。所有方法可并发调用（内部单连接串行化）。
type Queue struct {
	db        *sql.DB
	maxBatch  int64 // 指标批次数上限
	maxEvents int64 // 事件条数上限
	now       func() time.Time
}

// MetricsRow 为一条排队指标批次（payload 已是完整 ingest/metrics JSON）。
type MetricsRow struct {
	ID        int64
	Seq       int64
	Payload   []byte
	CreatedAt time.Time
}

// EventRow 为一条排队事件（payload 为单个事件 JSON）。
type EventRow struct {
	ID        int64
	Seq       int64
	Payload   []byte
	CreatedAt time.Time
}

// Overflow 为累计溢出记录：恢复连通后汇总为一条 queue_overflow 事件。
type Overflow struct {
	Dropped int64
	FirstAt time.Time // 最早被丢条目的入队时刻
	LastAt  time.Time // 最近被丢条目的入队时刻
}

// Open 打开（或创建）dir 下的队列库。
func Open(dir string, maxBatches, maxEvents int) (*Queue, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("queue dir: %w", err)
	}
	path := filepath.Join(dir, dbFileName)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者；串行化简化顺序语义
	for _, stmt := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS meta (
			k TEXT PRIMARY KEY,
			v INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS metrics_q (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			seq INTEGER NOT NULL,
			payload BLOB NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS events_q (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			seq INTEGER NOT NULL,
			payload BLOB NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS overflow (
			kind TEXT PRIMARY KEY,
			dropped INTEGER NOT NULL,
			first_at INTEGER NOT NULL,
			last_at INTEGER NOT NULL
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("queue schema: %w", err)
		}
	}
	return &Queue{
		db:        db,
		maxBatch:  int64(maxBatches),
		maxEvents: int64(maxEvents),
		now:       time.Now,
	}, nil
}

// Close 关闭库。进程退出前调用；已入队数据不受影响。
func (q *Queue) Close() error {
	return q.db.Close()
}

// nextSeq 分配指定流的下一个序号（meta 表持久化，重启续号）。
func (q *Queue) nextSeq(key string) (int64, error) {
	var v int64
	err := q.db.QueryRow(
		`INSERT INTO meta(k, v) VALUES(?, 1)
		 ON CONFLICT(k) DO UPDATE SET v = v + 1
		 RETURNING v`, key).Scan(&v)
	return v, err
}

// NextMetricsSeq 返回下一指标批次序号（单调递增，重启不清零）。
func (q *Queue) NextMetricsSeq() (int64, error) {
	return q.nextSeq("metrics_seq")
}

// NextEventSeq 返回下一事件序号（事件流独立计数）。
func (q *Queue) NextEventSeq() (int64, error) {
	return q.nextSeq("events_seq")
}

// EnqueueMetrics 入队一个指标批次；超出上限丢最旧并累计 overflow 记录。
// 返回本次入队导致的丢弃条数。
func (q *Queue) EnqueueMetrics(seq int64, payload []byte) (int64, error) {
	return q.enqueue("metrics_q", KindMetrics, q.maxBatch, seq, payload)
}

// EnqueueEvent 入队一个事件；超出上限丢最旧并累计 overflow 记录。
func (q *Queue) EnqueueEvent(seq int64, payload []byte) (int64, error) {
	return q.enqueue("events_q", KindEvents, q.maxEvents, seq, payload)
}

// EnqueueEventNoTrim 入队系统自产事件（queue_overflow 等汇总事件）：
// 不裁剪队列——汇总事件本身就是丢弃的凭证，再为它丢条目会自我循环。
func (q *Queue) EnqueueEventNoTrim(seq int64, payload []byte) error {
	_, err := q.db.Exec(
		`INSERT INTO events_q(seq, payload, created_at) VALUES(?, ?, ?)`,
		seq, payload, q.now().UnixNano())
	return err
}

func (q *Queue) enqueue(table, kind string, max, seq int64, payload []byte) (int64, error) {
	tx, err := q.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO `+table+`(seq, payload, created_at) VALUES(?, ?, ?)`,
		seq, payload, q.now().UnixNano()); err != nil {
		return 0, err
	}

	var dropped int64
	var count int64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		return 0, err
	}
	if count > max {
		excess := count - max
		var firstNs, lastNs int64
		if err := tx.QueryRow(
			`SELECT MIN(created_at), MAX(created_at) FROM (
				SELECT created_at FROM `+table+` ORDER BY id ASC LIMIT ?)`, excess,
		).Scan(&firstNs, &lastNs); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(
			`DELETE FROM `+table+` WHERE id IN (
				SELECT id FROM `+table+` ORDER BY id ASC LIMIT ?)`, excess); err != nil {
			return 0, err
		}
		if err := addOverflow(tx, kind, excess, firstNs, lastNs); err != nil {
			return 0, err
		}
		dropped = excess
	}
	return dropped, tx.Commit()
}

// addOverflow 累计溢出记录（同 kind 多次溢出合并为一个时间窗）。
func addOverflow(tx *sql.Tx, kind string, dropped, firstNs, lastNs int64) error {
	_, err := tx.Exec(
		`INSERT INTO overflow(kind, dropped, first_at, last_at) VALUES(?, ?, ?, ?)
		 ON CONFLICT(kind) DO UPDATE SET
			dropped = dropped + excluded.dropped,
			first_at = MIN(first_at, excluded.first_at),
			last_at = MAX(last_at, excluded.last_at)`,
		kind, dropped, firstNs, lastNs)
	return err
}

// TakeOverflow 取出并清除指定 kind 的累计溢出记录。
func (q *Queue) TakeOverflow(kind string) (Overflow, bool, error) {
	var ov Overflow
	var firstNs, lastNs int64
	err := q.db.QueryRow(
		`SELECT dropped, first_at, last_at FROM overflow WHERE kind = ?`, kind,
	).Scan(&ov.Dropped, &firstNs, &lastNs)
	if errors.Is(err, sql.ErrNoRows) {
		return ov, false, nil
	}
	if err != nil {
		return ov, false, err
	}
	if _, err := q.db.Exec(`DELETE FROM overflow WHERE kind = ?`, kind); err != nil {
		return ov, false, err
	}
	ov.FirstAt = time.Unix(0, firstNs)
	ov.LastAt = time.Unix(0, lastNs)
	return ov, true, nil
}

// MetricsPending / EventsPending 返回排队条数。
func (q *Queue) MetricsPending() (int64, error) {
	return q.pending("metrics_q")
}

// EventsPending 返回排队事件条数。
func (q *Queue) EventsPending() (int64, error) {
	return q.pending("events_q")
}

func (q *Queue) pending(table string) (int64, error) {
	var n int64
	err := q.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
	return n, err
}

// PeekMetrics 按入队序取最早 limit 条（不删除）。
func (q *Queue) PeekMetrics(limit int) ([]MetricsRow, error) {
	rows, err := q.db.Query(
		`SELECT id, seq, payload, created_at FROM metrics_q ORDER BY id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricsRow
	for rows.Next() {
		var r MetricsRow
		var ns int64
		if err := rows.Scan(&r.ID, &r.Seq, &r.Payload, &ns); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(0, ns)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PeekEvents 按入队序取最早 limit 条事件（不删除）。
func (q *Queue) PeekEvents(limit int) ([]EventRow, error) {
	rows, err := q.db.Query(
		`SELECT id, seq, payload, created_at FROM events_q ORDER BY id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var r EventRow
		var ns int64
		if err := rows.Scan(&r.ID, &r.Seq, &r.Payload, &ns); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(0, ns)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteMetrics 删除已成功送达的指标批次。
func (q *Queue) DeleteMetrics(ids []int64) error {
	return q.deleteIDs("metrics_q", ids)
}

// DeleteEvents 删除已处理的事件。
func (q *Queue) DeleteEvents(ids []int64) error {
	return q.deleteIDs("events_q", ids)
}

func (q *Queue) deleteIDs(table string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`DELETE FROM ` + table + ` WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DropOldestMetrics 丢弃最早 n 条指标批次并记录 overflow。
// 用于补传降采样：积压超上限时只补最近若干批（contracts/events.md §5）。
func (q *Queue) DropOldestMetrics(n int64) (int64, error) {
	return q.dropOldest("metrics_q", KindMetrics, n)
}

// DropOldestEvents 丢弃最早 n 条事件并记录 overflow。
func (q *Queue) DropOldestEvents(n int64) (int64, error) {
	return q.dropOldest("events_q", KindEvents, n)
}

func (q *Queue) dropOldest(table, kind string, n int64) (int64, error) {
	if n <= 0 {
		return 0, nil
	}
	tx, err := q.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// 聚合查询恒返回一行：空集时 MIN/MAX 为 NULL，用 NullInt64 承接。
	var firstNs, lastNs sql.NullInt64
	err = tx.QueryRow(
		`SELECT MIN(created_at), MAX(created_at) FROM (
			SELECT created_at FROM `+table+` ORDER BY id ASC LIMIT ?)`, n,
	).Scan(&firstNs, &lastNs)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, tx.Commit()
	}
	if err != nil {
		return 0, err
	}
	if !firstNs.Valid {
		return 0, tx.Commit() // 空表：无事可做
	}
	res, err := tx.Exec(
		`DELETE FROM `+table+` WHERE id IN (
			SELECT id FROM `+table+` ORDER BY id ASC LIMIT ?)`, n)
	if err != nil {
		return 0, err
	}
	dropped, _ := res.RowsAffected()
	if dropped > 0 {
		if err := addOverflow(tx, kind, dropped, firstNs.Int64, lastNs.Int64); err != nil {
			return 0, err
		}
	}
	return dropped, tx.Commit()
}
