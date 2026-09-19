package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
)

// ---- sources ----

func (a *app) handleSources(w http.ResponseWriter, r *http.Request) {
	sources, err := a.st.listSources()
	if err != nil {
		errInternal(w)
		return
	}
	hb := a.st.heartbeatSeconds()
	out := []any{}
	for _, s := range sources {
		out = append(out, toSourceJSON(s, hb))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

// installBlock 生成创建 / 轮换时一次性返回的安装指引。
func (a *app) installBlock(accessKey string) map[string]any {
	base := a.cfg.baseURL
	// 单行 systemd 安装命令（假定 assistant-agent 二进制已置于 /usr/local/bin）。
	command := fmt.Sprintf(
		`sudo sh -c 'printf "[Unit]\nDescription=Assistant Agent\nAfter=network-online.target\n\n[Service]\nEnvironment=ASSIST_HUB_URL=%s\nEnvironment=ASSIST_SOURCE_KEY=%s\nExecStart=/usr/local/bin/assistant-agent\nRestart=always\nRestartSec=5\n\n[Install]\nWantedBy=multi-user.target\n" > /etc/systemd/system/assistant-agent.service && systemctl daemon-reload && systemctl enable --now assistant-agent'`,
		base, accessKey)
	note := "## 安装说明\n\n" +
		"前提：已把 `assistant-agent` 二进制部署到 `/usr/local/bin/`（需 systemd 的 Linux 主机）。\n\n" +
		"1. 执行上方 `command` 单行命令：写入 `assistant-agent.service` 并启用启动。\n" +
		"2. 环境变量：`ASSIST_HUB_URL` 为中枢地址，`ASSIST_SOURCE_KEY` 为本来源密钥（仅此一次明文展示，请妥善保存）。\n" +
		"3. `systemctl status assistant-agent` 确认运行；日志经 `journalctl -u assistant-agent` 查看。\n" +
		"4. 密钥泄露时用「轮换密钥」接口重新签发，旧密钥即刻失效。"
	return map[string]any{
		"env": map[string]any{
			"ASSIST_HUB_URL":    base,
			"ASSIST_SOURCE_KEY": accessKey,
		},
		"command": command,
		"note":    note,
	}
}

func (a *app) handleSourceCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name              string          `json:"name"`
		Kind              string          `json:"kind"`
		AttachmentBaseURL *string         `json:"attachmentBaseUrl"`
		MgmtKey           json.RawMessage `json:"mgmtKey"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		errInvalid(w, "name 必填")
		return
	}
	if req.Kind != "device" && req.Kind != "feedback" {
		errInvalid(w, "kind 非法（device|feedback）")
		return
	}

	now := fmtTS(nowUTC())
	src := &sourceRow{
		ID:        newID("src_"),
		Name:      req.Name,
		Kind:      req.Kind,
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	key := randToken("ask_")
	src.KeyPlain = key
	src.KeyHash = hashToken(key)
	src.KeyHint = key[len(key)-4:]
	if req.AttachmentBaseURL != nil && *req.AttachmentBaseURL != "" {
		if !validAttachmentBaseURL(*req.AttachmentBaseURL) {
			errInvalid(w, "attachmentBaseUrl 必须为 http/https URL")
			return
		}
		src.AttachmentBaseURL = sql.NullString{String: *req.AttachmentBaseURL, Valid: true}
	}
	// v1.1 mgmtKey：仅 feedback 类来源可配置；字段出现（含 null）于 device 类 → 拒绝。
	if len(req.MgmtKey) > 0 {
		if req.Kind != "feedback" {
			errInvalid(w, "mgmtKey 仅 feedback 类来源可配置")
			return
		}
		if string(req.MgmtKey) != "null" {
			var mk string
			if err := json.Unmarshal(req.MgmtKey, &mk); err != nil || mk == "" || len(mk) > 200 {
				errInvalid(w, "mgmtKey 需为非空字符串（≤200 字符）")
				return
			}
			src.MgmtKeyPlain = mk
			src.MgmtKeyHint = hintTail(mk)
		}
	}

	var changes []*changeEntry
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO sources(`+sourceCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			src.ID, src.Name, src.Kind, 1, src.KeyHash, src.KeyPlain, src.KeyHint,
			src.AttachmentBaseURL, src.AgentVersion, src.AgentOS, src.AgentArch, src.Hostname,
			src.Capabilities, src.LastSeenAt, 0, 0, src.LastBootTime, src.LastSummary,
			src.PrevSample, src.DeletedAt, src.CreatedAt, src.UpdatedAt,
			src.MgmtKeyPlain, src.MgmtKeyHint,
		); err != nil {
			return err
		}
		return a.emitSourceChangeTx(tx, src, &changes)
	})
	if err != nil {
		errInternal(w)
		return
	}
	a.st.publish(changes)

	writeJSON(w, http.StatusCreated, map[string]any{
		"source":    toSourceJSON(src, a.st.heartbeatSeconds()),
		"accessKey": key,
		"install":   a.installBlock(key),
	})
}

