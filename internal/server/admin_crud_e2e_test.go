package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/store"
)

// fakeOllama serves /api/tags, an Anthropic-style /v1/messages and /api/chat.
func fakeOllama(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			io.WriteString(w, `{"models":[{"name":"qwen3:8b"},{"name":"llama3:latest"}]}`)
		case "/v1/messages":
			w.WriteHeader(404)
			io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`)
		case "/api/chat":
			writeLines(w, ollamaText(reply), ollamaDone)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAdminProviders(t *testing.T) {
	_, a := newAdminHarness(t, nil)
	a.login(adminPassword)
	up := fakeOllama(t, "x")
	plain := httptest.NewServer(http.NotFoundHandler()) // no /v1/messages, no tags
	defer plain.Close()

	resp, out := a.do("POST", "/admin/api/providers", `{"name":"local","type":"ollama","base_url":"`+up.URL+`/","api_key":"upstream-secret"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}
	p := out["provider"].(map[string]any)
	check := out["check"].(map[string]any)
	if p["supports_anthropic"] != true || check["ok"] != true || len(p["models"].([]any)) != 2 || p["has_api_key"] != true ||
		p["base_url"] != up.URL || p["health"].(map[string]any)["status"] != "ok" {
		t.Errorf("created provider = %v / check = %v", p, check)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "upstream-secret") {
		t.Error("API key leaked in response")
	}
	eff := p["effective"].(map[string]any)["ttft_timeout"].(map[string]any)
	if eff["source"] != "global" || eff["ms"] != 300000.0 {
		t.Errorf("effective = %v", eff)
	}
	id := itoa(int64(p["id"].(float64)))

	resp, out = a.do("POST", "/admin/api/providers", `{"name":"plain","type":"openai_compatible","base_url":"`+plain.URL+`"}`)
	if resp.StatusCode != 201 || out["provider"].(map[string]any)["supports_anthropic"] != false || out["check"].(map[string]any)["ok"] != false {
		t.Errorf("plain provider: %v", out)
	}
	if resp, _ := a.do("POST", "/admin/api/providers", `{"name":"local","type":"ollama","base_url":"http://x"}`); resp.StatusCode != 409 {
		t.Errorf("duplicate: %d", resp.StatusCode)
	}
	if resp, _ := a.do("POST", "/admin/api/providers", `{"name":"bad","type":"ollama","base_url":"ftp://x"}`); resp.StatusCode != 400 {
		t.Errorf("bad url: %d", resp.StatusCode)
	}

	// Update: timeout override + clear key.
	resp, out = a.do("PATCH", "/admin/api/providers/"+id, `{"ttft_timeout_ms":60000,"api_key":null}`)
	if resp.StatusCode != 200 || out["has_api_key"] != false ||
		out["effective"].(map[string]any)["ttft_timeout"].(map[string]any)["source"] != "provider" {
		t.Errorf("update: %d %v", resp.StatusCode, out)
	}
	if resp, out := a.do("POST", "/admin/api/providers/"+id+"/test", ""); resp.StatusCode != 200 || out["check"].(map[string]any)["anthropic_detected"] != true {
		t.Errorf("test: %v", out)
	}

	// In use -> 409
	a.do("POST", "/admin/api/aliases", `{"name":"m","targets":[{"provider_id":`+id+`,"upstream_model":"qwen3:8b"}]}`)
	if resp, out := a.do("DELETE", "/admin/api/providers/"+id, ""); resp.StatusCode != 409 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "m") {
		t.Errorf("delete in use: %d %v", resp.StatusCode, out)
	}
}

