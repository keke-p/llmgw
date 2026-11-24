package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeOllamaAdmin is an Ollama with a mutable model list and the model
// management endpoints; it records the request bodies it received.
type fakeOllamaAdmin struct {
	mu     sync.Mutex
	models []string
	bodies map[string]map[string]any
	auth   string
}

func newFakeOllamaAdmin(t *testing.T) (*fakeOllamaAdmin, *httptest.Server) {
	f := &fakeOllamaAdmin{models: []string{"qwen3:8b"}, bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = r.Header.Get("Authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.bodies[r.URL.Path] = body
		switch r.URL.Path {
		case "/api/tags":
			var ms []map[string]any
			for _, m := range f.models {
				ms = append(ms, map[string]any{"name": m, "size": 123, "details": map[string]any{"parameter_size": "8B"}})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": ms})
		case "/api/ps":
			io.WriteString(w, `{"models":[{"name":"qwen3:8b","size_vram":42,"expires_at":"2030-01-01T00:00:00Z"}]}`)
		case "/api/show":
			io.WriteString(w, `{"modelfile":"FROM x","parameters":"num_ctx 4096","details":{"family":"qwen3"},"tensors":[{"name":"t"}]}`)
		case "/api/pull":
			if body["model"] == "missing" {
				w.WriteHeader(500)
				io.WriteString(w, `{"error":"pull model manifest: file does not exist"}`)
				return
			}
			io.WriteString(w, `{"status":"pulling manifest"}`+"\n"+`{"status":"pulling abc","digest":"sha256:abc","total":100,"completed":50}`+"\n")
			w.(http.Flusher).Flush()
			if body["model"] == "broken" {
				io.WriteString(w, `{"error":"digest mismatch"}`+"\n")
				return
			}
			f.models = append(f.models, body["model"].(string))
			io.WriteString(w, `{"status":"success"}`+"\n")
		case "/api/create":
			f.models = append(f.models, body["model"].(string))
			io.WriteString(w, `{"status":"using existing layer"}`+"\n"+`{"status":"success"}`+"\n")
		case "/api/copy":
			f.models = append(f.models, body["destination"].(string))
		case "/api/delete":
			for i, m := range f.models {
				if m == body["model"] {
					f.models = append(f.models[:i], f.models[i+1:]...)
					return
				}
			}
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"model 'x' not found"}`)
		case "/api/generate":
			io.WriteString(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeOllamaAdmin) body(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[path]
}

// raw sends a request and returns the raw response body.
func (a *adminClient) raw(method, path, body string) (*http.Response, string) {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", a.csrf)
	resp, err := a.c.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestAdminOllamaModels(t *testing.T) {
	h, a := newAdminHarness(t, nil)
	a.login(adminPassword)
	fake, up := newFakeOllamaAdmin(t)
	_, out := a.do("POST", "/admin/api/providers", `{"name":"ol","type":"ollama","base_url":"`+up.URL+`","api_key":"k1"}`)
	id := itoa(int64(out["provider"].(map[string]any)["id"].(float64)))
	base := "/admin/api/providers/" + id + "/ollama"
	models := func() []any {
		_, p := a.do("GET", "/admin/api/providers/"+id, "")
		return p["models"].([]any)
	}

	// List with loaded state.
	resp, out := a.do("GET", base+"/models", "")
	if resp.StatusCode != 200 {
		t.Fatalf("models: %d %v", resp.StatusCode, out)
	}
	m := out["data"].([]any)[0].(map[string]any)
	if m["name"] != "qwen3:8b" || m["loaded"].(map[string]any)["size_vram"] != 42.0 {
		t.Errorf("models = %v", out)
	}
	if fake.auth != "Bearer k1" {
		t.Errorf("upstream auth = %q", fake.auth)
	}

	// Show drops tensors.
	resp, out = a.do("GET", base+"/show?model=qwen3:8b", "")
	if resp.StatusCode != 200 || out["parameters"] != "num_ctx 4096" || out["tensors"] != nil {
		t.Errorf("show: %d %v", resp.StatusCode, out)
	}

	// Pull streams progress and refreshes the stored model list.
	resp, s := a.raw("POST", base+"/pull", `{"model":"llama3.2:1b"}`)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ndjson" ||
		!strings.Contains(s, `"completed":50`) || !strings.HasSuffix(s, `{"status":"success"}`+"\n") {
		t.Errorf("pull: %d %q", resp.StatusCode, s)
	}
	if len(models()) != 2 {
		t.Errorf("models after pull = %v", models())
	}
	// Upstream error before the stream -> admin JSON error with upstream message.
	resp, out = a.do("POST", base+"/pull", `{"model":"missing"}`)
	if resp.StatusCode != 502 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "file does not exist") {
		t.Errorf("pull missing: %d %v", resp.StatusCode, out)
	}
	// Error in the stream is passed through as a line.
	if _, s := a.raw("POST", base+"/pull", `{"model":"broken"}`); !strings.Contains(s, `{"error":"digest mismatch"}`) {
		t.Errorf("pull broken: %q", s)
	}
	if resp, _ := a.do("POST", base+"/pull", `{"model":"a b"}`); resp.StatusCode != 400 {
		t.Errorf("bad name: %d", resp.StatusCode)
	}

	// Create from a Modelfile.
	resp, s = a.raw("POST", base+"/create", `{"model":"mine","modelfile":"FROM qwen3:8b\nPARAMETER num_ctx 8192\nSYSTEM hi"}`)
	if resp.StatusCode != 200 || !strings.Contains(s, "success") {
		t.Errorf("create: %d %q", resp.StatusCode, s)
	}
	cb := fake.body("/api/create")
	if cb["from"] != "qwen3:8b" || cb["system"] != "hi" || cb["stream"] != true ||
		cb["parameters"].(map[string]any)["num_ctx"] != 8192.0 {
		t.Errorf("create body = %v", cb)
	}
	if resp, _ := a.do("POST", base+"/create", `{"model":"x","modelfile":"FROM ./a.gguf"}`); resp.StatusCode != 400 {
		t.Errorf("create from file: %d", resp.StatusCode)
	}

	// Copy, unload, delete.
	if resp, out := a.do("POST", base+"/copy", `{"source":"mine","destination":"mine2"}`); resp.StatusCode != 204 {
		t.Errorf("copy: %d %v", resp.StatusCode, out)
	}
	if resp, _ := a.do("POST", base+"/unload", `{"model":"qwen3:8b"}`); resp.StatusCode != 204 || fake.body("/api/generate")["keep_alive"] != 0.0 {
		t.Errorf("unload: %d %v", resp.StatusCode, fake.body("/api/generate"))
	}
	if resp, _ := a.do("DELETE", base+"/models?model=mine2", ""); resp.StatusCode != 204 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	if resp, _ := a.do("DELETE", base+"/models?model=mine2", ""); resp.StatusCode != 404 {
		t.Errorf("delete missing: %d", resp.StatusCode)
	}
	if n := len(models()); n != 3 { // qwen3:8b, llama3.2:1b, mine
		t.Errorf("models after delete = %v", models())
	}

	// Audit trail.
	_, au := a.do("GET", "/admin/api/audit-logs", "")
	acts := map[string]bool{}
	for _, e := range au["data"].([]any) {
		acts[e.(map[string]any)["action"].(string)] = true
	}
	for _, want := range []string{"ollama.pull", "ollama.create", "ollama.copy", "ollama.unload", "ollama.delete"} {
		if !acts[want] {
			t.Errorf("audit missing %s", want)
		}
	}

	// CSRF is required; non-ollama providers are rejected.
	csrf := a.csrf
	a.csrf = ""
	if resp, _ := a.do("POST", base+"/pull", `{"model":"x"}`); resp.StatusCode != 403 {
		t.Errorf("no csrf: %d", resp.StatusCode)
	}
	a.csrf = csrf
	oid := h.provider("oa", "openai_compatible", up.URL)
	if resp, _ := a.do("GET", "/admin/api/providers/"+itoa(oid)+"/ollama/models", ""); resp.StatusCode != 400 {
		t.Errorf("openai provider: %d", resp.StatusCode)
	}
}
