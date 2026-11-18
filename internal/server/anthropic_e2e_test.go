package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

const antBody = `{"model":"claude-sonnet-4-5","max_tokens":256,"messages":[{"role":"user","content":"hi"}]%s}`

func aBody(extra string) string { return strings.Replace(antBody, "%s", extra, 1) }

var evRe = regexp.MustCompile(`(?m)^event: (\S+)`)

func events(s string) string {
	var out []string
	for _, m := range evRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return strings.Join(out, ",")
}

func TestAnthropicViaOllamaConversion(t *testing.T) {
	h := newHarness(t)
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		writeLines(w,
			`{"message":{"role":"assistant","content":"","thinking":"plan"},"done":false}`,
			`{"message":{"role":"assistant","content":"Reading."},"done":false}`,
			`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"Read","arguments":{"file_path":"/a"}}}]},"done":false}`,
			`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":50,"eval_count":9}`)
	}))
	defer up.Close()
	h.alias("claude-sonnet-4-5", tgt{h.provider("ol", "ollama", up.URL), "qwen3-coder"})

	resp, b := h.mustPost("/v1/messages", aBody(`,"stream":true,"system":[{"type":"text","text":"sys"}],
		"thinking":{"type":"enabled","budget_tokens":1024},
		"tools":[{"name":"Read","input_schema":{"type":"object"}}]`))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if got["model"] != "qwen3-coder" || got["think"] != true {
		t.Errorf("upstream got %v", got)
	}
	want := "message_start,content_block_start,content_block_delta,content_block_stop," +
		"content_block_start,content_block_delta,content_block_stop," +
		"content_block_start,content_block_delta,content_block_stop,message_delta,message_stop"
	if e := events(b); e != want {
		t.Errorf("events:\n got %s\nwant %s\n%s", e, want, b)
	}
	for _, sub := range []string{`"thinking":"plan"`, `"text":"Reading."`, `"name":"Read"`,
		`"partial_json":"{\"file_path\":\"/a\"}"`, `"stop_reason":"tool_use"`,
		`"usage":{"input_tokens":50,"output_tokens":9}`, `"model":"claude-sonnet-4-5"`} {
		if !strings.Contains(b, sub) {
			t.Errorf("missing %s", sub)
		}
	}

	// non-streaming
	resp, b = h.mustPost("/v1/messages", aBody(""))
	var m struct {
		Type       string           `json:"type"`
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
	}
	json.Unmarshal([]byte(b), &m)
	if resp.StatusCode != 200 || m.Type != "message" || m.StopReason != "tool_use" || len(m.Content) != 2 {
		t.Errorf("non-stream (thinking not requested -> dropped): %d %s", resp.StatusCode, b)
	}
	if got["think"] != false {
		t.Errorf("no thinking param must send think:false to Ollama, got %v", got["think"])
	}
}

