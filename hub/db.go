package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// schema 为中枢库结构；启动时幂等建立（IF NOT EXISTS 即迁移基线）。
const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sources (
  id                   TEXT PRIMARY KEY,
  name                 TEXT NOT NULL,
  kind                 TEXT NOT NULL CHECK (kind IN ('device','feedback')),
  enabled              INTEGER NOT NULL DEFAULT 1,
  key_hash             TEXT NOT NULL,
  key_plain            TEXT NOT NULL DEFAULT '',
  key_hint             TEXT NOT NULL DEFAULT '',
  mgmt_key_plain       TEXT NOT NULL DEFAULT '',
  mgmt_key_hint        TEXT NOT NULL DEFAULT '',
  attachment_base_url  TEXT,
  agent_version        TEXT,
  agent_os             TEXT,
  agent_arch           TEXT,
  hostname             TEXT,
  capabilities         TEXT,
  last_seen_at         TEXT,
  last_metrics_seq     INTEGER NOT NULL DEFAULT 0,
  last_event_seq       INTEGER NOT NULL DEFAULT 0,
  last_boot_time       TEXT,
  last_summary         TEXT,
  prev_sample          TEXT,
  deleted_at           TEXT,
  created_at           TEXT NOT NULL,
  updated_at           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS metrics_samples (
  source_id   TEXT NOT NULL,
  ts          TEXT NOT NULL,
  received_at TEXT NOT NULL,
  sample_json TEXT NOT NULL,
  PRIMARY KEY (source_id, ts)
);

CREATE TABLE IF NOT EXISTS metrics_raw (
  source_id TEXT NOT NULL,
  ts        TEXT NOT NULL,
  metric    TEXT NOT NULL,
  label     TEXT NOT NULL DEFAULT '',
  value     REAL NOT NULL,
  PRIMARY KEY (source_id, ts, metric, label)
);
CREATE INDEX IF NOT EXISTS idx_metrics_raw_query ON metrics_raw(source_id, metric, label, ts);

CREATE TABLE IF NOT EXISTS metrics_5m (
  source_id TEXT NOT NULL,
  bucket    TEXT NOT NULL,
  metric    TEXT NOT NULL,
  label     TEXT NOT NULL DEFAULT '',
  avg       REAL NOT NULL,
  min       REAL NOT NULL,
  max       REAL NOT NULL,
  n         INTEGER NOT NULL,
  PRIMARY KEY (source_id, bucket, metric, label)
);

CREATE TABLE IF NOT EXISTS events (
  source_id       TEXT NOT NULL,
  event_id        TEXT NOT NULL,
  seq             INTEGER NOT NULL,
  kind            TEXT NOT NULL,
  occurred_at     TEXT NOT NULL,
  severity        TEXT NOT NULL,
  fault_key       TEXT,
  incident_action TEXT,
  title           TEXT NOT NULL,
  body            TEXT,
  ref_json        TEXT,
  attachments_json TEXT,
  out_of_order    INTEGER NOT NULL DEFAULT 0,
  internal        INTEGER NOT NULL DEFAULT 0,
  received_at     TEXT NOT NULL,
  PRIMARY KEY (source_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_events_seq ON events(source_id, seq);

CREATE TABLE IF NOT EXISTS messages (
  id          TEXT PRIMARY KEY,
  source_id   TEXT NOT NULL,
  kind        TEXT NOT NULL,
  severity    TEXT NOT NULL,
  title       TEXT NOT NULL,
  body        TEXT,
  occurred_at TEXT NOT NULL,
  received_at TEXT NOT NULL,
  read_at     TEXT,
  fault_id    TEXT,
  incident    INTEGER,
  ref         TEXT,
  attachments TEXT,
  event_id    TEXT NOT NULL DEFAULT '',
  event_seq   INTEGER NOT NULL DEFAULT 0,
  fault_key   TEXT,
  out_of_order INTEGER NOT NULL DEFAULT 0,
  change_seq  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_fault ON messages(fault_id);
CREATE INDEX IF NOT EXISTS idx_messages_source ON messages(source_id, id);
CREATE INDEX IF NOT EXISTS idx_messages_fk ON messages(source_id, fault_key, event_seq);

CREATE TABLE IF NOT EXISTS faults (
  id              TEXT PRIMARY KEY,
  source_id       TEXT NOT NULL,
  fault_key       TEXT NOT NULL,
  severity        TEXT NOT NULL,
  title           TEXT NOT NULL,
  summary         TEXT,
  state           TEXT NOT NULL CHECK (state IN ('open','resolved')),
  incident        INTEGER NOT NULL,
  opened_at       TEXT NOT NULL,
  opened_by_seq   INTEGER NOT NULL,
  last_event_at   TEXT NOT NULL,
  resolved_at     TEXT,
  resolved_by_seq INTEGER,
  resolved_by     TEXT,
  event_count     INTEGER NOT NULL DEFAULT 0,
  read_at         TEXT,
  muted_at        TEXT,
  muted_until     TEXT,
  change_seq      INTEGER NOT NULL DEFAULT 0,
  UNIQUE (source_id, fault_key)
);

CREATE TABLE IF NOT EXISTS pending_resolves (
  source_id   TEXT NOT NULL,
  fault_key   TEXT NOT NULL,
  seq         INTEGER NOT NULL,
  occurred_at TEXT NOT NULL,
  title       TEXT NOT NULL DEFAULT '',
  event_id    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (source_id, fault_key, seq)
);

CREATE TABLE IF NOT EXISTS changes (
  seq         INTEGER PRIMARY KEY,
  type        TEXT NOT NULL,
  ref_id      TEXT NOT NULL DEFAULT '',
  data_json   TEXT NOT NULL,
  notify_json TEXT,
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_changes_created ON changes(created_at);

CREATE TABLE IF NOT EXISTS sessions (
  id                  TEXT PRIMARY KEY,
  token_hash          TEXT NOT NULL,
  refresh_hash        TEXT NOT NULL,
  username            TEXT NOT NULL,
  device_label        TEXT,
  created_at          TEXT NOT NULL,
  expires_at          TEXT NOT NULL,
  refresh_expires_at  TEXT NOT NULL,
  revoked             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sessions_token ON sessions(token_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_refresh ON sessions(refresh_hash);

CREATE TABLE IF NOT EXISTS rules (
  id                INTEGER PRIMARY KEY CHECK (id = 1),
  version           INTEGER NOT NULL,
  heartbeat_seconds INTEGER NOT NULL,
  rules_json        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
  id                       INTEGER PRIMARY KEY CHECK (id = 1),
  version                  INTEGER NOT NULL,
  dnd_json                 TEXT NOT NULL,
  report_interval_seconds  INTEGER NOT NULL
);
`

// openDB 打开（必要时创建）SQLite 库并应用 schema；启用 WAL。
func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 串行写模型：单连接即可，配合 store.mu 保证写事务串行。
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateSourcesV11(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate sources v1.1: %w", err)
	}
	return db, nil
}

// migrateSourcesV11 为既有库幂等增补 v1.1 列：sources 表加
// mgmt_key_plain / mgmt_key_hint（PRAGMA 检查缺列再 ALTER，可重入）。
// 新库经 CREATE TABLE 直接具备两列，本函数为空操作。
func migrateSourcesV11(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(sources)`)
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, c := range []struct{ name, ddl string }{
		{"mgmt_key_plain", `ALTER TABLE sources ADD COLUMN mgmt_key_plain TEXT NOT NULL DEFAULT ''`},
		{"mgmt_key_hint", `ALTER TABLE sources ADD COLUMN mgmt_key_hint TEXT NOT NULL DEFAULT ''`},
	} {
		if cols[c.name] {
			continue
		}
		if _, err := db.Exec(c.ddl); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}
	return nil
}