func (a *app) handleSourceGet(w http.ResponseWriter, r *http.Request) {
	src, err := a.st.loadSource(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && src.DeletedAt.Valid) {
		errNotFound(w)
		return
	}
	if err != nil {
		errInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": toSourceJSON(src, a.st.heartbeatSeconds())})
}

// handleSourcePatch 更新 name/enabled/attachmentBaseUrl/mgmtKey（字段出现才更新，
// attachmentBaseUrl 与 mgmtKey 可为 null 清除；mgmtKey 仅 feedback 类来源可配置）。
func (a *app) handleSourcePatch(w http.ResponseWriter, r *http.Request) {
	var req map[string]json.RawMessage
	if !decodeBody(w, r, &req) {
		return
	}
	var changes []*changeEntry
	var out *sourceJSON
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		src, err := loadSourceTx(tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		if src.DeletedAt.Valid {
			return sql.ErrNoRows
		}
		if v, ok := req["name"]; ok {
			var name string
			if err := json.Unmarshal(v, &name); err != nil || name == "" {
				return errInvalidRequest("name 非法")
			}
			src.Name = name
		}
		if v, ok := req["enabled"]; ok {
			var en bool
			if err := json.Unmarshal(v, &en); err != nil {
				return errInvalidRequest("enabled 非法")
			}
			src.Enabled = en
		}
		if v, ok := req["attachmentBaseUrl"]; ok {
			if string(v) == "null" {
				src.AttachmentBaseURL = sql.NullString{}
			} else {
				var u string
				if err := json.Unmarshal(v, &u); err != nil {
					return errInvalidRequest("attachmentBaseUrl 非法")
				}
				if u != "" && !validAttachmentBaseURL(u) {
					return errInvalidRequest("attachmentBaseUrl 必须为 http/https URL")
				}
				src.AttachmentBaseURL = nullStr(u)
			}
		}
		if v, ok := req["mgmtKey"]; ok {
			if src.Kind != "feedback" {
				return errInvalidRequest("mgmtKey 仅 feedback 类来源可配置")
			}
			if string(v) == "null" {
				src.MgmtKeyPlain = ""
				src.MgmtKeyHint = ""
			} else {
				var mk string
				if err := json.Unmarshal(v, &mk); err != nil || mk == "" || len(mk) > 200 {
					return errInvalidRequest("mgmtKey 需为非空字符串（≤200 字符）")
				}
				src.MgmtKeyPlain = mk
				src.MgmtKeyHint = hintTail(mk)
			}
		}
		src.UpdatedAt = fmtTS(nowUTC())
		en := 0
		if src.Enabled {
			en = 1
		}
		if _, err := tx.Exec(
			`UPDATE sources SET name=?, enabled=?, attachment_base_url=?,
			 mgmt_key_plain=?, mgmt_key_hint=?, updated_at=? WHERE id=?`,
			src.Name, en, src.AttachmentBaseURL,
			src.MgmtKeyPlain, src.MgmtKeyHint, src.UpdatedAt, src.ID); err != nil {
			return err
		}
		if err := a.emitSourceChangeTx(tx, src, &changes); err != nil {
			return err
		}
		out = toSourceJSON(src, a.st.heartbeatSeconds())
		return nil
	})
	if err != nil {
		var iv *invalidErr
		if errors.As(err, &iv) {
			errInvalid(w, iv.msg)
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			errNotFound(w)
			return
		}
		errInternal(w)
		return
	}
	a.st.publish(changes)
	writeJSON(w, http.StatusOK, map[string]any{"source": out})
}

// invalidErr 为事务内携带的参数错误。
type invalidErr struct{ msg string }

func (e *invalidErr) Error() string      { return e.msg }
func errInvalidRequest(msg string) error { return &invalidErr{msg} }

func (a *app) handleSourceRotateKey(w http.ResponseWriter, r *http.Request) {
	var changes []*changeEntry
	key := randToken("ask_")
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		src, err := loadSourceTx(tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		if src.DeletedAt.Valid {
			return sql.ErrNoRows
		}
		src.KeyPlain = key
		src.KeyHash = hashToken(key)
		src.KeyHint = key[len(key)-4:]
		src.UpdatedAt = fmtTS(nowUTC())
		if _, err := tx.Exec(
			`UPDATE sources SET key_hash=?, key_plain=?, key_hint=?, updated_at=? WHERE id=?`,
			src.KeyHash, src.KeyPlain, src.KeyHint, src.UpdatedAt, src.ID); err != nil {
			return err
		}
		return a.emitSourceChangeTx(tx, src, &changes)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"accessKey": key,
		"install":   a.installBlock(key),
	})
}

