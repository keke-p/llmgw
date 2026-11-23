package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"llmgw/internal/store"
	"llmgw/internal/timeouts"
)

func (a *API) registerAliases(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/aliases", a.protected(a.listAliases))
	mux.HandleFunc("POST /admin/api/aliases", a.protected(a.createAlias))
	mux.HandleFunc("GET /admin/api/aliases/{id}", a.protected(a.getAlias))
	mux.HandleFunc("PATCH /admin/api/aliases/{id}", a.protected(a.updateAlias))
	mux.HandleFunc("DELETE /admin/api/aliases/{id}", a.protected(a.deleteAlias))
}

type targetJSON struct {
	ProviderID      int64  `json:"provider_id"`
	ProviderName    string `json:"provider_name"`
	ProviderType    string `json:"provider_type"`
	ProviderEnabled bool   `json:"provider_enabled"`
	UpstreamModel   string `json:"upstream_model"`
}

type effectiveJSON struct {
	ProviderID            int64     `json:"provider_id"`
	ProviderName          string    `json:"provider_name"`
	ConnectTimeout        valueJSON `json:"connect_timeout"`
	QueueTimeout          valueJSON `json:"queue_timeout"`
	TTFTTimeout           valueJSON `json:"ttft_timeout"`
	IdleTimeout           valueJSON `json:"idle_timeout"`
	MaxDuration           valueJSON `json:"max_duration"`
	FallbackOnTTFTTimeout bool      `json:"fallback_on_ttft_timeout"`
}

type aliasJSON struct {
	ID                    int64           `json:"id"`
	Name                  string          `json:"name"`
	Description           string          `json:"description"`
	Enabled               bool            `json:"enabled"`
	Targets               []targetJSON    `json:"targets"`
	TTFTTimeoutMS         *int64          `json:"ttft_timeout_ms"`
	IdleTimeoutMS         *int64          `json:"idle_timeout_ms"`
	MaxDurationMS         *int64          `json:"max_duration_ms"`
	FallbackOnTTFTTimeout *bool           `json:"fallback_on_ttft_timeout"`
	Params                json.RawMessage `json:"params"`
	ModelInfo             store.ModelInfo `json:"model_info"`
	Effective             []effectiveJSON `json:"effective"`
	CreatedAt             time.Time       `json:"created_at"`
}

func aliasToJSON(al *store.Alias, s store.Settings) aliasJSON {
	j := aliasJSON{ID: al.ID, Name: al.Name, Description: al.Description, Enabled: al.Enabled,
		Targets: []targetJSON{}, TTFTTimeoutMS: msOrNil(al.TTFTTimeout), IdleTimeoutMS: msOrNil(al.IdleTimeout),
		MaxDurationMS: msOrNil(al.MaxDuration), FallbackOnTTFTTimeout: al.FallbackOnTTFTTimeout,
		Params: al.Params, ModelInfo: al.ModelInfo, Effective: []effectiveJSON{}, CreatedAt: al.CreatedAt.UTC()}
	if len(j.Params) == 0 {
		j.Params = json.RawMessage("{}")
	}
	for _, t := range al.Targets {
		j.Targets = append(j.Targets, targetJSON{ProviderID: t.Provider.ID, ProviderName: t.Provider.Name,
			ProviderType: t.Provider.Type, ProviderEnabled: t.Provider.Enabled, UpstreamModel: t.UpstreamModel})
		e := timeouts.Resolve(s, t.Provider, al)
		j.Effective = append(j.Effective, effectiveJSON{ProviderID: t.Provider.ID, ProviderName: t.Provider.Name,
			ConnectTimeout: val(e.Connect), QueueTimeout: val(e.Queue), TTFTTimeout: val(e.TTFT),
			IdleTimeout: val(e.Idle), MaxDuration: val(e.MaxDuration), FallbackOnTTFTTimeout: e.FallbackOnTTFT})
	}
	return j
}

