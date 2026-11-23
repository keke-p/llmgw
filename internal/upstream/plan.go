// Package upstream contains the outbound adapters: building requests for
// upstream providers (Ollama native / OpenAI-compatible / Anthropic-compatible)
// and parsing their (streaming) responses into common events.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"llmgw/internal/chat"
	"llmgw/internal/params"
	"llmgw/internal/store"
)

// ConversionError means the request cannot be expressed in the upstream format
// (reported to the client as 400).
type ConversionError struct{ Msg string }

func (e *ConversionError) Error() string { return e.Msg }

// StreamError is an error reported by upstream inside an otherwise successful
// (200) response.
type StreamError struct{ Message string }

func (e *StreamError) Error() string { return "upstream error: " + e.Message }

// HTTPError is a non-2xx upstream response.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("upstream returned %d: %s", e.Status, e.Message)
}

// Inbound is the client request in both parsed (common) and raw form. Raw is
// used for passthrough when the upstream speaks the inbound format.
type Inbound struct {
	Format chat.Format
	Kind   Kind
	Req    *chat.Request
	Raw    map[string]json.RawMessage
	// Header holds client headers relevant for same-format passthrough
	// (anthropic-version, anthropic-beta).
	Header http.Header
}

// Kind is the inbound endpoint family.
type Kind string

const (
	KindChat       Kind = ""           // chat (default)
	KindGenerate   Kind = "generate"   // Ollama /api/generate
	KindCompletion Kind = "completion" // OpenAI /v1/completions
)

// ChatPlan describes one upstream chat call.
type ChatPlan struct {
	URL         string
	Body        []byte
	Format      chat.Format
	Passthrough bool
	// Header is added to the upstream request.
	Header http.Header
	// Parse reads the upstream response body and emits events.
	Parse func(resp *http.Response, emit func(*chat.Event) error) error
}

func bodyParser(f func(io.Reader, func(*chat.Event) error) error) func(*http.Response, func(*chat.Event) error) error {
	return func(resp *http.Response, emit func(*chat.Event) error) error { return f(resp.Body, emit) }
}

func checkConvertible(req *chat.Request) error {
	if len(req.Unconvertible) > 0 {
		return &ConversionError{fmt.Sprintf("field(s) %s are only supported when the upstream provider is Ollama",
			strings.Join(req.Unconvertible, ", "))}
	}
	return nil
}

// PlanChat decides the upstream format and builds the request body. When the
// upstream speaks the inbound format the request is passed through (only the
// model name is rewritten, streaming is forced and alias parameter rules are
// applied to the raw JSON); otherwise it is converted via the common format.
func PlanChat(p *store.Provider, model string, in *Inbound, prm *params.Params) (*ChatPlan, error) {
	passthrough := in.Raw != nil && prm.PassthroughOK(in.Format)
	if in.Format == chat.FormatAnthropic && passthrough && p.SupportsAnthropic {
		raw := prm.ApplyRaw(in.Raw, chat.FormatAnthropic, false)
		if _, ok := raw["thinking"]; !ok {
			// Anthropic semantics: no "thinking" means thinking off. Some
			// implementations (Ollama) think by default, so make it explicit.
			raw = withField(raw, "thinking", json.RawMessage(`{"type":"disabled"}`))
		}
		body, err := PassthroughAnthropic(raw, model, true)
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		for _, k := range []string{"anthropic-version", "anthropic-beta"} {
			if v := in.Header.Values(k); len(v) > 0 {
				h[http.CanonicalHeaderKey(k)] = v
			}
		}
		if h.Get("anthropic-version") == "" {
			h.Set("anthropic-version", "2023-06-01")
		}
		if p.APIKey != "" {
			h.Set("x-api-key", p.APIKey)
		}
		return &ChatPlan{URL: AnthropicURL(p, ""), Body: body, Format: chat.FormatAnthropic,
			Passthrough: true, Header: h, Parse: parseAnthropicResponse}, nil
	}
	// converted returns the common request with the alias rules applied.
	converted := func() (*chat.Request, error) {
		if err := checkConvertible(in.Req); err != nil {
			return nil, err
		}
		req := in.Req.Clone()
		prm.ApplyChat(req)
		return req, nil
	}
	switch p.Type {
	case store.ProviderOllama:
		base := p.BaseURL
		if in.Format == chat.FormatOllama && passthrough {
			generate := in.Kind == KindGenerate
			body, err := PassthroughOllama(prm.ApplyRaw(in.Raw, chat.FormatOllama, generate), model, true)
			if err != nil {
				return nil, err
			}
			if generate {
				return &ChatPlan{URL: joinURL(base, "/api/generate"), Body: body, Format: chat.FormatOllama,
					Passthrough: true, Parse: bodyParser(ParseOllamaGenerateStream)}, nil
			}
			return &ChatPlan{URL: joinURL(base, "/api/chat"), Body: body, Format: chat.FormatOllama,
				Passthrough: true, Parse: bodyParser(ParseOllamaChatStream)}, nil
		}
		req, err := converted()
		if err != nil {
			return nil, err
		}
		if in.Kind == KindCompletion {
			body, err := BuildOllamaGenerate(req, model)
			if err != nil {
				return nil, err
			}
			return &ChatPlan{URL: joinURL(base, "/api/generate"), Body: body, Format: chat.FormatOllama,
				Parse: bodyParser(ParseOllamaGenerateStream)}, nil
		}
		body, err := BuildOllamaChat(req, model)
		if err != nil {
			return nil, err
		}
		return &ChatPlan{URL: joinURL(base, "/api/chat"), Body: body, Format: chat.FormatOllama,
			Parse: bodyParser(ParseOllamaChatStream)}, nil

	case store.ProviderOpenAICompatible:
		base := OpenAIBase(p.BaseURL)
		if in.Format == chat.FormatOpenAI && passthrough {
			body, err := PassthroughOpenAIChat(prm.ApplyRaw(in.Raw, chat.FormatOpenAI, false), model)
			if err != nil {
				return nil, err
			}
			if in.Kind == KindCompletion {
				return &ChatPlan{URL: joinURL(base, "/completions"), Body: body, Format: chat.FormatOpenAI,
					Passthrough: true, Parse: parseOpenAICompletionResponse}, nil
			}
			return &ChatPlan{URL: joinURL(base, "/chat/completions"), Body: body, Format: chat.FormatOpenAI,
				Passthrough: true, Parse: parseOpenAIResponse}, nil
		}
		if in.Kind == KindCompletion {
			return nil, &ConversionError{"text completions require an OpenAI-compatible request"}
		}
		req, err := converted()
		if err != nil {
			return nil, err
		}
		body, err := BuildOpenAIChat(req, model)
		if err != nil {
			return nil, err
		}
		return &ChatPlan{URL: joinURL(base, "/chat/completions"), Body: body, Format: chat.FormatOpenAI,
			Parse: parseOpenAIResponse}, nil
	}
	return nil, fmt.Errorf("unknown provider type %q", p.Type)
}

