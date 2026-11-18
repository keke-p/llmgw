package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"llmgw/internal/probe"
	"llmgw/internal/store"
	"llmgw/internal/timeouts"
)

func (a *API) registerProviders(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/providers", a.protected(a.listProviders))
	mux.HandleFunc("POST /admin/api/providers", a.protected(a.createProvider))
	mux.HandleFunc("GET /admin/api/providers/{id}", a.protected(a.getProvider))
	mux.HandleFunc("PATCH /admin/api/providers/{id}", a.protected(a.updateProvider))
	mux.HandleFunc("DELETE /admin/api/providers/{id}", a.protected(a.deleteProvider))
	mux.HandleFunc("POST /admin/api/providers/{id}/test", a.protected(a.testProvider))
}

type valueJSON struct {
	MS     int64  `json:"ms"`
	Source string `json:"source"`
}

func val(v timeouts.Value) valueJSON { return valueJSON{MS: v.D.Milliseconds(), Source: v.Source} }

type healthJSON struct {
	Status    string     `json:"status"`
	Error     string     `json:"error,omitempty"`
	CheckedAt *time.Time `json:"checked_at"`
}

type providerJSON struct {
	ID                int64                `json:"id"`
	Name              string               `json:"name"`
	Type              string               `json:"type"`
	BaseURL           string               `json:"base_url"`
	HasAPIKey         bool                 `json:"has_api_key"`
	SupportsAnthropic bool                 `json:"supports_anthropic"`
	ConnectTimeoutMS  *int64               `json:"connect_timeout_ms"`
	QueueTimeoutMS    *int64               `json:"queue_timeout_ms"`
	TTFTTimeoutMS     *int64               `json:"ttft_timeout_ms"`
	IdleTimeoutMS     *int64               `json:"idle_timeout_ms"`
	MaxDurationMS     *int64               `json:"max_duration_ms"`
	MaxConcurrency    *int                 `json:"max_concurrency"`
	Enabled           bool                 `json:"enabled"`
	Health            healthJSON           `json:"health"`
	Models            []string             `json:"models"`
	Aliases           []string             `json:"aliases"`
	Effective         map[string]valueJSON `json:"effective"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
}

func msOrNil(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	ms := d.Milliseconds()
	return &ms
}

func (a *API) providerJSON(ctx context.Context, p *store.Provider, s store.Settings) providerJSON {
	j := providerJSON{
		ID: p.ID, Name: p.Name, Type: p.Type, BaseURL: p.BaseURL, HasAPIKey: p.APIKey != "",
		SupportsAnthropic: p.SupportsAnthropic, ConnectTimeoutMS: msOrNil(p.ConnectTimeout),
		QueueTimeoutMS: msOrNil(p.QueueTimeout), TTFTTimeoutMS: msOrNil(p.TTFTTimeout),
		IdleTimeoutMS: msOrNil(p.IdleTimeout), MaxDurationMS: msOrNil(p.MaxDuration),
		MaxConcurrency: intOrNil(p.MaxConcurrency), Enabled: p.Enabled,
		Health:    healthJSON{Status: p.HealthStatus, Error: p.HealthError, CheckedAt: utc(p.HealthCheckedAt)},
		Models:    []string{},
		CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(),
	}
	if p.HealthStatus == "" {
		j.Health.Status = "unknown"
	}
	if p.ModelsJSON != "" {
		_ = json.Unmarshal([]byte(p.ModelsJSON), &j.Models)
	}
	j.Aliases, _ = a.Store.ProviderAliases(ctx, p.ID)
	e := timeouts.Resolve(s, p, nil)
	j.Effective = map[string]valueJSON{
		"connect_timeout": val(e.Connect), "queue_timeout": val(e.Queue), "ttft_timeout": val(e.TTFT),
		"idle_timeout": val(e.Idle), "max_duration": val(e.MaxDuration),
	}
	return j
}

func (a *API) settings(ctx context.Context) store.Settings {
	s, err := a.Store.Settings(ctx)
	if err != nil {
		return store.DefaultSettings()
	}
	return s
}

func (a *API) listProviders(w http.ResponseWriter, r *http.Request, _ *actor) {
	ps, err := a.Store.ListProviders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s := a.settings(r.Context())
	out := make([]providerJSON, 0, len(ps))
	for _, p := range ps {
		out = append(out, a.providerJSON(r.Context(), p, s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (a *API) loadProvider(w http.ResponseWriter, r *http.Request) (*store.Provider, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "provider not found")
		return nil, false
	}
	p, err := a.Store.GetProvider(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "provider not found")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, false
	}
	return p, true
}

func (a *API) getProvider(w http.ResponseWriter, r *http.Request, _ *actor) {
	if p, ok := a.loadProvider(w, r); ok {
		writeJSON(w, http.StatusOK, a.providerJSON(r.Context(), p, a.settings(r.Context())))
	}
}

var providerFields = map[string]bool{
	"name": true, "type": true, "base_url": true, "api_key": true, "supports_anthropic": true,
	"connect_timeout_ms": true, "queue_timeout_ms": true, "ttft_timeout_ms": true, "idle_timeout_ms": true,
	"max_duration_ms": true, "max_concurrency": true, "enabled": true,
}

func isNull(b json.RawMessage) bool { return strings.TrimSpace(string(b)) == "null" }

// optDuration parses an inheritable duration in ms: null = inherit.
func optDuration(name string, v json.RawMessage) (*time.Duration, error) {
	if isNull(v) {
		return nil, nil
	}
	var ms int64
	if err := json.Unmarshal(v, &ms); err != nil || ms < 0 {
		return nil, errors.New(name + " must be a non-negative integer (ms) or null")
	}
	d := time.Duration(ms) * time.Millisecond
	return &d, nil
}

// applyProviderInput applies fields; setKey reports whether api_key was given.
func applyProviderInput(p *store.Provider, raw map[string]json.RawMessage) (setKey bool, changed []string, err error) {
	for f := range raw {
		if !providerFields[f] {
			return false, nil, errors.New("unknown field " + strconv.Quote(f))
		}
	}
	for f, v := range raw {
		switch f {
		case "name":
			var s string
			if json.Unmarshal(v, &s) != nil || strings.TrimSpace(s) == "" || len(s) > 100 {
				return false, nil, errors.New("name must be 1-100 characters")
			}
			p.Name = strings.TrimSpace(s)
		case "type":
			var s string
			_ = json.Unmarshal(v, &s)
			if s != store.ProviderOllama && s != store.ProviderOpenAICompatible {
				return false, nil, errors.New("type must be ollama or openai_compatible")
			}
			p.Type = s
		case "base_url":
			var s string
			_ = json.Unmarshal(v, &s)
			s = strings.TrimRight(strings.TrimSpace(s), "/")
			u, err := url.Parse(s)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return false, nil, errors.New("base_url must be an http(s) URL")
			}
			p.BaseURL = s
		case "api_key":
			setKey = true
			p.APIKey = ""
			if !isNull(v) {
				if json.Unmarshal(v, &p.APIKey) != nil {
					return false, nil, errors.New("api_key must be a string or null")
				}
			}
		case "supports_anthropic", "enabled":
			var b bool
			if json.Unmarshal(v, &b) != nil {
				return false, nil, errors.New(f + " must be a boolean")
			}
			if f == "enabled" {
				p.Enabled = b
			} else {
				p.SupportsAnthropic = b
			}
		case "max_concurrency":
			n := 0
			if !isNull(v) && (json.Unmarshal(v, &n) != nil || n < 0) {
				return false, nil, errors.New("max_concurrency must be a non-negative integer or null")
			}
			p.MaxConcurrency = n
		default: // *_timeout_ms, max_duration_ms
			d, err := optDuration(f, v)
			if err != nil {
				return false, nil, err
			}
			switch f {
			case "connect_timeout_ms":
				p.ConnectTimeout = d
			case "queue_timeout_ms":
				p.QueueTimeout = d
			case "ttft_timeout_ms":
				p.TTFTTimeout = d
			case "idle_timeout_ms":
				p.IdleTimeout = d
			case "max_duration_ms":
				p.MaxDuration = d
			}
		}
		if f != "api_key" {
			changed = append(changed, f)
		} else {
			changed = append(changed, "api_key(redacted)")
		}
	}
	return setKey, changed, nil
}

func storeErr(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", what+" with the same name already exists")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", what+" not found")
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (a *API) createProvider(w http.ResponseWriter, r *http.Request, act *actor) {
	raw, ok := decodeObject(w, r)
	if !ok {
		return
	}
	for _, req := range []string{"name", "type", "base_url"} {
		if _, ok := raw[req]; !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", req+" is required")
			return
		}
	}
	p := &store.Provider{Enabled: true}
	if _, _, err := applyProviderInput(p, raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := a.Store.CreateProvider(r.Context(), p); err != nil {
		storeErr(w, err, "provider")
		return
	}
	a.audit(r, act.Name, "provider.create", "provider", strconv.FormatInt(p.ID, 10), map[string]any{"name": p.Name, "base_url": p.BaseURL})
	// Initial connectivity check; detect Anthropic support unless set explicitly.
	_, explicit := raw["supports_anthropic"]
	res := a.runCheck(r.Context(), p, !explicit)
	if !explicit && res.Anthropic != nil && *res.Anthropic {
		_ = a.Store.SetProviderAnthropic(r.Context(), p.ID, true)
	}
	p, _ = a.Store.GetProvider(r.Context(), p.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"provider": a.providerJSON(r.Context(), p, a.settings(r.Context())), "check": res})
}

func (a *API) updateProvider(w http.ResponseWriter, r *http.Request, act *actor) {
	p, ok := a.loadProvider(w, r)
	if !ok {
		return
	}
	raw, ok := decodeObject(w, r)
	if !ok {
		return
	}
	setKey, changed, err := applyProviderInput(p, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := a.Store.UpdateProvider(r.Context(), p, setKey); err != nil {
		storeErr(w, err, "provider")
		return
	}
	a.audit(r, act.Name, "provider.update", "provider", strconv.FormatInt(p.ID, 10), map[string]any{"name": p.Name, "fields": changed})
	p, _ = a.Store.GetProvider(r.Context(), p.ID)
	writeJSON(w, http.StatusOK, a.providerJSON(r.Context(), p, a.settings(r.Context())))
}

func (a *API) deleteProvider(w http.ResponseWriter, r *http.Request, act *actor) {
	p, ok := a.loadProvider(w, r)
	if !ok {
		return
	}
	if err := a.Store.DeleteProvider(r.Context(), p.ID); err != nil {
		if errors.Is(err, store.ErrInUse) {
			names, _ := a.Store.ProviderAliases(r.Context(), p.ID)
			writeError(w, http.StatusConflict, "in_use", "provider is used by aliases: "+strings.Join(names, ", "))
			return
		}
		storeErr(w, err, "provider")
		return
	}
	a.audit(r, act.Name, "provider.delete", "provider", strconv.FormatInt(p.ID, 10), map[string]any{"name": p.Name})
	w.WriteHeader(http.StatusNoContent)
}

// runCheck probes a provider and stores the health result and model list.
func (a *API) runCheck(ctx context.Context, p *store.Provider, detectAnthropic bool) probe.Result {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res := probe.Check(cctx, a.HTTPClient, p, detectAnthropic)
	status, models := "error", ""
	if res.OK {
		status = "ok"
		b, _ := json.Marshal(res.Models)
		models = string(b)
	}
	_ = a.Store.SetProviderHealth(ctx, p.ID, status, res.Error, models, a.Clock.Now())
	return res
}

// testProvider runs a connectivity check now. Anthropic support is detected and
// reported; pass {"apply_anthropic": true} to store the detected value.
func (a *API) testProvider(w http.ResponseWriter, r *http.Request, act *actor) {
	p, ok := a.loadProvider(w, r)
	if !ok {
		return
	}
	var req struct {
		ApplyAnthropic bool `json:"apply_anthropic"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	res := a.runCheck(r.Context(), p, true)
	if req.ApplyAnthropic && res.Anthropic != nil && *res.Anthropic != p.SupportsAnthropic {
		_ = a.Store.SetProviderAnthropic(r.Context(), p.ID, *res.Anthropic)
		a.audit(r, act.Name, "provider.update", "provider", strconv.FormatInt(p.ID, 10),
			map[string]any{"name": p.Name, "fields": []string{"supports_anthropic"}, "detected": *res.Anthropic})
	}
	p, _ = a.Store.GetProvider(r.Context(), p.ID)
	writeJSON(w, http.StatusOK, map[string]any{"provider": a.providerJSON(r.Context(), p, a.settings(r.Context())), "check": res})
}
