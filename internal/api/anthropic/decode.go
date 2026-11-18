// Package anthropic implements the Anthropic-compatible inbound API
// (/v1/messages, /v1/messages/count_tokens), so Anthropic SDK clients such as
// Claude Code can use local models via ANTHROPIC_BASE_URL.
package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"

	"llmgw/internal/chat"
	"llmgw/internal/gwerr"
	"llmgw/internal/upstream"
)

type messagesRequest struct {
	Model         string          `json:"model"`
	MaxTokens     *int            `json:"max_tokens"`
	Messages      []message       `json:"messages"`
	System        json.RawMessage `json:"system"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	Tools         []tool          `json:"tools"`
	ToolChoice    *toolChoice     `json:"tool_choice"`
	Thinking      *thinking       `json:"thinking"`
	// metadata, cache_control, service_tier, context_management, ... are ignored.
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Source    *imageSource    `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result: string or blocks
	IsError   bool            `json:"is_error"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
}

type imageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type toolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name"`
}

type thinking struct {
	Type         string `json:"type"` // enabled | disabled
	BudgetTokens int    `json:"budget_tokens"`
}

// ParseMessages decodes a Messages API request into the common format.
func ParseMessages(body []byte) (*upstream.Inbound, *gwerr.Error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, gwerr.BadRequest("invalid JSON body: %v", err)
	}
	var r messagesRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, gwerr.BadRequest("invalid request: %v", err)
	}
	if len(r.Messages) == 0 {
		return nil, gwerr.BadRequest("messages: at least one message is required")
	}
	req := &chat.Request{
		Model: r.Model, Stream: r.Stream, MaxTokens: r.MaxTokens, Temperature: r.Temperature,
		TopP: r.TopP, TopK: r.TopK, Stop: r.StopSequences, IncludeUsage: true,
	}
	sys, ge := parseSystem(r.System)
	if ge != nil {
		return nil, ge
	}
	if sys != "" {
		req.Messages = append(req.Messages, chat.Message{Role: "system", Content: []chat.Part{chat.TextPart(sys)}})
	}
	for i, m := range r.Messages {
		msgs, ge := parseMessage(m)
		if ge != nil {
			ge.Message = "messages." + itoa(i) + ": " + ge.Message
			return nil, ge
		}
		req.Messages = append(req.Messages, msgs...)
	}
	for _, t := range r.Tools {
		// Server tools (web_search, code_execution, ...) cannot run locally.
		if t.Type != "" && t.Type != "custom" {
			continue
		}
		req.Tools = append(req.Tools, chat.Tool{Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	if tc := r.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto":
			req.ToolChoice = &chat.ToolChoice{Mode: "auto"}
		case "any":
			req.ToolChoice = &chat.ToolChoice{Mode: "required"}
		case "tool":
			req.ToolChoice = &chat.ToolChoice{Mode: "function", Name: tc.Name}
		case "none":
			req.ToolChoice = &chat.ToolChoice{Mode: "none"}
		}
	}
	if th := r.Thinking; th != nil {
		switch th.Type {
		case "enabled", "adaptive":
			req.Think = &chat.Think{Enabled: true, BudgetTokens: th.BudgetTokens}
		case "disabled":
			req.Think = &chat.Think{Enabled: false}
		}
	} else {
		// Anthropic semantics: no "thinking" means extended thinking is off.
		req.Think = &chat.Think{Enabled: false, Implicit: true}
	}
	return &upstream.Inbound{Format: chat.FormatAnthropic, Req: req, Raw: raw}, nil
}

func parseSystem(raw json.RawMessage) (string, *gwerr.Error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", gwerr.BadRequest("system must be a string or an array of text blocks")
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func parseBlocks(raw json.RawMessage) ([]block, *gwerr.Error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{{Type: "text", Text: s}}, nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, gwerr.BadRequest("content must be a string or an array of content blocks")
	}
	return blocks, nil
}

