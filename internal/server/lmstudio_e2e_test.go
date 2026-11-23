package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/store"
)

// checkLMStudioSchema validates a GET /api/v1/models body against what
// OpenCode accepts (its Effect Schema fails the whole list on any mismatch)
// and returns the entries by key.
func checkLMStudioSchema(t *testing.T, body string) map[string]map[string]any {
	t.Helper()
	var res map[string]any
	d := json.NewDecoder(strings.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, body)
	}
	models, ok := res["models"].([]any)
	if !ok {
		t.Fatalf("models must be an array: %s", body)
	}
	isInt := func(v any) bool {
		n, ok := v.(json.Number)
		return ok && !strings.ContainsAny(n.String(), ".eE")
	}
	out := map[string]map[string]any{}
	for i, x := range models {
		m, ok := x.(map[string]any)
		if !ok {
			t.Fatalf("models[%d] is not an object", i)
		}
		key, _ := m["key"].(string)
		if key == "" {
			t.Errorf("models[%d].key must be a non-empty string: %v", i, m)
		}
		if typ, ok := m["type"].(string); !ok || (typ != "llm" && typ != "embedding") {
			t.Errorf("%s: type = %v", key, m["type"])
		}
		if _, ok := m["display_name"].(string); !ok {
			t.Errorf("%s: display_name must be a string", key)
		}
		if v, ok := m["architecture"]; ok && v != nil {
			if _, ok := v.(string); !ok {
				t.Errorf("%s: architecture must be string|null", key)
			}
		}
		insts, ok := m["loaded_instances"].([]any)
		if !ok {
			t.Errorf("%s: loaded_instances must be an array", key)
		}
		for _, in := range insts {
			cfg, _ := in.(map[string]any)["config"].(map[string]any)
			if cfg == nil || !isInt(cfg["context_length"]) {
				t.Errorf("%s: loaded_instances entry %v", key, in)
			}
		}
		v, present := m["max_context_length"]
		if !present || (v != nil && !isInt(v)) {
			t.Errorf("%s: max_context_length must be present and integer|null: %v", key, v)
		}
		caps, ok := m["capabilities"].(map[string]any)
		if !ok {
			t.Errorf("%s: capabilities missing", key)
		} else {
			for _, k := range []string{"vision", "trained_for_tool_use"} {
				if _, ok := caps[k].(bool); !ok {
					t.Errorf("%s: capabilities.%s must be a boolean", key, k)
				}
			}
		}
		out[key] = m
	}
	return out
}

func num(v any) any {
	if n, ok := v.(json.Number); ok {
		i, _ := n.Int64()
		return int(i)
	}
	return v
}

func caps(m map[string]any) (tools, vision bool) {
	c := m["capabilities"].(map[string]any)
	return c["trained_for_tool_use"].(bool), c["vision"].(bool)
}

func TestLMStudioModelsAuth(t *testing.T) {
	h := newHarness(t)
	for _, key := range []string{"", "sk-gw-bogus"} {
		if resp, b := h.get("/api/v1/models", key); resp.StatusCode != 401 {
			t.Errorf("key %q: %d %s", key, resp.StatusCode, b)
		}
	}
	resp, b := h.get("/api/v1/models", h.key)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if got := checkLMStudioSchema(t, b); len(got) != 0 {
		t.Errorf("no aliases: %s", b)
	}
}

func TestLMStudioModelsFromOllama(t *testing.T) {
	h := newHarness(t)
	shows := map[string]string{
		// Modelfile num_ctx, tools + vision
		"gemma3:12b": `{"parameters":"num_ctx                        32768\nstop  \"<end>\"","capabilities":["completion","tools","vision"],
			"model_info":{"general.architecture":"gemma3","gemma3.context_length":131072},"details":{"family":"gemma3"}}`,
		// no num_ctx: running context from /api/ps (OLLAMA_CONTEXT_LENGTH)
		"qwen3:8b": `{"parameters":"","capabilities":["completion","tools","thinking"],
			"model_info":{"general.architecture":"qwen3","qwen3.context_length":40960}}`,
		// not loaded, no num_ctx: trained maximum; no tools
		"old:7b":           `{"capabilities":["completion"],"model_info":{"general.architecture":"llama","llama.context_length":4096}}`,
		"nomic-embed-text": `{"capabilities":["embedding"],"model_info":{"general.architecture":"nomic-bert","nomic-bert.context_length":2048}}`,
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			var q struct{ Model string }
			json.NewDecoder(r.Body).Decode(&q)
			if s, ok := shows[q.Model]; ok {
				w.Write([]byte(s))
				return
			}
			http.Error(w, `{"error":"model not found"}`, 404)
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"qwen3:8b","model":"qwen3:8b","context_length":16384}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	ol := h.provider("ol", "ollama", up.URL)
	h.alias("gemma", tgt{ol, "gemma3:12b"})
	h.alias("qwen", tgt{ol, "qwen3:8b"})
	h.alias("old", tgt{ol, "old:7b"})
	h.alias("embed", tgt{ol, "nomic-embed-text"})
	h.alias("missing", tgt{ol, "gone"})
	h.exec(`UPDATE aliases SET params_json = '{"overrides":{"num_ctx":8192}}' WHERE name = 'gemma'`)
	h.exec(`UPDATE aliases SET model_info_json = '{"context_length":12000,"tools":true,"vision":true}' WHERE name = 'old'`)
	h.exec(`INSERT INTO aliases(name, enabled, created_at, updated_at) VALUES ('off', 0, 0, 0)`)

	resp, b := h.get("/api/v1/models", h.key)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	got := checkLMStudioSchema(t, b)
	if len(got) != 5 || got["off"] != nil {
		t.Fatalf("models: %s", b)
	}

	g := got["gemma"]
	if g["type"] != "llm" || g["architecture"] != "gemma3" || num(g["max_context_length"]) != 8192 || g["display_name"] != "gemma" {
		t.Errorf("gemma (alias num_ctx wins over Modelfile): %v", g)
	}
	if tools, vision := caps(g); !tools || !vision {
		t.Errorf("gemma caps: %v", g["capabilities"])
	}
	if q := got["qwen"]; num(q["max_context_length"]) != 16384 {
		t.Errorf("qwen (running context): %v", q)
	}
	o := got["old"]
	if num(o["max_context_length"]) != 12000 {
		t.Errorf("old (manual override): %v", o)
	}
	if tools, vision := caps(o); !tools || !vision {
		t.Errorf("old caps override: %v", o["capabilities"])
	}
	if e := got["embed"]; e["type"] != "embedding" {
		t.Errorf("embedding-only model: %v", e)
	}
	if m := got["missing"]; m["type"] != "llm" || m["max_context_length"] != nil {
		t.Errorf("unknown upstream model falls back to defaults: %v", m)
	}

	// A key restricted to some aliases sees only those.
	limited := h.newKey(&store.VirtualKey{Name: "limited", AllowedAliases: []string{"qwen"}})
	_, b = h.get("/api/v1/models", limited)
	if got := checkLMStudioSchema(t, b); len(got) != 1 || got["qwen"] == nil {
		t.Errorf("limited key: %s", b)
	}
}

