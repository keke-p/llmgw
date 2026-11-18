package anthropic

import (
	"regexp"
	"strings"
	"testing"

	"llmgw/internal/chat"
)

func TestParseMessages(t *testing.T) {
	in, ge := ParseMessages([]byte(`{
		"model": "claude-sonnet-4-5",
		"max_tokens": 1024,
		"system": [{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Be brief."}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAAA"}}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"plan","signature":"sig"},
				{"type":"text","text":"calling"},
				{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a.txt"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"file body"}]},
				{"type":"text","text":"continue"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"boom","is_error":true}]}
		],
		"tools": [{"name":"read","description":"Read a file","input_schema":{"type":"object"}},
		          {"type":"web_search_20250305","name":"web_search"}],
		"tool_choice": {"type":"any"},
		"thinking": {"type":"enabled","budget_tokens":2048},
		"stop_sequences": ["END"],
		"metadata": {"user_id":"x"}
	}`))
	if ge != nil {
		t.Fatal(ge)
	}
	r := in.Req
	if *r.MaxTokens != 1024 || r.Stop[0] != "END" || r.Think == nil || !r.Think.Enabled || r.Think.BudgetTokens != 2048 {
		t.Errorf("params: %+v", r)
	}
	if r.ToolChoice.Mode != "required" || len(r.Tools) != 1 || r.Tools[0].Name != "read" {
		t.Errorf("tools: %+v %+v", r.Tools, r.ToolChoice)
	}
	roles := []string{}
	for _, m := range r.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user,tool" {
		t.Fatalf("roles = %v", roles)
	}
	if r.Messages[0].Text() != "You are helpful.\n\nBe brief." {
		t.Errorf("system = %q", r.Messages[0].Text())
	}
	if img := r.Messages[1].Images(); len(img) != 1 || img[0].MediaType != "image/jpeg" {
		t.Errorf("image = %+v", img)
	}
	a := r.Messages[2]
	if a.Reasoning != "plan" || a.ReasoningSignature != "sig" || a.ToolCalls[0].Arguments != `{"path":"a.txt"}` || a.Text() != "calling" {
		t.Errorf("assistant = %+v", a)
	}
	if r.Messages[3].ToolCallID != "toolu_1" || r.Messages[3].Text() != "file body" || r.Messages[4].Text() != "continue" {
		t.Errorf("tool result split wrong: %+v / %+v", r.Messages[3], r.Messages[4])
	}
	if r.Messages[5].Text() != "Error: boom" {
		t.Errorf("is_error = %q", r.Messages[5].Text())
	}
}

func TestParseMessagesErrors(t *testing.T) {
	for name, body := range map[string]string{
		"no messages": `{"model":"m","max_tokens":1,"messages":[]}`,
		"bad role":    `{"model":"m","messages":[{"role":"tool","content":"x"}]}`,
		"bad system":  `{"model":"m","system":5,"messages":[{"role":"user","content":"x"}]}`,
	} {
		if _, ge := ParseMessages([]byte(body)); ge == nil || ge.Status != 400 {
			t.Errorf("%s: %v", name, ge)
		}
	}
}

var eventRe = regexp.MustCompile(`(?m)^event: (\S+)`)

func eventNames(s string) string {
	var out []string
	for _, m := range eventRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return strings.Join(out, ",")
}

