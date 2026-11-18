package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const olChat = `{"model":"local","messages":[{"role":"user","content":"hi"}]%s}`

func olBody(extra string) string { return strings.Replace(olChat, "%s", extra, 1) }

func TestOllamaChatPassthrough(t *testing.T) {
	h := newHarness(t)
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		writeLines(w,
			`{"model":"qwen3:8b","created_at":"t","message":{"role":"assistant","content":"Hel"},"done":false}`,
			`{"model":"qwen3:8b","created_at":"t","message":{"role":"assistant","content":"lo"},"done":false}`,
			`{"model":"qwen3:8b","created_at":"t","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","total_duration":123,"eval_count":2,"x_extra":1}`)
	}))
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "qwen3:8b"})

	// stream defaults to true -> NDJSON
	resp, b := h.mustPost("/api/chat", olBody(`,"options":{"num_ctx":16384},"keep_alive":-1,"custom":true`))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if got["model"] != "qwen3:8b" || got["stream"] != true || got["custom"] != true || got["keep_alive"] != -1.0 {
		t.Errorf("upstream got %v", got)
	}
	if opts := got["options"].(map[string]any); opts["num_ctx"] != 16384.0 {
		t.Errorf("options not passed through: %v", opts)
	}
	lines := strings.Split(strings.TrimSpace(b), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"model":"local:latest"`) || strings.Contains(b, "qwen3:8b") ||
		!strings.Contains(lines[2], `"x_extra":1`) || !strings.Contains(lines[2], `"total_duration":123`) {
		t.Errorf("ndjson:\n%s", b)
	}
	if rec := h.record(); !rec.Passthrough || rec.InboundFormat != "ollama" || *rec.CompletionTokens != 2 {
		t.Errorf("record = %+v", rec)
	}

	// stream:false -> single JSON assembled from the (still streamed) upstream
	resp, b = h.mustPost("/api/chat", olBody(`,"stream":false`))
	var one map[string]any
	if err := json.Unmarshal([]byte(b), &one); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if got["stream"] != true || one["done"] != true || one["message"].(map[string]any)["content"] != "Hello" {
		t.Errorf("non-stream: %s", b)
	}
}

func TestOllamaChatViaOpenAIUpstream(t *testing.T) {
	h := newHarness(t)
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		writeSSE(w,
			`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"Hi"}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c9","function":{"name":"get","arguments":"{\"q\""}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5}}`,
			`[DONE]`)
	}))
	defer up.Close()
	h.alias("local", tgt{h.provider("lms", "openai_compatible", up.URL), "real"})

	resp, b := h.mustPost("/api/chat", `{"model":"local:latest","messages":[
		{"role":"user","content":"x","images":["iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"]},
		{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get","arguments":{"q":0}}}]},
		{"role":"tool","tool_name":"get","content":"r"}],
		"options":{"temperature":0.5,"num_predict":10},"tools":[{"type":"function","function":{"name":"get","parameters":{}}}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	// conversion: images -> data URL, tool ids linked, options -> sampling params
	msgs := got["messages"].([]any)
	img := msgs[0].(map[string]any)["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(img, "data:image/png;base64,") {
		t.Errorf("image url = %s", img)
	}
	callID := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["id"]
	if msgs[2].(map[string]any)["tool_call_id"] != callID || got["temperature"] != 0.5 || got["max_tokens"] != 10.0 {
		t.Errorf("upstream got %v", got)
	}
	lines := strings.Split(strings.TrimSpace(b), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(b, `"thinking":"think"`) || !strings.Contains(b, `"content":"Hi"`) ||
		!strings.Contains(b, `"arguments":{"q":1}`) || !strings.Contains(last, `"done":true`) ||
		!strings.Contains(last, `"eval_count":5`) || !strings.Contains(last, `"prompt_eval_count":7`) {
		t.Errorf("ndjson:\n%s", b)
	}
}

func TestOllamaGenerate(t *testing.T) {
	h := newHarness(t)
	var gotPath string
	var got map[string]any
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		writeLines(w, `{"model":"m","response":"Hi","done":false}`,
			`{"model":"m","response":"","done":true,"done_reason":"stop","context":[1,2,3],"eval_count":1}`)
	}))
	defer ol.Close()
	oa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"Yo"},"finish_reason":"stop"}]}`, `[DONE]`)
	}))
	defer oa.Close()
	h.alias("ol", tgt{h.provider("ol", "ollama", ol.URL), "m"})
	h.alias("oa", tgt{h.provider("oa", "openai_compatible", oa.URL), "m"})

	resp, b := h.mustPost("/api/generate", `{"model":"ol","prompt":"hi","raw":true,"context":[9]}`)
	if resp.StatusCode != 200 || gotPath != "/api/generate" || got["raw"] != true || !strings.Contains(b, `"context":[1,2,3]`) ||
		!strings.Contains(b, `"model":"ol:latest"`) {
		t.Errorf("passthrough: %d %s %v", resp.StatusCode, b, got)
	}
	resp, b = h.mustPost("/api/generate", `{"model":"oa","prompt":"hi","system":"sys","stream":false}`)
	if resp.StatusCode != 200 || !strings.Contains(b, `"response":"Yo"`) || !strings.Contains(b, `"done":true`) {
		t.Errorf("converted: %d %s", resp.StatusCode, b)
	}
	if msgs := got["messages"].([]any); len(msgs) != 2 {
		t.Errorf("generate -> chat messages: %v", msgs)
	}
	resp, b = h.mustPost("/api/generate", `{"model":"oa","prompt":"hi","template":"{{.Prompt}}"}`)
	if resp.StatusCode != 400 || !strings.Contains(b, `{"error":"field(s) template`) {
		t.Errorf("unconvertible: %d %s", resp.StatusCode, b)
	}
}

