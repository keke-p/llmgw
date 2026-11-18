package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"

	"llmgw/internal/chat"
)

// ---- request ---------------------------------------------------------------

type olChatRequest struct {
	Model     string          `json:"model"`
	Messages  []olMessage     `json:"messages"`
	Stream    bool            `json:"stream"`
	Tools     []oaTool        `json:"tools,omitempty"`
	Format    json.RawMessage `json:"format,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
	KeepAlive json.RawMessage `json:"keep_alive,omitempty"`
	Think     json.RawMessage `json:"think,omitempty"`
}

type olMessage struct {
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	Thinking  string       `json:"thinking,omitempty"`
	Images    []string     `json:"images,omitempty"`
	ToolCalls []olToolCall `json:"tool_calls,omitempty"`
	ToolName  string       `json:"tool_name,omitempty"`
}

type olToolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Index     *int            `json:"index,omitempty"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// BuildOllamaChat converts a common request to an Ollama /api/chat body
// (always streaming).
func BuildOllamaChat(req *chat.Request, model string) ([]byte, error) {
	out := olChatRequest{Model: model, Stream: true, KeepAlive: req.KeepAlive}

	toolNames := map[string]string{} // tool_call_id -> function name
	for _, m := range req.Messages {
		for _, tc := range m.ToolCalls {
			toolNames[tc.ID] = tc.Name
		}
	}
	for _, m := range req.Messages {
		om := olMessage{Role: m.Role, Content: m.Text(), Thinking: m.Reasoning}
		for _, img := range m.Images() {
			if img.Data == "" {
				return nil, &ConversionError{"remote image URLs are not supported by Ollama providers; send base64 data instead"}
			}
			om.Images = append(om.Images, img.Data)
		}
		for _, tc := range m.ToolCalls {
			var otc olToolCall
			otc.Function.Name = tc.Name
			otc.Function.Arguments = argsObject(tc.Arguments)
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		if m.Role == "tool" {
			om.ToolName = m.Name
			if om.ToolName == "" {
				om.ToolName = toolNames[m.ToolCallID]
			}
		}
		out.Messages = append(out.Messages, om)
	}

	if req.ToolChoice == nil || req.ToolChoice.Mode != "none" {
		for _, t := range req.Tools {
			out.Tools = append(out.Tools, toOATool(t))
		}
	}

	out.Options = ollamaOptions(req)
	out.Format = ollamaFormat(req)
	out.Think = ollamaThink(req)
	return json.Marshal(out)
}

// ollamaOptions merges Ollama options with the explicit sampling fields
// (explicit fields win).
func ollamaOptions(req *chat.Request) map[string]any {
	opts := map[string]any{}
	maps.Copy(opts, req.Options)
	setOpt := func(k string, v any, ok bool) {
		if ok {
			opts[k] = v
		}
	}
	setOpt("temperature", deref(req.Temperature), req.Temperature != nil)
	setOpt("top_p", deref(req.TopP), req.TopP != nil)
	setOpt("top_k", deref(req.TopK), req.TopK != nil)
	setOpt("seed", deref(req.Seed), req.Seed != nil)
	setOpt("num_predict", deref(req.MaxTokens), req.MaxTokens != nil)
	setOpt("presence_penalty", deref(req.PresencePenalty), req.PresencePenalty != nil)
	setOpt("frequency_penalty", deref(req.FrequencyPenalty), req.FrequencyPenalty != nil)
	setOpt("stop", req.Stop, len(req.Stop) > 0)
	if len(opts) == 0 {
		return nil
	}
	return opts
}

func ollamaFormat(req *chat.Request) json.RawMessage {
	switch {
	case len(req.Format) > 0:
		return req.Format
	case req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object":
		return json.RawMessage(`"json"`)
	case req.ResponseFormat != nil && req.ResponseFormat.Type == "json_schema" && len(req.ResponseFormat.Schema) > 0:
		return req.ResponseFormat.Schema
	}
	return nil
}

func ollamaThink(req *chat.Request) json.RawMessage {
	if req.Think == nil {
		return nil
	}
	switch {
	case !req.Think.Enabled:
		return json.RawMessage(`false`)
	case req.Think.Effort != "":
		b, _ := json.Marshal(req.Think.Effort)
		return b
	}
	return json.RawMessage(`true`)
}

// argsObject turns a JSON-text arguments string into a JSON object for Ollama.
func argsObject(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(s)) && strings.HasPrefix(s, "{") {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(map[string]string{"input": s})
	return b
}

// ---- stream ----------------------------------------------------------------

type olChatChunk struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   *struct {
		Role      string       `json:"role"`
		Content   string       `json:"content"`
		Thinking  string       `json:"thinking"`
		ToolCalls []olToolCall `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

// ParseOllamaChatStream reads an Ollama /api/chat NDJSON stream and emits
// common events. Ollama returns complete tool calls without IDs, so IDs are
// generated here.
func ParseOllamaChatStream(r io.Reader, emit func(*chat.Event) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	toolIndex := 0
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			raw := bytes.TrimSpace(line)
			var c olChatChunk
			if jerr := json.Unmarshal(raw, &c); jerr != nil {
				if err != nil { // partial last line: the connection was cut
					return io.ErrUnexpectedEOF
				}
				return fmt.Errorf("ollama: invalid stream chunk: %w", jerr)
			}
			if c.Error != "" {
				return &StreamError{Message: c.Error}
			}
			ev := &chat.Event{Raw: append([]byte(nil), raw...), RawFormat: chat.FormatOllama}
			if c.Message != nil {
				ev.Role = c.Message.Role
				ev.Content = c.Message.Content
				ev.Reasoning = c.Message.Thinking
				for _, tc := range c.Message.ToolCalls {
					id := tc.ID
					if id == "" {
						id = chat.NewToolCallID()
					}
					ev.ToolCalls = append(ev.ToolCalls, chat.ToolCallDelta{
						Index: toolIndex, ID: id, Name: tc.Function.Name,
						Arguments: compactJSON(tc.Function.Arguments),
					})
					toolIndex++
				}
			}
			if c.Done {
				switch {
				case c.DoneReason == "length":
					ev.FinishReason = chat.FinishLength
				case toolIndex > 0:
					ev.FinishReason = chat.FinishToolCalls
				default:
					ev.FinishReason = chat.FinishStop
				}
				ev.Usage = &chat.Usage{PromptTokens: c.PromptEvalCount, CompletionTokens: c.EvalCount}
			}
			if eerr := emit(ev); eerr != nil {
				return eerr
			}
			if c.Done {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
}

func compactJSON(b json.RawMessage) string {
	if len(b) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return string(b)
	}
	return buf.String()
}

// ---- embeddings ------------------------------------------------------------

type olEmbedRequest struct {
	Model      string          `json:"model"`
	Input      []string        `json:"input"`
	Truncate   *bool           `json:"truncate,omitempty"`
	Dimensions *int            `json:"dimensions,omitempty"`
	Options    map[string]any  `json:"options,omitempty"`
	KeepAlive  json.RawMessage `json:"keep_alive,omitempty"`
}

// BuildOllamaEmbed builds an Ollama /api/embed body.
func BuildOllamaEmbed(req *EmbedRequest, model string) ([]byte, error) {
	return json.Marshal(olEmbedRequest{
		Model: model, Input: req.Input, Truncate: req.Truncate, Dimensions: req.Dimensions,
		Options: req.Options, KeepAlive: req.KeepAlive,
	})
}

// ParseOllamaEmbed parses an Ollama /api/embed response.
func ParseOllamaEmbed(body []byte) (*EmbedResult, error) {
	var r struct {
		Embeddings      [][]float64 `json:"embeddings"`
		PromptEvalCount int         `json:"prompt_eval_count"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("ollama: invalid embed response: %w", err)
	}
	return &EmbedResult{Embeddings: r.Embeddings, PromptTokens: r.PromptEvalCount}, nil
}

func deref[T any](p *T) T {
	if p == nil {
		var z T
		return z
	}
	return *p
}

// ---- passthrough / generate ------------------------------------------------

// PassthroughOllama rewrites a raw Ollama request (chat, generate, embed) for
// upstream: the model is replaced and, for streaming endpoints, stream=true is
// forced. Unknown fields pass through unchanged.
func PassthroughOllama(raw map[string]json.RawMessage, model string, forceStream bool) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(raw)+1)
	for k, v := range raw {
		out[k] = v
	}
	delete(out, "name") // legacy alias of "model"
	out["model"], _ = json.Marshal(model)
	if forceStream {
		out["stream"] = json.RawMessage(`true`)
	}
	return json.Marshal(out)
}

