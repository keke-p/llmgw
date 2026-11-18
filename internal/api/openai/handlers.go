package openai

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"llmgw/internal/chat"
	"llmgw/internal/gateway"
	"llmgw/internal/gwerr"
	"llmgw/internal/upstream"
)

// Authenticator authenticates an API request.
type Authenticator func(r *http.Request) (*gateway.Principal, *gwerr.Error)

// Handler serves the OpenAI-compatible API.
type Handler struct {
	Engine       *gateway.Engine
	Auth         Authenticator
	MaxBodyBytes int64
	ClientIP     func(r *http.Request) string
}

// Register adds the routes to mux. Like LiteLLM, the endpoints are also served
// without the /v1 prefix, so clients configured with base URL "http://host:4000"
// keep working after migrating.
func (h *Handler) Register(mux *http.ServeMux) {
	for _, prefix := range []string{"/v1", ""} {
		mux.HandleFunc("POST "+prefix+"/chat/completions", h.chatCompletions)
		mux.HandleFunc("POST "+prefix+"/completions", h.completions)
		mux.HandleFunc("POST "+prefix+"/embeddings", h.embeddings)
		mux.HandleFunc("GET "+prefix+"/models", h.models)
		mux.HandleFunc("GET "+prefix+"/models/{model...}", h.model)
	}
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

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	h.chatLike(w, r, ParseChatRequest, false)
}

func (h *Handler) completions(w http.ResponseWriter, r *http.Request) {
	h.chatLike(w, r, ParseCompletionRequest, true)
}

func (h *Handler) chatLike(w http.ResponseWriter, r *http.Request, parse func([]byte) (*upstream.Inbound, *gwerr.Error), completion bool) {
	p, ge := h.Auth(r)
	if ge != nil {
		h.Engine.RecordError(h.meta(r, nil), nil, chat.FormatOpenAI, "", ge)
		WriteError(w, ge)
		return
	}
	body, ge := h.readBody(w, r)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	meta := h.meta(r, body)
	in, ge := parse(body)
	if ge != nil {
		h.Engine.RecordError(meta, p, chat.FormatOpenAI, "", ge)
		WriteError(w, ge)
		return
	}
	alias, ge := h.Engine.ResolveAlias(r.Context(), p, in.Req.Model)
	if ge != nil {
		h.Engine.RecordError(meta, p, chat.FormatOpenAI, in.Req.Model, ge)
		WriteError(w, ge)
		return
	}
	sink := newChatSink(alias.Name, in.Req.IncludeUsage, h.Engine.Clock.Now().Unix())
	if completion {
		sink.completion = true
		sink.id = "cmpl-" + chat.RandomString(29)
	}
	h.Engine.Chat(w, r, &gateway.ChatCall{Meta: meta, Inbound: in, Alias: alias, Principal: p, Sink: sink})
}

func (h *Handler) embeddings(w http.ResponseWriter, r *http.Request) {
	p, ge := h.Auth(r)
	if ge != nil {
		h.Engine.RecordError(h.meta(r, nil), nil, chat.FormatOpenAI, "", ge)
		WriteError(w, ge)
		return
	}
	body, ge := h.readBody(w, r)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	meta := h.meta(r, body)
	in, enc, ge := ParseEmbedRequest(body)
	if ge != nil {
		h.Engine.RecordError(meta, p, chat.FormatOpenAI, "", ge)
		WriteError(w, ge)
		return
	}
	alias, ge := h.Engine.ResolveAlias(r.Context(), p, in.Req.Model)
	if ge != nil {
		h.Engine.RecordError(meta, p, chat.FormatOpenAI, in.Req.Model, ge)
		WriteError(w, ge)
		return
	}
	h.Engine.Embed(w, r, &gateway.EmbedCall{Meta: meta, Inbound: in, Alias: alias, Principal: p,
		Sink: &embedSink{alias: alias.Name, encodingFormat: enc}})
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// models lists the aliases the key may use.
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	p, ge := h.Auth(r)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	aliases, err := h.Engine.Store.ListAliases(r.Context(), true)
	if err != nil {
		WriteError(w, gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error"))
		return
	}
	data := []modelObject{}
	for _, a := range aliases {
		if !p.AllowsAlias(a.Name) {
			continue
		}
		data = append(data, modelObject{ID: a.Name, Object: "model", Created: a.CreatedAt.Unix(), OwnedBy: "llmgw"})
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

func (h *Handler) model(w http.ResponseWriter, r *http.Request) {
	p, ge := h.Auth(r)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	name := r.PathValue("model")
	a, ge := h.Engine.ResolveAlias(r.Context(), p, name)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	writeJSON(w, modelObject{ID: a.Name, Object: "model", Created: a.CreatedAt.Unix(), OwnedBy: "llmgw"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}
