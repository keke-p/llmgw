package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"llmgw/internal/chat"
)

// ---- request (conversion) --------------------------------------------------

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

func toOATool(t chat.Tool) oaTool {
	var o oaTool
	o.Type = "function"
	o.Function.Name = t.Name
	o.Function.Description = t.Description
	o.Function.Parameters = t.Parameters
	return o
}

type oaOutMessage struct {
	Role       string          `json:"role"`
	Content    any             `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []oaOutToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type oaOutToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// passthroughOptions are Ollama option names that OpenAI-compatible local servers
// (llama.cpp, vLLM, LM Studio) accept as top-level sampling parameters.
var passthroughOptions = map[string]string{
	"temperature": "temperature", "top_p": "top_p", "top_k": "top_k", "min_p": "min_p",
	"seed": "seed", "stop": "stop", "num_predict": "max_tokens",
	"repeat_penalty": "repeat_penalty", "presence_penalty": "presence_penalty",
	"frequency_penalty": "frequency_penalty",
}

// BuildOpenAIChat converts a common request to an OpenAI Chat Completions body
// (always streaming, with usage).
func BuildOpenAIChat(req *chat.Request, model string) ([]byte, error) {
	body := map[string]any{
		"model":          model,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	// Ollama options first so explicit fields win.
	for k, v := range req.Options {
		if name, ok := passthroughOptions[k]; ok {
			body[name] = v
		}
	}
	msgs := make([]oaOutMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := oaOutMessage{Role: m.Role, ToolCallID: m.ToolCallID}
		if m.Role != "tool" {
			om.Name = m.Name
		}
		imgs := m.Images()
		if len(imgs) == 0 {
			om.Content = m.Text()
			if m.Role == "assistant" && len(m.ToolCalls) > 0 && om.Content == "" {
				om.Content = nil
			}
		} else {
			var parts []oaContentPart
			for _, p := range m.Content {
				switch p.Type {
				case "text":
					parts = append(parts, oaContentPart{Type: "text", Text: p.Text})
				case "image":
					cp := oaContentPart{Type: "image_url"}
					cp.ImageURL = &struct {
						URL string `json:"url"`
					}{URL: imageURL(p.Image)}
					parts = append(parts, cp)
				}
			}
			om.Content = parts
		}
		for _, tc := range m.ToolCalls {
			var o oaOutToolCall
			o.ID, o.Type = tc.ID, "function"
			o.Function.Name, o.Function.Arguments = tc.Name, tc.Arguments
			om.ToolCalls = append(om.ToolCalls, o)
		}
		msgs = append(msgs, om)
	}
	body["messages"] = msgs

	set := func(k string, v any, ok bool) {
		if ok {
			body[k] = v
		}
	}
	set("max_tokens", deref(req.MaxTokens), req.MaxTokens != nil)
	set("temperature", deref(req.Temperature), req.Temperature != nil)
	set("top_p", deref(req.TopP), req.TopP != nil)
	set("top_k", deref(req.TopK), req.TopK != nil)
	set("seed", deref(req.Seed), req.Seed != nil)
	set("presence_penalty", deref(req.PresencePenalty), req.PresencePenalty != nil)
	set("frequency_penalty", deref(req.FrequencyPenalty), req.FrequencyPenalty != nil)
	set("stop", req.Stop, len(req.Stop) > 0)

	if len(req.Tools) > 0 {
		tools := make([]oaTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, toOATool(t))
		}
		body["tools"] = tools
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Mode {
		case "auto", "none", "required":
			body["tool_choice"] = tc.Mode
		case "function":
			body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
		}
	}
	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			body["response_format"] = map[string]any{"type": "json_object"}
		case "json_schema":
			js := map[string]any{"name": rf.Name, "schema": rf.Schema}
			if js["name"] == "" {
				js["name"] = "response"
			}
			if rf.Strict != nil {
				js["strict"] = *rf.Strict
			}
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": js}
		}
	} else if len(req.Format) > 0 {
		// Ollama "format": "json" or a schema.
		if string(req.Format) == `"json"` {
			body["response_format"] = map[string]any{"type": "json_object"}
		} else if bytes.HasPrefix(bytes.TrimSpace(req.Format), []byte("{")) {
			body["response_format"] = map[string]any{"type": "json_schema",
				"json_schema": map[string]any{"name": "response", "schema": req.Format}}
		}
	}
	if req.Think != nil && req.Think.Effort != "" {
		body["reasoning_effort"] = req.Think.Effort
	} else if req.Think != nil && !req.Think.Enabled && !req.Think.Implicit {
		body["reasoning_effort"] = "none"
	}
	return json.Marshal(body)
}

func imageURL(img *chat.Image) string {
	if img.Data != "" {
		mt := img.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + img.Data
	}
	return img.URL
}

// PassthroughOpenAIChat rewrites a raw OpenAI request for upstream: the model
// name is replaced and streaming with usage is forced. Unknown fields pass
// through unchanged.
func PassthroughOpenAIChat(raw map[string]json.RawMessage, model string) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(raw)+2)
	for k, v := range raw {
		out[k] = v
	}
	out["model"], _ = json.Marshal(model)
	out["stream"] = json.RawMessage(`true`)
	so := map[string]json.RawMessage{}
	if v, ok := raw["stream_options"]; ok {
		_ = json.Unmarshal(v, &so)
		if so == nil {
			so = map[string]json.RawMessage{}
		}
	}
	so["include_usage"] = json.RawMessage(`true`)
	out["stream_options"], _ = json.Marshal(so)
	return json.Marshal(out)
}

// ---- stream ----------------------------------------------------------------

type oaChunk struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int       `json:"index"`
		Delta oaMessage `json:"delta"`
		// Message is used by non-streaming responses.
		Message      *oaMessage `json:"message"`
		FinishReason *string    `json:"finish_reason"`
		StopReason   any        `json:"stop_reason"` // vLLM: matched stop string
		Text         string     `json:"text"`        // /v1/completions
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type oaMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`
	ToolCalls        []struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func (c *oaChunk) events(raw []byte, stream bool, rawFmt chat.Format) []*chat.Event {
	var evs []*chat.Event
	for i, ch := range c.Choices {
		m := ch.Delta
		if !stream && ch.Message != nil {
			m = *ch.Message
		}
		ev := &chat.Event{Choice: ch.Index, ID: c.ID, Created: c.Created, Role: m.Role,
			Content: m.Content, Reasoning: m.ReasoningContent}
		if ch.Text != "" {
			ev.Content = ch.Text
		}
		if ev.Reasoning == "" {
			ev.Reasoning = m.Reasoning
		}
		for j, tc := range m.ToolCalls {
			idx := j
			if tc.Index != nil {
				idx = *tc.Index
			}
			ev.ToolCalls = append(ev.ToolCalls, chat.ToolCallDelta{Index: idx, ID: tc.ID,
				Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			ev.FinishReason = *ch.FinishReason
			if s, ok := ch.StopReason.(string); ok {
				ev.StopSequence = s
			}
		}
		if i == 0 && raw != nil {
			ev.Raw, ev.RawFormat = raw, rawFmt
		}
		evs = append(evs, ev)
	}
	if c.Usage != nil {
		u := &chat.Usage{PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens}
		if len(evs) == 0 {
			ev := &chat.Event{ID: c.ID, Created: c.Created}
			if raw != nil {
				ev.Raw, ev.RawFormat = raw, rawFmt
			}
			evs = append(evs, ev)
		}
		evs[len(evs)-1].Usage = u
	}
	if len(evs) == 0 && raw != nil {
		evs = append(evs, &chat.Event{ID: c.ID, Created: c.Created, Raw: raw, RawFormat: rawFmt})
	}
	return evs
}

// ParseOpenAIChatStream reads an OpenAI-compatible SSE stream and emits common
// events. Each event keeps the raw `data:` payload for passthrough.
func ParseOpenAIChatStream(r io.Reader, emit func(*chat.Event) error) error {
	return parseOpenAIStream(r, emit, chat.FormatOpenAI)
}

// ParseOpenAICompletionStream reads a legacy /v1/completions SSE stream.
func ParseOpenAICompletionStream(r io.Reader, emit func(*chat.Event) error) error {
	return parseOpenAIStream(r, emit, chat.RawOpenAICompletion)
}

func parseOpenAIStream(r io.Reader, emit func(*chat.Event) error, rawFmt chat.Format) error {
	br := bufio.NewReaderSize(r, 64*1024)
	finished := false
	for {
		line, err := br.ReadBytes('\n')
		if t := bytes.TrimSpace(line); bytes.HasPrefix(t, []byte("data:")) {
			payload := bytes.TrimSpace(t[len("data:"):])
			if string(payload) == "[DONE]" {
				return nil
			}
			var c oaChunk
			if jerr := json.Unmarshal(payload, &c); jerr != nil {
				if err != nil { // partial last line: the connection was cut
					return io.ErrUnexpectedEOF
				}
				return fmt.Errorf("openai: invalid stream chunk: %w", jerr)
			}
			if len(c.Error) > 0 && string(c.Error) != "null" {
				return &StreamError{Message: errorMessage(c.Error)}
			}
			for _, ev := range c.events(append([]byte(nil), payload...), true, rawFmt) {
				if ev.FinishReason != "" {
					finished = true
				}
				if eerr := emit(ev); eerr != nil {
					return eerr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Some servers end the stream without [DONE]; accept it if the
				// generation finished, otherwise the connection was cut.
				if finished {
					return nil
				}
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
}

// ParseOpenAIChatCompletion handles servers that ignore stream=true and return a
// single JSON response.
func ParseOpenAIChatCompletion(body []byte, emit func(*chat.Event) error) error {
	var c oaChunk
	if err := json.Unmarshal(body, &c); err != nil {
		return fmt.Errorf("openai: invalid response: %w", err)
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		return &StreamError{Message: errorMessage(c.Error)}
	}
	for _, ev := range c.events(nil, false, "") {
		if err := emit(ev); err != nil {
			return err
		}
	}
	return nil
}

// errorMessage extracts a message from {"message": ...} or a plain string.
func errorMessage(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &o) == nil && o.Message != "" {
		return o.Message
	}
	return strings.TrimSpace(string(raw))
}

// ---- embeddings ------------------------------------------------------------

// BuildOpenAIEmbed builds an OpenAI /v1/embeddings body (conversion path).
func BuildOpenAIEmbed(req *EmbedRequest, model string) ([]byte, error) {
	body := map[string]any{"model": model, "input": req.Input, "encoding_format": "float"}
	if req.Dimensions != nil {
		body["dimensions"] = *req.Dimensions
	}
	return json.Marshal(body)
}

// PassthroughOpenAIEmbed rewrites the model of a raw embeddings request.
func PassthroughOpenAIEmbed(raw map[string]json.RawMessage, model string) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	out["model"], _ = json.Marshal(model)
	return json.Marshal(out)
}

// ParseOpenAIEmbed parses an OpenAI embeddings response (float encoding).
func ParseOpenAIEmbed(body []byte) (*EmbedResult, error) {
	var r struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("openai: invalid embeddings response: %w", err)
	}
	out := &EmbedResult{Embeddings: make([][]float64, len(r.Data)), PromptTokens: r.Usage.PromptTokens}
	for i, d := range r.Data {
		idx := d.Index
		if idx < 0 || idx >= len(out.Embeddings) {
			idx = i
		}
		out.Embeddings[idx] = d.Embedding
	}
	return out, nil
}
