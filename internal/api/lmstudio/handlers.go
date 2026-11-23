// Package lmstudio serves an LM Studio compatible model list
// (GET /api/v1/models), so clients that discover models the LM Studio way
// (e.g. OpenCode with provider key "lmstudio") list llmgw's aliases with their
// context length and capabilities. Inference itself stays on the OpenAI
// compatible API.
package lmstudio

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"llmgw/internal/api/openai"
	"llmgw/internal/gateway"
	"llmgw/internal/gwerr"
	"llmgw/internal/params"
	"llmgw/internal/store"
	"llmgw/internal/upstream"
)

// Authenticator authenticates an API request.
type Authenticator func(r *http.Request) (*gateway.Principal, *gwerr.Error)

const (
	listTimeout = 10 * time.Second // whole request; OpenCode gives up after 15s
	callTimeout = 5 * time.Second  // one upstream metadata call
	showTTL     = 5 * time.Minute  // Ollama /api/show (rarely changes)
	listTTL     = 30 * time.Second // Ollama /api/ps, LM Studio /api/v1/models
	failTTL     = 30 * time.Second // retry interval after an upstream failure
)

// Handler serves GET /api/v1/models.
type Handler struct {
	Engine *gateway.Engine
	Auth   Authenticator

	cache    cache
	mu       sync.Mutex
	observed map[string]int // provider/model -> context length last seen in Ollama /api/ps
}

// Register adds the route to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/models", h.models)
}

// Model is one entry of the list. Field types follow LM Studio's REST API;
// loaded_instances and max_context_length are always present because clients
// validate them strictly.
type Model map[string]any

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	p, ge := h.Auth(r)
	if ge != nil {
		openai.WriteError(w, ge)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), listTimeout)
	defer cancel()
	list, err := h.Engine.Store.ListAliases(ctx, true)
	if err != nil {
		openai.WriteError(w, gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error"))
		return
	}
	var aliases []*store.Alias
	for _, a := range list {
		if !p.AllowsAlias(a.Name) {
			continue
		}
		full, err := h.Engine.Store.ResolveAlias(ctx, a.Name)
		if err != nil || len(full.Targets) == 0 {
			continue // no enabled upstream: the alias cannot be used right now
		}
		aliases = append(aliases, full)
	}
	out := make([]Model, len(aliases))
	var wg sync.WaitGroup
	for i, a := range aliases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = h.Describe(ctx, a)
		}()
	}
	wg.Wait()
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(map[string]any{"models": out})
	_, _ = w.Write(b)
}

// Describe builds the list entry for an alias from its first target.
func (h *Handler) Describe(ctx context.Context, a *store.Alias) Model {
	var m Model
	if len(a.Targets) > 0 {
		t := a.Targets[0]
		switch t.Provider.Type {
		case store.ProviderOllama:
			m = h.fromOllama(ctx, a, t)
		case store.ProviderOpenAICompatible:
			m = h.fromLMStudio(ctx, a, t)
		}
	}
	if m == nil {
		m = baseModel(a.Name)
	}
	applyOverrides(m, a.ModelInfo)
	return m
}

// baseModel is an entry with no upstream metadata. Tool use is assumed (an
// entry without it is useless to agentic clients); override per alias if not.
func baseModel(name string) Model {
	return Model{
		"type":               "llm",
		"key":                name,
		"display_name":       name,
		"architecture":       nil,
		"loaded_instances":   []any{},
		"max_context_length": nil,
		"capabilities":       map[string]any{"vision": false, "trained_for_tool_use": true},
	}
}

func applyOverrides(m Model, o store.ModelInfo) {
	if o.ContextLength != nil {
		m["max_context_length"] = *o.ContextLength
		// Clients prefer loaded instances' context length; drop them so the
		// override wins.
		m["loaded_instances"] = []any{}
	}
	caps, _ := m["capabilities"].(map[string]any)
	if o.Tools != nil {
		caps["trained_for_tool_use"] = *o.Tools
	}
	if o.Vision != nil {
		caps["vision"] = *o.Vision
	}
}

// ---- Ollama ------------------------------------------------------------------

type ollamaShow struct {
	Parameters   string         `json:"parameters"`
	ModelInfo    map[string]any `json:"model_info"`
	Capabilities []string       `json:"capabilities"`
	Details      struct {
		Family string `json:"family"`
	} `json:"details"`
}

