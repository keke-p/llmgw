package openai

import (
	"strings"
	"testing"

	"llmgw/internal/chat"
)

func TestParseChatRequest(t *testing.T) {
	body := `{
		"model": "gpt-local",
		"stream": true,
		"stream_options": {"include_usage": true},
		"max_completion_tokens": 50,
		"stop": "END",
		"reasoning_effort": "minimal",
		"messages": [
			{"role": "developer", "content": "sys"},
			{"role": "user", "content": [
				{"type": "text", "text": "look"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBOR"}}
			]},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "c1", "type": "function", "function": {"name": "f", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "c1", "content": "res"}
		],
		"tools": [{"type": "function", "function": {"name": "f", "parameters": {"type": "object"}}}],
		"tool_choice": {"type": "function", "function": {"name": "f"}},
		"response_format": {"type": "json_schema", "json_schema": {"name": "x", "schema": {"type": "object"}}}
	}`
	in, ge := ParseChatRequest([]byte(body))
	if ge != nil {
		t.Fatal(ge)
	}
	r := in.Req
	if r.Model != "gpt-local" || !r.Stream || !r.IncludeUsage || *r.MaxTokens != 50 {
		t.Errorf("basic fields: %+v", r)
	}
	if len(r.Stop) != 1 || r.Stop[0] != "END" {
		t.Errorf("stop = %v", r.Stop)
	}
	if r.Think == nil || r.Think.Effort != "low" {
		t.Errorf("think = %+v", r.Think)
	}
	if r.Messages[0].Role != "system" {
		t.Errorf("developer must map to system")
	}
	img := r.Messages[1].Images()
	if len(img) != 1 || img[0].Data != "iVBOR" || img[0].MediaType != "image/png" {
		t.Errorf("image = %+v", img)
	}
	if r.Messages[2].ToolCalls[0].ID != "c1" || r.Messages[3].ToolCallID != "c1" {
		t.Errorf("tool messages wrong")
	}
	if r.ToolChoice.Mode != "function" || r.ToolChoice.Name != "f" {
		t.Errorf("tool choice = %+v", r.ToolChoice)
	}
	if r.ResponseFormat.Type != "json_schema" || string(r.ResponseFormat.Schema) != `{"type": "object"}` {
		t.Errorf("response format = %+v", r.ResponseFormat)
	}
	if in.Format != chat.FormatOpenAI || in.Raw["model"] == nil {
		t.Error("raw map must be kept")
	}
}

func TestParseChatRequestErrors(t *testing.T) {
	cases := map[string]string{
		"invalid json":  `{`,
		"no messages":   `{"model":"m"}`,
		"bad role":      `{"model":"m","messages":[{"role":"wizard","content":"x"}]}`,
		"bad image url": `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"ftp://x"}}]}]}`,
		"bad stop":      `{"model":"m","messages":[{"role":"user","content":"x"}],"stop":5}`,
	}
	for name, body := range cases {
		if _, ge := ParseChatRequest([]byte(body)); ge == nil || ge.Status != 400 {
			t.Errorf("%s: expected 400, got %v", name, ge)
		}
	}
}

func TestParseEmbedRequest(t *testing.T) {
	in, enc, ge := ParseEmbedRequest([]byte(`{"model":"e","input":"hello","encoding_format":"base64"}`))
	if ge != nil || enc != "base64" || len(in.Req.Input) != 1 {
		t.Fatalf("in=%+v enc=%q ge=%v", in, enc, ge)
	}
	in, _, ge = ParseEmbedRequest([]byte(`{"model":"e","input":[[1,2,3]]}`))
	if ge != nil || in.Req.Input != nil {
		t.Fatalf("token arrays should parse with nil Input; ge=%v", ge)
	}
	if _, _, ge := ParseEmbedRequest([]byte(`{"model":"e"}`)); ge == nil {
		t.Fatal("missing input must fail")
	}
}

func TestChatSinkConversion(t *testing.T) {
	s := newChatSink("alias", true, 1)
	var b strings.Builder
	evs := []*chat.Event{
		{Reasoning: "r"},
		{Content: "hi"},
		{ToolCalls: []chat.ToolCallDelta{{Index: 0, ID: "call_1", Name: "f", Arguments: "{}"}}},
		{FinishReason: "tool_calls", Usage: &chat.Usage{PromptTokens: 1, CompletionTokens: 2}},
	}
	for _, ev := range evs {
		if err := s.Event(&b, ev, false); err != nil {
			t.Fatal(err)
		}
	}
	out := b.String()
	for _, want := range []string{
		`"delta":{"role":"assistant","reasoning_content":"r"}`,
		`"delta":{"content":"hi"}`,
		`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]`,
		`"finish_reason":"tool_calls"`,
		`"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}`,
		`"model":"alias"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
}

func TestChatSinkPassthrough(t *testing.T) {
	s := newChatSink("alias", false, 1)
	var b strings.Builder
	_ = s.Event(&b, &chat.Event{Raw: []byte(`{"id":"u","model":"real","choices":[{"index":0,"delta":{"content":"x"}}],"x_custom":1}`), RawFormat: chat.FormatOpenAI}, true)
	_ = s.Event(&b, &chat.Event{Raw: []byte(`{"id":"u","model":"real","choices":[],"usage":{"prompt_tokens":1}}`), RawFormat: chat.FormatOpenAI}, true)
	_ = s.Event(&b, &chat.Event{Content: "not raw"}, true)
	out := b.String()
	if !strings.Contains(out, `"model":"alias"`) || !strings.Contains(out, `"x_custom":1`) {
		t.Errorf("raw chunk not forwarded properly: %s", out)
	}
	if strings.Contains(out, "usage") || strings.Contains(out, "not raw") {
		t.Errorf("usage-only chunk / non-raw event must be dropped: %s", out)
	}
}
