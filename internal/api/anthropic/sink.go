package anthropic

import (
	"encoding/json"
	"io"
	"net/http"

	"llmgw/internal/chat"
	"llmgw/internal/gwerr"
)

// errorType maps gateway error kinds to Anthropic error types.
func errorType(e *gwerr.Error) string {
	switch e.Kind {
	case gwerr.InvalidRequest:
		return "invalid_request_error"
	case gwerr.Authentication:
		return "authentication_error"
	case gwerr.Permission:
		return "permission_error"
	case gwerr.NotFound:
		return "not_found_error"
	case gwerr.RateLimit:
		return "rate_limit_error"
	case gwerr.Overloaded:
		return "overloaded_error"
	default: // api_error, timeout
		return "api_error"
	}
}

// ErrorBody renders {"type":"error","error":{"type","message"}}.
func ErrorBody(e *gwerr.Error) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errorType(e), "message": e.Message},
	})
	return b
}

// WriteError writes an Anthropic-format error response.
func WriteError(w http.ResponseWriter, e *gwerr.Error) {
	e.SetRetryAfter(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(ErrorBody(e))
}

// stopReason maps a common finish reason to an Anthropic stop_reason.
func stopReason(c *chat.Choice) (string, *string) {
	if c.StopSequence != "" {
		s := c.StopSequence
		return "stop_sequence", &s
	}
	switch c.FinishReason {
	case chat.FinishLength:
		return "max_tokens", nil
	case chat.FinishToolCalls:
		return "tool_use", nil
	case chat.FinishFilter:
		return "refusal", nil
	}
	if len(c.ToolCalls()) > 0 {
		return "tool_use", nil
	}
	return "end_turn", nil
}

// sink renders responses as Anthropic Messages (SSE events or JSON).
type sink struct {
	alias    string
	id       string
	thinking bool // emit thinking blocks (only when the request enabled thinking)

	started     bool
	passthrough bool
	blockIndex  int    // index of the open block (-1 = none)
	blockType   string // type of the open block
	toolOpen    int    // tool ordinal of the open tool_use block
	nextIndex   int
}

func newSink(alias string, thinking bool) *sink {
	return &sink{alias: alias, id: "msg_" + chat.RandomString(24), thinking: thinking, blockIndex: -1, toolOpen: -1}
}

func (s *sink) StreamContentType() string { return "text/event-stream" }
func (s *sink) Keepalive() []byte         { return []byte("event: ping\ndata: {\"type\": \"ping\"}\n\n") }

func writeEvent(w io.Writer, typ string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "event: "+typ+"\ndata: "+string(b)+"\n\n")
	return err
}

type usageOut struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (s *sink) start(w io.Writer) error {
	if s.started {
		return nil
	}
	s.started = true
	return writeEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.id, "type": "message", "role": "assistant", "model": s.alias,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": usageOut{},
		},
	})
}