func TestOpenAICompletions(t *testing.T) {
	h := newHarness(t)
	var got map[string]any
	var gotPath string
	oa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		writeSSE(w, `{"id":"cmpl-1","object":"text_completion","model":"real","choices":[{"text":"def ","index":0,"finish_reason":null}]}`,
			`{"id":"cmpl-1","object":"text_completion","model":"real","choices":[{"text":"f()","index":0,"finish_reason":"stop"}]}`,
			`[DONE]`)
	}))
	defer oa.Close()
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		writeLines(w, `{"response":"x = 1","done":false}`, `{"response":"","done":true,"done_reason":"stop","prompt_eval_count":2,"eval_count":3}`)
	}))
	defer ol.Close()
	h.alias("oa", tgt{h.provider("oa", "openai_compatible", oa.URL), "real"})
	h.alias("ol", tgt{h.provider("ol", "ollama", ol.URL), "codellama"})

	resp, b := h.mustPost("/v1/completions", `{"model":"oa","prompt":"# py","stream":true}`)
	if resp.StatusCode != 200 || gotPath != "/v1/completions" || !strings.Contains(b, `"text":"def "`) ||
		!strings.Contains(b, `"model":"oa"`) || !strings.Contains(b, "[DONE]") {
		t.Errorf("passthrough: %d %s", resp.StatusCode, b)
	}
	resp, b = h.mustPost("/v1/completions", `{"model":"ol","prompt":"x","suffix":"\nprint(x)","max_tokens":5}`)
	if resp.StatusCode != 200 || gotPath != "/api/generate" || got["suffix"] != "\nprint(x)" || got["prompt"] != "x" {
		t.Fatalf("ollama conversion: %d %s %v", resp.StatusCode, b, got)
	}
	var c struct {
		Object  string `json:"object"`
		Choices []struct {
			Text         string `json:"text"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	json.Unmarshal([]byte(b), &c)
	if c.Object != "text_completion" || c.Choices[0].Text != "x = 1" || c.Usage.TotalTokens != 5 {
		t.Errorf("response: %s", b)
	}
	if opts := got["options"].(map[string]any); opts["num_predict"] != 5.0 {
		t.Errorf("options = %v", opts)
	}
}

func TestOllamaEmbeddings(t *testing.T) {
	h := newHarness(t)
	var paths []string
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/api/embeddings" {
			io.WriteString(w, `{"embedding":[0.1,0.2]}`)
			return
		}
		io.WriteString(w, `{"model":"nomic","embeddings":[[1,2]],"prompt_eval_count":1}`)
	}))
	defer ol.Close()
	oa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"index":0,"embedding":[3,4]}],"usage":{"prompt_tokens":2}}`)
	}))
	defer oa.Close()
	h.alias("emb", tgt{h.provider("ol", "ollama", ol.URL), "nomic"})
	h.alias("emb-oa", tgt{h.provider("oa", "openai_compatible", oa.URL), "x"})

	if resp, b := h.mustPost("/api/embed", `{"model":"emb","input":["a"]}`); resp.StatusCode != 200 || !strings.Contains(b, `"model":"emb:latest"`) {
		t.Errorf("embed passthrough: %d %s", resp.StatusCode, b)
	}
	if resp, b := h.mustPost("/api/embeddings", `{"model":"emb","prompt":"a"}`); resp.StatusCode != 200 || b != `{"embedding":[0.1,0.2]}` {
		t.Errorf("legacy passthrough: %d %s", resp.StatusCode, b)
	}
	if strings.Join(paths, ",") != "/api/embed,/api/embeddings" {
		t.Errorf("paths = %v", paths)
	}
	if resp, b := h.mustPost("/api/embed", `{"model":"emb-oa","input":"a"}`); resp.StatusCode != 200 || !strings.Contains(b, `"embeddings":[[3,4]]`) {
		t.Errorf("embed converted: %d %s", resp.StatusCode, b)
	}
	if resp, b := h.mustPost("/api/embeddings", `{"model":"emb-oa","prompt":"a"}`); resp.StatusCode != 200 || b != `{"embedding":[3,4]}` {
		t.Errorf("legacy converted: %d %s", resp.StatusCode, b)
	}
}

