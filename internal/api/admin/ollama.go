package admin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"llmgw/internal/store"
	"llmgw/internal/upstream"
)

// Ollama model management for ollama-type providers (pull / create / copy /
// delete / unload). The public Ollama API never relays these endpoints; only
// administrators reach them through /admin/api.

func (a *API) registerOllama(mux *http.ServeMux) {
	const base = "/admin/api/providers/{id}/ollama"
	mux.HandleFunc("GET "+base+"/models", a.protected(a.ollamaModels))
	mux.HandleFunc("GET "+base+"/show", a.protected(a.ollamaShow))
	mux.HandleFunc("POST "+base+"/pull", a.protected(a.ollamaPull))
	mux.HandleFunc("POST "+base+"/create", a.protected(a.ollamaCreate))
	mux.HandleFunc("POST "+base+"/copy", a.protected(a.ollamaCopy))
	mux.HandleFunc("POST "+base+"/unload", a.protected(a.ollamaUnload))
	mux.HandleFunc("DELETE "+base+"/models", a.protected(a.ollamaDelete))
}

const ollamaCallTimeout = 60 * time.Second

// loadOllamaProvider loads the {id} provider and requires type ollama.
func (a *API) loadOllamaProvider(w http.ResponseWriter, r *http.Request) (*store.Provider, bool) {
	p, ok := a.loadProvider(w, r)
	if !ok {
		return nil, false
	}
	if p.Type != store.ProviderOllama {
		writeError(w, http.StatusBadRequest, "not_ollama", "model management is only available for ollama providers")
		return nil, false
	}
	return p, true
}

