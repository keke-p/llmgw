package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgw/internal/chat"
)

const chatBody = `{"model":"local","messages":[{"role":"user","content":"hi"}]%s}`

func body(extra string) string { return strings.Replace(chatBody, "%s", extra, 1) }

func TestChatNonStreamViaOllama(t *testing.T) {
	h := newHarness(t)
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		writeLines(w, `{"message":{"role":"assistant","content":"","thinking":"t"},"done":false}`,
			ollamaText("Hel"), ollamaText("lo"), ollamaDone)
	}))
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "gemma:real"})

	resp, b := h.mustPost("/v1/chat/completions", body(`,"max_tokens":32,"temperature":0.1`))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	// upstream is always streamed, even for non-streaming clients
	if got["stream"] != true || got["model"] != "gemma:real" {
		t.Errorf("upstream request = %v", got)
	}
	opts, _ := got["options"].(map[string]any)
	if opts["num_predict"] != 32.0 || opts["temperature"] != 0.1 {
		t.Errorf("options = %v", opts)
	}
	var c struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens, CompletionTokens, TotalTokens int
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(b), &c); err != nil {
		t.Fatal(err)
	}
	if c.Object != "chat.completion" || c.Model != "local" || c.Choices[0].Message.Content != "Hello" ||
		c.Choices[0].Message.ReasoningContent != "t" || c.Choices[0].FinishReason != "stop" {
		t.Errorf("response = %s", b)
	}
	rec := h.record()
	if rec.StatusCode != 200 || rec.UpstreamFormat != chat.FormatOllama || rec.Passthrough || rec.TTFTMS == nil ||
		*rec.PromptTokens != 5 || len(rec.Attempts) != 1 || rec.Attempts[0].Result != "ok" {
		t.Errorf("record = %+v", rec)
	}
}

func TestChatStreamViaOllamaWithTools(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w,
			`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Tokyo"}}}]},"done":false}`,
			ollamaDone)
	}))
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "m"})

	resp, b := h.mustPost("/v1/chat/completions", body(`,"stream":true,"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]`))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d ct %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{`"role":"assistant"`, `"name":"get_weather"`, `"arguments":"{\"city\":\"Tokyo\"}"`,
		`"id":"call_`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %s in\n%s", want, b)
		}
	}
	if strings.Contains(b, `"usage"`) {
		t.Error("usage must not be sent unless stream_options.include_usage")
	}
}

func TestChatPassthroughOpenAI(t *testing.T) {
	h := newHarness(t)
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		writeSSE(w,
			`{"id":"u1","model":"real","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
			`{"id":"u1","model":"real","choices":[{"index":0,"delta":{"content":"Hi"}}],"x_vendor":true}`,
			`{"id":"u1","model":"real","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"u1","model":"real","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
			`[DONE]`)
	}))
	defer up.Close()
	h.alias("local", tgt{h.provider("lms", "openai_compatible", up.URL), "real"})

	resp, b := h.mustPost("/v1/chat/completions", body(`,"stream":true,"x_custom":{"a":1}`))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if got["model"] != "real" || got["x_custom"] == nil || got["stream"] != true {
		t.Errorf("upstream request = %v", got)
	}
	if !strings.Contains(b, `"x_vendor":true`) || strings.Contains(b, `"model":"real"`) || !strings.Contains(b, `"model":"local"`) {
		t.Errorf("passthrough output wrong:\n%s", b)
	}
	if strings.Contains(b, "prompt_tokens") {
		t.Error("gateway-requested usage chunk must be hidden from the client")
	}
	if !strings.HasSuffix(strings.TrimSpace(b), "data: [DONE]") {
		t.Errorf("missing [DONE]:\n%s", b)
	}
	rec := h.record()
	if !rec.Passthrough || rec.UpstreamFormat != chat.FormatOpenAI || *rec.CompletionTokens != 1 {
		t.Errorf("record = %+v", rec)
	}

	// Non-streaming request through the same passthrough upstream is assembled.
	resp, b = h.mustPost("/v1/chat/completions", body(""))
	if resp.StatusCode != 200 || !strings.Contains(b, `"content":"Hi"`) || !strings.Contains(b, `"total_tokens":4`) {
		t.Errorf("non-stream: %d %s", resp.StatusCode, b)
	}
}