func TestOllamaModelInfo(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			io.WriteString(w, `{"models":[{"name":"qwen3:8b","model":"qwen3:8b","modified_at":"2026-01-01T00:00:00Z","size":5000,"digest":"abc","details":{"family":"qwen3","parameter_size":"8B"}}]}`)
		case "/api/show":
			var q map[string]any
			json.NewDecoder(r.Body).Decode(&q)
			if q["model"] != "qwen3:8b" {
				t.Errorf("show model = %v", q["model"])
			}
			io.WriteString(w, `{"template":"T","capabilities":["completion","tools","thinking"]}`)
		case "/api/ps":
			io.WriteString(w, `{"models":[{"name":"qwen3:8b","model":"qwen3:8b","size_vram":42,"expires_at":"x"}]}`)
		}
	}))
	defer up.Close()
	ol := h.provider("ol", "ollama", up.URL)
	oa := h.provider("oa", "openai_compatible", "http://127.0.0.1:1")
	h.alias("chat", tgt{ol, "qwen3:8b"})
	h.alias("remote:7b", tgt{oa, "x"})
	h.alias("secret", tgt{ol, "qwen3:8b"})
	restricted := h.newKey(storeKey("r", "chat", "remote:7b"))

	resp, b := h.get("/api/tags", restricted)
	var tags struct {
		Models []struct {
			Name    string         `json:"name"`
			Size    int64          `json:"size"`
			Digest  string         `json:"digest"`
			Details map[string]any `json:"details"`
		} `json:"models"`
	}
	json.Unmarshal([]byte(b), &tags)
	if resp.StatusCode != 200 || len(tags.Models) != 2 {
		t.Fatalf("tags: %d %s", resp.StatusCode, b)
	}
	if m := tags.Models[0]; m.Name != "chat:latest" || m.Size != 5000 || m.Digest != "abc" || m.Details["family"] != "qwen3" {
		t.Errorf("ollama-backed entry = %+v", m)
	}
	if m := tags.Models[1]; m.Name != "remote:7b" || len(m.Digest) != 64 || m.Details["format"] != "gguf" {
		t.Errorf("dummy entry = %+v", m)
	}

	h.key = restricted
	if resp, b := h.mustPost("/api/show", `{"model":"chat"}`); resp.StatusCode != 200 || !strings.Contains(b, `"thinking"`) {
		t.Errorf("show (ollama): %d %s", resp.StatusCode, b)
	}
	if resp, b := h.mustPost("/api/show", `{"name":"remote:7b"}`); resp.StatusCode != 200 || !strings.Contains(b, `"capabilities":["completion","tools"]`) {
		t.Errorf("show (minimal): %d %s", resp.StatusCode, b)
	}
	if resp, b := h.mustPost("/api/show", `{"model":"secret"}`); resp.StatusCode != 403 || !strings.HasPrefix(b, `{"error":`) {
		t.Errorf("show (not allowed): %d %s", resp.StatusCode, b)
	}
	if resp, b := h.get("/api/ps", restricted); resp.StatusCode != 200 || !strings.Contains(b, `"name":"chat:latest"`) ||
		!strings.Contains(b, `"size_vram":42`) || strings.Contains(b, "secret") {
		t.Errorf("ps: %d %s", resp.StatusCode, b)
	}
}

