package openai

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"

	"llmgw/internal/chat"
	"llmgw/internal/gwerr"
	"llmgw/internal/upstream"
)

// ---- errors ----------------------------------------------------------------

func errorType(k gwerr.Kind) string {
	switch k {
	case gwerr.InvalidRequest, gwerr.NotFound:
		return "invalid_request_error"
	case gwerr.Authentication:
		return "authentication_error"
	case gwerr.Permission:
		return "permission_error"
	case gwerr.RateLimit:
		return "rate_limit_error"
	case gwerr.Overloaded:
		return "overloaded_error"
	case gwerr.Timeout:
		return "timeout_error"
	default:
		return "api_error"
	}
}

type errorEnvelope struct {
	Error errorObject `json:"error"`
}

type errorObject struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    *string `json:"code"`
}

// ErrorBody renders {"error": {"message", "type", "code"}}.
func ErrorBody(e *gwerr.Error) []byte {
	obj := errorObject{Message: e.Message, Type: errorType(e.Kind)}
	if e.Code != "" {
		c := e.Code
		obj.Code = &c
	}
	b, _ := json.Marshal(errorEnvelope{Error: obj})
	return b
}

// WriteError writes an OpenAI-format error response.
func WriteError(w http.ResponseWriter, e *gwerr.Error) {
	e.SetRetryAfter(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(ErrorBody(e))
}

// ---- chat ------------------------------------------------------------------

// chatSink renders chat responses as OpenAI Chat Completions.
type chatSink struct {
	alias        string
	completion   bool // legacy /v1/completions (text_completion objects)
	id           string
	created      int64
	includeUsage bool
	roleSent     map[int]bool
}

func newChatSink(alias string, includeUsage bool, now int64) *chatSink {
	return &chatSink{alias: alias, id: chat.NewCompletionID(), created: now,
		includeUsage: includeUsage, roleSent: map[int]bool{}}
}

func (s *chatSink) StreamContentType() string { return "text/event-stream" }
func (s *chatSink) Keepalive() []byte         { return []byte(": keepalive\n\n") }

type chunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	SystemFingerprint *string       `json:"system_fingerprint,omitempty"`
	Choices           []chunkChoice `json:"choices"`
	Usage             *usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
	Logprobs     any     `json:"logprobs"`
}

type delta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []deltaToolCall `json:"tool_calls,omitempty"`
}

type deltaToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func toUsage(u *chat.Usage) *usage {
	if u == nil {
		return nil
	}
	return &usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
		TotalTokens: u.PromptTokens + u.CompletionTokens}
}

func writeData(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n\n")
	return err
}

func (s *chatSink) Event(w io.Writer, ev *chat.Event, passthrough bool) error {
	if passthrough {
		want := chat.FormatOpenAI
		if s.completion {
			want = chat.RawOpenAICompletion
		}
		if ev.Raw == nil || ev.RawFormat != want {
			return nil
		}
		return s.forwardRaw(w, ev.Raw)
	}
	if s.completion {
		return s.completionEvent(w, ev)
	}

	cc := chunkChoice{Index: ev.Choice}
	hasDelta := false
	if !s.roleSent[ev.Choice] && (ev.IsToken() || ev.FinishReason != "" || ev.Role != "") {
		cc.Delta.Role = "assistant"
		s.roleSent[ev.Choice] = true
		hasDelta = true
		if !ev.IsToken() {
			empty := ""
			cc.Delta.Content = &empty
		}
	}
	if ev.Content != "" {
		c := ev.Content
		cc.Delta.Content = &c
		hasDelta = true
	}
	if ev.Reasoning != "" {
		cc.Delta.ReasoningContent = ev.Reasoning
		hasDelta = true
	}
	for _, tc := range ev.ToolCalls {
		d := deltaToolCall{Index: tc.Index, ID: tc.ID}
		if tc.ID != "" {
			d.Type = "function"
		}
		d.Function.Name, d.Function.Arguments = tc.Name, tc.Arguments
		cc.Delta.ToolCalls = append(cc.Delta.ToolCalls, d)
		hasDelta = true
	}
	if ev.FinishReason != "" {
		fr := ev.FinishReason
		cc.FinishReason = &fr
	}
	if hasDelta || cc.FinishReason != nil {
		if err := writeData(w, chunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created,
			Model: s.alias, Choices: []chunkChoice{cc}}); err != nil {
			return err
		}
	}
	if ev.Usage != nil && s.includeUsage {
		return writeData(w, chunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created,
			Model: s.alias, Choices: []chunkChoice{}, Usage: toUsage(ev.Usage)})
	}
	return nil
}

// forwardRaw forwards an upstream chunk, rewriting the model name to the alias.
// The usage-only chunk (requested by the gateway, not the client) is dropped
// unless the client asked for usage.
func (s *chatSink) forwardRaw(w io.Writer, raw []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	if !s.includeUsage {
		var choices []json.RawMessage
		_ = json.Unmarshal(m["choices"], &choices)
		if len(choices) == 0 {
			if _, ok := m["usage"]; ok {
				return nil
			}
		}
		if u, ok := m["usage"]; ok && string(u) != "null" {
			delete(m, "usage")
		}
	}
	m["model"], _ = json.Marshal(s.alias)
	return writeData(w, m)
}