func parseOpenAIResponse(resp *http.Response, emit func(*chat.Event) error) error {
	if isJSON(resp.Header.Get("Content-Type")) {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return ParseOpenAIChatCompletion(body, emit)
	}
	return ParseOpenAIChatStream(resp.Body, emit)
}

func parseOpenAICompletionResponse(resp *http.Response, emit func(*chat.Event) error) error {
	if isJSON(resp.Header.Get("Content-Type")) {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return ParseOpenAIChatCompletion(body, emit) // choices[].text is handled too
	}
	return ParseOpenAICompletionStream(resp.Body, emit)
}

func parseAnthropicResponse(resp *http.Response, emit func(*chat.Event) error) error {
	if isJSON(resp.Header.Get("Content-Type")) {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return ParseAnthropicMessage(body, emit)
	}
	return ParseAnthropicStream(resp.Body, emit)
}

func isJSON(ct string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	return mt == "application/json"
}

// ---- embeddings ------------------------------------------------------------

// EmbedRequest is an embeddings request in the common format.
type EmbedRequest struct {
	Model      string
	Input      []string
	Dimensions *int
	Truncate   *bool
	Options    map[string]any
	KeepAlive  json.RawMessage
}

// EmbedResult is an embeddings response in the common format.
type EmbedResult struct {
	Embeddings   [][]float64
	PromptTokens int
}

// EmbedInbound is the client embeddings request.
type EmbedInbound struct {
	Format chat.Format
	// Legacy marks Ollama's old /api/embeddings ({"prompt"} -> {"embedding"}).
	Legacy bool
	Req    *EmbedRequest
	Raw    map[string]json.RawMessage
}

// EmbedPlan describes one upstream embeddings call. When Passthrough is true the
// caller may return the raw body (after rewriting the model name).
type EmbedPlan struct {
	URL         string
	Body        []byte
	Format      chat.Format
	Passthrough bool
	Parse       func(body []byte) (*EmbedResult, error)
}

