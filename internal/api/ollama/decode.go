// Package ollama implements the Ollama-native inbound API (/api/chat,
// /api/generate, /api/embed, /api/tags, ...), so Ollama clients such as Open
// WebUI can point at the gateway unchanged.
package ollama

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"

	"llmgw/internal/chat"
	"llmgw/internal/gwerr"
	"llmgw/internal/upstream"
)

type toolIn struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type toolCallIn struct {
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type messageIn struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	Images     []string     `json:"images"`
	Thinking   string       `json:"thinking"`
	ToolCalls  []toolCallIn `json:"tool_calls"`
	ToolName   string       `json:"tool_name"`
	ToolCallID string       `json:"tool_call_id"`
}

type chatIn struct {
	Model     string          `json:"model"`
	Name      string          `json:"name"` // legacy alias of model
	Messages  []messageIn     `json:"messages"`
	Tools     []toolIn        `json:"tools"`
	Format    json.RawMessage `json:"format"`
	Options   map[string]any  `json:"options"`
	Stream    *bool           `json:"stream"`
	KeepAlive json.RawMessage `json:"keep_alive"`
	Think     json.RawMessage `json:"think"`
}

type generateIn struct {
	Model     string          `json:"model"`
	Name      string          `json:"name"`
	Prompt    string          `json:"prompt"`
	Suffix    string          `json:"suffix"`
	System    string          `json:"system"`
	Template  string          `json:"template"`
	Context   json.RawMessage `json:"context"`
	Raw       bool            `json:"raw"`
	Images    []string        `json:"images"`
	Format    json.RawMessage `json:"format"`
	Options   map[string]any  `json:"options"`
	Stream    *bool           `json:"stream"`
	KeepAlive json.RawMessage `json:"keep_alive"`
	Think     json.RawMessage `json:"think"`
}

func decode(body []byte, v any) (map[string]json.RawMessage, *gwerr.Error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, gwerr.BadRequest("invalid JSON body: %v", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return nil, gwerr.BadRequest("invalid request: %v", err)
	}
	return raw, nil
}

// ParseChat decodes an Ollama /api/chat request. stream defaults to true.
func ParseChat(body []byte) (*upstream.Inbound, *gwerr.Error) {
	var r chatIn
	raw, ge := decode(body, &r)
	if ge != nil {
		return nil, ge
	}
	req := &chat.Request{Model: firstNonEmpty(r.Model, r.Name), Stream: r.Stream == nil || *r.Stream,
		Format: nullToNil(r.Format), KeepAlive: nullToNil(r.KeepAlive), IncludeUsage: true}
	if ge := applyOptions(req, r.Options); ge != nil {
		return nil, ge
	}
	req.Think = parseThink(r.Think)

	// Ollama tool calls have no IDs and tool results reference the function
	// name; assign IDs so conversions to OpenAI/Anthropic can link them.
	type pending struct{ id, name string }
	var open []pending
	n := 0
	for i, m := range r.Messages {
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			return nil, gwerr.BadRequest("messages[%d]: unsupported role %q", i, m.Role)
		}
		cm := chat.Message{Role: m.Role, Reasoning: m.Thinking}
		if m.Content != "" {
			cm.Content = append(cm.Content, chat.TextPart(m.Content))
		}
		for _, img := range m.Images {
			cm.Content = append(cm.Content, chat.Part{Type: "image", Image: &chat.Image{Data: img, MediaType: sniffImage(img)}})
		}
		for _, tc := range m.ToolCalls {
			id := tc.ID
			if id == "" {
				n++
				id = "call_" + strconv.Itoa(n)
			}
			args := "{}"
			if a := nullToNil(tc.Function.Arguments); a != nil {
				var buf bytes.Buffer
				if json.Compact(&buf, a) == nil {
					args = buf.String()
				}
			}
			cm.ToolCalls = append(cm.ToolCalls, chat.ToolCall{ID: id, Name: tc.Function.Name, Arguments: args})
			open = append(open, pending{id, tc.Function.Name})
		}
		if m.Role == "tool" {
			cm.Name = m.ToolName
			cm.ToolCallID = m.ToolCallID
			if cm.ToolCallID == "" {
				for j, p := range open {
					if m.ToolName == "" || p.name == m.ToolName {
						cm.ToolCallID = p.id
						if cm.Name == "" {
							cm.Name = p.name
						}
						open = append(open[:j], open[j+1:]...)
						break
					}
				}
			}
		}
		req.Messages = append(req.Messages, cm)
	}
	for _, t := range r.Tools {
		req.Tools = append(req.Tools, chat.Tool{Name: t.Function.Name, Description: t.Function.Description,
			Parameters: t.Function.Parameters})
	}
	return &upstream.Inbound{Format: chat.FormatOllama, Req: req, Raw: raw}, nil
}

