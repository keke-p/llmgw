package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"testing"

	"llmgw/internal/chat"
	"llmgw/internal/store"
)

func TestBuildOpenAIChat(t *testing.T) {
	cases := []struct {
		name string
		req  *chat.Request
		want string
	}{
		{
			name: "text",
			req: &chat.Request{Messages: []chat.Message{{Role: "user", Content: []chat.Part{chat.TextPart("hi")}}},
				Temperature: ptr(0.5), Stop: []string{"x"}},
			want: `{"model":"m","stream":true,"stream_options":{"include_usage":true},
				"messages":[{"role":"user","content":"hi"}],"temperature":0.5,"stop":["x"]}`,
		},
		{
			name: "image becomes data url",
			req: &chat.Request{Messages: []chat.Message{{Role: "user", Content: []chat.Part{
				chat.TextPart("see"), {Type: "image", Image: &chat.Image{Data: "QUJD", MediaType: "image/jpeg"}}}}}},
			want: `{"model":"m","stream":true,"stream_options":{"include_usage":true},
				"messages":[{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QUJD"}}]}]}`,
		},
		{
			name: "tools and ollama options",
			req: &chat.Request{
				Messages: []chat.Message{
					{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "c1", Name: "f", Arguments: `{"a":1}`}}},
					{Role: "tool", ToolCallID: "c1", Name: "f", Content: []chat.Part{chat.TextPart("ok")}},
				},
				Tools:      []chat.Tool{{Name: "f", Parameters: json.RawMessage(`{"type":"object"}`)}},
				ToolChoice: &chat.ToolChoice{Mode: "function", Name: "f"},
				Options:    map[string]any{"num_ctx": 4096.0, "num_predict": 100.0, "min_p": 0.05},
				Format:     json.RawMessage(`"json"`),
			},
			want: `{"model":"m","stream":true,"stream_options":{"include_usage":true},
				"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
					{"role":"tool","content":"ok","tool_call_id":"c1"}],
				"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
				"tool_choice":{"type":"function","function":{"name":"f"}},
				"max_tokens":100,"min_p":0.05,"response_format":{"type":"json_object"}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := BuildOpenAIChat(c.req, "m")
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, got, c.want)
		})
	}
}

func TestPassthroughOpenAIChat(t *testing.T) {
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"model":"alias","stream":false,"messages":[],"custom_field":{"x":1},"stream_options":{"foo":true}}`), &raw)
	got, err := PassthroughOpenAIChat(raw, "real")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, got, `{"model":"real","stream":true,"messages":[],"custom_field":{"x":1},
		"stream_options":{"foo":true,"include_usage":true}}`)
}

func TestParseOpenAIChatStream(t *testing.T) {
	t.Run("tool call fragments and reasoning", func(t *testing.T) {
		in := `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}

data: {"id":"x","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}

: comment line
data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get","arguments":""}}]}}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}

data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"x","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7}}

data: [DONE]

`
		evs, err := collect(t, ParseOpenAIChatStream, in)
		if err != nil {
			t.Fatal(err)
		}
		acc := chat.NewAccumulator()
		for _, ev := range evs {
			acc.Add(ev)
			if ev.RawFormat != chat.FormatOpenAI {
				t.Error("raw format must be openai")
			}
		}
		if evs[0].IsToken() {
			t.Error("role-only chunk must not count as a token")
		}
		c := acc.Choices()[0]
		tcs := c.ToolCalls()
		if len(tcs) != 1 || tcs[0].ID != "call_a" || tcs[0].Arguments != `{"q":1}` || c.Reasoning() != "think" {
			t.Fatalf("tool calls = %+v reasoning=%q", tcs, c.Reasoning())
		}
		if c.FinishReason != "tool_calls" || acc.Usage.CompletionTokens != 7 || acc.ID != "x" {
			t.Errorf("finish=%q usage=%+v id=%q", c.FinishReason, acc.Usage, acc.ID)
		}
	})
	t.Run("reasoning field alias", func(t *testing.T) {
		evs, _ := collect(t, ParseOpenAIChatStream, `data: {"choices":[{"index":0,"delta":{"reasoning":"r"},"finish_reason":"stop"}]}`+"\n\n")
		if evs[0].Reasoning != "r" {
			t.Errorf("reasoning = %q", evs[0].Reasoning)
		}
	})
	t.Run("error chunk", func(t *testing.T) {
		_, err := collect(t, ParseOpenAIChatStream, `data: {"error":{"message":"bad"}}`+"\n\n")
		var se *StreamError
		if !errors.As(err, &se) || se.Message != "bad" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("eof after finish is ok", func(t *testing.T) {
		_, err := collect(t, ParseOpenAIChatStream, `data: {"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}]}`+"\n\n")
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		_, err := collect(t, ParseOpenAIChatStream, `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`+"\n\n")
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestParseOpenAIChatCompletion(t *testing.T) {
	var evs []*chat.Event
	err := ParseOpenAIChatCompletion([]byte(`{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hi",
		"tool_calls":[{"id":"t1","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":1,"completion_tokens":2}}`), func(ev *chat.Event) error { evs = append(evs, ev); return nil })
	if err != nil {
		t.Fatal(err)
	}
	acc := chat.NewAccumulator()
	for _, ev := range evs {
		acc.Add(ev)
	}
	c := acc.Choices()[0]
	if c.Content() != "hi" || len(c.ToolCalls()) != 1 || acc.Usage.CompletionTokens != 2 {
		t.Errorf("content=%q tcs=%v usage=%v", c.Content(), c.ToolCalls(), acc.Usage)
	}
}

func TestOpenAIBase(t *testing.T) {
	cases := map[string]string{
		"http://h:1234":      "http://h:1234/v1",
		"http://h:1234/":     "http://h:1234/v1",
		"http://h:1234/v1":   "http://h:1234/v1",
		"http://h/custom/v1": "http://h/custom/v1",
	}
	for in, want := range cases {
		if got := OpenAIBase(in); got != want {
			t.Errorf("OpenAIBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlanChat(t *testing.T) {
	req := &chat.Request{Messages: []chat.Message{{Role: "user", Content: []chat.Part{chat.TextPart("x")}}}}
	raw := map[string]json.RawMessage{"model": json.RawMessage(`"a"`), "messages": json.RawMessage(`[]`)}
	cases := []struct {
		ptype       string
		inFmt       chat.Format
		wantURL     string
		wantFmt     chat.Format
		passthrough bool
	}{
		{store.ProviderOllama, chat.FormatOpenAI, "http://o:11434/api/chat", chat.FormatOllama, false},
		{store.ProviderOpenAICompatible, chat.FormatOpenAI, "http://o:11434/v1/chat/completions", chat.FormatOpenAI, true},
		{store.ProviderOpenAICompatible, chat.FormatAnthropic, "http://o:11434/v1/chat/completions", chat.FormatOpenAI, false},
	}
	for _, c := range cases {
		p := &store.Provider{Type: c.ptype, BaseURL: "http://o:11434"}
		plan, err := PlanChat(p, "m", &Inbound{Format: c.inFmt, Req: req, Raw: raw}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if plan.URL != c.wantURL || plan.Format != c.wantFmt || plan.Passthrough != c.passthrough {
			t.Errorf("%s/%s: got %s %s %v", c.ptype, c.inFmt, plan.URL, plan.Format, plan.Passthrough)
		}
	}
}