func (s *sink) closeBlock(w io.Writer) error {
	if s.blockIndex < 0 {
		return nil
	}
	idx := s.blockIndex
	s.blockIndex, s.blockType, s.toolOpen = -1, "", -1
	return writeEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (s *sink) openBlock(w io.Writer, typ string, cb map[string]any) error {
	if err := s.closeBlock(w); err != nil {
		return err
	}
	s.blockIndex, s.blockType = s.nextIndex, typ
	s.nextIndex++
	return writeEvent(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": s.blockIndex, "content_block": cb})
}

func (s *sink) delta(w io.Writer, d map[string]any) error {
	return writeEvent(w, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.blockIndex, "delta": d})
}

func (s *sink) Event(w io.Writer, ev *chat.Event, passthrough bool) error {
	s.passthrough = passthrough
	if passthrough {
		if ev.Raw == nil || ev.RawFormat != chat.FormatAnthropic {
			return nil
		}
		return s.forwardRaw(w, ev.Raw)
	}
	if ev.Choice != 0 {
		return nil
	}
	if !ev.IsToken() {
		return nil // finish/usage are rendered in StreamEnd
	}
	if err := s.start(w); err != nil {
		return err
	}
	if ev.Reasoning != "" && s.thinking {
		if s.blockType != "thinking" {
			if err := s.openBlock(w, "thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""}); err != nil {
				return err
			}
		}
		if err := s.delta(w, map[string]any{"type": "thinking_delta", "thinking": ev.Reasoning}); err != nil {
			return err
		}
	}
	if ev.Content != "" {
		if s.blockType != "text" {
			if err := s.openBlock(w, "text", map[string]any{"type": "text", "text": ""}); err != nil {
				return err
			}
		}
		if err := s.delta(w, map[string]any{"type": "text_delta", "text": ev.Content}); err != nil {
			return err
		}
	}
	for _, tc := range ev.ToolCalls {
		if s.blockType != "tool_use" || s.toolOpen != tc.Index {
			id := tc.ID
			if id == "" {
				id = "toolu_" + chat.RandomString(24)
			}
			if err := s.openBlock(w, "tool_use", map[string]any{"type": "tool_use", "id": id, "name": tc.Name, "input": map[string]any{}}); err != nil {
				return err
			}
			s.toolOpen = tc.Index
		}
		if tc.Arguments != "" {
			if err := s.delta(w, map[string]any{"type": "input_json_delta", "partial_json": tc.Arguments}); err != nil {
				return err
			}
		}
	}
	return nil
}

// forwardRaw forwards an upstream Anthropic event, rewriting the model name.
func (s *sink) forwardRaw(w io.Writer, raw []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	var typ string
	_ = json.Unmarshal(m["type"], &typ)
	if typ == "message_start" {
		var msg map[string]json.RawMessage
		if json.Unmarshal(m["message"], &msg) == nil {
			msg["model"], _ = json.Marshal(s.alias)
			m["message"], _ = json.Marshal(msg)
		}
	}
	b, _ := json.Marshal(m)
	_, err := io.WriteString(w, "event: "+typ+"\ndata: "+string(b)+"\n\n")
	return err
}

func usageOf(acc *chat.Accumulator) usageOut {
	if acc.Usage == nil {
		return usageOut{}
	}
	return usageOut{InputTokens: acc.Usage.PromptTokens, OutputTokens: acc.Usage.CompletionTokens}
}

func (s *sink) StreamEnd(w io.Writer, acc *chat.Accumulator) error {
	if s.passthrough {
		return nil // upstream's message_delta / message_stop were forwarded
	}
	if err := s.start(w); err != nil {
		return err
	}
	if err := s.closeBlock(w); err != nil {
		return err
	}
	reason, seq := stopReason(acc.Choices()[0])
	if err := writeEvent(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": seq},
		"usage": usageOf(acc),
	}); err != nil {
		return err
	}
	return writeEvent(w, "message_stop", map[string]any{"type": "message_stop"})
}

// StreamError sends an "error" event.
func (s *sink) StreamError(w io.Writer, e *gwerr.Error) error {
	_, err := io.WriteString(w, "event: error\ndata: "+string(ErrorBody(e))+"\n\n")
	return err
}

func (s *sink) Response(acc *chat.Accumulator) ([]byte, error) {
	c := acc.Choices()[0]
	content := []map[string]any{}
	if r := c.Reasoning(); r != "" && s.thinking {
		content = append(content, map[string]any{"type": "thinking", "thinking": r, "signature": ""})
	}
	if t := c.Content(); t != "" {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	for _, tc := range c.ToolCalls() {
		id := tc.ID
		if id == "" {
			id = "toolu_" + chat.RandomString(24)
		}
		var input any = map[string]any{}
		if tc.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
				input = map[string]any{"input": tc.Arguments}
			}
		}
		content = append(content, map[string]any{"type": "tool_use", "id": id, "name": tc.Name, "input": input})
	}
	reason, seq := stopReason(c)
	return json.Marshal(map[string]any{
		"id": s.id, "type": "message", "role": "assistant", "model": s.alias,
		"content": content, "stop_reason": reason, "stop_sequence": seq, "usage": usageOf(acc),
	})
}

func (s *sink) ErrorBody(e *gwerr.Error) []byte { return ErrorBody(e) }
