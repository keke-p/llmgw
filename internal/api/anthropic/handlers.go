package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"llmgw/internal/chat"
	"llmgw/internal/gateway"
	"llmgw/internal/gwerr"
	"llmgw/internal/store"
	"llmgw/internal/upstream"
)

// Authenticator authenticates an API request.
type Authenticator func(r *http.Request) (*gateway.Principal, *gwerr.Error)

// Handler serves the Anthropic-compatible API.
type Handler struct {
	Engine       *gateway.Engine
	Auth         Authenticator
	MaxBodyBytes int64
	ClientIP     func(r *http.Request) string
}

// Register adds the routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/messages", h.messages)
	mux.HandleFunc("POST /v1/messages/count_tokens", h.countTokens)
}

func (h *Handler) meta(r *http.Request, body []byte) gateway.Meta {
	ip := r.RemoteAddr
	if h.ClientIP != nil {
		ip = h.ClientIP(r)
	}
	return gateway.Meta{Endpoint: r.URL.Path, ClientIP: ip, UserAgent: r.UserAgent(), RawBody: body}
}

func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, *gwerr.Error) {
	limit := h.MaxBodyBytes
	if limit <= 0 {
		limit = 64 << 20
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, gwerr.New(http.StatusRequestEntityTooLarge, gwerr.InvalidRequest, "request body too large")
		}
		return nil, gwerr.BadRequest("failed to read request body")
	}
	return body, nil
}

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	p, ge := h.Auth(r)
	if ge != nil {
		h.Engine.RecordError(h.meta(r, nil), nil, chat.FormatAnthropic, "", ge)
		WriteError(w, ge)
		return
	}
	body, ge := h.readBody(w, r)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	meta := h.meta(r, body)
	in, ge := ParseMessages(body)
	if ge != nil {
		h.Engine.RecordError(meta, p, chat.FormatAnthropic, "", ge)
		WriteError(w, ge)
		return
	}
	in.Header = r.Header
	alias, ge := h.Engine.ResolveAlias(r.Context(), p, in.Req.Model)
	if ge != nil {
		h.Engine.RecordError(meta, p, chat.FormatAnthropic, in.Req.Model, ge)
		WriteError(w, ge)
		return
	}
	thinking := in.Req.Think != nil && in.Req.Think.Enabled
	h.Engine.Chat(w, r, &gateway.ChatCall{Meta: meta, Inbound: in, Alias: alias, Principal: p,
		Sink: newSink(alias.Name, thinking)})
}

// countTokens asks an Anthropic-capable upstream to count tokens and falls back
// to an estimate. It never returns 404 (Claude Code calls it routinely).
func (h *Handler) countTokens(w http.ResponseWriter, r *http.Request) {
	p, ge := h.Auth(r)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	body, ge := h.readBody(w, r)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	// count_tokens does not require max_tokens; reuse the messages parser.
	in, ge := ParseMessages(body)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	alias, ge := h.Engine.ResolveAlias(r.Context(), p, in.Req.Model)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	for _, t := range alias.Targets {
		if !t.Provider.SupportsAnthropic {
			continue
		}
		if n, ok := h.upstreamCount(r, t.Provider, t.UpstreamModel, in); ok {
			writeJSON(w, map[string]int{"input_tokens": n})
			return
		}
		break
	}
	writeJSON(w, map[string]int{"input_tokens": EstimateTokens(in, in.Raw["tools"])})
}

func (h *Handler) upstreamCount(r *http.Request, t *store.Provider, model string, in *upstream.Inbound) (int, bool) {
	body, err := upstream.PassthroughAnthropic(in.Raw, model, false)
	if err != nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.AnthropicURL(t, "/count_tokens"), bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if t.APIKey != "" {
		req.Header.Set("x-api-key", t.APIKey)
		req.Header.Set("Authorization", "Bearer "+t.APIKey)
	}
	resp, err := h.Engine.Client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var out struct {
		InputTokens *int `json:"input_tokens"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.InputTokens == nil {
		return 0, false
	}
	return *out.InputTokens, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}
