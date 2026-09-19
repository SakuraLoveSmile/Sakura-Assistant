package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	tokenTTL   = 30 * 24 * time.Hour
	refreshTTL = 90 * 24 * time.Hour
)

// hashToken 存取令牌的哈希（库内不存明文）。
func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// bearerToken 取 Authorization: Bearer 值。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// ---- 会话上下文 ----

type ctxKey int

const ctxSession ctxKey = iota

func withSession(ctx context.Context, s *session) context.Context {
	return context.WithValue(ctx, ctxSession, s)
}

func sessionOf(r *http.Request) *session {
	s, _ := r.Context().Value(ctxSession).(*session)
	return s
}

// session 为已认证会话。
type session struct {
	ID          string
	Username    string
	DeviceLabel sql.NullString
	ExpiresAt   time.Time
}

// clientSession 按访问令牌解析会话（过期 / 吊销 → nil）。
func (a *app) clientSession(r *http.Request) (*session, error) {
	tok := bearerToken(r)
	if tok == "" {
		return nil, nil
	}
	var s session
	var expires string
	err := a.st.db.QueryRow(
		`SELECT id, username, device_label, expires_at FROM sessions
		 WHERE token_hash=? AND revoked=0`, hashToken(tok),
	).Scan(&s.ID, &s.Username, &s.DeviceLabel, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t, err := parseTS(expires)
	if err != nil {
		return nil, err
	}
	if time.Now().After(t) {
		return nil, nil
	}
	s.ExpiresAt = t
	return &s, nil
}

// requireClient 为客户端令牌认证中间件。
func (a *app) requireClient(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := a.clientSession(r)
		if err != nil {
			errInternal(w)
			return
		}
		if s == nil {
			errUnauthorized(w, "令牌缺失或失效")
			return
		}
		next(w, r.WithContext(withSession(r.Context(), s)))
	}
}

// sourceKeyAuth 为来源密钥认证：未知 / 吊销 → 401；停用 → 403 source_disabled。
func (a *app) sourceKeyAuth(next func(http.ResponseWriter, *http.Request, *sourceRow)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			errUnauthorized(w, "来源密钥缺失")
			return
		}
		src, err := a.st.loadSourceByKeyHash(hashToken(key))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			errInternal(w)
			return
		}
		if src == nil {
			errUnauthorized(w, "未知来源密钥")
			return
		}
		if !src.Enabled {
			errForbidden(w, "source_disabled", "来源已停用")
			return
		}
		next(w, r, src)
	}
}

// ---- 登录限流（IP+用户名 滑动窗口 10 次 / 5 分钟） ----

type loginLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{hits: map[string][]time.Time{}}
}

// allow 记录一次尝试；返回是否允许与重试秒数。
func (l *loginLimiter) allow(key string) (bool, int) {
	const window = 5 * time.Minute
	const maxHits = 10
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-window)
	keep := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	l.hits[key] = keep
	if len(keep) >= maxHits {
		retry := int(keep[0].Add(window).Sub(now).Seconds()) + 1
		return false, retry
	}
	l.hits[key] = append(l.hits[key], now)
	return true, 0
}

// ---- 认证端点 ----

type loginReq struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DeviceLabel string `json:"deviceLabel"`
}

// issueSession 签发 token + refreshToken 并落库（哈希存储）。
func (a *app) issueSession(username, deviceLabel string) (map[string]any, error) {
	now := nowUTC()
	tok := randToken("tok_")
	rtok := randToken("rtok_")
	exp := now.Add(tokenTTL)
	rexp := now.Add(refreshTTL)
	var dl sql.NullString
	if deviceLabel != "" {
		dl = sql.NullString{String: deviceLabel, Valid: true}
	}
	_, err := a.st.db.Exec(
		`INSERT INTO sessions(id, token_hash, refresh_hash, username, device_label,
		   created_at, expires_at, refresh_expires_at, revoked)
		 VALUES(?,?,?,?,?,?,?,?,0)`,
		newID("ses_"), hashToken(tok), hashToken(rtok), username, dl,
		fmtTS(now), fmtTS(exp), fmtTS(rexp),
	)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"token":        tok,
		"expiresAt":    fmtTS(exp),
		"refreshToken": rtok,
		"user":         map[string]any{"username": username},
		"serverTime":   fmtTS(now),
	}, nil
}