func (h *Handler) fromOllama(ctx context.Context, a *store.Alias, t store.Target) Model {
	m := baseModel(a.Name)
	caps := m["capabilities"].(map[string]any)

	v, _ := h.cache.get("show\x00"+strconv.FormatInt(t.Provider.ID, 10)+"\x00"+t.UpstreamModel, h.now(), showTTL,
		func() (any, bool) {
			cctx, cancel := context.WithTimeout(ctx, callTimeout)
			defer cancel()
			q, _ := json.Marshal(map[string]any{"model": t.UpstreamModel})
			b, err := upstream.Call(cctx, h.Engine.Client, t.Provider, http.MethodPost, "/api/show", q)
			if err != nil {
				h.Engine.Log.Warn("lmstudio models: upstream /api/show failed", "provider", t.Provider.Name, "model", t.UpstreamModel, "err", err)
				return nil, false
			}
			var s ollamaShow
			if err := json.Unmarshal(b, &s); err != nil {
				return nil, false
			}
			return &s, true
		})
	show, _ := v.(*ollamaShow)

	var trained int
	if show != nil {
		if len(show.Capabilities) > 0 && !slices.Contains(show.Capabilities, "completion") {
			m["type"] = "embedding"
		}
		caps["trained_for_tool_use"] = slices.Contains(show.Capabilities, "tools")
		caps["vision"] = slices.Contains(show.Capabilities, "vision")
		arch, _ := show.ModelInfo["general.architecture"].(string)
		if arch == "" {
			arch = show.Details.Family
		}
		if arch != "" {
			m["architecture"] = arch
			trained = toInt(show.ModelInfo[arch+".context_length"])
		}
	}

	// Context length actually used, most specific first: the alias's num_ctx
	// parameter (llmgw sends it), the Modelfile's num_ctx, the context length
	// Ollama was seen running the model with (reflects OLLAMA_CONTEXT_LENGTH),
	// and finally the model's trained maximum.
	ctxLen := aliasNumCtx(a)
	if ctxLen == 0 && show != nil {
		ctxLen = parameterInt(show.Parameters, "num_ctx")
	}
	if ctxLen == 0 {
		ctxLen = h.runningContext(ctx, t)
	}
	if trained > 0 && (ctxLen == 0 || ctxLen > trained) {
		ctxLen = trained // Ollama clamps num_ctx to the trained length
	}
	if ctxLen > 0 {
		m["max_context_length"] = ctxLen
	}
	return m
}

// aliasNumCtx returns num_ctx from the alias parameter rules (override first).
func aliasNumCtx(a *store.Alias) int {
	p := params.Parse(a.Params)
	if p == nil {
		return 0
	}
	if n := toInt(p.Overrides["num_ctx"]); n > 0 {
		return n
	}
	return toInt(p.Defaults["num_ctx"])
}

