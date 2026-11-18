package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"llmgw/internal/api/admin"
	"llmgw/internal/config"
	"llmgw/internal/db"
)

const adminPassword = "admin-password-123"

// adminClient talks to the admin API with a cookie jar and CSRF token.
type adminClient struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

func newAdminHarness(t *testing.T, mutate func(*config.Config)) (*harness, *adminClient) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	cfg := &config.Config{SecretKey: config.DeriveKey("t"), MaxBodyBytes: 1 << 20}
	if mutate != nil {
		mutate(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &harness{t: t}
	h.srv = New(cfg, d, log)
	if err := admin.EnsureAdmin(context.Background(), h.srv.Store(), adminPassword, log); err != nil {
		t.Fatal(err)
	}
	h.gw = httptest.NewServer(h.srv.Handler())
	t.Cleanup(h.gw.Close)
	jar, _ := cookiejar.New(nil)
	return h, &adminClient{t: t, base: h.gw.URL, c: &http.Client{Jar: jar}}
}

func (a *adminClient) do(method, path, body string, hdr ...string) (*http.Response, map[string]any) {
	a.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, a.base+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.csrf != "" {
		req.Header.Set(admin.CSRFHeader, a.csrf)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := a.c.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp, out
}

func (a *adminClient) login(password string) *http.Response {
	a.t.Helper()
	resp, out := a.do("POST", "/admin/api/login", `{"password":"`+password+`"}`)
	if resp.StatusCode == 200 {
		a.csrf, _ = out["csrf_token"].(string)
	}
	return resp
}

func TestAdminLoginAndCSRF(t *testing.T) {
	h, a := newAdminHarness(t, nil)

	if resp, _ := a.do("GET", "/admin/api/keys", ""); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	if resp := a.login("wrong"); resp.StatusCode != 401 {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp := a.login(adminPassword)
	if resp.StatusCode != 200 || a.csrf == "" {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == admin.SessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/admin" {
		t.Errorf("cookie = %+v", cookie)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers missing")
	}

	// CSRF token is required for state-changing requests.
	csrf := a.csrf
	a.csrf = ""
	if resp, out := a.do("POST", "/admin/api/keys", `{"name":"x"}`); resp.StatusCode != 403 {
		t.Errorf("missing csrf: %d %v", resp.StatusCode, out)
	}
	a.csrf = "bogus"
	if resp, _ := a.do("POST", "/admin/api/keys", `{"name":"x"}`); resp.StatusCode != 403 {
		t.Errorf("bad csrf: %d", resp.StatusCode)
	}
	a.csrf = csrf
	if resp, _ := a.do("POST", "/admin/api/keys", `{"name":"x"}`, "Origin", "https://evil.example"); resp.StatusCode != 403 {
		t.Errorf("cross-site origin: %d", resp.StatusCode)
	}
	if resp, _ := a.do("POST", "/admin/api/keys", `{"name":"x"}`); resp.StatusCode != 201 {
		t.Errorf("valid csrf: %d", resp.StatusCode)
	}
	if resp, out := a.do("GET", "/admin/api/session", ""); resp.StatusCode != 200 || out["username"] != "admin" || out["csrf_token"] != csrf {
		t.Errorf("session: %d %v", resp.StatusCode, out)
	}

	if resp, _ := a.do("POST", "/admin/api/logout", ""); resp.StatusCode != 204 {
		t.Errorf("logout: %d", resp.StatusCode)
	}
	if resp, _ := a.do("GET", "/admin/api/keys", ""); resp.StatusCode != 401 {
		t.Errorf("after logout: %d", resp.StatusCode)
	}
	_ = h
}

func TestAdminLoginRateLimit(t *testing.T) {
	_, a := newAdminHarness(t, nil)
	for i := 0; i < 5; i++ {
		a.login("wrong")
	}
	resp := a.login(adminPassword) // blocked even with the right password
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("status %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestAdminKeyManagement(t *testing.T) {
	h, a := newAdminHarness(t, nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w, ollamaText("ok"), ollamaDone)
	}))
	defer up.Close()
	p := h.provider("ol", "ollama", up.URL)
	h.alias("allowed", tgt{p, "m"})
	h.alias("other", tgt{p, "m"})
	a.login(adminPassword)

	// Create
	resp, k := a.do("POST", "/admin/api/keys", `{"name":"open-webui","allowed_aliases":["allowed"," allowed ",""],"log_mode":"metadata_only","rpm_limit":100}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, k)
	}
	plain, _ := k["key"].(string)
	id := int64(k["id"].(float64))
	if !strings.HasPrefix(plain, "sk-gw-") || k["key_prefix"] != plain[:10]+"…" {
		t.Errorf("key = %q prefix = %v", plain, k["key_prefix"])
	}
	if aa := k["allowed_aliases"].([]any); len(aa) != 1 || aa[0] != "allowed" {
		t.Errorf("allowed_aliases not normalized: %v", aa)
	}
	path := "/admin/api/keys/" + itoa(id)

	// The plaintext is never returned again.
	if _, got := a.do("GET", path, ""); got["key"] != nil {
		t.Error("plaintext key must not be returned by GET")
	}
	_, list := a.do("GET", "/admin/api/keys", "")
	if data := list["data"].([]any); len(data) != 1 {
		t.Errorf("list = %v", list)
	}

	// The key only sees allowed aliases.
	_, mb := h.get("/v1/models", plain)
	if !strings.Contains(mb, `"allowed"`) || strings.Contains(mb, `"other"`) {
		t.Errorf("models = %s", mb)
	}
	h.key = plain
	if resp, b := h.mustPost("/v1/chat/completions", `{"model":"other","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 403 ||
		!strings.Contains(b, "permission_error") {
		t.Errorf("not-allowed alias: %d %s", resp.StatusCode, b)
	}
	if resp, _ := h.mustPost("/v1/chat/completions", `{"model":"allowed","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 200 {
		t.Errorf("allowed alias: %d", resp.StatusCode)
	}

	// Update allowed aliases: effective immediately.
	if resp, _ := a.do("PATCH", path, `{"allowed_aliases":[]}`); resp.StatusCode != 200 {
		t.Fatalf("patch: %d", resp.StatusCode)
	}
	if _, mb := h.get("/v1/models", plain); !strings.Contains(mb, `"other"`) {
		t.Errorf("after patch models = %s", mb)
	}
	if resp, out := a.do("PATCH", path, `{"log_mode":"verbose"}`); resp.StatusCode != 400 {
		t.Errorf("invalid patch: %d %v", resp.StatusCode, out)
	}
	if resp, _ := a.do("PATCH", path, `{"unknown":1}`); resp.StatusCode != 400 {
		t.Errorf("unknown field: %d", resp.StatusCode)
	}

	// Disable / enable
	a.do("POST", path+"/disable", "")
	if resp, b := h.get("/v1/models", plain); resp.StatusCode != 401 || !strings.Contains(b, "API key is disabled") ||
		!strings.Contains(b, `"type":"authentication_error"`) {
		t.Errorf("disabled: %d %s", resp.StatusCode, b)
	}
	a.do("POST", path+"/enable", "")
	if resp, _ := h.get("/v1/models", plain); resp.StatusCode != 200 {
		t.Errorf("enabled: %d", resp.StatusCode)
	}

	// Expiry
	a.do("PATCH", path, `{"expires_at":"2000-01-01T00:00:00Z"}`)
	if resp, b := h.get("/v1/models", plain); resp.StatusCode != 401 || !strings.Contains(b, "expired") {
		t.Errorf("expired: %d %s", resp.StatusCode, b)
	}
	a.do("PATCH", path, `{"expires_at":null}`)

	// Rotate with grace: both keys work; record settings are kept.
	resp, rk := a.do("POST", path+"/rotate", `{"grace_minutes":5}`)
	newKey, _ := rk["key"].(string)
	if resp.StatusCode != 200 || newKey == "" || newKey == plain || rk["previous_key_valid_until"] == nil || rk["name"] != "open-webui" {
		t.Fatalf("rotate: %d %v", resp.StatusCode, rk)
	}
	if r1, _ := h.get("/v1/models", plain); r1.StatusCode != 200 {
		t.Error("old key must work during grace")
	}
	// Rotate without grace: previous keys stop immediately.
	_, rk2 := a.do("POST", path+"/rotate", "")
	newest := rk2["key"].(string)
	if r1, _ := h.get("/v1/models", newKey); r1.StatusCode != 401 {
		t.Error("old key must fail after rotation without grace")
	}
	// x-api-key works as well.
	req, _ := http.NewRequest("GET", h.gw.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", newest)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 200 {
		t.Errorf("x-api-key: %d", r.StatusCode)
	}

	// Delete
	if resp, _ := a.do("DELETE", path, ""); resp.StatusCode != 204 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	if r, _ := h.get("/v1/models", newest); r.StatusCode != 401 {
		t.Error("deleted key must fail")
	}
	if resp, _ := a.do("GET", path, ""); resp.StatusCode != 404 {
		t.Errorf("get deleted: %d", resp.StatusCode)
	}

	// Audit log
	_, al := a.do("GET", "/admin/api/audit-logs", "")
	actions := map[string]bool{}
	for _, e := range al["data"].([]any) {
		actions[e.(map[string]any)["action"].(string)] = true
	}
	for _, want := range []string{"login.success", "key.create", "key.update", "key.disable", "key.enable", "key.rotate", "key.delete"} {
		if !actions[want] {
			t.Errorf("audit log missing %s (have %v)", want, actions)
		}
	}
}

func TestClientAuthErrors(t *testing.T) {
	h, _ := newAdminHarness(t, nil)
	resp, b := h.get("/v1/models", "")
	if resp.StatusCode != 401 || !strings.Contains(b, `"code":"invalid_api_key"`) || !strings.Contains(b, "missing API key") {
		t.Errorf("missing: %d %s", resp.StatusCode, b)
	}
	resp, b = h.get("/v1/models", "sk-gw-wrong")
	if resp.StatusCode != 401 || !strings.Contains(b, "invalid API key") {
		t.Errorf("invalid: %d %s", resp.StatusCode, b)
	}
}

func TestPasswordChange(t *testing.T) {
	_, a := newAdminHarness(t, nil)
	a.login(adminPassword)
	if resp, _ := a.do("POST", "/admin/api/password", `{"current_password":"nope","new_password":"new-password-456"}`); resp.StatusCode != 403 {
		t.Errorf("wrong current: %d", resp.StatusCode)
	}
	if resp, _ := a.do("POST", "/admin/api/password", `{"current_password":"`+adminPassword+`","new_password":"short"}`); resp.StatusCode != 400 {
		t.Errorf("short: %d", resp.StatusCode)
	}
	if resp, _ := a.do("POST", "/admin/api/password", `{"current_password":"`+adminPassword+`","new_password":"new-password-456"}`); resp.StatusCode != 204 {
		t.Errorf("change: %d", resp.StatusCode)
	}
	// Current session survives.
	if resp, _ := a.do("GET", "/admin/api/session", ""); resp.StatusCode != 200 {
		t.Errorf("session after change: %d", resp.StatusCode)
	}
	if resp := a.login(adminPassword); resp.StatusCode != 401 {
		t.Errorf("old password still works: %d", resp.StatusCode)
	}
	if resp := a.login("new-password-456"); resp.StatusCode != 200 {
		t.Errorf("new password: %d", resp.StatusCode)
	}
}

func TestTrustHeader(t *testing.T) {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	_, a := newAdminHarness(t, func(c *config.Config) {
		c.AdminTrustHeader = "X-Forwarded-User"
		c.TrustedProxies = loopback
	})
	resp, out := a.do("GET", "/admin/api/session", "", "X-Forwarded-User", "alice")
	if resp.StatusCode != 200 || out["username"] != "proxy:alice" || out["mode"] != "trusted_header" {
		t.Fatalf("session: %d %v", resp.StatusCode, out)
	}
	if resp, _ := a.do("POST", "/admin/api/keys", `{"name":"via-proxy"}`, "X-Forwarded-User", "alice"); resp.StatusCode != 201 {
		t.Errorf("create via trusted header: %d", resp.StatusCode)
	}
	if resp, _ := a.do("GET", "/admin/api/keys", ""); resp.StatusCode != 401 {
		t.Errorf("without header: %d", resp.StatusCode)
	}

	// Header from an untrusted peer is ignored.
	_, b := newAdminHarness(t, func(c *config.Config) { c.AdminTrustHeader = "X-Forwarded-User" })
	if resp, _ := b.do("GET", "/admin/api/session", "", "X-Forwarded-User", "mallory"); resp.StatusCode != 401 {
		t.Errorf("untrusted peer: %d", resp.StatusCode)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
