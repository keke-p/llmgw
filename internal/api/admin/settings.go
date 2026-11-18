package admin

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"llmgw/internal/gateway"
	"llmgw/internal/gwerr"
	"llmgw/internal/store"
)

func (a *API) registerSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/settings", a.protected(a.getSettings))
	mux.HandleFunc("PATCH /admin/api/settings", a.protected(a.updateSettings))
	mux.HandleFunc("POST /admin/api/playground/{path...}", a.protected(a.playground))
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request, _ *actor) {
	writeJSON(w, http.StatusOK, map[string]any{
		"values":   store.SettingsMap(a.settings(r.Context())),
		"defaults": store.SettingsMap(store.DefaultSettings()),
	})
}

func (a *API) updateSettings(w http.ResponseWriter, r *http.Request, act *actor) {
	raw, ok := decodeObject(w, r)
	if !ok {
		return
	}
	// Validate everything first so a bad value does not leave a partial update.
	known := store.SettingsMap(store.DefaultSettings())
	for k, v := range raw {
		if _, ok := known[k]; !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", "unknown setting "+k)
			return
		}
		if err := store.ValidateSetting(k, v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}
	keys := make([]string, 0, len(raw))
	for k, v := range raw {
		var val any
		_ = json.Unmarshal(v, &val)
		if err := a.Store.SetSetting(r.Context(), k, val); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	a.audit(r, act.Name, "settings.update", "settings", "", map[string]any{"keys": keys})
	a.getSettings(w, r, act)
}

// playgroundPaths are the API endpoints the Playground may call.
var playgroundPaths = []string{
	"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/messages",
	"/api/chat", "/api/generate", "/api/embed",
}

// playground runs an API request on behalf of a key (?key_id=N) — or, with
// key_id=0 / omitted, as an unrestricted admin principal — so the admin can try
// aliases without knowing a key's plaintext.
func (a *API) playground(w http.ResponseWriter, r *http.Request, act *actor) {
	path := "/" + strings.TrimPrefix(r.PathValue("path"), "/")
	if !slices.Contains(playgroundPaths, path) {
		writeError(w, http.StatusNotFound, "not_found", "unsupported playground endpoint "+path)
		return
	}
	var p *gateway.Principal
	if idStr := r.URL.Query().Get("key_id"); idStr != "" && idStr != "0" {
		var id int64
		if err := json.Unmarshal([]byte(idStr), &id); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "key_id must be an integer")
			return
		}
		var ge *gwerr.Error
		if p, ge = a.Keys.PrincipalFor(r.Context(), id); ge != nil {
			writeError(w, ge.Status, "key_unusable", ge.Message)
			return
		}
	} else {
		p = &gateway.Principal{KeyName: "playground:" + act.Name, LogMode: store.LogFull}
	}
	if a.Dispatch == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "playground dispatch unavailable")
		return
	}
	a.Dispatch(w, r, p, path)
}