func TestAnthropicToolRoundTripViaOpenAI(t *testing.T) {
	h := newHarness(t)
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"Done."},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1}}`, `[DONE]`)
	}))
	defer up.Close()
	h.alias("claude-sonnet-4-5", tgt{h.provider("lms", "openai_compatible", up.URL), "real"})
	resp, b := h.mustPost("/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":10,"stop_sequences":["X"],
		"tool_choice":{"type":"tool","name":"Read"},
		"tools":[{"name":"Read","input_schema":{"type":"object"}}],
		"messages":[
		{"role":"user","content":"read a"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"p":"a"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"AAA"},{"type":"text","text":"summarize"}]}]}`)
	if resp.StatusCode != 200 || !strings.Contains(b, `"text":"Done."`) || !strings.Contains(b, `"stop_reason":"end_turn"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %v", msgs)
	}
	asst := msgs[1].(map[string]any)
	tool := msgs[2].(map[string]any)
	if asst["tool_calls"].([]any)[0].(map[string]any)["id"] != "toolu_1" || tool["role"] != "tool" ||
		tool["tool_call_id"] != "toolu_1" || tool["content"] != "AAA" || msgs[3].(map[string]any)["content"] != "summarize" {
		t.Errorf("converted messages = %v", msgs)
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Errorf("implicit thinking-off must not send reasoning_effort: %v", got["reasoning_effort"])
	}
	if got["max_tokens"] != 10.0 || got["stop"].([]any)[0] != "X" ||
		got["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "Read" {
		t.Errorf("params = %v", got)
	}
}

func TestAnthropicPassthrough(t *testing.T) {
	h := newHarness(t)
	var got map[string]any
	var hdr http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		hdr = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_up","type":"message","role":"assistant","model":"qwen3","content":[],"usage":{"input_tokens":7,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
	defer up.Close()
	pid := h.provider("ol", "ollama", up.URL)
	h.exec(`UPDATE providers SET supports_anthropic = 1 WHERE id = ?`, pid)
	h.alias("claude-sonnet-4-5", tgt{pid, "qwen3"})

	req, _ := http.NewRequest("POST", h.gw.URL+"/v1/messages", strings.NewReader(aBody(`,"stream":true,"context_management":{"x":1}`)))
	req.Header.Set("x-api-key", h.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "fine-grained-tool-streaming-2025-05-14")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if got["model"] != "qwen3" || got["context_management"] == nil || got["stream"] != true {
		t.Errorf("upstream body = %v", got)
	}
	if hdr.Get("anthropic-beta") != "fine-grained-tool-streaming-2025-05-14" || hdr.Get("x-api-key") == h.key {
		t.Errorf("upstream headers: beta=%q x-api-key leaked=%v", hdr.Get("anthropic-beta"), hdr.Get("x-api-key") == h.key)
	}
	out := string(b)
	if events(out) != "message_start,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop" ||
		!strings.Contains(out, `"model":"claude-sonnet-4-5"`) || strings.Contains(out, "qwen3") || !strings.Contains(out, `"id":"msg_up"`) {
		t.Errorf("passthrough output:\n%s", out)
	}
	if rec := h.record(); !rec.Passthrough || rec.UpstreamFormat != "anthropic" || *rec.PromptTokens != 7 {
		t.Errorf("record = %+v", rec)
	}
}

func TestAnthropicErrorsAndCountTokens(t *testing.T) {
	h := newHarness(t)
	var countCalls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages/count_tokens" {
			countCalls++
			io.WriteString(w, `{"input_tokens":1234}`)
			return
		}
		writeLines(w, ollamaText("x"), ollamaDone)
	}))
	defer up.Close()
	conv := h.provider("conv", "ollama", up.URL)
	pass := h.provider("pass", "ollama", up.URL)
	h.exec(`UPDATE providers SET supports_anthropic = 1 WHERE id = ?`, pass)
	h.alias("claude-haiku", tgt{conv, "m"})
	h.alias("claude-opus", tgt{pass, "m"})

	resp, b := h.mustPost("/v1/messages", `{"model":"nope","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 404 || b != `{"error":{"message":"model \"nope\" not found","type":"not_found_error"},"type":"error"}` {
		t.Errorf("not found: %d %s", resp.StatusCode, b)
	}
	resp, b = h.mustPost("/v1/messages", `{"model":"claude-haiku","messages":[]}`)
	if resp.StatusCode != 400 || !strings.Contains(b, `"type":"invalid_request_error"`) {
		t.Errorf("bad request: %d %s", resp.StatusCode, b)
	}
	h.key = ""
	resp, b = h.mustPost("/v1/messages", aBody(""))
	if resp.StatusCode != 401 || !strings.Contains(b, `"type":"authentication_error"`) {
		t.Errorf("unauthenticated: %d %s", resp.StatusCode, b)
	}
	h.key = h.newKey(storeKey("k"))

	// Estimate (no Anthropic-capable upstream) and upstream count.
	resp, b = h.mustPost("/v1/messages/count_tokens", `{"model":"claude-haiku","messages":[{"role":"user","content":"hello there"}]}`)
	if resp.StatusCode != 200 || !regexp.MustCompile(`^\{"input_tokens":\d+\}$`).MatchString(b) {
		t.Errorf("estimate: %d %s", resp.StatusCode, b)
	}
	resp, b = h.mustPost("/v1/messages/count_tokens", `{"model":"claude-opus","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != 200 || b != `{"input_tokens":1234}` || countCalls != 1 {
		t.Errorf("upstream count: %d %s calls=%d", resp.StatusCode, b, countCalls)
	}
}

func TestAnthropicPingAndStreamError(t *testing.T) {
	h := newHarness(t)
	up, received, _ := blockingUpstream()
	defer up.Close()
	h.alias("claude-sonnet-4-5", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.onlyTimeouts(map[string]any{"keepalive_interval_ms": 15_000, "ttft_timeout_ms": 20_000})
	ch := h.postAsync(context.Background(), "/v1/messages", aBody(`,"stream":true`))
	<-received
	h.waitTimers(2)
	h.clock.Advance(15 * time.Second)
	resp := waitResp(t, ch)
	defer resp.Body.Close()
	h.clock.Advance(5 * time.Second)
	b, _ := io.ReadAll(resp.Body)
	out := string(b)
	if !strings.HasPrefix(out, "event: ping\ndata: {\"type\": \"ping\"}") ||
		!strings.Contains(out, "event: error\ndata: {\"error\":{\"message\":\"timeout: ttft exceeded 20s\",\"type\":\"api_error\"},\"type\":\"error\"}") {
		t.Errorf("out = %q", out)
	}
}

func TestAnthropicQueueStyleErrors(t *testing.T) {
	// rpm limit -> 429 rate_limit_error with Retry-After
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w, ollamaText("x"), ollamaDone)
	}))
	defer up.Close()
	h.alias("claude-sonnet-4-5", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.key = h.newKey(&storeKeyRPM)
	h.mustPost("/v1/messages", aBody(""))
	resp, b := h.mustPost("/v1/messages", aBody(""))
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || !strings.Contains(b, `"type":"rate_limit_error"`) {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
}