func TestAdminAliases(t *testing.T) {
	h, a := newAdminHarness(t, nil)
	a.login(adminPassword)
	up1 := fakeOllama(t, "from-one")
	up2 := fakeOllama(t, "from-two")
	_, o1 := a.do("POST", "/admin/api/providers", `{"name":"one","type":"ollama","base_url":"`+up1.URL+`"}`)
	_, o2 := a.do("POST", "/admin/api/providers", `{"name":"two","type":"ollama","base_url":"`+up2.URL+`","idle_timeout_ms":5000}`)
	p1 := itoa(int64(o1["provider"].(map[string]any)["id"].(float64)))
	p2 := itoa(int64(o2["provider"].(map[string]any)["id"].(float64)))

	resp, al := a.do("POST", "/admin/api/aliases", `{"name":"coder","description":"d","targets":[{"provider_id":`+p1+`,"upstream_model":"qwen3:8b"},{"provider_id":`+p2+`,"upstream_model":"llama3"}],
		"ttft_timeout_ms":900000,"fallback_on_ttft_timeout":true,"params":{"overrides":{"num_ctx":32768},"system_prompt":{"mode":"prepend","text":"be nice"}},
		"model_info":{"context_length":65536,"tools":true,"vision":null}}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, al)
	}
	effs := al["effective"].([]any)
	e1, e2 := effs[0].(map[string]any), effs[1].(map[string]any)
	if e1["ttft_timeout"].(map[string]any)["source"] != "alias" || e2["idle_timeout"].(map[string]any)["source"] != "provider" ||
		e1["idle_timeout"].(map[string]any)["source"] != "global" || e1["fallback_on_ttft_timeout"] != true {
		t.Errorf("effective = %v", effs)
	}
	id := itoa(int64(al["id"].(float64)))

	for name, body := range map[string]string{
		"bad params":   `{"name":"x","targets":[{"provider_id":` + p1 + `,"upstream_model":"m"}],"params":{"bogus":1}}`,
		"bad sysmode":  `{"name":"x","targets":[{"provider_id":` + p1 + `,"upstream_model":"m"}],"params":{"system_prompt":{"mode":"append"}}}`,
		"no targets":   `{"name":"x","targets":[]}`,
		"bad provider": `{"name":"x","targets":[{"provider_id":999,"upstream_model":"m"}]}`,
		"space name":   `{"name":"a b","targets":[{"provider_id":` + p1 + `,"upstream_model":"m"}]}`,
		"bad ctx":      `{"name":"x","targets":[{"provider_id":` + p1 + `,"upstream_model":"m"}],"model_info":{"context_length":1.5}}`,
		"bad tools":    `{"name":"x","targets":[{"provider_id":` + p1 + `,"upstream_model":"m"}],"model_info":{"tools":"yes"}}`,
		"bad mi key":   `{"name":"x","targets":[{"provider_id":` + p1 + `,"upstream_model":"m"}],"model_info":{"ctx":1}}`,
	} {
		if resp, _ := a.do("POST", "/admin/api/aliases", body); resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := a.do("POST", "/admin/api/aliases", `{"name":"coder","targets":[{"provider_id":`+p1+`,"upstream_model":"m"}]}`); resp.StatusCode != 409 {
		t.Errorf("duplicate: %d", resp.StatusCode)
	}

	// Re-pointing an alias takes effect immediately.
	h.key = h.newKey(&store.VirtualKey{Name: "k"})
	_, b := h.mustPost("/v1/chat/completions", `{"model":"coder","messages":[{"role":"user","content":"x"}]}`)
	if !strings.Contains(b, "from-one") {
		t.Fatalf("before: %s", b)
	}
	if resp, _ := a.do("PATCH", "/admin/api/aliases/"+id, `{"targets":[{"provider_id":`+p2+`,"upstream_model":"llama3"}]}`); resp.StatusCode != 200 {
		t.Fatalf("patch: %d", resp.StatusCode)
	}
	_, b = h.mustPost("/v1/chat/completions", `{"model":"coder","messages":[{"role":"user","content":"x"}]}`)
	if !strings.Contains(b, "from-two") {
		t.Errorf("after re-point: %s", b)
	}
	_, got := a.do("GET", "/admin/api/aliases/"+id, "")
	if got["params"].(map[string]any)["overrides"] == nil || got["description"] != "d" || len(got["targets"].([]any)) != 1 {
		t.Errorf("patch must keep other fields: %v", got)
	}
	if mi := got["model_info"].(map[string]any); mi["context_length"] != 65536.0 || mi["tools"] != true || mi["vision"] != nil {
		t.Errorf("model_info = %v", mi)
	}
	if resp, _ := a.do("DELETE", "/admin/api/aliases/"+id, ""); resp.StatusCode != 204 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
}

func TestAdminSettings(t *testing.T) {
	h, a := newAdminHarness(t, nil)
	a.login(adminPassword)
	_, s := a.do("GET", "/admin/api/settings", "")
	if s["values"].(map[string]any)["ttft_timeout_ms"] != 300000.0 || s["defaults"].(map[string]any)["log_retention_days"] != 30.0 {
		t.Errorf("settings = %v", s)
	}
	resp, s := a.do("PATCH", "/admin/api/settings", `{"ttft_timeout_ms":600000,"log_upstream_request":true}`)
	if resp.StatusCode != 200 || s["values"].(map[string]any)["ttft_timeout_ms"] != 600000.0 {
		t.Errorf("patch: %d %v", resp.StatusCode, s)
	}
	got, _ := h.srv.Store().Settings(t.Context())
	if !got.LogUpstreamRequest {
		t.Error("setting not stored")
	}
	if resp, _ := a.do("PATCH", "/admin/api/settings", `{"ttft_timeout_ms":-1}`); resp.StatusCode != 400 {
		t.Errorf("negative: %d", resp.StatusCode)
	}
	if resp, _ := a.do("PATCH", "/admin/api/settings", `{"idle_timeout_ms":1,"nope":1}`); resp.StatusCode != 400 {
		t.Errorf("unknown: %d", resp.StatusCode)
	}
	if got, _ := h.srv.Store().Settings(t.Context()); got.IdleTimeout != store.DefaultSettings().IdleTimeout {
		t.Error("a rejected PATCH must not apply partially")
	}
}

func TestAdminPlayground(t *testing.T) {
	h, a := newAdminHarness(t, nil)
	a.login(adminPassword)
	up := fakeOllama(t, "pg-reply")
	p := h.provider("ol", "ollama", up.URL)
	h.alias("open", tgt{p, "m"})
	h.alias("closed", tgt{p, "m"})
	_, k := a.do("POST", "/admin/api/keys", `{"name":"limited","allowed_aliases":["open"]}`)
	kid := itoa(int64(k["id"].(float64)))

	resp, out := a.do("POST", "/admin/api/playground/v1/chat/completions?key_id="+kid, `{"model":"open","messages":[{"role":"user","content":"x"}]}`)
	raw, _ := json.Marshal(out)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "pg-reply") {
		t.Errorf("as key: %d %s", resp.StatusCode, raw)
	}
	if resp, _ := a.do("POST", "/admin/api/playground/v1/chat/completions?key_id="+kid, `{"model":"closed","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 403 {
		t.Errorf("key restrictions must apply: %d", resp.StatusCode)
	}
	if resp, _ := a.do("POST", "/admin/api/playground/api/chat", `{"model":"closed","stream":false,"messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 200 {
		t.Errorf("admin (no key): %d", resp.StatusCode)
	}
	if resp, _ := a.do("POST", "/admin/api/playground/api/pull", `{}`); resp.StatusCode != 404 {
		t.Errorf("unsupported path: %d", resp.StatusCode)
	}
	a.do("POST", "/admin/api/keys/"+kid+"/disable", "")
	if resp, _ := a.do("POST", "/admin/api/playground/v1/chat/completions?key_id="+kid, `{"model":"open","messages":[]}`); resp.StatusCode != 401 {
		t.Errorf("disabled key: %d", resp.StatusCode)
	}
	csrf := a.csrf
	a.csrf = ""
	if resp, _ := a.do("POST", "/admin/api/playground/v1/chat/completions", `{}`); resp.StatusCode != 403 {
		t.Errorf("csrf required: %d", resp.StatusCode)
	}
	a.csrf = csrf
}

func TestAdminUI(t *testing.T) {
	h, _ := newAdminHarness(t, nil)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := noRedirect.Get(h.gw.URL + "/admin")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/admin/" {
		t.Errorf("redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = http.Get(h.gw.URL + "/admin/")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `assets/app.js?v=`) || strings.Contains(string(b), "{{VERSION}}") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Errorf("index: %d %s", resp.StatusCode, resp.Header)
	}
	for file, ct := range map[string]string{"app.js": "text/javascript", "app.css": "text/css"} {
		resp, _ := http.Get(h.gw.URL + "/admin/assets/" + file)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), ct) {
			t.Errorf("%s: %d %s", file, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	if resp, _ := http.Get(h.gw.URL + "/admin/assets/..%2fui.go"); resp.StatusCode != 404 {
		t.Errorf("traversal: %d", resp.StatusCode)
	}
}
