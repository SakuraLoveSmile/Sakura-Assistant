package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ---- api-v1 §3.1 Feedback 管理代理组（v1.1）----
//
// 按来源代理 Feedback 服务的只读组与管理组；凭证职责互斥：
//   - 详情 / 附件（只读）→ Bearer = 来源 access key（key_plain）
//   - 列表 / 操作（管理）→ Bearer = 来源 mgmt_key_plain
//
// APK 只持有客户端令牌，永不接触回连凭证；全部端点以 srcId 命名空间隔离。

// fbRequestIDRe 为 action 幂等键 requestId 的合法字符集（feedback-integration §4.2）。
var fbRequestIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// fbLifecycleActions / fbRevisionActions：管理动作七项枚举按所需乐观锁字段分组。
var fbLifecycleActions = map[string]bool{
	"archive": true, "unarchive": true, "trash": true,
	"restore": true, "resume_processing": true,
}
var fbRevisionActions = map[string]bool{"retry": true, "recheck": true}

// fbPassThroughCodes 为上游错误信封中可原样透传的已知 code（api-v1 错误信封表 v1.1）。
var fbPassThroughCodes = map[string]bool{
	"invalid_request": true, "not_found": true,
	"version_conflict": true, "revision_conflict": true,
	"invalid_state": true, "busy": true,
	"request_id_conflict": true, "request_in_flight": true, "outcome_uncertain": true,
}

const (
	fbTimeoutRead  = 10 * time.Second // 详情 / 附件
	fbTimeoutWrite = 30 * time.Second // 列表 / 操作
)