type olGenerateRequest struct {
	Model     string          `json:"model"`
	Prompt    string          `json:"prompt"`
	Suffix    string          `json:"suffix,omitempty"`
	System    string          `json:"system,omitempty"`
	Images    []string        `json:"images,omitempty"`
	Raw       bool            `json:"raw,omitempty"`
	Stream    bool            `json:"stream"`
	Format    json.RawMessage `json:"format,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
	KeepAlive json.RawMessage `json:"keep_alive,omitempty"`
	Think     json.RawMessage `json:"think,omitempty"`
}

// BuildOllamaGenerate converts a prompt-style request (OpenAI /v1/completions)
// to an Ollama /api/generate body.
func BuildOllamaGenerate(req *chat.Request, model string) ([]byte, error) {
	think := ollamaThink(req)
	if think == nil {
		// Text completions have no reasoning channel; do not let thinking models
		// spend the token budget on hidden reasoning.
		think = json.RawMessage(`false`)
	}
	// System messages (e.g. an alias system prompt) become the "system" field.
	var system []string
	for _, m := range req.Messages {
		if m.Role == "system" {
			system = append(system, m.Text())
		}
	}
	return json.Marshal(olGenerateRequest{
		Model: model, Prompt: req.Prompt, Suffix: req.Suffix, Raw: req.Raw, Stream: true,
		System: strings.Join(system, "\n\n"),
		Format: ollamaFormat(req), Options: ollamaOptions(req), KeepAlive: req.KeepAlive, Think: think,
	})
}

type olGenerateChunk struct {
	Response        string `json:"response"`
	Thinking        string `json:"thinking"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

// ParseOllamaGenerateStream reads an Ollama /api/generate NDJSON stream.
func ParseOllamaGenerateStream(r io.Reader, emit func(*chat.Event) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if raw := bytes.TrimSpace(line); len(raw) > 0 {
			var c olGenerateChunk
			if jerr := json.Unmarshal(raw, &c); jerr != nil {
				if err != nil { // partial last line: the connection was cut
					return io.ErrUnexpectedEOF
				}
				return fmt.Errorf("ollama: invalid stream chunk: %w", jerr)
			}
			if c.Error != "" {
				return &StreamError{Message: c.Error}
			}
			ev := &chat.Event{Content: c.Response, Reasoning: c.Thinking,
				Raw: append([]byte(nil), raw...), RawFormat: chat.RawOllamaGenerate}
			if c.Done {
				ev.FinishReason = chat.FinishStop
				if c.DoneReason == "length" {
					ev.FinishReason = chat.FinishLength
				}
				ev.Usage = &chat.Usage{PromptTokens: c.PromptEvalCount, CompletionTokens: c.EvalCount}
			}
			if eerr := emit(ev); eerr != nil {
				return eerr
			}
			if c.Done {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
}

// ParseOllamaLegacyEmbed parses an Ollama /api/embeddings response.
func ParseOllamaLegacyEmbed(body []byte) (*EmbedResult, error) {
	var r struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("ollama: invalid embeddings response: %w", err)
	}
	return &EmbedResult{Embeddings: [][]float64{r.Embedding}}, nil
}
