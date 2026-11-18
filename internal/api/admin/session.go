// Package admin implements the admin REST API (/admin/api/...): login
// sessions with CSRF protection, virtual key management and audit logs.
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"llmgw/internal/auth"
	"llmgw/internal/clock"
	"llmgw/internal/gateway"
	"llmgw/internal/keys"
	"llmgw/internal/store"
)

const (
	// SessionCookie is the admin session cookie name.
	SessionCookie = "llmgw_session"
	// CSRFHeader carries the CSRF token on state-changing requests.
	CSRFHeader = "X-CSRF-Token"

	defaultSessionTTL = 24 * time.Hour
	touchEvery        = time.Minute

	loginWindow      = 15 * time.Minute
	loginMaxFailures = 5
)

// API serves the admin REST API.
type API struct {
	Store        *store.Store
	Keys         *keys.Service
	Clock        clock.Clock
	Log          *slog.Logger
	CookieSecure bool
	// TrustHeader, when set, authenticates requests carrying this header if
	// they come from a trusted proxy (PeerTrusted).
	TrustHeader string
	PeerTrusted func(*http.Request) bool
	ClientIP    func(*http.Request) string
	SessionTTL  time.Duration
	// HTTPClient is used for provider connectivity checks.
	HTTPClient *http.Client
	// Dispatch serves an API request (path) as the given principal; used by
	// the Playground.
	Dispatch func(w http.ResponseWriter, r *http.Request, p *gateway.Principal, path string)
	// BackupDir holds temporary backup files (the database directory).
	BackupDir string

	limiter loginLimiter
}

// actor is the authenticated administrator of a request.
type actor struct {
	Name    string
	AdminID int64          // 0 in trust-header mode
	Session *store.Session // nil in trust-header mode
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, a *actor)

func (a *API) sessionTTL() time.Duration {
	if a.SessionTTL > 0 {
		return a.SessionTTL
	}
	return defaultSessionTTL
}

func (a *API) clientIP(r *http.Request) string {
	if a.ClientIP != nil {
		return a.ClientIP(r)
	}
	return r.RemoteAddr
}

// securityHeaders are set on every admin API response.
func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// crossSite reports whether a browser request originates from another site.
func crossSite(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
		return true
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return true
		}
	}
	return false
}

// public wraps unauthenticated endpoints (login).
func (a *API) public(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		if !isSafeMethod(r.Method) && crossSite(r) {
			writeError(w, http.StatusForbidden, "cross_site", "cross-site request rejected")
			return
		}
		h(w, r)
	}
}

// protected wraps endpoints that require an authenticated admin.
func (a *API) protected(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		mutating := !isSafeMethod(r.Method)
		if mutating && crossSite(r) {
			writeError(w, http.StatusForbidden, "cross_site", "cross-site request rejected")
			return
		}

		// Forward-auth mode: a trusted proxy has already authenticated the user.
		if a.TrustHeader != "" && a.PeerTrusted != nil {
			if v := strings.TrimSpace(r.Header.Get(a.TrustHeader)); v != "" && a.PeerTrusted(r) {
				// No session -> no CSRF token. Cross-site checks above plus a
				// JSON-only body (forms cannot send it without a preflight) protect
				// state-changing requests.
				if mutating && r.ContentLength != 0 && !isJSON(r) {
					writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
					return
				}
				h(w, r, &actor{Name: "proxy:" + v})
				return
			}
		}

		c, err := r.Cookie(SessionCookie)
		if err != nil || c.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "login required")
			return
		}
		now := a.Clock.Now()
		sess, err := a.Store.GetSession(r.Context(), auth.SHA256Hex(c.Value), now)
		if errors.Is(err, store.ErrNotFound) {
			a.clearCookie(w)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "session expired; login again")
			return
		}
		if err != nil {
			a.Log.Error("load session", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if mutating && subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(sess.CSRFToken)) != 1 {
			writeError(w, http.StatusForbidden, "csrf", "missing or invalid "+CSRFHeader+" header")
			return
		}
		if now.Sub(sess.LastSeenAt) > touchEvery {
			exp := now.Add(a.sessionTTL())
			if err := a.Store.TouchSession(r.Context(), sess.ID, now, exp); err == nil {
				sess.ExpiresAt = exp
				a.setCookie(w, c.Value, exp)
			}
		}
		h(w, r, &actor{Name: sess.Username, AdminID: sess.AdminID, Session: sess})
	}
}

func isJSON(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json")
}

func (a *API) setCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/admin", Expires: exp,
		HttpOnly: true, Secure: a.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
}

func (a *API) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/admin", MaxAge: -1,
		HttpOnly: true, Secure: a.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
}

func (a *API) audit(r *http.Request, act, action, targetType, targetID string, detail any) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := a.Store.AddAudit(ctx, act, action, targetType, targetID, detail, a.clientIP(r)); err != nil {
		a.Log.Error("write audit log", "err", err, "action", action)
	}
}

