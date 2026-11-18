package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/chat"
	"llmgw/internal/upstream"
)

// The conversion matrix: every inbound format (OpenAI / Ollama / Anthropic) ×
// every upstream format (Ollama native / OpenAI-compatible / Anthropic
// passthrough-capable Ollama) × stream / non-stream, with a request carrying
// text + image + tools + thinking, and an upstream answer carrying thinking +
// text + a tool call. Responses are parsed with the client-side parsers of each
// wire format and checked semantically.

const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// matrixUpstream answers in all three formats and records what it received.
type matrixUpstream struct {
	mu   sync.Mutex
	path string
	body map[string]any
}

func (m *matrixUpstream) handler(w http.ResponseWriter, r *http.Request) {
	var b map[string]any
	json.NewDecoder(r.Body).Decode(&b)
	m.mu.Lock()
	m.path, m.body = r.URL.Path, b
	m.mu.Unlock()
	switch r.URL.Path {
	case "/api/chat":
		writeLines(w,
			`{"message":{"role":"assistant","content":"","thinking":"T1"},"done":false}`,
			`{"message":{"role":"assistant","content":"Hello"},"done":false}`,
			`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Tokyo"}}}]},"done":false}`,
			`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":11,"eval_count":7}`)
	case "/v1/chat/completions":
		writeSSE(w,
			`{"id":"c","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"T1"}}]}`,
			`{"id":"c","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
			`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
			`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Tokyo\"}"}}]}}]}`,
			`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"id":"c","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
			`[DONE]`)
	case "/v1/messages":
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(typ, data string) { io.WriteString(w, "event: "+typ+"\ndata: "+data+"\n\n") }
		ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[],"usage":{"input_tokens":11,"output_tokens":0}}}`)
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"T1"}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
		ev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Tokyo\"}"}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":2}`)
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}`)
		ev("message_stop", `{"type":"message_stop"}`)
	default:
		http.NotFound(w, r)
	}
}

type inbound struct {
	name    string
	path    string
	request func(stream bool) string
	parse   func(t *testing.T, stream bool, body string) *chat.Accumulator
}

var toolSchema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`

func accumulate(t *testing.T, parse func(io.Reader, func(*chat.Event) error) error, body string) *chat.Accumulator {
	t.Helper()
	acc := chat.NewAccumulator()
	if err := parse(strings.NewReader(body), func(ev *chat.Event) error { acc.Add(ev); return nil }); err != nil {
		t.Fatalf("parse client response: %v\n%s", err, body)
	}
	return acc
}

func accumulateJSON(t *testing.T, parse func([]byte, func(*chat.Event) error) error, body string) *chat.Accumulator {
	t.Helper()
	acc := chat.NewAccumulator()
	if err := parse([]byte(body), func(ev *chat.Event) error { acc.Add(ev); return nil }); err != nil {
		t.Fatalf("parse client response: %v\n%s", err, body)
	}
	return acc
}