func TestChatErrors(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"context length exceeded"}`)
	}))
	defer up.Close()
	h.alias("bad", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.exec(`INSERT INTO aliases(name, enabled, created_at, updated_at) VALUES ('off', 0, 0, 0)`)

	resp, b := h.mustPost("/v1/chat/completions", `{"model":"nope","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 404 || !strings.Contains(b, `"code":"model_not_found"`) || !strings.Contains(b, `"type":"invalid_request_error"`) {
		t.Errorf("unknown model: %d %s", resp.StatusCode, b)
	}
	resp, _ = h.mustPost("/v1/chat/completions", `{"model":"off","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 404 {
		t.Errorf("disabled alias: %d", resp.StatusCode)
	}
	resp, b = h.mustPost("/v1/chat/completions", `{"model":"bad","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 400 || !strings.Contains(b, "context length exceeded") {
		t.Errorf("upstream 4xx: %d %s", resp.StatusCode, b)
	}
	resp, b = h.mustPost("/v1/chat/completions", `{"model":"bad"}`)
	if resp.StatusCode != 400 || !strings.Contains(b, `"error":{"message":"messages is required"`) {
		t.Errorf("bad request: %d %s", resp.StatusCode, b)
	}
}

func TestFallbackOn5xxAndConnectError(t *testing.T) {
	h := newHarness(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, `{"error":"model failed to load"}`)
	}))
	defer failing.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w, ollamaText("ok"), ollamaDone)
	}))
	defer ok.Close()
	// A closed listener gives "connection refused".
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	deadURL := "http://" + ln.Addr().String()
	ln.Close()

	h.alias("local", tgt{h.provider("dead", "ollama", deadURL), "m"},
		tgt{h.provider("failing", "ollama", failing.URL), "m"},
		tgt{h.provider("ok", "ollama", ok.URL), "m"})

	resp, b := h.mustPost("/v1/chat/completions", body(""))
	if resp.StatusCode != 200 || !strings.Contains(b, `"content":"ok"`) {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	rec := h.record()
	if len(rec.Attempts) != 3 || rec.Attempts[0].Result != "connect_error" ||
		rec.Attempts[1].Result != "http_error" || rec.Attempts[1].Status != 500 || rec.Attempts[2].Result != "ok" {
		t.Errorf("attempts = %+v", rec.Attempts)
	}
}

func TestAllTargetsFail(t *testing.T) {
	h := newHarness(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer failing.Close()
	h.alias("local", tgt{h.provider("f", "ollama", failing.URL), "m"})
	resp, b := h.mustPost("/v1/chat/completions", body(""))
	if resp.StatusCode != 502 || !strings.Contains(b, "upstream error (503)") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
}

// blockingUpstream sends the given lines, then blocks until the client (the
// gateway) closes the connection. closed is closed when that happens.
func blockingUpstream(lines ...string) (*httptest.Server, <-chan struct{}, <-chan struct{}) {
	received := make(chan struct{}, 8)
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http only detects a closed client connection once the body is consumed.
		io.Copy(io.Discard, r.Body)
		received <- struct{}{}
		if len(lines) > 0 {
			writeLines(w, lines...)
		}
		select {
		case <-r.Context().Done():
			close(closed)
		case <-time.After(10 * time.Second): // safety net so a failing test cannot hang Close
		}
	}))
	return srv, received, closed
}

