package main

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// hubVersion 为中枢版本号，经 /api/v1/health 与 /api/v1/version 暴露。
const hubVersion = "1.1.0"

// tsFmt 为契约规定的时间格式：RFC3339 UTC 毫秒精度。
const tsFmt = "2006-01-02T15:04:05.000Z"

// nowUTC 返回当前 UTC 时间。
func nowUTC() time.Time { return time.Now().UTC() }

// fmtTS 将时间格式化为契约时间字符串。
func fmtTS(t time.Time) string { return t.UTC().Format(tsFmt) }

// fmtTSPtr 将可空时间格式化为可空字符串。
func fmtTSPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := fmtTS(*t)
	return &s
}

// parseTS 解析契约时间字符串（容忍任意 RFC3339 精度）。
func parseTS(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// parseTSOpt 解析可空时间字符串，空串返回 nil。
func parseTSOpt(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := parseTS(s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// newID 生成带前缀 ULID。
func newID(prefix string) string {
	return prefix + ulid.Make().String()
}

// randToken 生成 <prefix><48 位小写十六进制> 形式的随机令牌。
func randToken(prefix string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// hintTail 取凭证末 4 位用于回显（不足 4 字符取全长，UTF-8 安全）。
func hintTail(s string) string {
	rs := []rune(s)
	if len(rs) > 4 {
		rs = rs[len(rs)-4:]
	}
	return string(rs)
}

// ---- JSON 与错误信封 ----

// writeJSON 以给定状态码输出 JSON。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr 输出契约错误信封 {"error":{"code","message"}}。
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
}

func errInvalid(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusBadRequest, "invalid_request", msg)
}
func errUnauthorized(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusUnauthorized, "unauthorized", msg)
}
func errForbidden(w http.ResponseWriter, code, msg string) {
	writeErr(w, http.StatusForbidden, code, msg)
}
func errNotFound(w http.ResponseWriter) {
	writeErr(w, http.StatusNotFound, "not_found", "对象不存在")
}
func errConflict(w http.ResponseWriter, code, msg string) {
	writeErr(w, http.StatusConflict, code, msg)
}
func errInternal(w http.ResponseWriter) {
	writeErr(w, http.StatusInternalServerError, "internal", "内部错误")
}

// decodeBody 解码请求体，限制 256KiB；出错时已写错误响应并返回 false。
// 支持 Content-Encoding: gzip（解压后同样限 256KiB）。
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	const maxBody = 256 * 1024
	// gzip 路径下压缩输入允许少量余量（不可压缩数据 gzip 开销极小），
	// 解压后仍以 maxBody 为权威上限。
	rawLimit := int64(maxBody)
	isGzip := false
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		isGzip = true
		rawLimit = maxBody + 8*1024
	default:
		writeErr(w, http.StatusUnsupportedMediaType, "unsupported_encoding", "不支持的内容编码")
		return false
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, rawLimit)
	if isGzip {
		gz, err := gzip.NewReader(body)
		if err != nil {
			errInvalid(w, "非法的 gzip 请求体")
			return false
		}
		defer gz.Close()
		body = gz
	}
	// 解压后内容同样受限（防 gzip 膨胀）；多读 1 字节判断是否超限。
	data, err := io.ReadAll(io.LimitReader(body, maxBody+1))
	if err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "请求体超限")
			return false
		}
		errInvalid(w, "非法的请求体")
		return false
	}
	if len(data) > maxBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "请求体超限")
		return false
	}
	if err := json.Unmarshal(data, dst); err != nil {
		errInvalid(w, "非法的 JSON 请求体")
		return false
	}
	return true
}

// sevRank 返回严重度排序值，用于故障合并取 max。
func sevRank(s string) int {
	switch s {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

// maxSev 返回两者中更严重者。
func maxSev(a, b string) string {
	if sevRank(b) > sevRank(a) {
		return b
	}
	return a
}
