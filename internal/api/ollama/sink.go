package ollama

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"llmgw/internal/chat"
	"llmgw/internal/clock"
	"llmgw/internal/gwerr"
	"llmgw/internal/upstream"
)

// ErrorBody renders {"error": "message"}.
func ErrorBody(e *gwerr.Error) []byte {
	b, _ := json.Marshal(map[string]string{"error": e.Message})
	return b
}

// WriteError writes an Ollama-format error response.
func WriteError(w http.ResponseWriter, e *gwerr.Error) {
	e.SetRetryAfter(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(ErrorBody(e))
}

// sink renders chat (/api/chat) or generate (/api/generate) responses as
// Ollama NDJSON / JSON.
type sink struct {
	alias       string
	generate    bool
	clock       clock.Clock
	start       time.Time
	firstToken  time.Time
	passthrough bool
	tools       map[int]*chat.ToolCall
}

func newSink(alias string, generate bool, c clock.Clock) *sink {
	return &sink{alias: alias, generate: generate, clock: c, start: c.Now(), tools: map[int]*chat.ToolCall{}}
}

func (s *sink) StreamContentType() string { return "application/x-ndjson" }

// Keepalive: NDJSON has no safe keepalive, so none is sent.
func (s *sink) Keepalive() []byte { return nil }

type toolCallOut struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Index     int             `json:"index"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type messageOut struct {
	Role      string        `json:"role"`
	Content   string        `json:"content"`
	Thinking  string        `json:"thinking,omitempty"`
	ToolCalls []toolCallOut `json:"tool_calls,omitempty"`
}

type chunkOut struct {
	Model     string      `json:"model"`
	CreatedAt string      `json:"created_at"`
	Message   *messageOut `json:"message,omitempty"`
	Response  *string     `json:"response,omitempty"`
	Thinking  string      `json:"thinking,omitempty"`
	Done      bool        `json:"done"`

	DoneReason         string `json:"done_reason,omitempty"`
	TotalDuration      int64  `json:"total_duration,omitempty"`
	LoadDuration       *int64 `json:"load_duration,omitempty"`
	PromptEvalCount    *int   `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration *int64 `json:"prompt_eval_duration,omitempty"`
	EvalCount          *int   `json:"eval_count,omitempty"`
	EvalDuration       *int64 `json:"eval_duration,omitempty"`
}

func (s *sink) now() string { return s.clock.Now().UTC().Format(time.RFC3339Nano) }

func (s *sink) chunk(content, thinking string, tools []toolCallOut) chunkOut {
	c := chunkOut{Model: s.alias, CreatedAt: s.now()}
	if s.generate {
		c.Response = &content
		c.Thinking = thinking
	} else {
		c.Message = &messageOut{Role: "assistant", Content: content, Thinking: thinking, ToolCalls: tools}
	}
	return c
}

func writeLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func (s *sink) Event(w io.Writer, ev *chat.Event, passthrough bool) error {
	if ev.IsToken() && s.firstToken.IsZero() {
		s.firstToken = s.clock.Now()
	}
	s.passthrough = passthrough
	if passthrough {
		want := chat.FormatOllama
		if s.generate {
			want = chat.RawOllamaGenerate
		}
		if ev.Raw == nil || ev.RawFormat != want {
			return nil
		}
		return forwardRaw(w, ev.Raw, s.alias)
	}
	// Ollama emits complete tool calls; fragments from OpenAI-style upstreams
	// are buffered until the choice finishes.
	for _, d := range ev.ToolCalls {
		tc := s.tools[d.Index]
		if tc == nil {
			tc = &chat.ToolCall{}
			s.tools[d.Index] = tc
		}
		if d.ID != "" {
			tc.ID = d.ID
		}
		tc.Name += d.Name
		tc.Arguments += d.Arguments
	}
	if ev.Choice != 0 {
		return nil // Ollama has a single choice
	}
	if ev.Content != "" || ev.Reasoning != "" {
		if err := writeLine(w, s.chunk(ev.Content, ev.Reasoning, nil)); err != nil {
			return err
		}
	}
	if ev.FinishReason != "" && len(s.tools) > 0 && !s.generate {
		tools := s.toolCallsOut()
		s.tools = map[int]*chat.ToolCall{}
		return writeLine(w, s.chunk("", "", tools))
	}
	return nil
}