func TestTTFTTimeout(t *testing.T) {
	h := newHarness(t)
	up, _, closed := blockingUpstream()
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.onlyTimeouts(map[string]any{"ttft_timeout_ms": 300_000})

	ch := h.postAsync(context.Background(), "/v1/chat/completions", body(""))
	h.waitTimers(1) // TTFT armed once the request was written upstream
	h.clock.Advance(299 * time.Second)
	select {
	case <-ch:
		t.Fatal("responded before the TTFT timeout")
	case <-time.After(20 * time.Millisecond):
	}
	h.clock.Advance(time.Second)
	resp := waitResp(t, ch)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 504 || !strings.Contains(string(b), "timeout: ttft exceeded 300s") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	waitClosed(t, closed, "upstream connection")
	rec := h.record()
	if rec.TimeoutPhase != "ttft" || rec.StatusCode != 504 {
		t.Errorf("record = %+v", rec)
	}
}

func TestTTFTTimeoutStreamBeforeHeaders(t *testing.T) {
	// With streaming but no keepalive, a TTFT timeout is still a plain HTTP error.
	h := newHarness(t)
	up, _, _ := blockingUpstream()
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.onlyTimeouts(map[string]any{"ttft_timeout_ms": 1000})
	ch := h.postAsync(context.Background(), "/v1/chat/completions", body(`,"stream":true`))
	h.waitTimers(1)
	h.clock.Advance(time.Second)
	resp := waitResp(t, ch)
	if resp.StatusCode != 504 || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestTTFTFallback(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "default_no_fallback", true: "opt_in"}[optIn], func(t *testing.T) {
			h := newHarness(t)
			slow, _, closed := blockingUpstream()
			defer slow.Close()
			fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeLines(w, ollamaText("fast"), ollamaDone)
			}))
			defer fast.Close()
			id := h.alias("local", tgt{h.provider("slow", "ollama", slow.URL), "m"}, tgt{h.provider("fast", "ollama", fast.URL), "m"})
			if optIn {
				h.exec(`UPDATE aliases SET fallback_on_ttft_timeout = 1 WHERE id = ?`, id)
			}
			h.onlyTimeouts(map[string]any{"ttft_timeout_ms": 5000})
			ch := h.postAsync(context.Background(), "/v1/chat/completions", body(""))
			h.waitTimers(1)
			h.clock.Advance(5 * time.Second)
			resp := waitResp(t, ch)
			b, _ := io.ReadAll(resp.Body)
			waitClosed(t, closed, "slow upstream")
			if optIn && (resp.StatusCode != 200 || !strings.Contains(string(b), "fast")) {
				t.Errorf("expected fallback: %d %s", resp.StatusCode, b)
			}
			if !optIn && resp.StatusCode != 504 {
				t.Errorf("expected 504 without fallback: %d %s", resp.StatusCode, b)
			}
		})
	}
}

func TestIdleTimeoutMidStream(t *testing.T) {
	h := newHarness(t)
	up, _, closed := blockingUpstream(ollamaText("partial"))
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.onlyTimeouts(map[string]any{"idle_timeout_ms": 120_000})

	resp, err := h.post(context.Background(), "/v1/chat/completions", body(`,"stream":true`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sse := newSSE(resp.Body)
	if first := sse.next(t); !strings.Contains(first, "partial") {
		t.Fatalf("first chunk = %q", first)
	}
	h.waitTimers(1) // idle armed
	h.clock.Advance(120 * time.Second)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), `data: {"error":{"message":"timeout: idle exceeded 120s"`) {
		t.Errorf("rest = %q", rest)
	}
	if strings.Contains(string(rest), "[DONE]") {
		t.Error("[DONE] must not be sent after an error")
	}
	waitClosed(t, closed, "upstream connection")
	rec := h.record()
	if rec.TimeoutPhase != "idle" || !rec.Interrupted || rec.StatusCode != 200 {
		t.Errorf("record = %+v", rec)
	}
	if rec.Response.Choices()[0].Content() != "partial" {
		t.Error("partial output must be kept for the log")
	}
}