func TestOllamaMisc(t *testing.T) {
	h := newHarness(t)
	// /api/version and / need no key
	if resp, b := h.get("/api/version", ""); resp.StatusCode != 200 || !strings.Contains(b, `"version"`) {
		t.Errorf("version: %d %s", resp.StatusCode, b)
	}
	if resp, b := h.get("/", ""); resp.StatusCode != 200 || b != "Ollama is running" {
		t.Errorf("root: %d %s", resp.StatusCode, b)
	}
	// model management is blocked, even with a valid key
	for _, p := range []string{"/api/pull", "/api/push", "/api/create", "/api/copy", "/api/blobs/sha256:abc"} {
		if resp, b := h.mustPost(p, `{"model":"x"}`); resp.StatusCode != 403 || !strings.Contains(b, `"error"`) {
			t.Errorf("%s: %d %s", p, resp.StatusCode, b)
		}
	}
	req, _ := http.NewRequest(http.MethodDelete, h.gw.URL+"/api/delete", strings.NewReader(`{"model":"x"}`))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 403 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	// errors use the Ollama format
	if resp, b := h.mustPost("/api/chat", `{"model":"nope","messages":[]}`); resp.StatusCode != 404 || b != `{"error":"model \"nope\" not found"}` {
		t.Errorf("not found: %d %s", resp.StatusCode, b)
	}
	if resp, b := h.get("/api/tags", ""); resp.StatusCode != 401 || !strings.HasPrefix(b, `{"error":"missing API key`) {
		t.Errorf("unauthenticated: %d %s", resp.StatusCode, b)
	}
}

func TestOllamaStreamErrorLine(t *testing.T) {
	h := newHarness(t)
	up, _, _ := blockingUpstream(`{"message":{"role":"assistant","content":"part"},"done":false}`)
	defer up.Close()
	h.alias("local", tgt{h.provider("ol", "ollama", up.URL), "m"})
	h.onlyTimeouts(map[string]any{"idle_timeout_ms": 1000})
	resp, err := h.post(t.Context(), "/api/chat", olBody(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf) // first line
	if !strings.Contains(string(buf[:n]), "part") {
		t.Fatalf("first = %q", buf[:n])
	}
	h.waitTimers(1)
	h.clock.Advance(1e9)
	rest, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(rest)) != `{"error":"timeout: idle exceeded 1s"}` {
		t.Errorf("rest = %q", rest)
	}
}
