package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"llmgw/internal/chat"
)

func ptr[T any](v T) *T { return &v }

func TestBuildOllamaChat(t *testing.T) {
	cases := []struct {
		name string
		req  *chat.Request
		want string // JSON (compared structurally)
		err  bool
	}{
		{
			name: "text with options",
			req: &chat.Request{
				Messages:    []chat.Message{{Role: "system", Content: []chat.Part{chat.TextPart("be brief")}}, {Role: "user", Content: []chat.Part{chat.TextPart("hi")}}},
				Temperature: ptr(0.2), MaxTokens: ptr(64), Stop: []string{"END"},
				Options:   map[string]any{"num_ctx": 8192.0, "temperature": 0.9},
				KeepAlive: json.RawMessage(`"10m"`),
			},
			want: `{"model":"m","stream":true,"keep_alive":"10m",
				"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],
				"options":{"num_ctx":8192,"temperature":0.2,"num_predict":64,"stop":["END"]}}`,
		},
		{
			name: "image and multi text",
			req: &chat.Request{Messages: []chat.Message{{Role: "user", Content: []chat.Part{
				chat.TextPart("what"), {Type: "image", Image: &chat.Image{Data: "AAAA", MediaType: "image/png"}}, chat.TextPart("is this")}}}},
			want: `{"model":"m","stream":true,"messages":[{"role":"user","content":"what\nis this","images":["AAAA"]}]}`,
		},
		{
			name: "remote image rejected",
			req:  &chat.Request{Messages: []chat.Message{{Role: "user", Content: []chat.Part{{Type: "image", Image: &chat.Image{URL: "https://x/y.png"}}}}}},
			err:  true,
		},
		{
			name: "tool round trip",
			req: &chat.Request{
				Messages: []chat.Message{
					{Role: "user", Content: []chat.Part{chat.TextPart("weather?")}},
					{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Tokyo"}`}}},
					{Role: "tool", ToolCallID: "call_1", Content: []chat.Part{chat.TextPart("sunny")}},
				},
				Tools: []chat.Tool{{Name: "get_weather", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
			},
			want: `{"model":"m","stream":true,
				"messages":[{"role":"user","content":"weather?"},
					{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Tokyo"}}}]},
					{"role":"tool","content":"sunny","tool_name":"get_weather"}],
				"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object"}}}]}`,
		},
		{
			name: "tool_choice none drops tools",
			req: &chat.Request{Messages: []chat.Message{{Role: "user"}}, Tools: []chat.Tool{{Name: "f"}},
				ToolChoice: &chat.ToolChoice{Mode: "none"}},
			want: `{"model":"m","stream":true,"messages":[{"role":"user","content":""}]}`,
		},
		{
			name: "json schema and think effort",
			req: &chat.Request{Messages: []chat.Message{{Role: "user"}},
				ResponseFormat: &chat.ResponseFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"object"}`)},
				Think:          &chat.Think{Enabled: true, Effort: "high"}},
			want: `{"model":"m","stream":true,"messages":[{"role":"user","content":""}],"format":{"type":"object"},"think":"high"}`,
		},
		{
			name: "json_object and think disabled",
			req: &chat.Request{Messages: []chat.Message{{Role: "user"}},
				ResponseFormat: &chat.ResponseFormat{Type: "json_object"}, Think: &chat.Think{Enabled: false}},
			want: `{"model":"m","stream":true,"messages":[{"role":"user","content":""}],"format":"json","think":false}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := BuildOllamaChat(c.req, "m")
			if c.err {
				var ce *ConversionError
				if !errors.As(err, &ce) {
					t.Fatalf("expected ConversionError, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, got, c.want)
		})
	}
}

func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got invalid JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want invalid JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func collect(t *testing.T, parse func(io.Reader, func(*chat.Event) error) error, input string) ([]*chat.Event, error) {
	t.Helper()
	var evs []*chat.Event
	err := parse(strings.NewReader(input), func(ev *chat.Event) error {
		evs = append(evs, ev)
		return nil
	})
	return evs, err
}

func TestParseOllamaChatStream(t *testing.T) {
	t.Run("text and thinking", func(t *testing.T) {
		in := `{"model":"m","message":{"role":"assistant","content":"","thinking":"hmm"},"done":false}
{"model":"m","message":{"role":"assistant","content":"Hel"},"done":false}
{"model":"m","message":{"role":"assistant","content":"lo"},"done":false}
{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":3}
`
		evs, err := collect(t, ParseOllamaChatStream, in)
		if err != nil {
			t.Fatal(err)
		}
		acc := chat.NewAccumulator()
		for _, ev := range evs {
			acc.Add(ev)
			if ev.RawFormat != chat.FormatOllama || len(ev.Raw) == 0 {
				t.Error("raw chunk must be kept")
			}
		}
		c := acc.Choices()[0]
		if c.Content() != "Hello" || c.Reasoning() != "hmm" || c.FinishReason != "stop" {
			t.Errorf("content=%q reasoning=%q finish=%q", c.Content(), c.Reasoning(), c.FinishReason)
		}
		if acc.Usage == nil || acc.Usage.PromptTokens != 10 || acc.Usage.CompletionTokens != 3 {
			t.Errorf("usage = %+v", acc.Usage)
		}
		if !evs[0].IsToken() {
			t.Error("thinking chunk must count as a token")
		}
	})
	t.Run("tool calls get ids", func(t *testing.T) {
		in := `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"a","arguments":{"x":1}}},{"function":{"name":"b","arguments":{}}}]},"done":false}
{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}
`
		evs, err := collect(t, ParseOllamaChatStream, in)
		if err != nil {
			t.Fatal(err)
		}
		acc := chat.NewAccumulator()
		for _, ev := range evs {
			acc.Add(ev)
		}
		c := acc.Choices()[0]
		tcs := c.ToolCalls()
		if len(tcs) != 2 || tcs[0].Name != "a" || tcs[0].Arguments != `{"x":1}` || tcs[1].Name != "b" {
			t.Fatalf("tool calls = %+v", tcs)
		}
		if !strings.HasPrefix(tcs[0].ID, "call_") || tcs[0].ID == tcs[1].ID {
			t.Errorf("ids = %q %q", tcs[0].ID, tcs[1].ID)
		}
		if c.FinishReason != chat.FinishToolCalls {
			t.Errorf("finish = %q", c.FinishReason)
		}
	})
	t.Run("length", func(t *testing.T) {
		evs, _ := collect(t, ParseOllamaChatStream, `{"message":{"content":"x"},"done":true,"done_reason":"length"}`+"\n")
		if evs[0].FinishReason != chat.FinishLength {
			t.Errorf("finish = %q", evs[0].FinishReason)
		}
	})
	t.Run("error line", func(t *testing.T) {
		_, err := collect(t, ParseOllamaChatStream, `{"message":{"content":"x"},"done":false}`+"\n"+`{"error":"boom"}`+"\n")
		var se *StreamError
		if !errors.As(err, &se) || se.Message != "boom" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		_, err := collect(t, ParseOllamaChatStream, `{"message":{"content":"x"},"done":false}`+"\n")
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestOllamaEmbed(t *testing.T) {
	b, _ := BuildOllamaEmbed(&EmbedRequest{Input: []string{"a", "b"}, Dimensions: ptr(2)}, "e")
	assertJSONEqual(t, b, `{"model":"e","input":["a","b"],"dimensions":2}`)
	r, err := ParseOllamaEmbed([]byte(`{"embeddings":[[0.1,0.2],[0.3,0.4]],"prompt_eval_count":4}`))
	if err != nil || len(r.Embeddings) != 2 || r.PromptTokens != 4 {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}