// ParseGenerate decodes an Ollama /api/generate request. For non-Ollama
// upstreams it is converted to a chat (system + user prompt); template,
// context, raw and suffix can only be honoured by an Ollama upstream.
func ParseGenerate(body []byte) (*upstream.Inbound, *gwerr.Error) {
	var r generateIn
	raw, ge := decode(body, &r)
	if ge != nil {
		return nil, ge
	}
	req := &chat.Request{Model: firstNonEmpty(r.Model, r.Name), Stream: r.Stream == nil || *r.Stream,
		Prompt: r.Prompt, Suffix: r.Suffix, Raw: r.Raw,
		Format: nullToNil(r.Format), KeepAlive: nullToNil(r.KeepAlive), IncludeUsage: true}
	if ge := applyOptions(req, r.Options); ge != nil {
		return nil, ge
	}
	req.Think = parseThink(r.Think)
	if r.System != "" {
		req.Messages = append(req.Messages, chat.Message{Role: "system", Content: []chat.Part{chat.TextPart(r.System)}})
	}
	user := chat.Message{Role: "user"}
	if r.Prompt != "" {
		user.Content = append(user.Content, chat.TextPart(r.Prompt))
	}
	for _, img := range r.Images {
		user.Content = append(user.Content, chat.Part{Type: "image", Image: &chat.Image{Data: img, MediaType: sniffImage(img)}})
	}
	req.Messages = append(req.Messages, user)
	if r.Template != "" {
		req.Unconvertible = append(req.Unconvertible, "template")
	}
	if c := nullToNil(r.Context); c != nil && string(c) != "[]" {
		req.Unconvertible = append(req.Unconvertible, "context")
	}
	if r.Raw {
		req.Unconvertible = append(req.Unconvertible, "raw")
	}
	if r.Suffix != "" {
		req.Unconvertible = append(req.Unconvertible, "suffix")
	}
	return &upstream.Inbound{Format: chat.FormatOllama, Kind: upstream.KindGenerate, Req: req, Raw: raw}, nil
}

// applyOptions copies well-known Ollama options into the explicit common
// fields (used by conversions and alias parameter rules) and keeps all options.
func applyOptions(req *chat.Request, opts map[string]any) *gwerr.Error {
	req.Options = opts
	num := func(k string) (float64, bool) {
		v, ok := opts[k]
		if !ok || v == nil {
			return 0, false
		}
		f, ok := v.(float64)
		return f, ok
	}
	if f, ok := num("temperature"); ok {
		req.Temperature = &f
	}
	if f, ok := num("top_p"); ok {
		req.TopP = &f
	}
	if f, ok := num("top_k"); ok {
		n := int(f)
		req.TopK = &n
	}
	if f, ok := num("seed"); ok {
		n := int64(f)
		req.Seed = &n
	}
	if f, ok := num("num_predict"); ok && f >= 0 {
		n := int(f)
		req.MaxTokens = &n
	}
	if f, ok := num("presence_penalty"); ok {
		req.PresencePenalty = &f
	}
	if f, ok := num("frequency_penalty"); ok {
		req.FrequencyPenalty = &f
	}
	switch s := opts["stop"].(type) {
	case string:
		req.Stop = []string{s}
	case []any:
		for _, v := range s {
			if str, ok := v.(string); ok {
				req.Stop = append(req.Stop, str)
			}
		}
	}
	return nil
}

func parseThink(raw json.RawMessage) *chat.Think {
	raw = nullToNil(raw)
	if raw == nil {
		return nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return &chat.Think{Enabled: b}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return &chat.Think{Enabled: true, Effort: s}
	}
	return nil
}

// ---- embeddings ------------------------------------------------------------

type embedIn struct {
	Model      string          `json:"model"`
	Input      json.RawMessage `json:"input"`
	Truncate   *bool           `json:"truncate"`
	Dimensions *int            `json:"dimensions"`
	Options    map[string]any  `json:"options"`
	KeepAlive  json.RawMessage `json:"keep_alive"`
}

// ParseEmbed decodes /api/embed.
func ParseEmbed(body []byte) (*upstream.EmbedInbound, *gwerr.Error) {
	var r embedIn
	raw, ge := decode(body, &r)
	if ge != nil {
		return nil, ge
	}
	req := &upstream.EmbedRequest{Model: r.Model, Truncate: r.Truncate, Dimensions: r.Dimensions,
		Options: r.Options, KeepAlive: nullToNil(r.KeepAlive)}
	in := nullToNil(r.Input)
	var s string
	var arr []string
	switch {
	case in == nil:
		return nil, gwerr.BadRequest("input is required")
	case json.Unmarshal(in, &s) == nil:
		req.Input = []string{s}
	case json.Unmarshal(in, &arr) == nil:
		req.Input = arr
	default:
		return nil, gwerr.BadRequest("input must be a string or an array of strings")
	}
	return &upstream.EmbedInbound{Format: chat.FormatOllama, Req: req, Raw: raw}, nil
}

// ParseLegacyEmbed decodes the deprecated /api/embeddings ({"prompt": ...}).
func ParseLegacyEmbed(body []byte) (*upstream.EmbedInbound, *gwerr.Error) {
	var r struct {
		Model     string          `json:"model"`
		Prompt    string          `json:"prompt"`
		Options   map[string]any  `json:"options"`
		KeepAlive json.RawMessage `json:"keep_alive"`
	}
	raw, ge := decode(body, &r)
	if ge != nil {
		return nil, ge
	}
	return &upstream.EmbedInbound{Format: chat.FormatOllama, Legacy: true, Raw: raw,
		Req: &upstream.EmbedRequest{Model: r.Model, Input: []string{r.Prompt}, Options: r.Options,
			KeepAlive: nullToNil(r.KeepAlive)}}, nil
}

// ---- helpers ---------------------------------------------------------------

// sniffImage detects the media type of base64 image data from magic bytes
// (Ollama sends bare base64 without a media type).
func sniffImage(b64 string) string {
	head := b64
	if len(head) > 64 {
		head = head[:64]
	}
	head = head[:len(head)/4*4]
	b, err := base64.StdEncoding.DecodeString(head)
	if err != nil || len(b) == 0 {
		return "image/png"
	}
	ct := http.DetectContentType(b)
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return ct
	}
	return "image/png"
}

func nullToNil(r json.RawMessage) json.RawMessage {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || string(r) == "null" {
		return nil
	}
	return r
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
