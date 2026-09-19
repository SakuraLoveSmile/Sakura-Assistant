package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// maxAttachmentBytes 为附件代理上限（10MiB，不落盘、内存中转）。
const maxAttachmentBytes = 10 << 20

// upstreamIDRe 为回连路径参数（feedbackId / logId）的合法字符集；
// 拒绝 .、/、? 等一切可参与路径/查询注入的字符。
var upstreamIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// validAttachmentBaseURL 校验回连基址：仅允许绝对 http/https URL。
func validAttachmentBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// handleAttachment 为 GET /api/v1/messages/{id}/attachments/{attId...}：
// 按来源 attachmentBaseUrl 回连只读接口取字节并透传（feedback-integration §4）。
func (a *app) handleAttachment(w http.ResponseWriter, r *http.Request) {
	msgID := r.PathValue("id")
	attID := r.PathValue("attId")

	m, err := a.st.loadMessage(msgID)
	if errors.Is(err, sql.ErrNoRows) {
		errNotFound(w)
		return
	}
	if err != nil {
		errInternal(w)
		return
	}

	// 附件描述符必须存在。
	var desc *attachmentDesc
	var descs []attachmentDesc
	if m.Attachments.Valid {
		_ = json.Unmarshal([]byte(m.Attachments.String), &descs)
	}
	for i := range descs {
		if descs[i].ID == attID {
			desc = &descs[i]
			break
		}
	}
	if desc == nil {
		errNotFound(w)
		return
	}

	src, err := a.st.loadSource(m.SourceID)
	if err != nil || src == nil || !src.AttachmentBaseURL.Valid || src.AttachmentBaseURL.String == "" {
		// 未配置回连地址：契约归一 404（可区分性差）→ 依 feedback-integration §4 为 404。
		errNotFound(w)
		return
	}

	// ref.feedbackId 解析。
	var feedbackID string
	if m.Ref.Valid {
		var ref struct {
			FeedbackID string `json:"feedbackId"`
		}
		if json.Unmarshal([]byte(m.Ref.String), &ref) == nil {
			feedbackID = ref.FeedbackID
		}
	}
	if !upstreamIDRe.MatchString(feedbackID) {
		errNotFound(w)
		return
	}

	base := strings.TrimRight(src.AttachmentBaseURL.String, "/")
	var upstream string
	switch {
	case attID == "screenshot":
		upstream = base + "/api/assist/feedback/" + feedbackID + "/attachments/screenshot"
	case strings.HasPrefix(attID, "logs/"):
		logID := strings.TrimPrefix(attID, "logs/")
		if !upstreamIDRe.MatchString(logID) {
			errNotFound(w)
			return
		}
		upstream = base + "/api/assist/feedback/" + feedbackID + "/attachments/logs/" + logID
	default:
		errNotFound(w)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream, nil)
	if err != nil {
		errInternal(w)
		return
	}
	if src.KeyPlain != "" {
		req.Header.Set("Authorization", "Bearer "+src.KeyPlain)
	}

	client := a.httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "attachment_unavailable", "附件上游不可达")
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		// 透传
	case resp.StatusCode == http.StatusNotFound:
		errNotFound(w)
		return
	default:
		// 上游 5xx / 401 等统一 502（配置错误细节不暴露给客户端）。
		writeErr(w, http.StatusBadGateway, "attachment_unavailable", "附件上游返回错误")
		return
	}

	// 上限 10MiB。
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "attachment_unavailable", "读取附件失败")
		return
	}
	if len(body) > maxAttachmentBytes {
		writeErr(w, http.StatusBadGateway, "attachment_unavailable", "附件超出大小上限")
		return
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = desc.Mime
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	} else {
		w.Header().Set("Content-Length", itoa(len(body)))
	}
	etag := resp.Header.Get("ETag")
	if etag == "" && desc.SHA256 != "" {
		etag = `"` + desc.SHA256 + `"`
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
