package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/store"
)

// seqUpstream records the order of upstream events ("start N", "closed N").
type seqUpstream struct {
	mu     sync.Mutex
	events []string
	n      int
}

func (s *seqUpstream) add(e string) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func (s *seqUpstream) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func TestProviderConcurrencyReleasedOnUpstreamClose(t *testing.T) {
	h := newHarness(t)
	seq := &seqUpstream{}
	first := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		seq.mu.Lock()
		seq.n++
		n := seq.n
		seq.mu.Unlock()
		seq.add("start" + itoa(int64(n)))
		if n == 1 {
			writeLines(w, ollamaText("one"))
			close(first)
			<-r.Context().Done() // generation continues until the gateway disconnects
			seq.add("closed1")
			return
		}
		writeLines(w, ollamaText("two"), ollamaDone)
	}))
	defer up.Close()
	pid := h.provider("ol", "ollama", up.URL)
	h.exec(`UPDATE providers SET max_concurrency = 1 WHERE id = ?`, pid)
	h.alias("m", tgt{pid, "x"})
	h.onlyTimeouts(nil)

	ctx1, cancel1 := context.WithCancel(context.Background())
	resp1, err := h.post(ctx1, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"a"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	newSSE(resp1.Body).next(t)
	<-first

	ch2 := h.postAsync(context.Background(), "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"b"}]}`)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if inUse, waiting := h.srv.Engine().Limits.Stats("provider:" + itoa(pid)); inUse == 1 && waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second request was not queued")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := seq.list(); len(got) != 1 {
		t.Fatalf("second request reached upstream while the slot was taken: %v", got)
	}
	cancel1() // client 1 disconnects -> gateway closes upstream connection -> slot freed
	resp1.Body.Close()
	resp2 := waitResp(t, ch2)
	b, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 200 || !strings.Contains(string(b), "two") {
		t.Fatalf("second: %d %s", resp2.StatusCode, b)
	}
	got := seq.list()
	if len(got) != 3 || got[0] != "start1" || got[1] != "closed1" || got[2] != "start2" {
		t.Errorf("slot must be released only after upstream 1 was closed: %v", got)
	}
}

