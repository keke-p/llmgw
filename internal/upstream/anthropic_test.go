package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"testing"

	"llmgw/internal/chat"
	"llmgw/internal/store"
)

const antStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"real","content":[],"usage":{"input_tokens":12,"output_tokens":0}}}

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_9","name":"read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"p\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"1}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":30}}

event: message_stop
data: {"type":"message_stop"}

`

func TestParseAnthropicStream(t *testing.T) {
	evs, err := collect(t, ParseAnthropicStream, antStream)
	if err != nil {
		t.Fatal(err)
	}
	acc := chat.NewAccumulator()
	for _, ev := range evs {
		acc.Add(ev)
		if ev.RawFormat != chat.FormatAnthropic || len(ev.Raw) == 0 {
			t.Error("raw must be kept")
		}
	}
	if len(evs) != 13 { // 14 events minus the ping
		t.Errorf("events = %d", len(evs))
	}
	c := acc.Choices()[0]
	tcs := c.ToolCalls()
	if c.Reasoning() != "hmm" || c.Content() != "Hi" || len(tcs) != 1 || tcs[0].ID != "toolu_9" || tcs[0].Arguments != `{"p":1}` {
		t.Errorf("assembled: r=%q c=%q tcs=%+v", c.Reasoning(), c.Content(), tcs)
	}
	if c.FinishReason != chat.FinishToolCalls || acc.Usage.PromptTokens != 12 || acc.Usage.CompletionTokens != 30 || acc.ID != "msg_1" {
		t.Errorf("finish=%q usage=%+v", c.FinishReason, acc.Usage)
	}
}

func TestParseAnthropicStreamErrors(t *testing.T) {
	_, err := collect(t, ParseAnthropicStream, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")
	var se *StreamError
	if !errors.As(err, &se) || se.Message != "busy" {
		t.Errorf("err = %v", err)
	}
	_, err = collect(t, ParseAnthropicStream, antStream[:300])
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated: %v", err)
	}
}

func TestParseAnthropicMessage(t *testing.T) {
	var evs []*chat.Event
	err := ParseAnthropicMessage([]byte(`{"id":"m","content":[{"type":"text","text":"a"},{"type":"tool_use","id":"t","name":"f","input":{"x":1}}],
		"stop_reason":"stop_sequence","stop_sequence":"END","usage":{"input_tokens":3,"output_tokens":4}}`),
		func(ev *chat.Event) error { evs = append(evs, ev); return nil })
	if err != nil {
		t.Fatal(err)
	}
	acc := chat.NewAccumulator()
	for _, ev := range evs {
		acc.Add(ev)
	}
	c := acc.Choices()[0]
	if c.Content() != "a" || c.ToolCalls()[0].Arguments != `{"x":1}` || c.StopSequence != "END" || acc.Usage.CompletionTokens != 4 {
		t.Errorf("c=%+v usage=%+v", c, acc.Usage)
	}
}

func TestAnthropicURL(t *testing.T) {
	cases := []struct{ typ, base, want string }{
		{store.ProviderOllama, "http://h:11434", "http://h:11434/v1/messages"},
		{store.ProviderOpenAICompatible, "http://h:8080/v1", "http://h:8080/v1/messages"},
		{store.ProviderOpenAICompatible, "http://h:8080", "http://h:8080/v1/messages"},
	}
	for _, c := range cases {
		if got := AnthropicURL(&store.Provider{Type: c.typ, BaseURL: c.base}, ""); got != c.want {
			t.Errorf("%s %s: %s", c.typ, c.base, got)
		}
	}
}

func TestPlanAnthropicPassthrough(t *testing.T) {
	raw := map[string]json.RawMessage{"model": json.RawMessage(`"claude"`), "stream": json.RawMessage(`false`)}
	in := &Inbound{Format: chat.FormatAnthropic, Req: &chat.Request{}, Raw: raw,
		Header: map[string][]string{"Anthropic-Beta": {"x"}, "X-Api-Key": {"client-key"}}}
	p := &store.Provider{Type: store.ProviderOllama, BaseURL: "http://o", SupportsAnthropic: true, APIKey: "up"}
	plan, err := PlanChat(p, "qwen", in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Passthrough || plan.Format != chat.FormatAnthropic || plan.URL != "http://o/v1/messages" {
		t.Errorf("plan = %+v", plan)
	}
	if plan.Header.Get("anthropic-beta") != "x" || plan.Header.Get("x-api-key") != "up" || plan.Header.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("headers = %v", plan.Header)
	}
	assertJSONEqual(t, plan.Body, `{"model":"qwen","stream":true,"thinking":{"type":"disabled"}}`)

	// An explicit thinking setting is kept.
	in.Raw = map[string]json.RawMessage{"model": json.RawMessage(`"c"`), "thinking": json.RawMessage(`{"type":"enabled","budget_tokens":1024}`)}
	plan, _ = PlanChat(p, "qwen", in, nil)
	assertJSONEqual(t, plan.Body, `{"model":"qwen","stream":true,"thinking":{"type":"enabled","budget_tokens":1024}}`)
	in.Raw = raw

	// Without supports_anthropic the request is converted.
	p.SupportsAnthropic = false
	plan, _ = PlanChat(p, "qwen", in, nil)
	if plan.Passthrough || plan.Format != chat.FormatOllama {
		t.Errorf("conversion plan = %+v", plan)
	}
}
