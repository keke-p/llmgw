package params

import (
	"encoding/json"
	"reflect"
	"testing"

	"llmgw/internal/chat"
)

func mustParse(t *testing.T, s string) *Params {
	t.Helper()
	p := Parse(json.RawMessage(s))
	if p == nil {
		t.Fatalf("parse %s", s)
	}
	return p
}

func ptr[T any](v T) *T { return &v }

func TestApplyChat(t *testing.T) {
	p := mustParse(t, `{"defaults":{"temperature":0.7,"top_k":40,"repeat_penalty":1.1},
		"overrides":{"num_ctx":32768,"keep_alive":"30m","max_tokens":1000,"think":false},
		"system_prompt":{"mode":"prepend","text":"ALIAS"}}`)
	r := &chat.Request{Temperature: ptr(0.2), MaxTokens: ptr(5000), Options: map[string]any{"repeat_penalty": 1.5},
		Messages: []chat.Message{{Role: "system", Content: []chat.Part{chat.TextPart("CLIENT")}}, {Role: "user"}}}
	p.ApplyChat(r)
	if *r.Temperature != 0.2 || *r.TopK != 40 || r.Options["repeat_penalty"] != 1.5 {
		t.Errorf("defaults must not override client values: %+v %v", r, r.Options)
	}
	if r.Options["num_ctx"] != 32768.0 || string(r.KeepAlive) != `"30m"` || r.Think == nil || r.Think.Enabled {
		t.Errorf("overrides: %v %s %+v", r.Options, r.KeepAlive, r.Think)
	}
	if *r.MaxTokens != 1000 {
		t.Errorf("max_tokens must be capped, got %d", *r.MaxTokens)
	}
	if len(r.Messages) != 2 || r.Messages[0].Text() != "ALIAS\n\nCLIENT" {
		t.Errorf("prepend: %+v", r.Messages)
	}

	r = &chat.Request{MaxTokens: ptr(100), Messages: []chat.Message{{Role: "user"}, {Role: "system"}}}
	mustParse(t, `{"overrides":{"max_tokens":1000},"system_prompt":{"mode":"replace","text":"ONLY"}}`).ApplyChat(r)
	if *r.MaxTokens != 100 {
		t.Error("smaller client max_tokens must be kept")
	}
	if len(r.Messages) != 2 || r.Messages[0].Role != "system" || r.Messages[0].Text() != "ONLY" || r.Messages[1].Role != "user" {
		t.Errorf("replace: %+v", r.Messages)
	}
	if Parse(json.RawMessage(`{}`)) != nil {
		t.Error("empty params must be nil")
	}
}

func rawOf(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func jsonEq(t *testing.T, got map[string]json.RawMessage, want string) {
	t.Helper()
	var g, w any
	b, _ := json.Marshal(got)
	json.Unmarshal(b, &g)
	json.Unmarshal([]byte(want), &w)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("\n got %s\nwant %s", b, want)
	}
}

func TestApplyRaw(t *testing.T) {
	p := mustParse(t, `{"defaults":{"temperature":0.7},"overrides":{"num_ctx":8192,"keep_alive":-1,"max_tokens":256,"stop":"END"},
		"system_prompt":{"mode":"prepend","text":"S"}}`)
	cases := []struct {
		name     string
		f        chat.Format
		generate bool
		in, want string
	}{
		{"ollama chat", chat.FormatOllama, false,
			`{"model":"m","messages":[{"role":"user","content":"hi"}],"options":{"temperature":0.1,"num_predict":999}}`,
			`{"model":"m","keep_alive":-1,"messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}],
			  "options":{"temperature":0.1,"num_predict":256,"num_ctx":8192,"stop":["END"]}}`},
		{"ollama generate", chat.FormatOllama, true,
			`{"model":"m","prompt":"p","system":"C"}`,
			`{"model":"m","prompt":"p","system":"S\n\nC","keep_alive":-1,
			  "options":{"temperature":0.7,"num_predict":256,"num_ctx":8192,"stop":["END"]}}`},
		{"openai", chat.FormatOpenAI, false,
			`{"model":"m","messages":[{"role":"system","content":"C"}],"max_completion_tokens":100}`,
			`{"model":"m","messages":[{"role":"system","content":"S\n\nC"}],"max_completion_tokens":100,
			  "temperature":0.7,"num_ctx":8192,"keep_alive":-1,"stop":"END"}`},
		{"anthropic", chat.FormatAnthropic, false,
			`{"model":"m","max_tokens":4096,"system":[{"type":"text","text":"C"}],"messages":[]}`,
			`{"model":"m","max_tokens":256,"system":[{"type":"text","text":"S"},{"type":"text","text":"C"}],"messages":[],
			  "temperature":0.7,"stop_sequences":["END"]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := rawOf(t, c.in)
			before, _ := json.Marshal(in)
			got := p.ApplyRaw(in, c.f, c.generate)
			jsonEq(t, got, c.want)
			after, _ := json.Marshal(in)
			if string(before) != string(after) {
				t.Error("input map must not be modified")
			}
		})
	}
}

func TestRawReplaceAndThink(t *testing.T) {
	p := mustParse(t, `{"overrides":{"think":false},"system_prompt":{"mode":"replace","text":"S"}}`)
	got := p.ApplyRaw(rawOf(t, `{"messages":[{"role":"developer","content":"x"},{"role":"user","content":"u"}]}`), chat.FormatOpenAI, false)
	jsonEq(t, got, `{"messages":[{"role":"system","content":"S"},{"role":"user","content":"u"}],"reasoning_effort":"none"}`)
	got = p.ApplyRaw(rawOf(t, `{"system":"old","messages":[{"role":"system","content":"x"},{"role":"user","content":"u"}]}`), chat.FormatAnthropic, false)
	jsonEq(t, got, `{"system":"S","messages":[{"role":"user","content":"u"}],"thinking":{"type":"disabled"}}`)
}

func TestPassthroughOK(t *testing.T) {
	if !mustParse(t, `{"overrides":{"temperature":0.1,"max_tokens":10}}`).PassthroughOK(chat.FormatAnthropic) {
		t.Error("sampling params fit an Anthropic request")
	}
	p := mustParse(t, `{"overrides":{"num_ctx":32768}}`)
	if p.PassthroughOK(chat.FormatAnthropic) || !p.PassthroughOK(chat.FormatOllama) || !p.PassthroughOK(chat.FormatOpenAI) {
		t.Error("num_ctx cannot be carried by Anthropic requests")
	}
	var nilP *Params
	if !nilP.PassthroughOK(chat.FormatAnthropic) {
		t.Error("nil params")
	}
}

func TestEmbedOptions(t *testing.T) {
	p := mustParse(t, `{"defaults":{"num_ctx":2048},"overrides":{"keep_alive":"1h","temperature":0.5,"num_thread":4}}`)
	opts, ka := p.EmbedOptions(map[string]any{"num_ctx": 512.0}, nil)
	if opts["num_ctx"] != 512.0 || opts["num_thread"] != 4.0 || opts["temperature"] != nil || string(ka) != `"1h"` {
		t.Errorf("opts=%v ka=%s", opts, ka)
	}
}