func TestQueueTimeout(t *testing.T) {
	h := newHarness(t)
	up, received, _ := blockingUpstream()
	defer up.Close()
	pid := h.provider("ol", "ollama", up.URL)
	h.exec(`UPDATE providers SET max_concurrency = 1 WHERE id = ?`, pid)
	h.alias("m", tgt{pid, "x"})
	h.onlyTimeouts(map[string]any{"queue_timeout_ms": 120_000})

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1() // lets the blocked upstream finish quickly
	h.postAsync(ctx1, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"a"}]}`)
	<-received
	ch := h.postAsync(context.Background(), "/v1/messages", `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"b"}]}`)
	h.waitTimers(1) // the queue timer of the second request
	h.clock.Advance(120 * time.Second)
	resp := waitResp(t, ch)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" ||
		!strings.Contains(string(b), `"type":"overloaded_error"`) || !strings.Contains(string(b), "timeout: queue exceeded 120s") {
		t.Errorf("%d %s %v", resp.StatusCode, b, resp.Header)
	}
	// The first request is still running, so the only record is the queued one.
	if rec := h.record(); rec.Endpoint != "/v1/messages" || rec.TimeoutPhase != "queue" || rec.StatusCode != 503 {
		t.Errorf("record = %+v", rec)
	}
}

func TestKeyConcurrency(t *testing.T) {
	h := newHarness(t)
	up, received, _ := blockingUpstream()
	defer up.Close()
	h.alias("m", tgt{h.provider("ol", "ollama", up.URL), "x"})
	h.onlyTimeouts(map[string]any{"queue_timeout_ms": 1000})
	h.key = h.newKey(&store.VirtualKey{Name: "one-at-a-time", MaxConcurrency: 1})
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	h.postAsync(ctx1, "/api/chat", `{"model":"m","messages":[{"role":"user","content":"a"}]}`)
	<-received
	ch := h.postAsync(context.Background(), "/api/chat", `{"model":"m","messages":[{"role":"user","content":"b"}]}`)
	h.waitTimers(1)
	h.clock.Advance(time.Second)
	resp := waitResp(t, ch)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 || string(b) != `{"error":"timeout: queue exceeded 1s"}` {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	// Another key is not affected by this key's limit.
	h.key = h.newKey(&store.VirtualKey{Name: "other"})
	ch = h.postAsync(ctx1, "/api/chat", `{"model":"m","messages":[{"role":"user","content":"c"}]}`)
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("other key must not be queued")
	}
	_ = ch
}

func TestAliasParamsApplied(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var bodies []map[string]any
	paths := []string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		bodies = append(bodies, m)
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		writeLines(w, ollamaText("ok"), ollamaDone)
	}))
	defer up.Close()
	pid := h.provider("ol", "ollama", up.URL)
	h.exec(`UPDATE providers SET supports_anthropic = 1 WHERE id = ?`, pid)
	id := h.alias("m", tgt{pid, "x"})
	h.exec(`UPDATE aliases SET params_json = ? WHERE id = ?`,
		`{"defaults":{"temperature":0.3},"overrides":{"num_ctx":32768,"keep_alive":"30m","max_tokens":100},"system_prompt":{"mode":"prepend","text":"ALIAS"}}`, id)

	// Ollama passthrough: raw rules
	h.mustPost("/api/chat", `{"model":"m","stream":false,"messages":[{"role":"user","content":"x"}],"options":{"temperature":0.9,"num_predict":5000}}`)
	// OpenAI -> Ollama conversion
	h.mustPost("/v1/chat/completions", `{"model":"m","messages":[{"role":"system","content":"CLIENT"},{"role":"user","content":"x"}]}`)
	// Anthropic with num_ctx: passthrough impossible -> converted to /api/chat
	h.mustPost("/v1/messages", `{"model":"m","max_tokens":50,"messages":[{"role":"user","content":"x"}]}`)

	if len(bodies) != 3 {
		t.Fatalf("bodies = %d", len(bodies))
	}
	o0 := bodies[0]["options"].(map[string]any)
	if o0["temperature"] != 0.9 || o0["num_predict"] != 100.0 || o0["num_ctx"] != 32768.0 || bodies[0]["keep_alive"] != "30m" {
		t.Errorf("passthrough body = %v", bodies[0])
	}
	if m := bodies[0]["messages"].([]any)[0].(map[string]any); m["role"] != "system" || m["content"] != "ALIAS" {
		t.Errorf("passthrough system = %v", m)
	}
	o1 := bodies[1]["options"].(map[string]any)
	if o1["temperature"] != 0.3 || o1["num_predict"] != 100.0 || o1["num_ctx"] != 32768.0 {
		t.Errorf("converted body = %v", bodies[1])
	}
	if m := bodies[1]["messages"].([]any)[0].(map[string]any); m["content"] != "ALIAS\n\nCLIENT" {
		t.Errorf("converted system = %v", m)
	}
	if paths[2] != "/api/chat" || bodies[2]["options"].(map[string]any)["num_ctx"] != 32768.0 {
		t.Errorf("anthropic with ollama options must be converted: %s %v", paths[2], bodies[2])
	}
	if bodies[2]["options"].(map[string]any)["num_predict"] != 50.0 {
		t.Errorf("client max_tokens below the cap must be kept: %v", bodies[2]["options"])
	}
}

func TestHealthCheck(t *testing.T) {
	h := newHarness(t)
	up := fakeOllama(t, "x")
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	okID := h.provider("ok", "ollama", up.URL)
	deadID := h.provider("dead", "ollama", dead.URL)
	offID := h.provider("off", "ollama", dead.URL)
	h.exec(`UPDATE providers SET enabled = 0 WHERE id = ?`, offID)
	h.srv.Health().CheckAll(context.Background())
	ok, _ := h.srv.Store().GetProvider(context.Background(), okID)
	bad, _ := h.srv.Store().GetProvider(context.Background(), deadID)
	off, _ := h.srv.Store().GetProvider(context.Background(), offID)
	if ok.HealthStatus != "ok" || !strings.Contains(ok.ModelsJSON, "qwen3:8b") || ok.HealthCheckedAt == nil {
		t.Errorf("ok provider = %+v", ok)
	}
	if bad.HealthStatus != "error" || bad.HealthError == "" {
		t.Errorf("dead provider = %+v", bad)
	}
	if off.HealthStatus != "" {
		t.Errorf("disabled providers must not be checked: %+v", off)
	}
}

func TestBackupAPI(t *testing.T) {
	dbDir := t.TempDir()
	h, a := newAdminHarness(t, func(c *config.Config) { c.DBPath = filepath.Join(dbDir, "llmgw.db") })
	a.login(adminPassword)
	h.alias("m", tgt{h.provider("p", "ollama", "http://x"), "y"})
	req, _ := http.NewRequest("GET", a.base+"/admin/api/backup", nil)
	resp, err := a.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := filepath.Join(t.TempDir(), "b.db")
	f, _ := os.Create(out)
	io.Copy(f, resp.Body)
	f.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "llmgw-") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	d, err := sql.Open("sqlite", "file:"+filepath.ToSlash(out)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM aliases WHERE name = 'm'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("backup content: n=%d err=%v", n, err)
	}
	entries, _ := os.ReadDir(dbDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".llmgw-backup-") {
			t.Errorf("temporary backup file left behind: %s", e.Name())
		}
	}
}
