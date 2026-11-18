package ollama

import (
	"strings"
	"testing"

	"llmgw/internal/chat"
	"llmgw/internal/clock"
)

func TestParseChat(t *testing.T) {
	in, ge := ParseChat([]byte(`{
		"model": "qwen",
		"messages": [
			{"role": "user", "content": "weather?", "images": ["iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"]},
			{"role": "assistant", "content": "", "tool_calls": [
				{"function": {"name": "get_weather", "arguments": {"city": "Tokyo"}}},
				{"function": {"name": "get_time", "arguments": {}}}
			]},
			{"role": "tool", "tool_name": "get_time", "content": "noon"},
			{"role": "tool", "tool_name": "get_weather", "content": "sunny"}
		],
		"options": {"temperature": 0.3, "num_ctx": 8192, "num_predict": 100, "stop": ["x"], "top_k": 20},
		"keep_alive": "5m",
		"think": "high"
	}`))
	if ge != nil {
		t.Fatal(ge)
	}
	r := in.Req
	if !r.Stream {
		t.Error("stream must default to true")
	}
	if *r.Temperature != 0.3 || *r.MaxTokens != 100 || *r.TopK != 20 || r.Stop[0] != "x" || r.Options["num_ctx"] != 8192.0 {
		t.Errorf("options mapping: %+v", r)
	}
	if r.Think == nil || r.Think.Effort != "high" || string(r.KeepAlive) != `"5m"` {
		t.Errorf("think/keep_alive: %+v %s", r.Think, r.KeepAlive)
	}
	img := r.Messages[0].Images()
	if len(img) != 1 || img[0].MediaType != "image/png" {
		t.Errorf("image media type: %+v", img)
	}
	tcs := r.Messages[1].ToolCalls
	if tcs[0].ID != "call_1" || tcs[0].Arguments != `{"city":"Tokyo"}` || tcs[1].ID != "call_2" {
		t.Errorf("tool calls: %+v", tcs)
	}
	// tool results are matched to calls by name
	if r.Messages[2].ToolCallID != "call_2" || r.Messages[3].ToolCallID != "call_1" {
		t.Errorf("tool result ids: %q %q", r.Messages[2].ToolCallID, r.Messages[3].ToolCallID)
	}

	in, _ = ParseChat([]byte(`{"model":"m","messages":[],"stream":false}`))
	if in.Req.Stream {
		t.Error("explicit stream:false")
	}
	if _, ge := ParseChat([]byte(`{"model":"m","messages":[{"role":"bot","content":"x"}]}`)); ge == nil {
		t.Error("bad role must fail")
	}
}

func TestParseGenerate(t *testing.T) {
	in, ge := ParseGenerate([]byte(`{"model":"m","prompt":"hi","system":"sys","template":"{{.Prompt}}","context":[1,2],"raw":true}`))
	if ge != nil {
		t.Fatal(ge)
	}
	r := in.Req
	if r.Prompt != "hi" || len(r.Messages) != 2 || r.Messages[0].Role != "system" || r.Messages[1].Text() != "hi" {
		t.Errorf("messages = %+v", r.Messages)
	}
	if strings.Join(r.Unconvertible, ",") != "template,context,raw" {
		t.Errorf("unconvertible = %v", r.Unconvertible)
	}
}

func TestSinkConvertedStream(t *testing.T) {
	c := clock.NewFake()
	s := newSink("qwen:latest", false, c)
	var b strings.Builder
	events := []*chat.Event{
		{Reasoning: "hmm"},
		{Content: "Hi"},
		{ToolCalls: []chat.ToolCallDelta{{Index: 0, ID: "call_a", Name: "f", Arguments: `{"x":`}}},
		{ToolCalls: []chat.ToolCallDelta{{Index: 0, Arguments: `1}`}}},
		{FinishReason: "tool_calls"},
	}
	acc := chat.NewAccumulator()
	for _, ev := range events {
		acc.Add(ev)
		if err := s.Event(&b, ev, false); err != nil {
			t.Fatal(err)
		}
	}
	acc.Add(&chat.Event{Usage: &chat.Usage{PromptTokens: 3, CompletionTokens: 4}})
	s.StreamEnd(&b, acc)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d\n%s", len(lines), b.String())
	}
	for i, want := range []string{
		`"message":{"role":"assistant","content":"","thinking":"hmm"},"done":false`,
		`"message":{"role":"assistant","content":"Hi"},"done":false`,
		`"tool_calls":[{"id":"call_a","function":{"index":0,"name":"f","arguments":{"x":1}}}]`,
		`"done":true,"done_reason":"stop"`,
	} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %s\nwant %s", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[3], `"prompt_eval_count":3`) || !strings.Contains(lines[3], `"eval_count":4`) ||
		!strings.Contains(lines[0], `"model":"qwen:latest"`) {
		t.Errorf("final = %s", lines[3])
	}
}

func TestSinkGenerateResponse(t *testing.T) {
	s := newSink("m:latest", true, clock.NewFake())
	acc := chat.NewAccumulator()
	acc.Add(&chat.Event{Content: "text", FinishReason: "length"})
	b, _ := s.Response(acc)
	if !strings.Contains(string(b), `"response":"text"`) || !strings.Contains(string(b), `"done_reason":"length"`) {
		t.Errorf("response = %s", b)
	}
}

func TestSniffImage(t *testing.T) {
	cases := map[string]string{
		"iVBORw0KGgoAAAANSUhEUgAA": "image/png",
		"/9j/4AAQSkZJRgABAQAAAQAB": "image/jpeg",
		"R0lGODlhAQABAIAAAAAAAP//": "image/gif",
		"!!notbase64":              "image/png",
	}
	for in, want := range cases {
		if got := sniffImage(in); got != want {
			t.Errorf("%s: %s want %s", in, got, want)
		}
	}
}