func TestSinkStream(t *testing.T) {
	s := newSink("claude-x", true)
	var b strings.Builder
	acc := chat.NewAccumulator()
	for _, ev := range []*chat.Event{
		{Reasoning: "hmm"},
		{Content: "Let me "}, {Content: "check."},
		{ToolCalls: []chat.ToolCallDelta{{Index: 0, ID: "call_1", Name: "read", Arguments: `{"pa`}}},
		{ToolCalls: []chat.ToolCallDelta{{Index: 0, Arguments: `th":1}`}}},
		{ToolCalls: []chat.ToolCallDelta{{Index: 1, ID: "call_2", Name: "ls", Arguments: `{}`}}},
		{FinishReason: "tool_calls", Usage: &chat.Usage{PromptTokens: 10, CompletionTokens: 20}},
	} {
		acc.Add(ev)
		if err := s.Event(&b, ev, false); err != nil {
			t.Fatal(err)
		}
	}
	s.StreamEnd(&b, acc)
	out := b.String()
	want := "message_start," +
		"content_block_start,content_block_delta,content_block_stop," + // thinking
		"content_block_start,content_block_delta,content_block_delta,content_block_stop," + // text
		"content_block_start,content_block_delta,content_block_delta,content_block_stop," + // tool 1
		"content_block_start,content_block_delta,content_block_stop," + // tool 2
		"message_delta,message_stop"
	if got := eventNames(out); got != want {
		t.Fatalf("events:\n got %s\nwant %s\n%s", got, want, out)
	}
	for _, sub := range []string{
		`"content_block":{"signature":"","thinking":"","type":"thinking"}`,
		`"delta":{"partial_json":"{\"pa","type":"input_json_delta"}`,
		`"content_block":{"id":"call_1","input":{},"name":"read","type":"tool_use"}`,
		`"index":3`,
		`"stop_reason":"tool_use"`,
		`"usage":{"input_tokens":10,"output_tokens":20}`,
		`"model":"claude-x"`,
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("missing %s", sub)
		}
	}
}

func TestSinkDropsThinkingWhenNotRequested(t *testing.T) {
	s := newSink("m", false)
	var b strings.Builder
	acc := chat.NewAccumulator()
	for _, ev := range []*chat.Event{{Reasoning: "secret"}, {Content: "hi"}, {FinishReason: "length"}} {
		acc.Add(ev)
		s.Event(&b, ev, false)
	}
	s.StreamEnd(&b, acc)
	if strings.Contains(b.String(), "secret") || !strings.Contains(b.String(), `"stop_reason":"max_tokens"`) {
		t.Errorf("out = %s", b.String())
	}
	body, _ := s.Response(acc)
	if strings.Contains(string(body), "thinking") || !strings.Contains(string(body), `"content":[{"text":"hi","type":"text"}]`) {
		t.Errorf("response = %s", body)
	}
}

func TestSinkResponse(t *testing.T) {
	s := newSink("m", true)
	acc := chat.NewAccumulator()
	acc.Add(&chat.Event{Reasoning: "r", Content: "ok"})
	acc.Add(&chat.Event{ToolCalls: []chat.ToolCallDelta{{Index: 0, ID: "c1", Name: "f", Arguments: `{"a":1}`}}})
	acc.Add(&chat.Event{FinishReason: "stop", Usage: &chat.Usage{PromptTokens: 1, CompletionTokens: 2}})
	b, _ := s.Response(acc)
	got := string(b)
	for _, sub := range []string{`"type":"message"`, `{"signature":"","thinking":"r","type":"thinking"}`,
		`{"text":"ok","type":"text"}`, `{"id":"c1","input":{"a":1},"name":"f","type":"tool_use"}`,
		`"stop_reason":"tool_use"`, `"usage":{"input_tokens":1,"output_tokens":2}`} {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %s in %s", sub, got)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	in, _ := ParseMessages([]byte(`{"model":"m","messages":[{"role":"user","content":"hello world, this is a test"}]}`))
	en := EstimateTokens(in, nil)
	in2, _ := ParseMessages([]byte(`{"model":"m","messages":[{"role":"user","content":"こんにちは世界、これはテストです"}]}`))
	ja := EstimateTokens(in2, nil)
	if en < 5 || en > 15 || ja < 15 {
		t.Errorf("en=%d ja=%d", en, ja)
	}
}

func TestSystemRoleInMessages(t *testing.T) {
	in, ge := ParseMessages([]byte(`{"model":"m","messages":[{"role":"user","content":"a"},{"role":"system","content":[{"type":"text","text":"reminder"}]},{"role":"user","content":"b"}]}`))
	if ge != nil {
		t.Fatal(ge)
	}
	if m := in.Req.Messages[1]; m.Role != "system" || m.Text() != "reminder" {
		t.Errorf("system message = %+v", m)
	}
}