var inbounds = []inbound{
	{
		name: "openai", path: "/v1/chat/completions",
		request: func(stream bool) string {
			return `{"model":"m","stream":` + boolStr(stream) + `,"reasoning_effort":"medium","messages":[{"role":"user","content":[
				{"type":"text","text":"weather?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + pngB64 + `"}}]}],
				"tools":[{"type":"function","function":{"name":"get_weather","parameters":` + toolSchema + `}}]}`
		},
		parse: func(t *testing.T, stream bool, body string) *chat.Accumulator {
			if stream {
				return accumulate(t, upstream.ParseOpenAIChatStream, body)
			}
			return accumulateJSON(t, upstream.ParseOpenAIChatCompletion, body)
		},
	},
	{
		name: "ollama", path: "/api/chat",
		request: func(stream bool) string {
			return `{"model":"m","stream":` + boolStr(stream) + `,"think":true,"messages":[{"role":"user","content":"weather?","images":["` + pngB64 + `"]}],
				"tools":[{"type":"function","function":{"name":"get_weather","parameters":` + toolSchema + `}}]}`
		},
		parse: func(t *testing.T, stream bool, body string) *chat.Accumulator {
			// A non-streaming Ollama response is one final NDJSON object.
			return accumulate(t, upstream.ParseOllamaChatStream, body+"\n")
		},
	},
	{
		name: "anthropic", path: "/v1/messages",
		request: func(stream bool) string {
			return `{"model":"m","max_tokens":200,"stream":` + boolStr(stream) + `,"thinking":{"type":"enabled","budget_tokens":1024},
				"messages":[{"role":"user","content":[{"type":"text","text":"weather?"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}]}],
				"tools":[{"name":"get_weather","input_schema":` + toolSchema + `}]}`
		},
		parse: func(t *testing.T, stream bool, body string) *chat.Accumulator {
			if stream {
				return accumulate(t, upstream.ParseAnthropicStream, body)
			}
			return accumulateJSON(t, upstream.ParseAnthropicMessage, body)
		},
	},
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// imageDelivered checks the upstream received the image in its own format.
func imageDelivered(path string, body map[string]any) bool {
	b, _ := json.Marshal(body)
	s := string(b)
	switch path {
	case "/api/chat":
		return strings.Contains(s, `"images":["`+pngB64+`"]`)
	case "/v1/chat/completions":
		return strings.Contains(s, `data:image/png;base64,`+pngB64)
	case "/v1/messages":
		return strings.Contains(s, `"data":"`+pngB64+`"`)
	}
	return false
}

func TestConversionMatrix(t *testing.T) {
	type upstreamKind struct {
		name, typ  string
		anthropic  bool
		wantPathOf func(inbound string) string
	}
	ups := []upstreamKind{
		{"ollama", "ollama", false, func(string) string { return "/api/chat" }},
		{"openai", "openai_compatible", false, func(string) string { return "/v1/chat/completions" }},
		{"anthropic-capable", "ollama", true, func(in string) string {
			if in == "anthropic" {
				return "/v1/messages"
			}
			return "/api/chat"
		}},
	}
	for _, u := range ups {
		for _, in := range inbounds {
			for _, stream := range []bool{true, false} {
				name := in.name + "->" + u.name + map[bool]string{true: "/stream", false: "/nonstream"}[stream]
				t.Run(name, func(t *testing.T) {
					h := newHarness(t)
					mu := &matrixUpstream{}
					srv := httptest.NewServer(http.HandlerFunc(mu.handler))
					defer srv.Close()
					pid := h.provider("p", u.typ, srv.URL)
					if u.anthropic {
						h.exec(`UPDATE providers SET supports_anthropic = 1 WHERE id = ?`, pid)
					}
					h.alias("m", tgt{pid, "up-model"})

					resp, body := h.mustPost(in.path, in.request(stream))
					if resp.StatusCode != 200 {
						t.Fatalf("status %d: %s", resp.StatusCode, body)
					}
					if want := u.wantPathOf(in.name); mu.path != want {
						t.Errorf("upstream path = %s, want %s", mu.path, want)
					}
					if mu.body["model"] != "up-model" || mu.body["stream"] != true {
						t.Errorf("upstream must get the real model and stream=true: %v / %v", mu.body["model"], mu.body["stream"])
					}
					if !imageDelivered(mu.path, mu.body) {
						t.Errorf("image not delivered in %s format: %v", mu.path, mu.body["messages"])
					}
					if !strings.Contains(mustJSON(mu.body), "get_weather") {
						t.Error("tools not delivered upstream")
					}

					acc := in.parse(t, stream, body)
					c := acc.Choices()[0]
					if c.Content() != "Hello" {
						t.Errorf("content = %q", c.Content())
					}
					if c.Reasoning() != "T1" {
						t.Errorf("thinking = %q", c.Reasoning())
					}
					tcs := c.ToolCalls()
					if len(tcs) != 1 || tcs[0].Name != "get_weather" || compactArgs(tcs[0].Arguments) != `{"city":"Tokyo"}` {
						t.Errorf("tool calls = %+v", tcs)
					}
					if tcs[0].ID == "" && in.name != "ollama" {
						t.Error("tool call id missing")
					}
					if c.FinishReason != chat.FinishToolCalls && !(in.name == "ollama") {
						t.Errorf("finish = %q", c.FinishReason)
					}
					if acc.Usage == nil || acc.Usage.CompletionTokens != 7 {
						if !(in.name == "openai" && stream) { // OpenAI streams omit usage unless include_usage
							t.Errorf("usage = %+v", acc.Usage)
						}
					}
				})
			}
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func compactArgs(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestOpenAIRoutesWithoutV1Prefix(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w, ollamaText("ok"), ollamaDone)
	}))
	defer up.Close()
	h.alias("m", tgt{h.provider("ol", "ollama", up.URL), "x"})
	if resp, b := h.mustPost("/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 200 || !strings.Contains(b, `"content":"ok"`) {
		t.Errorf("/chat/completions: %d %s", resp.StatusCode, b)
	}
	if resp, b := h.get("/models", h.key); resp.StatusCode != 200 || !strings.Contains(b, `"id":"m"`) {
		t.Errorf("/models: %d %s", resp.StatusCode, b)
	}
}
