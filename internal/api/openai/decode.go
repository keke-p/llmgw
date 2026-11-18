// Package openai implements the OpenAI-compatible inbound API:
// /v1/chat/completions, /v1/embeddings and /v1/models.
package openai

import (
	"bytes"
	"encoding/json"
	"strings"

	"llmgw/internal/chat"
	"llmgw/internal/gwerr"
	"llmgw/internal/upstream"
)

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []message       `json:"messages"`
	Stream              bool            `json:"stream"`
	StreamOptions       *streamOptions  `json:"stream_options"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	TopK                *int            `json:"top_k"`
	Seed                *int64          `json:"seed"`
	Stop                json.RawMessage `json:"stop"`
	PresencePenalty     *float64        `json:"presence_penalty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty"`
	Tools               []tool          `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ResponseFormat      *responseFormat `json:"response_format"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	// Non-standard extensions understood by llmgw (and Ollama's /v1 API).
	Options   map[string]any  `json:"options"`
	KeepAlive json.RawMessage `json:"keep_alive"`
	Think     json.RawMessage `json:"think"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type message struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             string          `json:"name"`
	ToolCalls        []toolCall      `json:"tool_calls"`
	ToolCallID       string          `json:"tool_call_id"`
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        string          `json:"reasoning"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type responseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict"`
	} `json:"json_schema"`
}

// ParseChatRequest decodes an OpenAI Chat Completions request into the common
// format. The raw field map is returned for passthrough.
func ParseChatRequest(body []byte) (*upstream.Inbound, *gwerr.Error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, gwerr.BadRequest("invalid JSON body: %v", err)
	}
	var r chatRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, gwerr.BadRequest("invalid request: %v", err)
	}
	if len(r.Messages) == 0 {
		return nil, gwerr.BadRequest("messages is required")
	}
	req := &chat.Request{
		Model: r.Model, Stream: r.Stream,
		MaxTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP, TopK: r.TopK,
		Seed: r.Seed, PresencePenalty: r.PresencePenalty, FrequencyPenalty: r.FrequencyPenalty,
		Options: r.Options, KeepAlive: nullToNil(r.KeepAlive),
	}
	if r.MaxCompletionTokens != nil {
		req.MaxTokens = r.MaxCompletionTokens
	}
	if r.StreamOptions != nil {
		req.IncludeUsage = r.StreamOptions.IncludeUsage
	}
	stop, err := parseStop(r.Stop)
	if err != nil {
		return nil, err
	}
	req.Stop = stop

	for i, m := range r.Messages {
		cm, err := parseMessage(m)
		if err != nil {
			err.Message = "messages[" + itoa(i) + "]: " + err.Message
			return nil, err
		}
		req.Messages = append(req.Messages, cm)
	}
	for _, t := range r.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		req.Tools = append(req.Tools, chat.Tool{Name: t.Function.Name,
			Description: t.Function.Description, Parameters: t.Function.Parameters})
	}
	if tc, err := parseToolChoice(r.ToolChoice); err != nil {
		return nil, err
	} else {
		req.ToolChoice = tc
	}
	if rf := r.ResponseFormat; rf != nil {
		crf := &chat.ResponseFormat{Type: rf.Type}
		if rf.JSONSchema != nil {
			crf.Name, crf.Schema, crf.Strict = rf.JSONSchema.Name, rf.JSONSchema.Schema, rf.JSONSchema.Strict
		}
		req.ResponseFormat = crf
	}
	switch e := r.ReasoningEffort; e {
	case "":
	case "none":
		req.Think = &chat.Think{Enabled: false}
	case "minimal":
		req.Think = &chat.Think{Enabled: true, Effort: "low"}
	default:
		req.Think = &chat.Think{Enabled: true, Effort: e}
	}
	if t := nullToNil(r.Think); t != nil {
		var b bool
		var s string
		switch {
		case json.Unmarshal(t, &b) == nil:
			req.Think = &chat.Think{Enabled: b}
		case json.Unmarshal(t, &s) == nil:
			req.Think = &chat.Think{Enabled: true, Effort: s}
		}
	}
	return &upstream.Inbound{Format: chat.FormatOpenAI, Req: req, Raw: raw}, nil
}

func parseMessage(m message) (chat.Message, *gwerr.Error) {
	role := m.Role
	switch role {
	case "developer":
		role = "system"
	case "system", "user", "assistant", "tool":
	case "function":
		role = "tool"
	default:
		return chat.Message{}, gwerr.BadRequest("unsupported role %q", m.Role)
	}
	cm := chat.Message{Role: role, Name: m.Name, ToolCallID: m.ToolCallID, Reasoning: m.ReasoningContent}
	if cm.Reasoning == "" {
		cm.Reasoning = m.Reasoning
	}
	content := nullToNil(m.Content)
	switch {
	case content == nil:
	case content[0] == '"':
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return cm, gwerr.BadRequest("invalid content")
		}
		cm.Content = []chat.Part{chat.TextPart(s)}
	case content[0] == '[':
		var parts []contentPart
		if err := json.Unmarshal(content, &parts); err != nil {
			return cm, gwerr.BadRequest("invalid content parts: %v", err)
		}
		for _, p := range parts {
			switch p.Type {
			case "text", "input_text":
				cm.Content = append(cm.Content, chat.TextPart(p.Text))
			case "image_url":
				if p.ImageURL == nil || p.ImageURL.URL == "" {
					return cm, gwerr.BadRequest("image_url.url is required")
				}
				img, err := ParseImageURL(p.ImageURL.URL)
				if err != nil {
					return cm, err
				}
				cm.Content = append(cm.Content, chat.Part{Type: "image", Image: img})
			default:
				// Unsupported part types (audio, file, ...) are dropped.
			}
		}
	default:
		return cm, gwerr.BadRequest("content must be a string or an array")
	}
	for _, tc := range m.ToolCalls {
		cm.ToolCalls = append(cm.ToolCalls, chat.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return cm, nil
}

// ParseImageURL handles data: URLs (inline base64) and http(s) URLs.
func ParseImageURL(u string) (*chat.Image, *gwerr.Error) {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		if !found || !strings.HasSuffix(meta, ";base64") {
			return nil, gwerr.BadRequest("image data URL must be base64 encoded")
		}
		return &chat.Image{Data: data, MediaType: strings.TrimSuffix(meta, ";base64")}, nil
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return &chat.Image{URL: u}, nil
	}
	return nil, gwerr.BadRequest("unsupported image URL")
}

