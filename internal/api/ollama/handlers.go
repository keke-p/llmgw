package ollama

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"llmgw/internal/chat"
	"llmgw/internal/gateway"
	"llmgw/internal/gwerr"
	"llmgw/internal/store"
	"llmgw/internal/upstream"
)

// Authenticator authenticates an API request.
type Authenticator func(r *http.Request) (*gateway.Principal, *gwerr.Error)

// Handler serves the Ollama-native API.
type Handler struct {
	Engine       *gateway.Engine
	Auth         Authenticator
	MaxBodyBytes int64
	ClientIP     func(r *http.Request) string
	Version      string

	mu    sync.Mutex
	cache map[int64]modelsCache // provider id -> /api/tags result
}

const (
	infoTimeout    = 5 * time.Second
	modelsCacheFor = 30 * time.Second
)

type modelsCache struct {
	at     time.Time
	models []modelInfo
}

// modelInfo is an entry of Ollama's /api/tags (also used for our response).
type modelInfo struct {
	Name       string          `json:"name"`
	Model      string          `json:"model"`
	ModifiedAt string          `json:"modified_at"`
	Size       int64           `json:"size"`
	Digest     string          `json:"digest"`
	Details    json.RawMessage `json:"details"`
}

// Register adds the routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.root)
	mux.HandleFunc("HEAD /{$}", h.root)
	mux.HandleFunc("GET /api/version", h.version)
	mux.HandleFunc("HEAD /api/version", h.version)

	mux.HandleFunc("POST /api/chat", h.chat)
	mux.HandleFunc("POST /api/generate", h.generate)
	mux.HandleFunc("POST /api/embed", h.embed)
	mux.HandleFunc("POST /api/embeddings", h.legacyEmbed)
	mux.HandleFunc("GET /api/tags", h.tags)
	mux.HandleFunc("POST /api/show", h.show)
	mux.HandleFunc("GET /api/ps", h.ps)

	// Model management is never relayed: a client key must not be able to
	// delete upstream models or start huge downloads.
	for _, p := range []string{"/api/pull", "/api/push", "/api/create", "/api/copy", "/api/delete", "/api/blobs/"} {
		mux.HandleFunc(p, h.forbidden)
	}
}

func (h *Handler) root(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "Ollama is running")
}

// version is unauthenticated: clients use it as a connectivity check.
func (h *Handler) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"version": h.Version})
}

