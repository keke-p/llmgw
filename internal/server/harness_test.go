package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llmgw/internal/clock"
	"llmgw/internal/config"
	"llmgw/internal/db"
	"llmgw/internal/gateway"
	"llmgw/internal/store"
)

// harness runs the full gateway against mock upstreams with a fake clock, so
// timeouts can be triggered without waiting in real time.
type harness struct {
	t       *testing.T
	srv     *Server
	gw      *httptest.Server
	clock   *clock.Fake
	records chan *gateway.Record
	key     string // a valid virtual key used by post/get
}

func newHarness(t *testing.T, opts ...Option) *harness {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	h := &harness{t: t, clock: clock.NewFake(), records: make(chan *gateway.Record, 32)}
	cfg := &config.Config{SecretKey: config.DeriveKey("test"), MaxBodyBytes: 1 << 20}
	all := append([]Option{WithClock(h.clock), WithRecorder(func(r *gateway.Record) { h.records <- r })}, opts...)
	h.srv = New(cfg, d, slog.New(slog.NewTextHandler(io.Discard, nil)), all...)
	h.gw = httptest.NewServer(h.srv.Handler())
	t.Cleanup(h.gw.Close)
	h.key = h.newKey(&store.VirtualKey{Name: "test"})
	return h
}

func (h *harness) exec(q string, args ...any) int64 {
	h.t.Helper()
	res, err := h.srv.store.DB().Exec(q, args...)
	if err != nil {
		h.t.Fatalf("exec %q: %v", q, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (h *harness) provider(name, typ, url string) int64 {
	return h.exec(`INSERT INTO providers(name, type, base_url, created_at, updated_at) VALUES (?, ?, ?, 0, 0)`, name, typ, url)
}

type tgt struct {
	provider int64
	model    string
}

func (h *harness) alias(name string, targets ...tgt) int64 {
	id := h.exec(`INSERT INTO aliases(name, created_at, updated_at) VALUES (?, 0, 0)`, name)
	for i, t := range targets {
		h.exec(`INSERT INTO alias_targets(alias_id, provider_id, upstream_model, priority) VALUES (?, ?, ?, ?)`, id, t.provider, t.model, i)
	}
	return id
}

// settings sets global settings; durations in ms.
func (h *harness) settings(kv map[string]any) {
	h.t.Helper()
	for k, v := range kv {
		if err := h.srv.store.SetSetting(context.Background(), k, v); err != nil {
			h.t.Fatal(err)
		}
	}
}

// onlyTimeouts disables every timer except the given ones (so the number of
// armed fake timers is predictable).
func (h *harness) onlyTimeouts(kv map[string]any) {
	base := map[string]any{"connect_timeout_ms": 0, "ttft_timeout_ms": 0, "idle_timeout_ms": 0,
		"max_duration_ms": 0, "keepalive_interval_ms": 0}
	for k, v := range kv {
		base[k] = v
	}
	h.settings(base)
}

func (h *harness) post(ctx context.Context, path, body string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.gw.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if h.key != "" {
		req.Header.Set("Authorization", "Bearer "+h.key)
	}
	return http.DefaultClient.Do(req)
}

// newKey issues a virtual key directly through the service.
func (h *harness) newKey(k *store.VirtualKey) string {
	h.t.Helper()
	plain, err := h.srv.Keys().Create(context.Background(), k)
	if err != nil {
		h.t.Fatal(err)
	}
	return plain
}

// get sends an authenticated GET with the given key (empty = no auth).
func (h *harness) get(path, key string) (*http.Response, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.gw.URL+path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (h *harness) mustPost(path, body string) (*http.Response, string) {
	h.t.Helper()
	resp, err := h.post(context.Background(), path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// postAsync sends the request in the background (Do blocks until headers).
func (h *harness) postAsync(ctx context.Context, path, body string) <-chan *http.Response {
	ch := make(chan *http.Response, 1)
	go func() {
		resp, err := h.post(ctx, path, body)
		if err != nil {
			close(ch)
			return
		}
		ch <- resp
	}()
	return ch
}

func (h *harness) record() *gateway.Record {
	h.t.Helper()
	select {
	case r := <-h.records:
		return r
	case <-time.After(5 * time.Second):
		h.t.Fatal("no request record")
		return nil
	}
}

// waitTimers polls (in real time, briefly) until n fake timers are armed.
func (h *harness) waitTimers(n int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.clock.Active() == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %d armed timers (have %d)", n, h.clock.Active())
}

func waitResp(t *testing.T, ch <-chan *http.Response) *http.Response {
	t.Helper()
	select {
	case r, ok := <-ch:
		if !ok {
			t.Fatal("request failed")
		}
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no response")
		return nil
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: not closed", what)
	}
}

// ---- mock upstreams --------------------------------------------------------

// writeLines writes NDJSON lines (Ollama) and flushes after each.
func writeLines(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	for _, l := range lines {
		io.WriteString(w, l+"\n")
		w.(http.Flusher).Flush()
	}
}

// writeSSE writes SSE data lines (OpenAI) and flushes after each.
func writeSSE(w http.ResponseWriter, payloads ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, p := range payloads {
		io.WriteString(w, "data: "+p+"\n\n")
		w.(http.Flusher).Flush()
	}
}

func ollamaText(s string) string {
	b, _ := json.Marshal(map[string]any{"model": "m", "message": map[string]any{"role": "assistant", "content": s}, "done": false})
	return string(b)
}

const ollamaDone = `{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":2}`

// sseReader reads SSE events from a streaming response.
type sseReader struct{ br *bufio.Reader }

func newSSE(r io.Reader) *sseReader { return &sseReader{br: bufio.NewReader(r)} }

// next returns the next non-empty block ("data: ..." or ": keepalive").
func (s *sseReader) next(t *testing.T) string {
	t.Helper()
	var block []string
	for {
		line, err := s.br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(block) > 0 {
				return strings.Join(block, "\n")
			}
			if err != nil {
				return ""
			}
			continue
		}
		block = append(block, line)
		if err != nil {
			return strings.Join(block, "\n")
		}
	}
}

func storeKey(name string, aliases ...string) *store.VirtualKey {
	return &store.VirtualKey{Name: name, AllowedAliases: aliases}
}

var storeKeyRPM = store.VirtualKey{Name: "rpm", RPMLimit: 1}