// mgmtInstallBlock 为管理凭证一次性配置指引：写入 Feedback 服务 env 并重启。
func (a *app) mgmtInstallBlock(mgmtKey string) map[string]any {
	note := "## 管理凭证配置\n\n" +
		"1. 在 Feedback 服务的环境变量中设置 `FEEDBACK_ASSIST_MGMT_KEY` 为上方值（仅此一次明文展示，请妥善保存）。\n" +
		"2. 该凭证必须与 `FEEDBACK_ASSIST_SOURCE_KEY` / `FEEDBACK_ASSIST_READ_KEY` 不同，否则 Feedback 端拒绝挂载管理面。\n" +
		"3. 重启 Feedback 服务后管理路由生效；旧管理凭证即刻失效。\n" +
		"4. 未配置该变量时反馈列表与管理操作不可用（只读详情与附件不受影响）。"
	return map[string]any{
		"env":  map[string]any{"FEEDBACK_ASSIST_MGMT_KEY": mgmtKey},
		"note": note,
	}
}

// handleSourceRotateMgmtKey 轮换管理凭证（v1.1）：中枢生成 amk_<48hex>，
// 仅此一次明文返回；仅 kind=feedback 来源可用（其他/不存在/已删 → 404）。
func (a *app) handleSourceRotateMgmtKey(w http.ResponseWriter, r *http.Request) {
	var changes []*changeEntry
	key := randToken("amk_")
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		src, err := loadSourceTx(tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		if src.DeletedAt.Valid || src.Kind != "feedback" {
			return sql.ErrNoRows
		}
		src.MgmtKeyPlain = key
		src.MgmtKeyHint = key[len(key)-4:]
		src.UpdatedAt = fmtTS(nowUTC())
		if _, err := tx.Exec(
			`UPDATE sources SET mgmt_key_plain=?, mgmt_key_hint=?, updated_at=? WHERE id=?`,
			src.MgmtKeyPlain, src.MgmtKeyHint, src.UpdatedAt, src.ID); err != nil {
			return err
		}
		return a.emitSourceChangeTx(tx, src, &changes)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"mgmtKey": key,
		"install": a.mgmtInstallBlock(key),
	})
}

// handleSourceDelete 软删除来源：密钥失效，历史保留。
func (a *app) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	var changes []*changeEntry
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		src, err := loadSourceTx(tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		if src.DeletedAt.Valid {
			return sql.ErrNoRows
		}
		now := fmtTS(nowUTC())
		if _, err := tx.Exec(
			`UPDATE sources SET deleted_at=?, key_hash='', key_plain='',
			 mgmt_key_plain='', mgmt_key_hint='', updated_at=? WHERE id=?`,
			now, now, src.ID); err != nil {
			return err
		}
		// 契约：删除来源广播 tombstone{type:"source",id}，客户端据此移除本地行。
		entry, err := insertTombstone(tx, "source", src.ID)
		if err != nil {
			return err
		}
		changes = append(changes, entry)
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
	w.WriteHeader(http.StatusNoContent)
}

// ---- rules ----