// validModelName accepts Ollama model references ([host/][ns/]name[:tag]) but
// rejects whitespace and control characters; Ollama validates the rest.
func validModelName(s string) bool {
	if s == "" || len(s) > 350 {
		return false
	}
	for _, c := range s {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// upstreamErr writes an error from a failed upstream call. Upstream 4xx are
// passed through (e.g. 404 model not found); everything else becomes 502.
func upstreamErr(w http.ResponseWriter, err error) {
	var he *upstream.HTTPError
	if errors.As(err, &he) {
		status := http.StatusBadGateway
		if he.Status >= 400 && he.Status < 500 {
			status = he.Status
		}
		writeError(w, status, "upstream_error", "ollama: "+he.Message)
		return
	}
	writeError(w, http.StatusBadGateway, "upstream_error", "ollama: "+err.Error())
}

func (a *API) ollamaCall(ctx context.Context, p *store.Provider, method, path string, body any) ([]byte, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, ollamaCallTimeout)
	defer cancel()
	return upstream.Call(ctx, a.HTTPClient, p, method, path, b)
}

// refreshModels updates the stored model list after a change.
func (a *API) refreshModels(r *http.Request, p *store.Provider) {
	a.runCheck(context.WithoutCancel(r.Context()), p, false)
}

// ollamaModels returns /api/tags with /api/ps (loaded models) merged in.
func (a *API) ollamaModels(w http.ResponseWriter, r *http.Request, _ *actor) {
	p, ok := a.loadOllamaProvider(w, r)
	if !ok {
		return
	}
	tags, err := a.ollamaCall(r.Context(), p, http.MethodGet, "/api/tags", nil)
	if err != nil {
		upstreamErr(w, err)
		return
	}
	var list struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(tags, &list); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "ollama: unexpected /api/tags response")
		return
	}
	loaded := map[string]map[string]any{}
	if ps, err := a.ollamaCall(r.Context(), p, http.MethodGet, "/api/ps", nil); err == nil {
		var running struct {
			Models []map[string]any `json:"models"`
		}
		if json.Unmarshal(ps, &running) == nil {
			for _, m := range running.Models {
				if n, _ := m["name"].(string); n != "" {
					loaded[n] = map[string]any{"size_vram": m["size_vram"], "expires_at": m["expires_at"], "context_length": m["context_length"]}
				}
			}
		}
	}
	out := make([]map[string]any, 0, len(list.Models))
	for _, m := range list.Models {
		n, _ := m["name"].(string)
		m["loaded"] = loaded[n] // nil when not loaded
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (a *API) ollamaShow(w http.ResponseWriter, r *http.Request, _ *actor) {
	p, ok := a.loadOllamaProvider(w, r)
	if !ok {
		return
	}
	model := r.URL.Query().Get("model")
	if !validModelName(model) {
		writeError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	b, err := a.ollamaCall(r.Context(), p, http.MethodPost, "/api/show", map[string]any{"model": model})
	if err != nil {
		upstreamErr(w, err)
		return
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "ollama: unexpected /api/show response")
		return
	}
	// tensors can be huge and is useless in the UI.
	delete(out, "tensors")
	writeJSON(w, http.StatusOK, out)
}

type modelRequest struct {
	Model    string `json:"model"`
	Insecure bool   `json:"insecure,omitempty"`
}

// ollamaPull streams the /api/pull progress (NDJSON) to the client.
func (a *API) ollamaPull(w http.ResponseWriter, r *http.Request, act *actor) {
	p, ok := a.loadOllamaProvider(w, r)
	if !ok {
		return
	}
	var req modelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if !validModelName(req.Model) {
		writeError(w, http.StatusBadRequest, "invalid_request", "model must be a model name without spaces")
		return
	}
	body := map[string]any{"model": req.Model, "stream": true}
	if req.Insecure {
		body["insecure"] = true
	}
	a.audit(r, act.Name, "ollama.pull", "provider", strconv.FormatInt(p.ID, 10), map[string]any{"provider": p.Name, "model": req.Model})
	if a.streamOllama(w, r, p, "/api/pull", body) {
		a.refreshModels(r, p)
	}
}

// ollamaCreate creates a model from JSON fields (Ollama /api/create) or a
// Modelfile, streaming progress (NDJSON).
func (a *API) ollamaCreate(w http.ResponseWriter, r *http.Request, act *actor) {
	p, ok := a.loadOllamaProvider(w, r)
	if !ok {
		return
	}
	raw, ok := decodeObject(w, r)
	if !ok {
		return
	}
	body, err := buildCreateRequest(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	a.audit(r, act.Name, "ollama.create", "provider", strconv.FormatInt(p.ID, 10),
		map[string]any{"provider": p.Name, "model": body["model"], "from": body["from"]})
	if a.streamOllama(w, r, p, "/api/create", body) {
		a.refreshModels(r, p)
	}
}

// createFields are the /api/create fields accepted from the admin API. files and
// adapters (blob uploads) are not supported.
var createFields = map[string]bool{
	"model": true, "from": true, "system": true, "template": true, "license": true,
	"parameters": true, "messages": true, "quantize": true, "modelfile": true,
}

func buildCreateRequest(raw map[string]json.RawMessage) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range raw {
		if !createFields[k] {
			return nil, errors.New("unknown field " + strconv.Quote(k))
		}
		if k == "modelfile" || isNull(v) {
			continue
		}
		var x any
		if err := json.Unmarshal(v, &x); err != nil {
			return nil, err
		}
		if s, ok := x.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		out[k] = x
	}
	if mf, ok := raw["modelfile"]; ok && !isNull(mf) {
		var text string
		if json.Unmarshal(mf, &text) != nil {
			return nil, errors.New("modelfile must be a string")
		}
		if strings.TrimSpace(text) != "" {
			parsed, err := ParseModelfile(text)
			if err != nil {
				return nil, err
			}
			for k, v := range parsed { // explicit JSON fields win
				if _, set := out[k]; !set {
					out[k] = v
				}
			}
		}
	}
	name, _ := out["model"].(string)
	if !validModelName(strings.TrimSpace(name)) {
		return nil, errors.New("model must be a model name without spaces")
	}
	out["model"] = strings.TrimSpace(name)
	if from, _ := out["from"].(string); strings.TrimSpace(from) == "" {
		return nil, errors.New("from (base model) is required")
	}
	if pm, ok := out["parameters"]; ok {
		if _, isObj := pm.(map[string]any); !isObj {
			return nil, errors.New("parameters must be an object")
		}
	}
	out["stream"] = true
	return out, nil
}

type copyRequest struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

func (a *API) ollamaCopy(w http.ResponseWriter, r *http.Request, act *actor) {
	p, ok := a.loadOllamaProvider(w, r)
	if !ok {
		return
	}
	var req copyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Source, req.Destination = strings.TrimSpace(req.Source), strings.TrimSpace(req.Destination)
	if !validModelName(req.Source) || !validModelName(req.Destination) {
		writeError(w, http.StatusBadRequest, "invalid_request", "source and destination must be model names without spaces")
		return
	}
	if _, err := a.ollamaCall(r.Context(), p, http.MethodPost, "/api/copy", req); err != nil {
		upstreamErr(w, err)
		return
	}
	a.audit(r, act.Name, "ollama.copy", "provider", strconv.FormatInt(p.ID, 10),
		map[string]any{"provider": p.Name, "source": req.Source, "destination": req.Destination})
	a.refreshModels(r, p)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) ollamaDelete(w http.ResponseWriter, r *http.Request, act *actor) {
	p, ok := a.loadOllamaProvider(w, r)
	if !ok {
		return
	}
	model := r.URL.Query().Get("model")
	if !validModelName(model) {
		writeError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	if _, err := a.ollamaCall(r.Context(), p, http.MethodDelete, "/api/delete", map[string]any{"model": model}); err != nil {
		upstreamErr(w, err)
		return
	}
	a.audit(r, act.Name, "ollama.delete", "provider", strconv.FormatInt(p.ID, 10), map[string]any{"provider": p.Name, "model": model})
	a.refreshModels(r, p)
	w.WriteHeader(http.StatusNoContent)
}

// ollamaUnload frees a loaded model's memory (keep_alive 0).
func (a *API) ollamaUnload(w http.ResponseWriter, r *http.Request, act *actor) {
	p, ok := a.loadOllamaProvider(w, r)
	if !ok {
		return
	}
	var req modelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validModelName(req.Model) {
		writeError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	if _, err := a.ollamaCall(r.Context(), p, http.MethodPost, "/api/generate",
		map[string]any{"model": req.Model, "keep_alive": 0, "stream": false}); err != nil {
		upstreamErr(w, err)
		return
	}
	a.audit(r, act.Name, "ollama.unload", "provider", strconv.FormatInt(p.ID, 10), map[string]any{"provider": p.Name, "model": req.Model})
	w.WriteHeader(http.StatusNoContent)
}

// streamOllama relays a streaming NDJSON upstream call. Errors before the
// stream starts are returned as admin JSON errors; later ones arrive as
// {"error": "..."} lines (as Ollama sends them). It reports whether the
// stream ended with {"status":"success"}.
//
// The upstream request is bound to the client connection: closing the page
// cancels the operation (Ollama resumes a cancelled pull on the next pull).
func (a *API) streamOllama(w http.ResponseWriter, r *http.Request, p *store.Provider, path string, body any) bool {
	b, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return false
	}
	req, err := upstream.NewRequest(r.Context(), p, strings.TrimRight(p.BaseURL, "/")+path, b)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return false
	}
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		upstreamErr(w, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		upstreamErr(w, upstream.ReadHTTPError(resp))
		return false
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	br := bufio.NewReaderSize(resp.Body, 64*1024)
	success := false
	for {
		line, err := br.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			var st struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}
			if json.Unmarshal(t, &st) == nil {
				success = st.Error == "" && st.Status == "success"
			}
			if _, werr := w.Write(append(t, '\n')); werr != nil {
				return false
			}
			_ = rc.Flush()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				msg, _ := json.Marshal(map[string]string{"error": "ollama stream interrupted: " + err.Error()})
				_, _ = w.Write(append(msg, '\n'))
				_ = rc.Flush()
				return false
			}
			return success
		}
	}
}