func parseStop(raw json.RawMessage) ([]string, *gwerr.Error) {
	raw = nullToNil(raw)
	if raw == nil {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, gwerr.BadRequest("stop must be a string or an array of strings")
	}
	return arr, nil
}

func parseToolChoice(raw json.RawMessage) (*chat.ToolChoice, *gwerr.Error) {
	raw = nullToNil(raw)
	if raw == nil {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return &chat.ToolChoice{Mode: s}, nil
	}
	var o struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, gwerr.BadRequest("invalid tool_choice")
	}
	return &chat.ToolChoice{Mode: "function", Name: o.Function.Name}, nil
}

func nullToNil(r json.RawMessage) json.RawMessage {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || string(r) == "null" {
		return nil
	}
	return r
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// ---- embeddings ------------------------------------------------------------

type embedRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format"`
	Dimensions     *int            `json:"dimensions"`
}

// ParseEmbedRequest decodes an OpenAI embeddings request. Token-array inputs are
// kept in raw form (usable for passthrough) but cannot be converted.
func ParseEmbedRequest(body []byte) (*upstream.EmbedInbound, string, *gwerr.Error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", gwerr.BadRequest("invalid JSON body: %v", err)
	}
	var r embedRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, "", gwerr.BadRequest("invalid request: %v", err)
	}
	in := nullToNil(r.Input)
	if in == nil {
		return nil, "", gwerr.BadRequest("input is required")
	}
	req := &upstream.EmbedRequest{Model: r.Model, Dimensions: r.Dimensions}
	var s string
	var arr []string
	switch {
	case json.Unmarshal(in, &s) == nil:
		req.Input = []string{s}
	case json.Unmarshal(in, &arr) == nil:
		req.Input = arr
	default:
		// token arrays: only usable via passthrough
		req.Input = nil
	}
	switch r.EncodingFormat {
	case "", "float", "base64":
	default:
		return nil, "", gwerr.BadRequest("unsupported encoding_format %q", r.EncodingFormat)
	}
	return &upstream.EmbedInbound{Format: chat.FormatOpenAI, Req: req, Raw: raw}, r.EncodingFormat, nil
}

// ---- legacy completions ----------------------------------------------------

type completionRequest struct {
	Model            string          `json:"model"`
	Prompt           json.RawMessage `json:"prompt"`
	Suffix           string          `json:"suffix"`
	Stream           bool            `json:"stream"`
	StreamOptions    *streamOptions  `json:"stream_options"`
	MaxTokens        *int            `json:"max_tokens"`
	Temperature      *float64        `json:"temperature"`
	TopP             *float64        `json:"top_p"`
	TopK             *int            `json:"top_k"`
	Seed             *int64          `json:"seed"`
	Stop             json.RawMessage `json:"stop"`
	PresencePenalty  *float64        `json:"presence_penalty"`
	FrequencyPenalty *float64        `json:"frequency_penalty"`
	Echo             bool            `json:"echo"`
	N                int             `json:"n"`
	BestOf           int             `json:"best_of"`
	Logprobs         *int            `json:"logprobs"`
}

// ParseCompletionRequest decodes a legacy /v1/completions request. Prompt
// arrays, echo, n/best_of and logprobs are only honoured via passthrough to an
// OpenAI-compatible upstream.
func ParseCompletionRequest(body []byte) (*upstream.Inbound, *gwerr.Error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, gwerr.BadRequest("invalid JSON body: %v", err)
	}
	var r completionRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, gwerr.BadRequest("invalid request: %v", err)
	}
	req := &chat.Request{
		Model: r.Model, Stream: r.Stream, Suffix: r.Suffix,
		MaxTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP, TopK: r.TopK, Seed: r.Seed,
		PresencePenalty: r.PresencePenalty, FrequencyPenalty: r.FrequencyPenalty,
	}
	if r.StreamOptions != nil {
		req.IncludeUsage = r.StreamOptions.IncludeUsage
	}
	stop, ge := parseStop(r.Stop)
	if ge != nil {
		return nil, ge
	}
	req.Stop = stop
	p := nullToNil(r.Prompt)
	var s string
	var arr []string
	switch {
	case p == nil:
	case json.Unmarshal(p, &s) == nil:
		req.Prompt = s
	case json.Unmarshal(p, &arr) == nil && len(arr) == 1:
		req.Prompt = arr[0]
	default:
		req.Unconvertible = append(req.Unconvertible, "prompt (array / tokens)")
	}
	if r.Echo {
		req.Unconvertible = append(req.Unconvertible, "echo")
	}
	if r.N > 1 || r.BestOf > 1 {
		req.Unconvertible = append(req.Unconvertible, "n/best_of")
	}
	if r.Logprobs != nil {
		req.Unconvertible = append(req.Unconvertible, "logprobs")
	}
	return &upstream.Inbound{Format: chat.FormatOpenAI, Kind: upstream.KindCompletion, Req: req, Raw: raw}, nil
}