func TestIdleTimeoutNonStreamReturns504(t *testing.T) {
	h := newHarness(t)
	up, _, _ := blockingUpstream(ollamaText("partial"))
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.onlyTimeouts(map[string]any{"idle_timeout_ms": 1000})
	ch := h.postAsync(context.Background(), "/v1/chat/completions", body(""))
	h.waitTimers(1)
	h.clock.Advance(time.Second)
	resp := waitResp(t, ch)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 504 || strings.Contains(string(b), "partial") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	if rec := h.record(); !rec.Interrupted || rec.Response.Choices()[0].Content() != "partial" {
		t.Errorf("partial output must be logged: %+v", rec)
	}
}

func TestMaxDuration(t *testing.T) {
	h := newHarness(t)
	up, _, closed := blockingUpstream(ollamaText("a"))
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.onlyTimeouts(map[string]any{"max_duration_ms": 60_000})
	resp, err := h.post(context.Background(), "/v1/chat/completions", body(`,"stream":true`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	newSSE(resp.Body).next(t)
	h.clock.Advance(60 * time.Second)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "timeout: max_duration exceeded 60s") {
		t.Errorf("rest = %q", rest)
	}
	waitClosed(t, closed, "upstream connection")
	if rec := h.record(); rec.TimeoutPhase != "max_duration" {
		t.Errorf("phase = %q", rec.TimeoutPhase)
	}
}

func TestKeepaliveCommitsHeaders(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	received := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		received <- struct{}{}
		<-release
		writeLines(w, ollamaText("late"), ollamaDone)
	}))
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.onlyTimeouts(map[string]any{"keepalive_interval_ms": 15_000})

	ch := h.postAsync(context.Background(), "/v1/chat/completions", body(`,"stream":true`))
	<-received
	select {
	case <-ch:
		t.Fatal("headers must not be sent before the first token or keepalive")
	case <-time.After(30 * time.Millisecond):
	}
	h.clock.Advance(15 * time.Second)
	resp := waitResp(t, ch)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d", resp.StatusCode)
	}
	sse := newSSE(resp.Body)
	if ka := sse.next(t); ka != ": keepalive" {
		t.Fatalf("expected keepalive, got %q", ka)
	}
	h.waitTimers(1) // keepalive re-armed
	h.clock.Advance(15 * time.Second)
	if ka := sse.next(t); ka != ": keepalive" {
		t.Fatalf("expected second keepalive, got %q", ka)
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "late") || !strings.Contains(string(rest), "[DONE]") {
		t.Errorf("rest = %q", rest)
	}
}

func TestKeepaliveThenTTFTTimeoutIsStreamError(t *testing.T) {
	h := newHarness(t)
	up, received, _ := blockingUpstream()
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.onlyTimeouts(map[string]any{"keepalive_interval_ms": 15_000, "ttft_timeout_ms": 20_000})
	ch := h.postAsync(context.Background(), "/v1/chat/completions", body(`,"stream":true`))
	<-received
	h.waitTimers(2) // keepalive + ttft
	h.clock.Advance(15 * time.Second)
	resp := waitResp(t, ch)
	defer resp.Body.Close()
	h.clock.Advance(5 * time.Second)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), ": keepalive") ||
		!strings.Contains(string(b), "timeout: ttft exceeded 20s") {
		t.Errorf("%d %q", resp.StatusCode, b)
	}
}

func TestConnectTimeoutFallsBack(t *testing.T) {
	// The first provider's dial hangs until cancelled; the second works.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w, ollamaText("second"), ollamaDone)
	}))
	defer ok.Close()
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "hang.invalid") {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}}
	h := newHarness(t, WithHTTPClient(client))
	h.alias("local", tgt{h.provider("hang", "ollama", "http://hang.invalid:11434"), "m"},
		tgt{h.provider("ok", "ollama", ok.URL), "m"})
	h.onlyTimeouts(map[string]any{"connect_timeout_ms": 10_000})
	ch := h.postAsync(context.Background(), "/v1/chat/completions", body(""))
	h.waitTimers(1)
	h.clock.Advance(10 * time.Second)
	resp := waitResp(t, ch)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "second") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	rec := h.record()
	if rec.Attempts[0].Result != "timeout" || !strings.Contains(rec.Attempts[0].Error, "connect exceeded 10s") {
		t.Errorf("attempts = %+v", rec.Attempts)
	}
}