// parameterInt reads an integer from Ollama's /api/show "parameters" text
// ("num_ctx    32768\nstop    ...").
func parameterInt(text, name string) int {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == name {
			if n, err := strconv.Atoi(strings.Trim(f[1], `"`)); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// runningContext returns the context length Ollama runs the model with,
// according to /api/ps. The last value seen is remembered, so the reported
// length does not flip when the model is unloaded.
func (h *Handler) runningContext(ctx context.Context, t store.Target) int {
	pid := strconv.FormatInt(t.Provider.ID, 10)
	v, _ := h.cache.get("ps\x00"+pid, h.now(), listTTL, func() (any, bool) {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		b, err := upstream.Call(cctx, h.Engine.Client, t.Provider, http.MethodGet, "/api/ps", nil)
		if err != nil {
			return nil, false
		}
		var res struct {
			Models []struct {
				Name          string `json:"name"`
				Model         string `json:"model"`
				ContextLength int    `json:"context_length"`
			} `json:"models"`
		}
		if json.Unmarshal(b, &res) != nil {
			return nil, false
		}
		running := map[string]int{}
		for _, m := range res.Models {
			if m.ContextLength > 0 {
				running[m.Name] = m.ContextLength
				running[m.Model] = m.ContextLength
			}
		}
		return running, true
	})
	ps, _ := v.(map[string]int)

	key := pid + "\x00" + t.UpstreamModel
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cand := range store.CandidateNames(t.UpstreamModel) {
		if n := ps[cand]; n > 0 {
			if h.observed == nil {
				h.observed = map[string]int{}
			}
			h.observed[key] = n
			return n
		}
	}
	return h.observed[key]
}

// ---- LM Studio / OpenAI compatible --------------------------------------------

// fromLMStudio relays the upstream's LM Studio model entry under the alias
// name. Upstreams without the LM Studio API (llama.cpp, vLLM, ...) get the
// base entry.
func (h *Handler) fromLMStudio(ctx context.Context, a *store.Alias, t store.Target) Model {
	v, _ := h.cache.get("lms\x00"+strconv.FormatInt(t.Provider.ID, 10), h.now(), listTTL, func() (any, bool) {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		u := upstream.ServerRoot(t.Provider.BaseURL) + "/api/v1/models"
		b, err := upstream.CallURL(cctx, h.Engine.Client, t.Provider, http.MethodGet, u, nil)
		if err != nil {
			return nil, false
		}
		var res struct {
			Models []map[string]any `json:"models"`
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		if d.Decode(&res) != nil {
			return nil, false
		}
		return res.Models, true
	})
	list, _ := v.([]map[string]any)

	for _, up := range list {
		if matchesLMS(up, t.UpstreamModel) {
			return sanitize(up, a.Name)
		}
	}
	return nil
}

func matchesLMS(up map[string]any, model string) bool {
	if k, _ := up["key"].(string); k == model {
		return true
	}
	insts, _ := up["loaded_instances"].([]any)
	for _, i := range insts {
		if o, ok := i.(map[string]any); ok {
			if id, _ := o["id"].(string); id == model {
				return true
			}
		}
	}
	return false
}

// sanitize copies an upstream entry, renames it to the alias and coerces the
// fields clients validate strictly to the expected types.
func sanitize(up map[string]any, alias string) Model {
	m := Model{}
	for k, v := range up {
		m[k] = v
	}
	m["key"] = alias
	if t, _ := up["type"].(string); t != "embedding" {
		m["type"] = "llm" // never relay e.g. "decision": clients treat it specially
	}
	if dn, ok := up["display_name"].(string); !ok || dn == "" {
		m["display_name"] = alias
	}
	if v, ok := up["architecture"]; ok {
		if _, isStr := v.(string); !isStr {
			m["architecture"] = nil
		}
	}
	insts := []any{}
	if raw, ok := up["loaded_instances"].([]any); ok {
		for _, i := range raw {
			o, _ := i.(map[string]any)
			cfg, _ := o["config"].(map[string]any)
			n := toInt(cfg["context_length"])
			if n <= 0 {
				continue
			}
			// Copy: up is shared through the cache.
			inst, c := map[string]any{}, map[string]any{}
			for k, v := range o {
				inst[k] = v
			}
			for k, v := range cfg {
				c[k] = v
			}
			c["context_length"] = n
			inst["config"] = c
			insts = append(insts, inst)
		}
	}
	m["loaded_instances"] = insts
	if n := toInt(up["max_context_length"]); n > 0 {
		m["max_context_length"] = n
	} else {
		m["max_context_length"] = nil
	}
	caps := map[string]any{"vision": false, "trained_for_tool_use": true}
	if c, ok := up["capabilities"].(map[string]any); ok {
		if b, ok := c["vision"].(bool); ok {
			caps["vision"] = b
		}
		if b, ok := c["trained_for_tool_use"].(bool); ok {
			caps["trained_for_tool_use"] = b
		}
	} else if b, ok := up["has_vision_adapter"].(bool); ok {
		caps["vision"] = b
	}
	m["capabilities"] = caps
	return m
}

// ---- helpers -----------------------------------------------------------------

func (h *Handler) now() time.Time { return h.Engine.Clock.Now() }

// toInt converts a JSON number (float64 or json.Number) to a positive int; 0
// for anything else.
func toInt(v any) int {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int:
		f = float64(x)
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return 0
		}
	default:
		return 0
	}
	if f <= 0 || f > math.MaxInt32 {
		return 0
	}
	return int(f)
}

// cache memoizes upstream metadata. Concurrent callers of a missing key wait
// for a single fetch.
type cache struct {
	mu sync.Mutex
	m  map[string]*cacheEntry
}

type cacheEntry struct {
	done    chan struct{}
	expires time.Time
	val     any
}

// get returns the cached value for key, fetching it when missing or expired.
// Successful results live for ttl, failures for failTTL (with a nil value).
func (c *cache) get(key string, now time.Time, ttl time.Duration, fetch func() (any, bool)) (any, bool) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*cacheEntry{}
	}
	if e, ok := c.m[key]; ok {
		select {
		case <-e.done:
			if now.Before(e.expires) {
				c.mu.Unlock()
				return e.val, e.val != nil
			}
		default:
			c.mu.Unlock()
			<-e.done
			return e.val, e.val != nil
		}
	}
	e := &cacheEntry{done: make(chan struct{})}
	c.m[key] = e
	c.mu.Unlock()

	val, ok := fetch()
	if ok {
		e.val, e.expires = val, now.Add(ttl)
	} else {
		e.expires = now.Add(failTTL)
	}
	close(e.done)
	return e.val, ok
}