func (a *app) handleRulesGet(w http.ResponseWriter, r *http.Request) {
	doc, err := a.st.loadRules()
	if err != nil {
		errInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

var ruleIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// validateRulesDoc 校验 PUT 的规则集。
func validateRulesDoc(rules []rule) string {
	seen := map[string]bool{}
	for i := range rules {
		r := &rules[i]
		if !ruleIDRe.MatchString(r.ID) {
			return "rule.id 缺失或非法"
		}
		if seen[r.ID] {
			return "rule.id 重复"
		}
		seen[r.ID] = true
		if r.Severity != "warning" && r.Severity != "critical" {
			return "severity 只允许 warning/critical"
		}
		switch r.Kind {
		case "threshold":
			if !seriesMetrics[r.Metric] {
				return "threshold 需要合法 metric"
			}
			if r.Value == nil {
				return "threshold 需要 value"
			}
			if r.Op != "" && r.Op != "gt" && r.Op != "gte" && r.Op != "lt" && r.Op != "lte" {
				return "op 非法"
			}
			if r.ForSeconds < 0 {
				return "forSeconds 非法"
			}
			if r.RecoverForSeconds < 0 {
				return "recoverForSeconds 非法"
			}
		case "container_exit":
			// match 可空 / "*" / 精确名
		case "smart", "pool":
			// 无附加参数
		default:
			return "kind 非法（threshold|container_exit|smart|pool）"
		}
	}
	return ""
}

func (a *app) handleRulesPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExpectedVersion  *int64 `json:"expectedVersion"`
		HeartbeatSeconds *int64 `json:"heartbeatSeconds"`
		Rules            []rule `json:"rules"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ExpectedVersion == nil {
		errInvalid(w, "expectedVersion 必填")
		return
	}
	if msg := validateRulesDoc(req.Rules); msg != "" {
		errInvalid(w, msg)
		return
	}
	if req.HeartbeatSeconds != nil && (*req.HeartbeatSeconds < 60 || *req.HeartbeatSeconds > 3600) {
		errInvalid(w, "heartbeatSeconds 需在 60..3600")
		return
	}

	var changes []*changeEntry
	var out *rulesDoc
	conflictVersion := int64(-1)
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		doc, err := a.st.loadRules()
		if err != nil {
			return err
		}
		if doc.Version != *req.ExpectedVersion {
			conflictVersion = doc.Version
			return errConflictSilent
		}
		doc.Version++
		if req.HeartbeatSeconds != nil {
			doc.HeartbeatSeconds = *req.HeartbeatSeconds
		}
		doc.Rules = req.Rules
		if doc.Rules == nil {
			doc.Rules = []rule{}
		}
		b, _ := json.Marshal(doc.Rules)
		if _, err := tx.Exec(
			`UPDATE rules SET version=?, heartbeat_seconds=?, rules_json=? WHERE id=1`,
			doc.Version, doc.HeartbeatSeconds, string(b)); err != nil {
			return err
		}
		entry, err := insertChangeObj(tx, "rules", "rules", func(int64) {}, doc, nil)
		if err != nil {
			return err
		}
		changes = append(changes, entry)
		out = doc
		return nil
	})
	if err != nil {
		if errors.Is(err, errConflictSilent) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   map[string]any{"code": "version_conflict", "message": "规则版本不匹配"},
				"version": conflictVersion,
			})
			return
		}
		errInternal(w)
		return
	}
	a.st.publish(changes)
	writeJSON(w, http.StatusOK, out)
}

var errConflictSilent = errors.New("version_conflict")

// ---- settings ----

func (a *app) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	doc, err := a.st.loadSettings()
	if err != nil {
		errInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

var dndTimeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func (a *app) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExpectedVersion       *int64     `json:"expectedVersion"`
		DND                   *dndConfig `json:"dnd"`
		ReportIntervalSeconds *int64     `json:"reportIntervalSeconds"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ExpectedVersion == nil {
		errInvalid(w, "expectedVersion 必填")
		return
	}
	if req.DND != nil {
		if !dndTimeRe.MatchString(req.DND.Start) || !dndTimeRe.MatchString(req.DND.End) {
			errInvalid(w, "dnd.start/end 需为 HH:mm")
			return
		}
		if req.DND.Timezone == "" {
			errInvalid(w, "dnd.timezone 必填")
			return
		}
	}
	if req.ReportIntervalSeconds != nil &&
		(*req.ReportIntervalSeconds < 10 || *req.ReportIntervalSeconds > 600) {
		errInvalid(w, "reportIntervalSeconds 需在 10..600")
		return
	}

	var changes []*changeEntry
	var out *settingsDoc
	conflictVersion := int64(-1)
	err := a.st.withTx(nil, func(tx *sql.Tx) error {
		doc, err := a.st.loadSettings()
		if err != nil {
			return err
		}
		if doc.Version != *req.ExpectedVersion {
			conflictVersion = doc.Version
			return errConflictSilent
		}
		doc.Version++
		if req.DND != nil {
			doc.DND = *req.DND
		}
		if req.ReportIntervalSeconds != nil {
			doc.ReportIntervalSeconds = *req.ReportIntervalSeconds
		}
		b, _ := json.Marshal(doc.DND)
		if _, err := tx.Exec(
			`UPDATE settings SET version=?, dnd_json=?, report_interval_seconds=? WHERE id=1`,
			doc.Version, string(b), doc.ReportIntervalSeconds); err != nil {
			return err
		}
		entry, err := insertChangeObj(tx, "settings", "settings", func(int64) {}, doc, nil)
		if err != nil {
			return err
		}
		changes = append(changes, entry)
		out = doc
		return nil
	})
	if err != nil {
		if errors.Is(err, errConflictSilent) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   map[string]any{"code": "version_conflict", "message": "设置版本不匹配"},
				"version": conflictVersion,
			})
			return
		}
		errInternal(w)
		return
	}
	a.st.publish(changes)
	writeJSON(w, http.StatusOK, out)
}