func TestLMStudioModelsFromLMStudio(t *testing.T) {
	h := newHarness(t)
	var auth string
	lms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"models":[
			{"type":"llm","publisher":"qwen","key":"qwen/qwen3-coder-30b","display_name":"Qwen3 Coder 30B","architecture":"qwen3moe",
			 "loaded_instances":[{"id":"qwen/qwen3-coder-30b","config":{"context_length":65536,"flash_attention":true}}],
			 "max_context_length":262144,"capabilities":{"vision":false,"trained_for_tool_use":true},"size_bytes":123},
			{"type":"decision","key":"router","display_name":"Router","loaded_instances":[],"max_context_length":4096.0},
			{"type":"embedding","key":"text-embedding-nomic","display_name":"Nomic","loaded_instances":[],"max_context_length":2048}]}`))
	}))
	defer lms.Close()
	other := httptest.NewServer(http.NotFoundHandler()) // e.g. llama.cpp: no LM Studio API
	defer other.Close()

	p := h.provider("lms", "openai_compatible", lms.URL+"/v1")
	h.exec(`UPDATE providers SET api_key_enc = NULL WHERE id = ?`, p)
	h.alias("coder", tgt{p, "qwen/qwen3-coder-30b"})
	h.alias("router", tgt{p, "router"})
	h.alias("emb", tgt{p, "text-embedding-nomic"})
	h.alias("cpp", tgt{h.provider("cpp", "openai_compatible", other.URL), "whatever"})

	_, b := h.get("/api/v1/models", h.key)
	got := checkLMStudioSchema(t, b)
	if len(got) != 4 {
		t.Fatalf("models: %s", b)
	}
	c := got["coder"]
	insts := c["loaded_instances"].([]any)
	if c["display_name"] != "Qwen3 Coder 30B" || c["architecture"] != "qwen3moe" || num(c["max_context_length"]) != 262144 ||
		len(insts) != 1 || num(c["size_bytes"]) != 123 || bytes.Contains([]byte(b), []byte(`"key":"qwen/qwen3-coder-30b"`)) {
		t.Errorf("coder (relayed, key renamed): %v", c)
	}
	if r := got["router"]; r["type"] != "llm" || num(r["max_context_length"]) != 4096 {
		t.Errorf("router: decision must become llm, float ctx an integer: %v", r)
	}
	if got["emb"]["type"] != "embedding" {
		t.Errorf("emb: %v", got["emb"])
	}
	x := got["cpp"]
	if x["max_context_length"] != nil || len(x["loaded_instances"].([]any)) != 0 {
		t.Errorf("non-LM Studio upstream: %v", x)
	}
	if tools, _ := caps(x); !tools {
		t.Errorf("non-LM Studio upstream assumes tool use: %v", x)
	}
	if auth != "" {
		t.Errorf("no provider key configured, got Authorization %q", auth)
	}

	// A context length override replaces the loaded instances' length.
	h.exec(`UPDATE aliases SET model_info_json = '{"context_length":32000,"tools":false}' WHERE name = 'coder'`)
	_, b = h.get("/api/v1/models", h.key)
	c = checkLMStudioSchema(t, b)["coder"]
	if num(c["max_context_length"]) != 32000 || len(c["loaded_instances"].([]any)) != 0 {
		t.Errorf("override: %v", c)
	}
	if tools, _ := caps(c); tools {
		t.Errorf("tools override: %v", c["capabilities"])
	}
}