func (s *chatSink) StreamEnd(w io.Writer, _ *chat.Accumulator) error {
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// StreamError sends `data: {"error": ...}` without [DONE].
func (s *chatSink) StreamError(w io.Writer, e *gwerr.Error) error {
	_, err := io.WriteString(w, "data: "+string(ErrorBody(e))+"\n\n")
	return err
}

type completion struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	Created           int64              `json:"created"`
	Model             string             `json:"model"`
	Choices           []completionChoice `json:"choices"`
	Usage             *usage             `json:"usage,omitempty"`
	SystemFingerprint *string            `json:"system_fingerprint,omitempty"`
}

type completionChoice struct {
	Index        int        `json:"index"`
	Message      outMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
	Logprobs     any        `json:"logprobs"`
}

type outMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
}

func (s *chatSink) Response(acc *chat.Accumulator) ([]byte, error) {
	if s.completion {
		return s.completionResponse(acc)
	}
	resp := completion{ID: s.id, Object: "chat.completion", Created: s.created, Model: s.alias,
		Usage: toUsage(acc.Usage)}
	if resp.Usage == nil {
		resp.Usage = &usage{}
	}
	for _, c := range acc.Choices() {
		m := outMessage{Role: "assistant", ReasoningContent: c.Reasoning()}
		content := c.Content()
		tcs := c.ToolCalls()
		if content != "" || len(tcs) == 0 {
			m.Content = &content
		}
		for _, tc := range tcs {
			var o toolCall
			o.ID, o.Type = tc.ID, "function"
			if o.ID == "" {
				o.ID = chat.NewToolCallID()
			}
			o.Function.Name, o.Function.Arguments = tc.Name, tc.Arguments
			m.ToolCalls = append(m.ToolCalls, o)
		}
		fr := c.FinishReason
		if fr == "" {
			fr = chat.FinishStop
		}
		resp.Choices = append(resp.Choices, completionChoice{Index: c.Index, Message: m, FinishReason: fr})
	}
	return json.Marshal(resp)
}

func (s *chatSink) ErrorBody(e *gwerr.Error) []byte { return ErrorBody(e) }

// ---- embeddings ------------------------------------------------------------

type embedSink struct {
	alias          string
	encodingFormat string
}

type embeddingData struct {
	Object    string `json:"object"`
	Index     int    `json:"index"`
	Embedding any    `json:"embedding"`
}

func (s *embedSink) Response(res *upstream.EmbedResult, raw []byte) ([]byte, error) {
	if raw != nil {
		// Passthrough: only rewrite the model name.
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err == nil {
			m["model"], _ = json.Marshal(s.alias)
			return json.Marshal(m)
		}
	}
	data := make([]embeddingData, len(res.Embeddings))
	for i, e := range res.Embeddings {
		var v any = e
		if s.encodingFormat == "base64" {
			v = encodeFloat32Base64(e)
		}
		data[i] = embeddingData{Object: "embedding", Index: i, Embedding: v}
	}
	return json.Marshal(map[string]any{
		"object": "list",
		"data":   data,
		"model":  s.alias,
		"usage":  map[string]int{"prompt_tokens": res.PromptTokens, "total_tokens": res.PromptTokens},
	})
}

func (s *embedSink) ErrorBody(e *gwerr.Error) []byte { return ErrorBody(e) }

// encodeFloat32Base64 matches OpenAI's base64 encoding (little-endian float32).
func encodeFloat32Base64(v []float64) string {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(float32(f)))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// ---- legacy text completions ----------------------------------------------

type textChoice struct {
	Text         string  `json:"text"`
	Index        int     `json:"index"`
	Logprobs     any     `json:"logprobs"`
	FinishReason *string `json:"finish_reason"`
}

type textCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []textChoice `json:"choices"`
	Usage   *usage       `json:"usage,omitempty"`
}

func (s *chatSink) completionEvent(w io.Writer, ev *chat.Event) error {
	if ev.Content != "" || ev.FinishReason != "" {
		c := textChoice{Text: ev.Content, Index: ev.Choice}
		if ev.FinishReason != "" {
			fr := ev.FinishReason
			c.FinishReason = &fr
		}
		if err := writeData(w, textCompletion{ID: s.id, Object: "text_completion", Created: s.created,
			Model: s.alias, Choices: []textChoice{c}}); err != nil {
			return err
		}
	}
	if ev.Usage != nil && s.includeUsage {
		return writeData(w, textCompletion{ID: s.id, Object: "text_completion", Created: s.created,
			Model: s.alias, Choices: []textChoice{}, Usage: toUsage(ev.Usage)})
	}
	return nil
}

func (s *chatSink) completionResponse(acc *chat.Accumulator) ([]byte, error) {
	resp := textCompletion{ID: s.id, Object: "text_completion", Created: s.created, Model: s.alias,
		Usage: toUsage(acc.Usage)}
	if resp.Usage == nil {
		resp.Usage = &usage{}
	}
	for _, c := range acc.Choices() {
		fr := c.FinishReason
		if fr == "" {
			fr = chat.FinishStop
		}
		resp.Choices = append(resp.Choices, textChoice{Text: c.Content(), Index: c.Index, FinishReason: &fr})
	}
	return json.Marshal(resp)
}
