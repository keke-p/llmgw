package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"llmgw/internal/chat"
	"llmgw/internal/store"
)

// AnthropicURL returns the Messages API URL of a provider that supports it.
// Ollama serves it at {base}/v1/messages; for OpenAI-compatible servers the
// trailing /v1 of the base URL is shared.
func AnthropicURL(p *store.Provider, path string) string {
	base := p.BaseURL
	if p.Type == store.ProviderOpenAICompatible {
		base = strings.TrimSuffix(OpenAIBase(base), "/v1")
	}
	if u, err := url.Parse(base); err == nil && strings.HasSuffix(u.Path, "/v1") {
		base = strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
	}
	return joinURL(base, "/v1/messages"+path)
}

// PassthroughAnthropic rewrites a raw Anthropic request: the model is replaced
// and streaming is forced. Unknown fields pass through unchanged.
func PassthroughAnthropic(raw map[string]json.RawMessage, model string, forceStream bool) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(raw)+1)
	for k, v := range raw {
		out[k] = v
	}
	out["model"], _ = json.Marshal(model)
	if forceStream {
		out["stream"] = json.RawMessage(`true`)
	}
	return json.Marshal(out)
}

type antUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
}

type antBlock struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Input    json.RawMessage `json:"input"`
}

type antEvent struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string    `json:"id"`
		Usage *antUsage `json:"usage"`
	} `json:"message"`
	Index        int       `json:"index"`
	ContentBlock *antBlock `json:"content_block"`
	Delta        *struct {
		Type         string  `json:"type"`
		Text         string  `json:"text"`
		Thinking     string  `json:"thinking"`
		PartialJSON  string  `json:"partial_json"`
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	Usage *antUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// AnthropicFinishReason maps an Anthropic stop_reason to the common vocabulary.
func AnthropicFinishReason(s string) string {
	switch s {
	case "max_tokens":
		return chat.FinishLength
	case "tool_use":
		return chat.FinishToolCalls
	case "refusal":
		return chat.FinishFilter
	default: // end_turn, stop_sequence, pause_turn
		return chat.FinishStop
	}
}

func argsFromInput(in json.RawMessage) string {
	in = bytes.TrimSpace(in)
	if len(in) == 0 || string(in) == "{}" || string(in) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, in) == nil {
		return buf.String()
	}
	return string(in)
}

// ParseAnthropicStream reads an Anthropic Messages SSE stream and emits common
// events. Every event keeps the raw data payload (whose "type" equals the SSE
// event name) for passthrough; upstream pings are dropped.
func ParseAnthropicStream(r io.Reader, emit func(*chat.Event) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	toolIndex := map[int]int{} // content block index -> tool ordinal
	inputTokens := 0
	finished := false
	for {
		line, err := br.ReadBytes('\n')
		if t := bytes.TrimSpace(line); bytes.HasPrefix(t, []byte("data:")) {
			payload := bytes.TrimSpace(t[len("data:"):])
			var e antEvent
			if jerr := json.Unmarshal(payload, &e); jerr != nil {
				if err != nil { // partial last line: the connection was cut
					return io.ErrUnexpectedEOF
				}
				return fmt.Errorf("anthropic: invalid stream event: %w", jerr)
			}
			raw := append([]byte(nil), payload...)
			ev := &chat.Event{Raw: raw, RawFormat: chat.FormatAnthropic}
			switch e.Type {
			case "ping":
				ev = nil
			case "error":
				msg := "unknown error"
				if e.Error != nil {
					msg = e.Error.Message
				}
				return &StreamError{Message: msg}
			case "message_start":
				if e.Message != nil {
					ev.ID = e.Message.ID
					if u := e.Message.Usage; u != nil && u.InputTokens != nil {
						inputTokens = *u.InputTokens
					}
				}
			case "content_block_start":
				if b := e.ContentBlock; b != nil {
					switch b.Type {
					case "tool_use", "server_tool_use":
						n := len(toolIndex)
						toolIndex[e.Index] = n
						ev.ToolCalls = []chat.ToolCallDelta{{Index: n, ID: b.ID, Name: b.Name, Arguments: argsFromInput(b.Input)}}
					case "text":
						ev.Content = b.Text
					case "thinking":
						ev.Reasoning = b.Thinking
					}
				}
			case "content_block_delta":
				if d := e.Delta; d != nil {
					switch d.Type {
					case "text_delta":
						ev.Content = d.Text
					case "thinking_delta":
						ev.Reasoning = d.Thinking
					case "input_json_delta":
						if n, ok := toolIndex[e.Index]; ok && d.PartialJSON != "" {
							ev.ToolCalls = []chat.ToolCallDelta{{Index: n, Arguments: d.PartialJSON}}
						}
					}
				}
			case "message_delta":
				if d := e.Delta; d != nil && d.StopReason != "" {
					ev.FinishReason = AnthropicFinishReason(d.StopReason)
					if d.StopSequence != nil {
						ev.StopSequence = *d.StopSequence
					}
					finished = true
				}
				if u := e.Usage; u != nil {
					if u.InputTokens != nil {
						inputTokens = *u.InputTokens
					}
					out := 0
					if u.OutputTokens != nil {
						out = *u.OutputTokens
					}
					ev.Usage = &chat.Usage{PromptTokens: inputTokens, CompletionTokens: out}
				}
			}
			if ev != nil {
				if eerr := emit(ev); eerr != nil {
					return eerr
				}
			}
			if e.Type == "message_stop" {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if finished {
					return nil
				}
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
}

// ParseAnthropicMessage handles servers that ignore stream=true and return one
// Messages API JSON response.
func ParseAnthropicMessage(body []byte, emit func(*chat.Event) error) error {
	var m struct {
		ID           string     `json:"id"`
		Content      []antBlock `json:"content"`
		StopReason   string     `json:"stop_reason"`
		StopSequence *string    `json:"stop_sequence"`
		Usage        antUsage   `json:"usage"`
		Error        *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("anthropic: invalid response: %w", err)
	}
	if m.Error != nil {
		return &StreamError{Message: m.Error.Message}
	}
	n := 0
	for _, b := range m.Content {
		ev := &chat.Event{ID: m.ID}
		switch b.Type {
		case "text":
			ev.Content = b.Text
		case "thinking":
			ev.Reasoning = b.Thinking
		case "tool_use":
			args := argsFromInput(b.Input)
			if args == "" {
				args = "{}"
			}
			ev.ToolCalls = []chat.ToolCallDelta{{Index: n, ID: b.ID, Name: b.Name, Arguments: args}}
			n++
		default:
			continue
		}
		if err := emit(ev); err != nil {
			return err
		}
	}
	fin := &chat.Event{ID: m.ID, FinishReason: AnthropicFinishReason(m.StopReason), Usage: &chat.Usage{}}
	if m.StopSequence != nil {
		fin.StopSequence = *m.StopSequence
	}
	if m.Usage.InputTokens != nil {
		fin.Usage.PromptTokens = *m.Usage.InputTokens
	}
	if m.Usage.OutputTokens != nil {
		fin.Usage.CompletionTokens = *m.Usage.OutputTokens
	}
	return emit(fin)
}