// fbPrecond 执行通用前置 1/2：来源存在 + kind=feedback + 未删除（否则 404）；
// attachmentBaseUrl 已配置（否则 409 feedback_upstream_not_configured）。
// 通过则返回来源行，否则已写错误响应并返回 nil。
func (a *app) fbPrecond(w http.ResponseWriter, r *http.Request) *sourceRow {
	src, err := a.st.loadSource(r.PathValue("srcId"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (src.DeletedAt.Valid || src.Kind != "feedback")) {
		errNotFound(w)
		return nil
	}
	if err != nil {
		errInternal(w)
		return nil
	}
	if !src.AttachmentBaseURL.Valid || src.AttachmentBaseURL.String == "" {
		writeErr(w, http.StatusConflict, "feedback_upstream_not_configured",
			"feedback 来源未配置 attachmentBaseUrl")
		return nil
	}
	return src
}

// fbPrecondMgmt 在 fbPrecond 之上追加前置 3：管理端点需 mgmtKey 已配置。
func (a *app) fbPrecondMgmt(w http.ResponseWriter, r *http.Request) *sourceRow {
	src := a.fbPrecond(w, r)
	if src == nil {
		return nil
	}
	if src.MgmtKeyPlain == "" {
		writeErr(w, http.StatusConflict, "feedback_mgmt_not_configured",
			"feedback 来源未配置 mgmtKey")
		return nil
	}
	return src
}

// fbBase 返回回连基址（去尾斜杠；调用前须先过 fbPrecond）。
func fbBase(src *sourceRow) string {
	return strings.TrimRight(src.AttachmentBaseURL.String, "/")
}

// fbDo 执行一次回连请求并读取全部响应体（上限 maxAttachmentBytes+1）。
// 返回的 *http.Response 仅用于读取状态码与标头（Body 已消费关闭）。
// timeout 由 ctx 兜底；>10s 的请求走 httpClientOp（管理组 30s 上限）。
func (a *app) fbDo(ctx context.Context, method, upstream, bearer string, payload []byte, timeout time.Duration) (*http.Response, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, upstream, rd)
	if err != nil {
		return nil, nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := a.httpClient
	if timeout > 10*time.Second && a.httpClientOp != nil {
		client = a.httpClientOp
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

// fbWriteUpstreamErr 按 §3.1 前置 4 把上游失败映射为契约错误；已写响应返回 true。
// 各端点专属的 404 语义由调用方先判定（list→unsupported；action→回探；detail→not_found）。
func fbWriteUpstreamErr(w http.ResponseWriter, resp *http.Response, body []byte, err error) bool {
	if err != nil {
		writeErr(w, http.StatusBadGateway, "feedback_unavailable", "反馈上游不可达或超时")
		return true
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "反馈上游限流")
		return true
	case resp.StatusCode == http.StatusUnauthorized:
		// 上游 401 = 凭证配置错误，不暴露细节。
		writeErr(w, http.StatusBadGateway, "feedback_unavailable", "反馈上游凭证失效")
		return true
	case resp.StatusCode >= http.StatusInternalServerError:
		writeErr(w, http.StatusBadGateway, "feedback_unavailable", "反馈上游返回错误")
		return true
	case resp.StatusCode >= http.StatusBadRequest:
		// 上游错误信封中已知 code 原样透传（含 detail 等附加字段）。
		var env struct {
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error != nil && fbPassThroughCodes[env.Error.Code] {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(body)
			return true
		}
		writeErr(w, http.StatusBadGateway, "feedback_unavailable", "反馈上游返回错误")
		return true
	}
	return false
}

// fbWriteBadUpstream 上游返回非预期 2xx/非 JSON 时统一 502。
func fbWriteBadUpstream(w http.ResponseWriter) {
	writeErr(w, http.StatusBadGateway, "feedback_unavailable", "反馈上游响应非法")
}

// handleFeedbackList 为 GET /api/v1/sources/{srcId}/feedback（管理凭证，30s 超时）。
func (a *app) handleFeedbackList(w http.ResponseWriter, r *http.Request) {
	src := a.fbPrecondMgmt(w, r)
	if src == nil {
		return
	}
	upstream := fbBase(src) + "/api/assist/manage/feedback"
	q := url.Values{}
	for _, k := range []string{"view", "cursor", "limit", "q"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	if len(q) > 0 {
		upstream += "?" + q.Encode()
	}
	resp, body, err := a.fbDo(r.Context(), http.MethodGet, upstream, src.MgmtKeyPlain, nil, fbTimeoutWrite)
	// 管理路由 404 = 旧版服务端无管理面（前置 5）。
	if err == nil && resp.StatusCode == http.StatusNotFound {
		writeErr(w, http.StatusConflict, "feedback_mgmt_unsupported",
			"上游 Feedback 服务无管理面，需升级")
		return
	}
	if fbWriteUpstreamErr(w, resp, body, err) {
		return
	}
	if resp.StatusCode != http.StatusOK {
		fbWriteBadUpstream(w)
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		fbWriteBadUpstream(w)
		return
	}
	obj["sourceId"] = src.ID
	obj["sourceName"] = src.Name
	writeJSON(w, http.StatusOK, obj)
}

// handleFeedbackDetail 为 GET /api/v1/sources/{srcId}/feedback/{fbId}（只读凭证，10s）。
func (a *app) handleFeedbackDetail(w http.ResponseWriter, r *http.Request) {
	src := a.fbPrecond(w, r)
	if src == nil {
		return
	}
	fbID := r.PathValue("fbId")
	if !upstreamIDRe.MatchString(fbID) {
		errNotFound(w)
		return
	}
	resp, body, err := a.fbDo(r.Context(), http.MethodGet,
		fbBase(src)+"/api/assist/feedback/"+fbID, src.KeyPlain, nil, fbTimeoutRead)
	if err == nil && resp.StatusCode == http.StatusNotFound {
		errNotFound(w)
		return
	}
	if fbWriteUpstreamErr(w, resp, body, err) {
		return
	}
	if resp.StatusCode != http.StatusOK {
		fbWriteBadUpstream(w)
		return
	}
	var detail map[string]any
	if err := json.Unmarshal(body, &detail); err != nil || detail == nil {
		fbWriteBadUpstream(w)
		return
	}
	detail["sourceId"] = src.ID
	detail["sourceName"] = src.Name
	// capabilities.manage 由中枢重算：上游 manage && 本来源已配置 mgmtKey；
	// 旧版服务端字段缺席视为 false。
	caps, _ := detail["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
	}
	upManage, _ := caps["manage"].(bool)
	caps["manage"] = upManage && src.MgmtKeyPlain != ""
	detail["capabilities"] = caps
	writeJSON(w, http.StatusOK, detail)
}

// fbNonNegInt 判定 JSON 值是否为 ≥0 整数（容忍 3.0 形式，拒绝字符串/布尔等）。
func fbNonNegInt(v json.RawMessage) bool {
	if len(v) == 0 || v[0] == '"' {
		return false
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		return false
	}
	if i, err := n.Int64(); err == nil {
		return i >= 0
	}
	f, err := n.Float64()
	return err == nil && f >= 0 && f == math.Trunc(f)
}

// handleFeedbackAction 为 POST /api/v1/sources/{srcId}/feedback/{fbId}/action
// （管理凭证，30s 超时；中枢只做形状校验后原样透传请求体）。
func (a *app) handleFeedbackAction(w http.ResponseWriter, r *http.Request) {
	src := a.fbPrecondMgmt(w, r)
	if src == nil {
		return
	}
	fbID := r.PathValue("fbId")
	if !upstreamIDRe.MatchString(fbID) {
		errNotFound(w)
		return
	}
	var req map[string]json.RawMessage
	if !decodeBody(w, r, &req) {
		return
	}
	var reqID string
	if v, ok := req["requestId"]; !ok ||
		json.Unmarshal(v, &reqID) != nil || !fbRequestIDRe.MatchString(reqID) {
		errInvalid(w, "requestId 必填且须为 [A-Za-z0-9._:-]{1,128}")
		return
	}
	var action string
	if v, ok := req["action"]; !ok ||
		json.Unmarshal(v, &action) != nil ||
		(!fbLifecycleActions[action] && !fbRevisionActions[action]) {
		errInvalid(w, "action 非法（archive|unarchive|trash|restore|resume_processing|retry|recheck）")
		return
	}
	if fbLifecycleActions[action] && !fbNonNegInt(req["expectedLifecycleVersion"]) {
		errInvalid(w, "expectedLifecycleVersion 必填（≥0 整数）")
		return
	}
	if fbRevisionActions[action] && !fbNonNegInt(req["expectedRevision"]) {
		errInvalid(w, "expectedRevision 必填（≥0 整数）")
		return
	}
	payload, err := json.Marshal(req)
	if err != nil {
		errInternal(w)
		return
	}
	resp, body, err := a.fbDo(r.Context(), http.MethodPost,
		fbBase(src)+"/api/assist/manage/feedback/"+fbID+"/action",
		src.MgmtKeyPlain, payload, fbTimeoutWrite)
	// 管理路由 404：先以只读凭证回探详情区分语义（前置 5）。
	if err == nil && resp.StatusCode == http.StatusNotFound {
		a.fbActionProbe(w, r, src, fbID)
		return
	}
	if fbWriteUpstreamErr(w, resp, body, err) {
		return
	}
	// 成功 / 幂等回放：透传 {ok, action, replayed, detail}，并向 detail 注入来源字段
	// （与 GET 详情同形状，客户端可直接以同一模型替换本地详情）。
	if resp.StatusCode == http.StatusOK {
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err == nil && obj != nil {
			if detail, ok := obj["detail"].(map[string]any); ok && detail != nil {
				detail["sourceId"] = src.ID
				detail["sourceName"] = src.Name
				if merged, err := json.Marshal(obj); err == nil {
					body = merged
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// fbActionProbe 处理操作端点上游 404：回探只读详情——详情 404 → not_found；
// 详情存在但缺 capabilities.manage → 409 feedback_mgmt_unsupported；
// 详情存在且有 manage → not_found（保守，视为对象不存在）。
func (a *app) fbActionProbe(w http.ResponseWriter, r *http.Request, src *sourceRow, fbID string) {
	resp, body, err := a.fbDo(r.Context(), http.MethodGet,
		fbBase(src)+"/api/assist/feedback/"+fbID, src.KeyPlain, nil, fbTimeoutRead)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "feedback_unavailable", "反馈上游不可达或超时")
		return
	}
	if resp.StatusCode == http.StatusNotFound {
		errNotFound(w)
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, "feedback_unavailable", "反馈上游返回错误")
		return
	}
	var detail map[string]any
	manage := false
	if json.Unmarshal(body, &detail) == nil {
		if caps, ok := detail["capabilities"].(map[string]any); ok {
			manage, _ = caps["manage"].(bool)
		}
	}
	if manage {
		errNotFound(w)
		return
	}
	writeErr(w, http.StatusConflict, "feedback_mgmt_unsupported",
		"上游 Feedback 服务无管理面，需升级")
}

// handleFeedbackAttachment 为 GET /api/v1/sources/{srcId}/feedback/{fbId}/attachments/{attId...}
// （只读凭证，10s/10MiB；错误映射同消息附件：404 → not_found，其余 → attachment_unavailable）。
func (a *app) handleFeedbackAttachment(w http.ResponseWriter, r *http.Request) {
	src := a.fbPrecond(w, r)
	if src == nil {
		return
	}
	fbID := r.PathValue("fbId")
	if !upstreamIDRe.MatchString(fbID) {
		errNotFound(w)
		return
	}
	attID := r.PathValue("attId")
	var upstream string
	switch {
	case attID == "screenshot":
		upstream = fbBase(src) + "/api/assist/feedback/" + fbID + "/attachments/screenshot"
	case strings.HasPrefix(attID, "logs/"):
		logID := strings.TrimPrefix(attID, "logs/")
		if !upstreamIDRe.MatchString(logID) {
			errNotFound(w)
			return
		}
		upstream = fbBase(src) + "/api/assist/feedback/" + fbID + "/attachments/logs/" + logID
	default:
		errNotFound(w)
		return
	}
	resp, body, err := a.fbDo(r.Context(), http.MethodGet, upstream, src.KeyPlain, nil, fbTimeoutRead)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "attachment_unavailable", "附件上游不可达")
		return
	}
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		errNotFound(w)
		return
	default:
		writeErr(w, http.StatusBadGateway, "attachment_unavailable", "附件上游返回错误")
		return
	}
	if len(body) > maxAttachmentBytes {
		writeErr(w, http.StatusBadGateway, "attachment_unavailable", "附件超出大小上限")
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	} else {
		w.Header().Set("Content-Length", itoa(len(body)))
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		w.Header().Set("ETag", etag)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