// ---- login / logout / session ---------------------------------------------

// dummyHash is verified when the user does not exist, to keep timing uniform.
var dummyHash = func() string {
	h, _ := auth.HashPassword("llmgw-dummy-password")
	return h
}()

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type sessionResponse struct {
	Username  string     `json:"username"`
	CSRFToken string     `json:"csrf_token,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Mode      string     `json:"mode"` // session | trusted_header
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	ip := a.clientIP(r)
	now := a.Clock.Now()
	if wait, blocked := a.limiter.blocked(ip, now); blocked {
		w.Header().Set("Retry-After", retryAfter(wait))
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed login attempts; try again later")
		return
	}
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Username == "" {
		req.Username = "admin"
	}
	adm, err := a.Store.GetAdminByUsername(r.Context(), req.Username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.Log.Error("load admin", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	hash := dummyHash
	if adm != nil {
		hash = adm.PasswordHash
	}
	ok, verr := auth.VerifyPassword(hash, req.Password)
	if verr != nil {
		a.Log.Error("verify password", "err", verr)
	}
	if adm == nil || !ok {
		a.limiter.fail(ip, now)
		a.audit(r, req.Username, "login.failure", "admin", "", nil)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	a.limiter.reset(ip)

	token := auth.RandomToken(32)
	exp := now.Add(a.sessionTTL())
	sess := &store.Session{ID: auth.SHA256Hex(token), AdminID: adm.ID, CSRFToken: auth.RandomToken(32),
		ExpiresAt: exp, LastSeenAt: now}
	if err := a.Store.CreateSession(r.Context(), sess); err != nil {
		a.Log.Error("create session", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	_ = a.Store.DeleteExpiredSessions(r.Context(), now)
	a.setCookie(w, token, exp)
	a.audit(r, adm.Username, "login.success", "admin", "", nil)
	writeJSON(w, http.StatusOK, sessionResponse{Username: adm.Username, CSRFToken: sess.CSRFToken, ExpiresAt: &exp, Mode: "session"})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request, act *actor) {
	if act.Session != nil {
		_ = a.Store.DeleteSession(r.Context(), act.Session.ID)
		a.audit(r, act.Name, "logout", "admin", "", nil)
	}
	a.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) session(w http.ResponseWriter, r *http.Request, act *actor) {
	if act.Session == nil {
		writeJSON(w, http.StatusOK, sessionResponse{Username: act.Name, Mode: "trusted_header"})
		return
	}
	exp := act.Session.ExpiresAt
	writeJSON(w, http.StatusOK, sessionResponse{Username: act.Name, CSRFToken: act.Session.CSRFToken, ExpiresAt: &exp, Mode: "session"})
}

type passwordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

const minPasswordLen = 12

func (a *API) changePassword(w http.ResponseWriter, r *http.Request, act *actor) {
	if act.Session == nil {
		writeError(w, http.StatusBadRequest, "not_supported", "password change requires a password session (not available with the trusted header)")
		return
	}
	var req passwordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.NewPassword) < minPasswordLen {
		writeError(w, http.StatusBadRequest, "invalid_request", "new_password must be at least 12 characters")
		return
	}
	adm, err := a.Store.GetAdmin(r.Context(), act.AdminID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if ok, _ := auth.VerifyPassword(adm.PasswordHash, req.CurrentPassword); !ok {
		a.audit(r, act.Name, "password.change_failure", "admin", "", nil)
		writeError(w, http.StatusForbidden, "invalid_credentials", "current_password is incorrect")
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// Other sessions are revoked; the current one stays.
	if err := a.Store.SetAdminPassword(r.Context(), adm.ID, hash, act.Session.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.audit(r, act.Name, "password.change", "admin", "", nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- login rate limit ------------------------------------------------------

// loginLimiter blocks an IP after loginMaxFailures failures within loginWindow.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func (l *loginLimiter) prune(ip string, now time.Time) []time.Time {
	f := l.failures[ip]
	i := 0
	for i < len(f) && now.Sub(f[i]) >= loginWindow {
		i++
	}
	f = f[i:]
	if len(f) == 0 {
		delete(l.failures, ip)
	} else {
		l.failures[ip] = f
	}
	return f
}

func (l *loginLimiter) blocked(ip string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures == nil {
		return 0, false
	}
	f := l.prune(ip, now)
	if len(f) >= loginMaxFailures {
		return f[0].Add(loginWindow).Sub(now), true
	}
	return 0, false
}

func (l *loginLimiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures == nil {
		l.failures = map[string][]time.Time{}
	}
	l.prune(ip, now)
	l.failures[ip] = append(l.failures[ip], now)
	if len(l.failures) > 10000 { // bound memory under a distributed attack
		for k := range l.failures {
			delete(l.failures, k)
			break
		}
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

// ---- helpers ---------------------------------------------------------------

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Message, b.Error.Code = msg, code
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func retryAfter(d time.Duration) string {
	s := int64((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	b, _ := json.Marshal(s)
	return string(b)
}