func (a *API) listAliases(w http.ResponseWriter, r *http.Request, _ *actor) {
	list, err := a.Store.ListAliasesFull(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s := a.settings(r.Context())
	out := make([]aliasJSON, 0, len(list))
	for _, al := range list {
		out = append(out, aliasToJSON(al, s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (a *API) loadAlias(w http.ResponseWriter, r *http.Request) (*store.Alias, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "alias not found")
		return nil, false
	}
	al, err := a.Store.GetAlias(r.Context(), id)
	if err != nil {
		storeErr(w, err, "alias")
		return nil, false
	}
	return al, true
}

func (a *API) getAlias(w http.ResponseWriter, r *http.Request, _ *actor) {
	if al, ok := a.loadAlias(w, r); ok {
		writeJSON(w, http.StatusOK, aliasToJSON(al, a.settings(r.Context())))
	}
}

var aliasFields = map[string]bool{
	"name": true, "description": true, "enabled": true, "targets": true, "ttft_timeout_ms": true,
	"idle_timeout_ms": true, "max_duration_ms": true, "fallback_on_ttft_timeout": true, "params": true,
	"model_info": true,
}

// ValidateModelInfo checks the alias model metadata overrides:
// {"context_length": positive integer | null, "tools": bool | null, "vision": bool | null}.
// A null or missing key means "detect from the upstream".
func ValidateModelInfo(raw json.RawMessage) (store.ModelInfo, error) {
	var mi store.ModelInfo
	if isNull(raw) || len(raw) == 0 {
		return mi, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return mi, errors.New("model_info must be an object")
	}
	for k, v := range m {
		if isNull(v) {
			continue
		}
		switch k {
		case "context_length":
			var n int
			if err := json.Unmarshal(v, &n); err != nil || n <= 0 {
				return mi, errors.New("model_info.context_length must be a positive integer or null")
			}
			mi.ContextLength = &n
		case "tools", "vision":
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				return mi, errors.New("model_info." + k + " must be a boolean or null")
			}
			if k == "tools" {
				mi.Tools = &b
			} else {
				mi.Vision = &b
			}
		default:
			return mi, errors.New("unknown model_info key " + strconv.Quote(k) + " (use context_length, tools, vision)")
		}
	}
	return mi, nil
}

// ValidateParams checks the alias parameter rules:
// {"defaults": {...}, "overrides": {...}, "system_prompt": {"mode": "prepend"|"replace", "text": "..."}}
func ValidateParams(raw json.RawMessage) (json.RawMessage, error) {
	if isNull(raw) || len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errors.New("params must be an object")
	}
	for k, v := range p {
		switch k {
		case "defaults", "overrides":
			var m map[string]any
			if err := json.Unmarshal(v, &m); err != nil {
				return nil, errors.New("params." + k + " must be an object")
			}
		case "system_prompt":
			if isNull(v) {
				continue
			}
			var sp struct {
				Mode string `json:"mode"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(v, &sp); err != nil || (sp.Mode != "prepend" && sp.Mode != "replace") {
				return nil, errors.New(`params.system_prompt must be {"mode": "prepend"|"replace", "text": "..."}`)
			}
		default:
			return nil, errors.New("unknown params key " + strconv.Quote(k) + " (use defaults, overrides, system_prompt)")
		}
	}
	return raw, nil
}

func validAliasName(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (a *API) applyAliasInput(r *http.Request, al *store.Alias, raw map[string]json.RawMessage) (bool, []string, error) {
	replaceTargets := false
	var changed []string
	for f := range raw {
		if !aliasFields[f] {
			return false, nil, errors.New("unknown field " + strconv.Quote(f))
		}
	}
	for f, v := range raw {
		switch f {
		case "name":
			var s string
			_ = json.Unmarshal(v, &s)
			s = strings.TrimSpace(s)
			if !validAliasName(s) {
				return false, nil, errors.New("name must be 1-200 characters without spaces")
			}
			al.Name = s
		case "description":
			if json.Unmarshal(v, &al.Description) != nil {
				return false, nil, errors.New("description must be a string")
			}
		case "enabled":
			if json.Unmarshal(v, &al.Enabled) != nil {
				return false, nil, errors.New("enabled must be a boolean")
			}
		case "fallback_on_ttft_timeout":
			if isNull(v) {
				al.FallbackOnTTFTTimeout = nil
				break
			}
			var b bool
			if json.Unmarshal(v, &b) != nil {
				return false, nil, errors.New("fallback_on_ttft_timeout must be a boolean or null")
			}
			al.FallbackOnTTFTTimeout = &b
		case "params":
			p, err := ValidateParams(v)
			if err != nil {
				return false, nil, err
			}
			al.Params = p
		case "model_info":
			mi, err := ValidateModelInfo(v)
			if err != nil {
				return false, nil, err
			}
			al.ModelInfo = mi
		case "targets":
			var ts []struct {
				ProviderID    int64  `json:"provider_id"`
				UpstreamModel string `json:"upstream_model"`
			}
			if err := json.Unmarshal(v, &ts); err != nil {
				return false, nil, errors.New("targets must be an array of {provider_id, upstream_model}")
			}
			if len(ts) == 0 {
				return false, nil, errors.New("at least one target is required")
			}
			al.Targets = nil
			for i, t := range ts {
				if strings.TrimSpace(t.UpstreamModel) == "" {
					return false, nil, errors.New("targets[" + strconv.Itoa(i) + "].upstream_model is required")
				}
				p, err := a.Store.GetProvider(r.Context(), t.ProviderID)
				if err != nil {
					return false, nil, errors.New("targets[" + strconv.Itoa(i) + "]: provider " + strconv.FormatInt(t.ProviderID, 10) + " not found")
				}
				al.Targets = append(al.Targets, store.Target{Provider: p, UpstreamModel: strings.TrimSpace(t.UpstreamModel), Priority: i})
			}
			replaceTargets = true
		default:
			d, err := optDuration(f, v)
			if err != nil {
				return false, nil, err
			}
			switch f {
			case "ttft_timeout_ms":
				al.TTFTTimeout = d
			case "idle_timeout_ms":
				al.IdleTimeout = d
			case "max_duration_ms":
				al.MaxDuration = d
			}
		}
		changed = append(changed, f)
	}
	return replaceTargets, changed, nil
}

func (a *API) createAlias(w http.ResponseWriter, r *http.Request, act *actor) {
	raw, ok := decodeObject(w, r)
	if !ok {
		return
	}
	for _, req := range []string{"name", "targets"} {
		if _, ok := raw[req]; !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", req+" is required")
			return
		}
	}
	al := &store.Alias{Enabled: true, Params: json.RawMessage("{}")}
	if _, _, err := a.applyAliasInput(r, al, raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := a.Store.CreateAlias(r.Context(), al); err != nil {
		storeErr(w, err, "alias")
		return
	}
	a.audit(r, act.Name, "alias.create", "alias", strconv.FormatInt(al.ID, 10), map[string]any{"name": al.Name})
	al, _ = a.Store.GetAlias(r.Context(), al.ID)
	writeJSON(w, http.StatusCreated, aliasToJSON(al, a.settings(r.Context())))
}

func (a *API) updateAlias(w http.ResponseWriter, r *http.Request, act *actor) {
	al, ok := a.loadAlias(w, r)
	if !ok {
		return
	}
	raw, ok := decodeObject(w, r)
	if !ok {
		return
	}
	replace, changed, err := a.applyAliasInput(r, al, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := a.Store.UpdateAlias(r.Context(), al, replace); err != nil {
		storeErr(w, err, "alias")
		return
	}
	a.audit(r, act.Name, "alias.update", "alias", strconv.FormatInt(al.ID, 10), map[string]any{"name": al.Name, "fields": changed})
	al, _ = a.Store.GetAlias(r.Context(), al.ID)
	writeJSON(w, http.StatusOK, aliasToJSON(al, a.settings(r.Context())))
}

func (a *API) deleteAlias(w http.ResponseWriter, r *http.Request, act *actor) {
	al, ok := a.loadAlias(w, r)
	if !ok {
		return
	}
	if err := a.Store.DeleteAlias(r.Context(), al.ID); err != nil {
		storeErr(w, err, "alias")
		return
	}
	a.audit(r, act.Name, "alias.delete", "alias", strconv.FormatInt(al.ID, 10), map[string]any{"name": al.Name})
	w.WriteHeader(http.StatusNoContent)
}