// PlanEmbed decides the upstream format for embeddings.
// Alias rules contribute keep_alive and Ollama options (num_ctx, ...) only.
func PlanEmbed(p *store.Provider, model string, in *EmbedInbound, prm *params.Params) (*EmbedPlan, error) {
	same := in.Raw != nil && ((p.Type == store.ProviderOllama && in.Format == chat.FormatOllama) ||
		(p.Type == store.ProviderOpenAICompatible && in.Format == chat.FormatOpenAI))
	if !same && in.Req.Input == nil {
		return nil, &ConversionError{"token array input is not supported by this provider; send strings"}
	}
	switch p.Type {
	case store.ProviderOllama:
		if same {
			raw := in.Raw
			if prm != nil {
				var curOpts map[string]any
				_ = json.Unmarshal(in.Raw["options"], &curOpts)
				opts, ka := prm.EmbedOptions(curOpts, in.Raw["keep_alive"])
				raw = make(map[string]json.RawMessage, len(in.Raw)+2)
				for k, v := range in.Raw {
					raw[k] = v
				}
				if opts != nil {
					raw["options"], _ = json.Marshal(opts)
				}
				if len(ka) > 0 {
					raw["keep_alive"] = ka
				}
			}
			body, err := PassthroughOllama(raw, model, false)
			if err != nil {
				return nil, err
			}
			if in.Legacy {
				return &EmbedPlan{URL: joinURL(p.BaseURL, "/api/embeddings"), Body: body, Format: chat.FormatOllama,
					Passthrough: true, Parse: ParseOllamaLegacyEmbed}, nil
			}
			return &EmbedPlan{URL: joinURL(p.BaseURL, "/api/embed"), Body: body, Format: chat.FormatOllama,
				Passthrough: true, Parse: ParseOllamaEmbed}, nil
		}
		req := in.Req
		if prm != nil {
			r := *in.Req
			r.Options, r.KeepAlive = prm.EmbedOptions(in.Req.Options, in.Req.KeepAlive)
			req = &r
		}
		body, err := BuildOllamaEmbed(req, model)
		if err != nil {
			return nil, err
		}
		return &EmbedPlan{URL: joinURL(p.BaseURL, "/api/embed"), Body: body,
			Format: chat.FormatOllama, Parse: ParseOllamaEmbed}, nil
	case store.ProviderOpenAICompatible:
		plan := &EmbedPlan{URL: joinURL(OpenAIBase(p.BaseURL), "/embeddings"),
			Format: chat.FormatOpenAI, Parse: ParseOpenAIEmbed}
		var err error
		if same {
			plan.Passthrough = true
			plan.Body, err = PassthroughOpenAIEmbed(in.Raw, model)
		} else {
			plan.Body, err = BuildOpenAIEmbed(in.Req, model)
		}
		if err != nil {
			return nil, err
		}
		return plan, nil
	}
	return nil, fmt.Errorf("unknown provider type %q", p.Type)
}

// ---- HTTP helpers ----------------------------------------------------------

// NewRequest builds the upstream POST request with auth headers.
func NewRequest(ctx context.Context, p *store.Provider, u string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream, application/x-ndjson")
	req.Header.Set("User-Agent", "llmgw")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	return req, nil
}

// ReadHTTPError builds an HTTPError from a non-2xx response (body is consumed).
func ReadHTTPError(resp *http.Response) *HTTPError {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	msg := strings.TrimSpace(string(b))
	var o struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(b, &o) == nil && len(o.Error) > 0 {
		msg = errorMessage(o.Error)
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	if len(msg) > 2000 {
		msg = msg[:2000] + "..."
	}
	return &HTTPError{Status: resp.StatusCode, Message: msg}
}

// OpenAIBase normalizes an OpenAI-compatible base URL: when no path is given,
// "/v1" is appended (so "http://host:1234" and "http://host:1234/v1" both work).
func OpenAIBase(base string) string {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return base
	}
	if u.Path == "" {
		u.Path = "/v1"
	}
	return u.String()
}

func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

// Call performs a small non-streaming JSON request to a provider (model lists,
// /api/show, /api/ps). body may be nil for GET.
func Call(ctx context.Context, client *http.Client, p *store.Provider, method, path string, body []byte) ([]byte, error) {
	base := p.BaseURL
	if p.Type == store.ProviderOpenAICompatible {
		base = OpenAIBase(base)
	}
	return CallURL(ctx, client, p, method, joinURL(base, path), body)
}

// ServerRoot returns a provider base URL without a trailing "/v1" or
// "/api/v1", e.g. "http://host:1234/v1" -> "http://host:1234". LM Studio serves
// its native REST API (/api/v1/...) there.
func ServerRoot(base string) string {
	base = strings.TrimRight(base, "/")
	for _, suf := range []string{"/api/v1", "/v1"} {
		if strings.HasSuffix(base, suf) {
			return strings.TrimSuffix(base, suf)
		}
	}
	return base
}

// CallURL is Call with an absolute URL (the provider supplies the API key).
func CallURL(ctx context.Context, client *http.Client, p *store.Provider, method, u string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "llmgw")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, ReadHTTPError(resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// withField returns a copy of raw with k set to v.
func withField(raw map[string]json.RawMessage, k string, v json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(raw)+1)
	for key, val := range raw {
		out[key] = val
	}
	out[k] = v
	return out
}