func TestClientCancelClosesUpstream(t *testing.T) {
	h := newHarness(t)
	up, _, closed := blockingUpstream(ollamaText("a"))
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "m"})
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := h.post(ctx, "/v1/chat/completions", body(`,"stream":true`))
	if err != nil {
		t.Fatal(err)
	}
	newSSE(resp.Body).next(t)
	cancel()
	resp.Body.Close()
	waitClosed(t, closed, "upstream connection")
	rec := h.record()
	if !rec.CancelledByClient || rec.Response.Choices()[0].Content() != "a" {
		t.Errorf("record = %+v", rec)
	}
}

func TestEmbeddings(t *testing.T) {
	h := newHarness(t)
	var got map[string]any
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"embeddings":[[0.5,-1]],"prompt_eval_count":2}`)
	}))
	defer ol.Close()
	oa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"list","model":"real","data":[{"object":"embedding","index":0,"embedding":[1]}],"usage":{"prompt_tokens":1,"total_tokens":1},"x":1}`)
	}))
	defer oa.Close()
	h.alias("emb", tgt{h.provider("ol", "ollama", ol.URL), "nomic"})
	h.alias("emb2", tgt{h.provider("oa", "openai_compatible", oa.URL), "real"})

	resp, b := h.mustPost("/v1/embeddings", `{"model":"emb","input":"hello"}`)
	if resp.StatusCode != 200 || !strings.Contains(b, `"embedding":[0.5,-1]`) || !strings.Contains(b, `"model":"emb"`) {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	if got["model"] != "nomic" {
		t.Errorf("upstream got %v", got)
	}

	resp, b = h.mustPost("/v1/embeddings", `{"model":"emb","input":["hello"],"encoding_format":"base64"}`)
	var r struct {
		Data []struct {
			Embedding string `json:"embedding"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(b), &r)
	raw, _ := base64.StdEncoding.DecodeString(r.Data[0].Embedding)
	if resp.StatusCode != 200 || len(raw) != 8 {
		t.Errorf("base64: %d %s", resp.StatusCode, b)
	}

	resp, b = h.mustPost("/v1/embeddings", `{"model":"emb2","input":"x"}`)
	if resp.StatusCode != 200 || !strings.Contains(b, `"model":"emb2"`) || !strings.Contains(b, `"x":1`) {
		t.Errorf("passthrough: %d %s", resp.StatusCode, b)
	}

	resp, _ = h.mustPost("/v1/embeddings", `{"model":"emb","input":[[1,2]]}`)
	if resp.StatusCode != 400 {
		t.Errorf("token arrays to ollama must be 400, got %d", resp.StatusCode)
	}
}

func TestModels(t *testing.T) {
	h := newHarness(t)
	p := h.provider("ol", "ollama", "http://x")
	h.alias("b-model", tgt{p, "m"})
	h.alias("a-model:7b", tgt{p, "m"})
	h.exec(`INSERT INTO aliases(name, enabled, created_at, updated_at) VALUES ('hidden', 0, 0, 0)`)
	_, mb := h.get("/v1/models", h.key)
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(mb), &out)
	if out.Object != "list" || len(out.Data) != 2 || out.Data[0].ID != "a-model:7b" {
		t.Errorf("models = %+v", out)
	}
	r2, _ := h.get("/v1/models/a-model:7b", h.key)
	if r2.StatusCode != 200 {
		t.Errorf("get model: %d", r2.StatusCode)
	}
}

func TestLatestTagResolution(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w, ollamaText("ok"), ollamaDone)
	}))
	defer up.Close()
	h.alias("qwen", tgt{h.provider("ol", "ollama", up.URL), "m"})
	resp, _ := h.mustPost("/v1/chat/completions", `{"model":"qwen:latest","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 200 {
		t.Errorf("qwen:latest should resolve to alias qwen: %d", resp.StatusCode)
	}
}