// handleLogin 为 POST /api/v1/auth/login。
func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		errInvalid(w, "username/password 必填")
		return
	}
	if len(req.DeviceLabel) > 100 {
		errInvalid(w, "deviceLabel 超长")
		return
	}
	ip := clientIP(r)
	if ok, retry := a.loginLimiter.allow(ip + "|" + req.Username); !ok {
		w.Header().Set("Retry-After", itoa(retry))
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "尝试过多，请稍后重试")
		return
	}
	if req.Username != a.cfg.adminUser || !a.checkPassword(req.Password) {
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "账号或密码错误")
		return
	}
	resp, err := a.issueSession(req.Username, req.DeviceLabel)
	if err != nil {
		errInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// checkPassword 校验管理密码（bcrypt；启动时自环境变量建立哈希）。
func (a *app) checkPassword(pw string) bool {
	if len(a.adminHash) == 0 {
		return false
	}
	return bcrypt.CompareHashAndPassword(a.adminHash, []byte(pw)) == nil
}

// handleRefresh 为 POST /api/v1/auth/refresh（一次性轮换）。
func (a *app) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.RefreshToken == "" {
		errInvalid(w, "refreshToken 必填")
		return
	}
	var sess session
	var rexp, dl string
	var revoked int
	err := a.st.db.QueryRow(
		`SELECT id, username, device_label, refresh_expires_at, revoked FROM sessions
		 WHERE refresh_hash=?`, hashToken(req.RefreshToken),
	).Scan(&sess.ID, &sess.Username, &dl, &rexp, &revoked)
	if err != nil || revoked != 0 {
		writeErr(w, http.StatusUnauthorized, "invalid_refresh", "refreshToken 失效")
		return
	}
	t, err := parseTS(rexp)
	if err != nil || time.Now().After(t) {
		writeErr(w, http.StatusUnauthorized, "invalid_refresh", "refreshToken 已过期")
		return
	}
	// 轮换：旧值即刻作废。
	now := nowUTC()
	tok := randToken("tok_")
	rtok := randToken("rtok_")
	exp := now.Add(tokenTTL)
	newRexp := now.Add(refreshTTL)
	res, err := a.st.db.Exec(
		`UPDATE sessions SET token_hash=?, refresh_hash=?, expires_at=?, refresh_expires_at=?
		 WHERE id=? AND revoked=0`,
		hashToken(tok), hashToken(rtok), fmtTS(exp), fmtTS(newRexp), sess.ID,
	)
	if err != nil {
		errInternal(w)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, http.StatusUnauthorized, "invalid_refresh", "refreshToken 失效")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":        tok,
		"expiresAt":    fmtTS(exp),
		"refreshToken": rtok,
		"user":         map[string]any{"username": sess.Username},
		"serverTime":   fmtTS(now),
	})
}

// handleSession 为 GET /api/v1/auth/session。
func (a *app) handleSession(w http.ResponseWriter, r *http.Request) {
	s := sessionOf(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          map[string]any{"username": s.Username},
		"expiresAt":     fmtTS(s.ExpiresAt),
	})
}

// handleLogout 为 POST /api/v1/auth/logout：撤销当前 token 及关联 refreshToken。
func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	s := sessionOf(r)
	_, _ = a.st.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, s.ID)
	w.WriteHeader(http.StatusNoContent)
}

// clientIP 取请求来源 IP。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func itoa(n int) string {
	if n <= 0 {
		return "1"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