func imagePart(src *imageSource) (chat.Part, *gwerr.Error) {
	if src == nil {
		return chat.Part{}, gwerr.BadRequest("image block requires source")
	}
	switch src.Type {
	case "base64":
		return chat.Part{Type: "image", Image: &chat.Image{Data: src.Data, MediaType: src.MediaType}}, nil
	case "url":
		return chat.Part{Type: "image", Image: &chat.Image{URL: src.URL}}, nil
	}
	return chat.Part{}, gwerr.BadRequest("unsupported image source type %q", src.Type)
}

// parseMessage converts one Anthropic message to common messages. A user turn
// carrying tool_result blocks becomes tool messages (first, so they directly
// follow the assistant's tool calls) plus a user message for the rest.
func parseMessage(m message) ([]chat.Message, *gwerr.Error) {
	switch m.Role {
	case "user", "assistant":
	case "system":
		// Newer clients (Claude Code) send system messages inside "messages".
		blocks, ge := parseBlocks(m.Content)
		if ge != nil {
			return nil, ge
		}
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		return []chat.Message{{Role: "system", Content: []chat.Part{chat.TextPart(strings.Join(texts, "\n\n"))}}}, nil
	default:
		return nil, gwerr.BadRequest("role must be user, assistant or system, got %q", m.Role)
	}
	blocks, ge := parseBlocks(m.Content)
	if ge != nil {
		return nil, ge
	}
	main := chat.Message{Role: m.Role}
	var tools []chat.Message
	var extra []chat.Part // images returned inside tool results
	for _, b := range blocks {
		switch b.Type {
		case "text":
			main.Content = append(main.Content, chat.TextPart(b.Text))
		case "image":
			p, ge := imagePart(b.Source)
			if ge != nil {
				return nil, ge
			}
			main.Content = append(main.Content, p)
		case "tool_use", "server_tool_use":
			args := "{}"
			if in := bytes.TrimSpace(b.Input); len(in) > 0 && string(in) != "null" {
				var buf bytes.Buffer
				if json.Compact(&buf, in) == nil {
					args = buf.String()
				}
			}
			main.ToolCalls = append(main.ToolCalls, chat.ToolCall{ID: b.ID, Name: b.Name, Arguments: args})
		case "tool_result":
			inner, ge := parseBlocks(b.Content)
			if ge != nil {
				return nil, ge
			}
			var texts []string
			for _, ib := range inner {
				switch ib.Type {
				case "text":
					texts = append(texts, ib.Text)
				case "image":
					if p, ge := imagePart(ib.Source); ge == nil {
						extra = append(extra, p)
					}
				}
			}
			text := strings.Join(texts, "\n")
			if b.IsError && !strings.HasPrefix(text, "Error") {
				text = "Error: " + text
			}
			tools = append(tools, chat.Message{Role: "tool", ToolCallID: b.ToolUseID, Content: []chat.Part{chat.TextPart(text)}})
		case "thinking":
			main.Reasoning += b.Thinking
			main.ReasoningSignature = b.Signature
		default:
			// redacted_thinking, document, search_result, ... are dropped.
		}
	}
	out := tools
	main.Content = append(main.Content, extra...)
	if len(main.Content) > 0 || len(main.ToolCalls) > 0 || main.Reasoning != "" || len(tools) == 0 {
		out = append(out, main)
	}
	return out, nil
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// ---- token estimation ------------------------------------------------------

// EstimateTokens approximates the input token count of a Messages request when
// the upstream cannot count it: ~4 ASCII characters per token, ~1 token per
// non-ASCII character (CJK), plus a small per-message overhead.
func EstimateTokens(in *upstream.Inbound, rawTools json.RawMessage) int {
	n := 0
	count := func(s string) {
		ascii := 0
		for _, r := range s {
			if r < 128 {
				ascii++
			} else {
				n++
			}
		}
		n += (ascii + 3) / 4
	}
	for _, m := range in.Req.Messages {
		n += 4
		count(m.Text())
		count(m.Reasoning)
		for _, tc := range m.ToolCalls {
			count(tc.Name)
			count(tc.Arguments)
		}
		n += 800 * len(m.Images()) // rough per-image cost
	}
	count(string(rawTools))
	if n < 1 {
		n = 1
	}
	return n
}