func (h *Handler) forbidden(w http.ResponseWriter, r *http.Request) {
	WriteError(w, gwerr.New(http.StatusForbidden, gwerr.Permission, "model management endpoints are disabled on this gateway"))
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

// prepare authenticates, reads and parses the body, and resolves the alias.
func prepare[T any](h *Handler, w http.ResponseWriter, r *http.Request, parse func([]byte) (T, *gwerr.Error),
	model func(T) string) (T, *gateway.Principal, *store.Alias, gateway.Meta, bool) {
	var zero T
	p, ge := h.Auth(r)
	if ge != nil {
		h.Engine.RecordError(h.meta(r, nil), nil, chat.FormatOllama, "", ge)
		WriteError(w, ge)
		return zero, nil, nil, gateway.Meta{}, false
	}
	body, ge := h.readBody(w, r)
	if ge != nil {
		WriteError(w, ge)
		return zero, nil, nil, gateway.Meta{}, false
	}
	meta := h.meta(r, body)
	in, ge := parse(body)
	if ge != nil {
		h.Engine.RecordError(meta, p, chat.FormatOllama, "", ge)
		WriteError(w, ge)
		return zero, nil, nil, gateway.Meta{}, false
	}
	alias, ge := h.Engine.ResolveAlias(r.Context(), p, model(in))
	if ge != nil {
		h.Engine.RecordError(meta, p, chat.FormatOllama, model(in), ge)
		WriteError(w, ge)
		return zero, nil, nil, gateway.Meta{}, false
	}
	return in, p, alias, meta, true
}

func chatModel(in *upstream.Inbound) string       { return in.Req.Model }
func embedModel(in *upstream.EmbedInbound) string { return in.Req.Model }

func (h *Handler) chat(w http.ResponseWriter, r *http.Request) { h.chatLike(w, r, ParseChat, false) }
func (h *Handler) generate(w http.ResponseWriter, r *http.Request) {
	h.chatLike(w, r, ParseGenerate, true)
}

func (h *Handler) chatLike(w http.ResponseWriter, r *http.Request, parse func([]byte) (*upstream.Inbound, *gwerr.Error), generate bool) {
	in, p, alias, meta, ok := prepare(h, w, r, parse, chatModel)
	if !ok {
		return
	}
	h.Engine.Chat(w, r, &gateway.ChatCall{Meta: meta, Inbound: in, Alias: alias, Principal: p,
		Sink: newSink(displayName(alias.Name), generate, h.Engine.Clock)})
}

func (h *Handler) embed(w http.ResponseWriter, r *http.Request) { h.embedLike(w, r, ParseEmbed) }
func (h *Handler) legacyEmbed(w http.ResponseWriter, r *http.Request) {
	h.embedLike(w, r, ParseLegacyEmbed)
}

func (h *Handler) embedLike(w http.ResponseWriter, r *http.Request, parse func([]byte) (*upstream.EmbedInbound, *gwerr.Error)) {
	in, p, alias, meta, ok := prepare(h, w, r, parse, embedModel)
	if !ok {
		return
	}
	h.Engine.Embed(w, r, &gateway.EmbedCall{Meta: meta, Inbound: in, Alias: alias, Principal: p,
		Sink: &embedSink{alias: displayName(alias.Name), legacy: in.Legacy, clock: h.Engine.Clock, start: h.Engine.Clock.Now()}})
}

// displayName adds the ":latest" tag Ollama clients expect.
func displayName(alias string) string {
	if strings.Contains(alias, ":") {
		return alias
	}
	return alias + ":latest"
}

// ---- model information -----------------------------------------------------

var defaultDetails = json.RawMessage(`{"parent_model":"","format":"gguf","family":"","families":[],"parameter_size":"","quantization_level":""}`)

// providerModels returns the upstream Ollama model list (cached briefly).
func (h *Handler) providerModels(ctx context.Context, p *store.Provider) []modelInfo {
	now := h.Engine.Clock.Now()
	h.mu.Lock()
	if c, ok := h.cache[p.ID]; ok && now.Sub(c.at) < modelsCacheFor {
		h.mu.Unlock()
		return c.models
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()
	b, err := upstream.Call(ctx, h.Engine.Client, p, http.MethodGet, "/api/tags", nil)
	var out struct {
		Models []modelInfo `json:"models"`
	}
	if err == nil {
		_ = json.Unmarshal(b, &out)
	} else {
		h.Engine.Log.Warn("fetch upstream model list", "provider", p.Name, "err", err)
	}
	h.mu.Lock()
	if h.cache == nil {
		h.cache = map[int64]modelsCache{}
	}
	h.cache[p.ID] = modelsCache{at: now, models: out.Models}
	h.mu.Unlock()
	return out.Models
}

func findModel(models []modelInfo, name string) *modelInfo {
	for _, cand := range store.CandidateNames(name) {
		for i := range models {
			if models[i].Name == cand || models[i].Model == cand {
				return &models[i]
			}
		}
	}
	return nil
}

// aliasInfo builds the /api/tags entry for an alias, using the upstream Ollama
// model's metadata when available and harmless defaults otherwise.
func (h *Handler) aliasInfo(ctx context.Context, a *store.Alias) modelInfo {
	name := displayName(a.Name)
	sum := sha256.Sum256([]byte("llmgw:" + a.Name))
	info := modelInfo{Name: name, Model: name, ModifiedAt: a.CreatedAt.UTC().Format(time.RFC3339Nano),
		Digest: hex.EncodeToString(sum[:]), Details: defaultDetails}
	if len(a.Targets) > 0 && a.Targets[0].Provider.Type == store.ProviderOllama {
		t := a.Targets[0]
		if m := findModel(h.providerModels(ctx, t.Provider), t.UpstreamModel); m != nil {
			info.Size, info.Digest, info.ModifiedAt = m.Size, m.Digest, m.ModifiedAt
			if len(m.Details) > 0 {
				info.Details = m.Details
			}
		}
	}
	return info
}

// allowedAliases returns the enabled aliases (with targets) the key may use.
func (h *Handler) allowedAliases(ctx context.Context, p *gateway.Principal) ([]*store.Alias, error) {
	list, err := h.Engine.Store.ListAliases(ctx, true)
	if err != nil {
		return nil, err
	}
	var out []*store.Alias
	for _, a := range list {
		if !p.AllowsAlias(a.Name) {
			continue
		}
		full, err := h.Engine.Store.ResolveAlias(ctx, a.Name)
		if err != nil {
			continue
		}
		out = append(out, full)
	}
	return out, nil
}

func (h *Handler) tags(w http.ResponseWriter, r *http.Request) {
	p, ge := h.Auth(r)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	aliases, err := h.allowedAliases(r.Context(), p)
	if err != nil {
		WriteError(w, gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error"))
		return
	}
	models := []modelInfo{}
	for _, a := range aliases {
		models = append(models, h.aliasInfo(r.Context(), a))
	}
	writeJSON(w, map[string]any{"models": models})
}

func (h *Handler) show(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		Model   string `json:"model"`
		Name    string `json:"name"`
		Verbose bool   `json:"verbose"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		WriteError(w, gwerr.BadRequest("invalid JSON body: %v", err))
		return
	}
	a, ge := h.Engine.ResolveAlias(r.Context(), p, firstNonEmpty(req.Model, req.Name))
	if ge != nil {
		WriteError(w, ge)
		return
	}
	if len(a.Targets) > 0 && a.Targets[0].Provider.Type == store.ProviderOllama {
		t := a.Targets[0]
		ctx, cancel := context.WithTimeout(r.Context(), infoTimeout)
		defer cancel()
		q, _ := json.Marshal(map[string]any{"model": t.UpstreamModel, "verbose": req.Verbose})
		if b, err := upstream.Call(ctx, h.Engine.Client, t.Provider, http.MethodPost, "/api/show", q); err == nil && json.Valid(b) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
			return
		} else if err != nil {
			h.Engine.Log.Warn("upstream /api/show failed; returning minimal info", "provider", t.Provider.Name, "err", err)
		}
	}
	info := h.aliasInfo(r.Context(), a)
	writeJSON(w, map[string]any{
		"license":      "",
		"modelfile":    "# llmgw alias " + a.Name + "\n",
		"parameters":   "",
		"template":     "{{ .Prompt }}",
		"details":      info.Details,
		"model_info":   map[string]any{},
		"capabilities": []string{"completion", "tools"},
		"modified_at":  info.ModifiedAt,
	})
}

// ps reports running upstream Ollama models under their alias names.
func (h *Handler) ps(w http.ResponseWriter, r *http.Request) {
	p, ge := h.Auth(r)
	if ge != nil {
		WriteError(w, ge)
		return
	}
	aliases, err := h.allowedAliases(r.Context(), p)
	if err != nil {
		WriteError(w, gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error"))
		return
	}
	running := map[int64][]map[string]json.RawMessage{}
	fetched := map[int64]bool{}
	out := []map[string]json.RawMessage{}
	for _, a := range aliases {
		for _, t := range a.Targets {
			if t.Provider.Type != store.ProviderOllama {
				continue
			}
			if !fetched[t.Provider.ID] {
				fetched[t.Provider.ID] = true
				ctx, cancel := context.WithTimeout(r.Context(), infoTimeout)
				b, err := upstream.Call(ctx, h.Engine.Client, t.Provider, http.MethodGet, "/api/ps", nil)
				cancel()
				if err == nil {
					var res struct {
						Models []map[string]json.RawMessage `json:"models"`
					}
					_ = json.Unmarshal(b, &res)
					running[t.Provider.ID] = res.Models
				}
			}
			for _, m := range running[t.Provider.ID] {
				var name string
				_ = json.Unmarshal(m["name"], &name)
				if !matches(name, t.UpstreamModel) {
					continue
				}
				entry := make(map[string]json.RawMessage, len(m))
				for k, v := range m {
					entry[k] = v
				}
				dn, _ := json.Marshal(displayName(a.Name))
				entry["name"], entry["model"] = dn, dn
				out = append(out, entry)
				break
			}
		}
	}
	writeJSON(w, map[string]any{"models": out})
}

func matches(running, model string) bool {
	for _, c := range store.CandidateNames(model) {
		if running == c {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}