func (s *sink) toolCallsOut() []toolCallOut {
	idx := make([]int, 0, len(s.tools))
	for i := range s.tools {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]toolCallOut, 0, len(idx))
	for n, i := range idx {
		out = append(out, toToolOut(n, *s.tools[i]))
	}
	return out
}

func toToolOut(i int, tc chat.ToolCall) toolCallOut {
	var o toolCallOut
	o.ID = tc.ID
	o.Function.Index, o.Function.Name = i, tc.Name
	o.Function.Arguments = argsObject(tc.Arguments)
	return o
}

func argsObject(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// forwardRaw forwards an upstream NDJSON line with the model name rewritten.
func forwardRaw(w io.Writer, raw []byte, alias string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	m["model"], _ = json.Marshal(alias)
	return writeLine(w, m)
}

// doneChunk fills the final statistics from the accumulator.
func (s *sink) doneChunk(c *chunkOut, acc *chat.Accumulator) {
	now := s.clock.Now()
	c.Done = true
	c.DoneReason = "stop"
	if ch := acc.Choices()[0]; ch.FinishReason == chat.FinishLength {
		c.DoneReason = "length"
	}
	zero := int64(0)
	c.TotalDuration = max(now.Sub(s.start).Nanoseconds(), 1)
	c.LoadDuration = &zero
	pe, ev := int64(0), int64(1)
	first := s.firstToken
	if first.IsZero() {
		first = acc.FirstTokenAt
	}
	if !first.IsZero() {
		pe = max(first.Sub(s.start).Nanoseconds(), 0)
		ev = max(now.Sub(first).Nanoseconds(), 1) // avoid division by zero in clients
	}
	c.PromptEvalDuration, c.EvalDuration = &pe, &ev
	p, e := 0, 0
	if acc.Usage != nil {
		p, e = acc.Usage.PromptTokens, acc.Usage.CompletionTokens
	}
	c.PromptEvalCount, c.EvalCount = &p, &e
}

func (s *sink) StreamEnd(w io.Writer, acc *chat.Accumulator) error {
	if s.passthrough {
		return nil // upstream's final line (with statistics) was already forwarded
	}
	if len(s.tools) > 0 && !s.generate {
		if err := writeLine(w, s.chunk("", "", s.toolCallsOut())); err != nil {
			return err
		}
	}
	c := s.chunk("", "", nil)
	s.doneChunk(&c, acc)
	return writeLine(w, c)
}

func (s *sink) StreamError(w io.Writer, e *gwerr.Error) error {
	_, err := w.Write(append(ErrorBody(e), '\n'))
	return err
}

func (s *sink) Response(acc *chat.Accumulator) ([]byte, error) {
	ch := acc.Choices()[0]
	var tools []toolCallOut
	for i, tc := range ch.ToolCalls() {
		tools = append(tools, toToolOut(i, tc))
	}
	c := s.chunk(ch.Content(), ch.Reasoning(), tools)
	s.doneChunk(&c, acc)
	return json.Marshal(c)
}

func (s *sink) ErrorBody(e *gwerr.Error) []byte { return ErrorBody(e) }

// ---- embeddings ------------------------------------------------------------

type embedSink struct {
	alias  string
	legacy bool
	clock  clock.Clock
	start  time.Time
}

func (s *embedSink) Response(res *upstream.EmbedResult, raw []byte) ([]byte, error) {
	if raw != nil {
		if s.legacy {
			return raw, nil
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err == nil {
			m["model"], _ = json.Marshal(s.alias)
			return json.Marshal(m)
		}
	}
	if s.legacy {
		var e []float64
		if len(res.Embeddings) > 0 {
			e = res.Embeddings[0]
		}
		return json.Marshal(map[string]any{"embedding": e})
	}
	return json.Marshal(map[string]any{
		"model":             s.alias,
		"embeddings":        res.Embeddings,
		"total_duration":    s.clock.Now().Sub(s.start).Nanoseconds(),
		"load_duration":     0,
		"prompt_eval_count": res.PromptTokens,
	})
}

func (s *embedSink) ErrorBody(e *gwerr.Error) []byte { return ErrorBody(e) }
