// Package chat defines llmgw's internal common format.
//
// The schema is modelled on OpenAI Chat Completions. Information that has no
// OpenAI equivalent (Ollama options, keep_alive, Anthropic thinking, ...) is kept
// in extension fields so that conversions lose as little as possible.
//
// Everything in this package is pure (no HTTP), so conversions can be tested with
// table-driven tests.
package chat

import (
	"encoding/json"
	"strings"
)

// Format identifies a wire API format.
type Format string

const (
	FormatOpenAI    Format = "openai"
	FormatOllama    Format = "ollama"
	FormatAnthropic Format = "anthropic"

	// Raw chunk formats (Event.RawFormat) for non-chat upstream endpoints.
	RawOllamaGenerate   Format = "ollama_generate"   // /api/generate NDJSON line
	RawOpenAICompletion Format = "openai_completion" // /v1/completions SSE payload
)

// Request is a chat request in the common format.
type Request struct {
	Model    string
	Messages []Message
	// Stream is what the *client* asked for. Upstream is always streamed.
	Stream bool
	// IncludeUsage mirrors OpenAI stream_options.include_usage.
	IncludeUsage bool

	MaxTokens        *int
	Temperature      *float64
	TopP             *float64
	TopK             *int
	Seed             *int64
	Stop             []string
	PresencePenalty  *float64
	FrequencyPenalty *float64

	Tools          []Tool
	ToolChoice     *ToolChoice
	ResponseFormat *ResponseFormat

	// Extensions.
	Think     *Think
	Options   map[string]any  // Ollama-native options (num_ctx, repeat_penalty, ...)
	KeepAlive json.RawMessage // Ollama keep_alive (string or number)
	Format    json.RawMessage // Ollama format ("json" or a JSON schema)

	// Prompt-style requests (/api/generate, /v1/completions).
	Prompt string
	Suffix string
	Raw    bool
	// Unconvertible lists request fields that only a same-format passthrough
	// can honour (e.g. Ollama "template", "context"). Conversion fails if set.
	Unconvertible []string
}

// Think controls reasoning output.
type Think struct {
	Enabled      bool
	Effort       string // "low" | "medium" | "high" (optional)
	BudgetTokens int    // Anthropic budget_tokens (optional)
	// Implicit marks a default chosen by the gateway (not the client): e.g.
	// Anthropic requests without "thinking" mean thinking off. Adapters that
	// cannot express "off" safely (OpenAI reasoning_effort) skip it.
	Implicit bool
}

// Message is one conversation turn.
type Message struct {
	Role       string // system | user | assistant | tool
	Content    []Part
	ToolCalls  []ToolCall // assistant
	ToolCallID string     // tool
	Name       string     // tool: function name; others: participant name
	Reasoning  string     // assistant thinking text
	// ReasoningSignature is Anthropic's thinking block signature (kept for round trips).
	ReasoningSignature string
}

// Part is a piece of message content.
type Part struct {
	Type  string // "text" | "image"
	Text  string
	Image *Image
}

// Image is either a remote URL or inline base64 data.
type Image struct {
	URL       string // http(s) URL, empty when Data is set
	Data      string // base64 (no data: prefix)
	MediaType string // e.g. image/png
}

// Tool is a function tool definition.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON schema
}

// ToolCall is a complete tool call made by the assistant.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON text
}

// ToolChoice: Mode is auto | none | required | function (Name set).
type ToolChoice struct {
	Mode string
	Name string
}

// ResponseFormat: Type is text | json_object | json_schema.
type ResponseFormat struct {
	Type   string
	Name   string
	Schema json.RawMessage
	Strict *bool
}

// Usage is token usage reported by upstream.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// Text returns the concatenation of the text parts.
func (m *Message) Text() string {
	if len(m.Content) == 1 && m.Content[0].Type == "text" {
		return m.Content[0].Text
	}
	var b strings.Builder
	first := true
	for _, p := range m.Content {
		if p.Type != "text" {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
		first = false
	}
	return b.String()
}

// Images returns the image parts.
func (m *Message) Images() []*Image {
	var out []*Image
	for _, p := range m.Content {
		if p.Type == "image" && p.Image != nil {
			out = append(out, p.Image)
		}
	}
	return out
}

// TextPart is a convenience constructor.
func TextPart(s string) Part { return Part{Type: "text", Text: s} }

// Finish reasons (OpenAI vocabulary).
const (
	FinishStop      = "stop"
	FinishLength    = "length"
	FinishToolCalls = "tool_calls"
	FinishFilter    = "content_filter"
)

// Event is one streamed delta from upstream, in the common format.
type Event struct {
	Choice    int
	Role      string
	Content   string
	Reasoning string
	ToolCalls []ToolCallDelta
	// FinishReason is set on the last event of a choice. StopSequence is the
	// matched stop sequence, when upstream reports it.
	FinishReason string
	StopSequence string
	Usage        *Usage
	// ID / Model / Created are upstream response metadata (optional).
	ID      string
	Created int64

	// Raw is the original upstream chunk (for passthrough). RawFormat tells which
	// wire format it is in.
	Raw       []byte
	RawFormat Format
}

// ToolCallDelta is a fragment of a tool call. Fragments with the same Index are
// concatenated; ID and Name usually appear only in the first fragment.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// IsToken reports whether the event carries generated output (content, reasoning
// or tool calls). Only such events count for TTFT / idle detection; empty
// keepalive-like chunks do not.
func (e *Event) IsToken() bool {
	return e.Content != "" || e.Reasoning != "" || len(e.ToolCalls) > 0
}

// IsTerminal reports whether the event carries end-of-generation information.
func (e *Event) IsTerminal() bool {
	return e.FinishReason != "" || e.Usage != nil
}

// Clone returns a copy of the request that can be modified (messages slice
// and options map are copied; message contents are shared).
func (r *Request) Clone() *Request {
	c := *r
	c.Messages = append([]Message(nil), r.Messages...)
	if r.Options != nil {
		c.Options = make(map[string]any, len(r.Options))
		for k, v := range r.Options {
			c.Options[k] = v
		}
	}
	return &c
}
