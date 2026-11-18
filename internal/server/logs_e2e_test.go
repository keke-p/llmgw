package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgw/internal/store"
)

// logHarness is an admin harness with the request log writer running.
func logHarness(t *testing.T) (*harness, *adminClient) {
	t.Helper()
	h, a := newAdminHarness(t, nil)
	stop := h.srv.StartBackground(context.Background())
	t.Cleanup(stop)
	a.login(adminPassword)
	return h, a
}

// waitLogs polls the admin API until n log rows exist.
func waitLogs(t *testing.T, a *adminClient, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, out := a.do("GET", "/admin/api/logs?limit=100", "")
		if data, ok := out["data"].([]any); ok && len(data) >= n {
			res := make([]map[string]any, len(data))
			for i, d := range data {
				res[i] = d.(map[string]any)
			}
			return res
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected %d log rows", n)
	return nil
}

func logDetail(t *testing.T, a *adminClient, id any) map[string]any {
	t.Helper()
	resp, out := a.do("GET", "/admin/api/logs/"+itoa(int64(id.(float64))), "")
	if resp.StatusCode != 200 {
		t.Fatalf("detail: %d", resp.StatusCode)
	}
	return out
}

func TestRequestLogsFull(t *testing.T) {
	h, a := logHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w,
			`{"message":{"role":"assistant","content":"","thinking":"hmm"},"done":false}`,
			ollamaText("Hello"),
			`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"f","arguments":{"x":1}}}]},"done":false}`,
			ollamaDone)
	}))
	defer up.Close()
	h.alias("m", tgt{h.provider("ol", "ollama", up.URL), "real"})
	h.key = h.newKey(&store.VirtualKey{Name: "app", LogMode: store.LogFull})

	img := strings.Repeat("iVBORw0K", 600)
	resp, _ := h.mustPost("/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":[
		{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,`+img+`"}}]}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows := waitLogs(t, a, 1)
	r := rows[0]
	if r["key_name"] != "app" || r["alias"] != "m" || r["stream"] != true || r["status_code"] != 200.0 ||
		r["upstream_format"] != "ollama" || r["prompt_tokens"] != 5.0 || r["ttft_ms"] == nil {
		t.Errorf("row = %v", r)
	}
	d := logDetail(t, a, r["id"])
	req, _ := json.Marshal(d["request_body"])
	if !strings.Contains(string(req), `[image: 4KB]`) || strings.Contains(string(req), "iVBORw0K") {
		t.Errorf("image not replaced: %s", req)
	}
	// The stream is stored as the equivalent complete OpenAI response.
	res, _ := json.Marshal(d["response_body"])
	for _, sub := range []string{`"object":"chat.completion"`, `"content":"Hello"`, `"reasoning_content":"hmm"`,
		`"name":"f"`, `"finish_reason":"tool_calls"`} {
		if !strings.Contains(string(res), sub) {
			t.Errorf("response_body missing %s: %s", sub, res)
		}
	}
	if d["upstream_request_body"] != nil {
		t.Error("upstream request must not be logged by default")
	}
	if att := d["attempts"].([]any); len(att) != 1 || att[0].(map[string]any)["result"] != "ok" {
		t.Errorf("attempts = %v", d["attempts"])
	}
}

func TestRequestLogsModesAndErrors(t *testing.T) {
	h, a := logHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w, ollamaText("ok"), ollamaDone)
	}))
	defer up.Close()
	h.alias("m", tgt{h.provider("ol", "ollama", up.URL), "real"})

	h.key = h.newKey(&store.VirtualKey{Name: "meta", LogMode: store.LogMetadataOnly})
	h.mustPost("/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"secret prompt"}]}`)
	h.key = h.newKey(&store.VirtualKey{Name: "silent", LogMode: store.LogNone})
	h.mustPost("/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	h.key = h.newKey(&store.VirtualKey{Name: "full", LogMode: store.LogFull})
	h.mustPost("/api/chat", `{"model":"nope","messages":[{"role":"user","content":"x"}]}`) // 404
	h.mustPost("/v1/messages", `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"anthropic"}]}`)

	rows := waitLogs(t, a, 3)
	time.Sleep(600 * time.Millisecond) // allow a possible (wrong) 4th row to be flushed
	rows = waitLogs(t, a, 3)
	if len(rows) != 3 {
		t.Fatalf("rows = %d (log_mode none must not be logged)", len(rows))
	}
	byKey := map[string]map[string]any{}
	for _, r := range rows {
		byKey[r["key_name"].(string)+":"+r["inbound_format"].(string)] = r
	}
	meta := logDetail(t, a, byKey["meta:openai"]["id"])
	if meta["request_body"] != nil || meta["response_body"] != nil {
		t.Errorf("metadata_only stored bodies: %v", meta)
	}
	nf := logDetail(t, a, byKey["full:ollama"]["id"])
	if nf["status_code"] != 404.0 || !strings.Contains(nf["error"].(string), "not found") {
		t.Errorf("404 row = %v", nf)
	}
	an := logDetail(t, a, byKey["full:anthropic"]["id"])
	if rb := an["response_body"].(map[string]any); rb["type"] != "message" || rb["role"] != "assistant" {
		t.Errorf("anthropic response_body = %v", an["response_body"])
	}

	// filters
	_, out := a.do("GET", "/admin/api/logs?status=4xx", "")
	if len(out["data"].([]any)) != 1 {
		t.Errorf("status filter: %v", out)
	}
	_, out = a.do("GET", "/admin/api/logs?q=anthropic", "")
	if len(out["data"].([]any)) != 1 {
		t.Errorf("body search: %v", out)
	}
	if _, out = a.do("GET", "/admin/api/logs?q=secret+prompt", ""); len(out["data"].([]any)) != 0 {
		t.Error("metadata_only body must not be searchable")
	}
	if resp, _ := a.do("GET", "/admin/api/logs?status=bogus", ""); resp.StatusCode != 400 {
		t.Errorf("bad filter: %d", resp.StatusCode)
	}

	// JSONL export
	req, _ := http.NewRequest("GET", a.base+"/admin/api/logs/export?alias=m", nil)
	resp, err := a.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("invalid JSONL line: %v", err)
		}
		n++
	}
	if resp.Header.Get("Content-Type") != "application/x-ndjson" || n != 2 {
		t.Errorf("export: %s lines=%d", resp.Header.Get("Content-Type"), n)
	}
}

func TestRequestLogInterrupted(t *testing.T) {
	h, a := logHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		writeLines(w, ollamaText("partial answer"))
		<-r.Context().Done()
	}))
	defer up.Close()
	h.alias("m", tgt{h.provider("ol", "ollama", up.URL), "real"})
	h.key = h.newKey(&store.VirtualKey{Name: "full", LogMode: store.LogFull})
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := h.post(ctx, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	newSSE(resp.Body).next(t)
	cancel()
	resp.Body.Close()
	rows := waitLogs(t, a, 1)
	d := logDetail(t, a, rows[0]["id"])
	res, _ := json.Marshal(d["response_body"])
	if d["interrupted"] != true || d["cancelled_by_client"] != true || !strings.Contains(string(res), "partial answer") {
		t.Errorf("interrupted row = %v", d)
	}
}
