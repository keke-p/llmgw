package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llmgw/internal/store"
)

// Register adds the admin API routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/api/login", a.public(a.login))
	mux.HandleFunc("POST /admin/api/logout", a.protected(a.logout))
	mux.HandleFunc("GET /admin/api/session", a.protected(a.session))
	mux.HandleFunc("POST /admin/api/password", a.protected(a.changePassword))

	mux.HandleFunc("GET /admin/api/keys", a.protected(a.listKeys))
	mux.HandleFunc("POST /admin/api/keys", a.protected(a.createKey))
	mux.HandleFunc("GET /admin/api/keys/{id}", a.protected(a.getKey))
	mux.HandleFunc("PATCH /admin/api/keys/{id}", a.protected(a.updateKey))
	mux.HandleFunc("DELETE /admin/api/keys/{id}", a.protected(a.deleteKey))
	mux.HandleFunc("POST /admin/api/keys/{id}/rotate", a.protected(a.rotateKey))
	mux.HandleFunc("POST /admin/api/keys/{id}/disable", a.protected(a.setKeyStatus(store.KeyDisabled)))
	mux.HandleFunc("POST /admin/api/keys/{id}/enable", a.protected(a.setKeyStatus(store.KeyActive)))

	mux.HandleFunc("GET /admin/api/audit-logs", a.protected(a.listAudit))
	a.registerLogs(mux)
	a.registerProviders(mux)
	a.registerAliases(mux)
	a.registerSettings(mux)
	a.registerUI(mux)
	a.registerBackup(mux)

	// Unknown admin API paths return JSON 404.
	mux.HandleFunc("/admin/api/", a.public(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
	}))
}

// keyJSON is the API representation of a virtual key.
type keyJSON struct {
	ID                    int64      `json:"id"`
	Name                  string     `json:"name"`
	KeyPrefix             string     `json:"key_prefix"`
	Status                string     `json:"status"`
	AllowedAliases        []string   `json:"allowed_aliases"`
	ExpiresAt             *time.Time `json:"expires_at"`
	Expired               bool       `json:"expired"`
	LogMode               string     `json:"log_mode"`
	MaxConcurrency        *int       `json:"max_concurrency"`
	RPMLimit              *int       `json:"rpm_limit"`
	NonStreamKeepalive    bool       `json:"nonstream_keepalive"`
	Imported              bool       `json:"imported"`
	PreviousKeyValidUntil *time.Time `json:"previous_key_valid_until"`
	LastUsedAt            *time.Time `json:"last_used_at"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	// Key is the plaintext key, present only in create/rotate responses.
	Key string `json:"key,omitempty"`
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func intOrNil(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

func (a *API) toJSON(k *store.VirtualKey) keyJSON {
	now := a.Clock.Now()
	j := keyJSON{
		ID: k.ID, Name: k.Name, KeyPrefix: k.KeyPrefix + "…", Status: k.Status,
		AllowedAliases: k.AllowedAliases, ExpiresAt: utc(k.ExpiresAt), LogMode: k.LogMode,
		MaxConcurrency: intOrNil(k.MaxConcurrency), RPMLimit: intOrNil(k.RPMLimit),
		NonStreamKeepalive: k.NonStreamKeepalive, Imported: k.Imported,
		LastUsedAt: utc(k.LastUsedAt), CreatedAt: k.CreatedAt.UTC(), UpdatedAt: k.UpdatedAt.UTC(),
	}
	if k.Imported {
		j.KeyPrefix = k.KeyPrefix
	}
	if k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) {
		j.Expired = true
	}
	if k.PrevKeyExpiresAt != nil && now.Before(*k.PrevKeyExpiresAt) {
		j.PreviousKeyValidUntil = utc(k.PrevKeyExpiresAt)
	}
	return j
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "key not found")
		return 0, false
	}
	return id, true
}

func (a *API) loadKey(w http.ResponseWriter, r *http.Request) (*store.VirtualKey, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return nil, false
	}
	k, err := a.Store.GetKey(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "key not found")
		return nil, false
	}
	if err != nil {
		a.Log.Error("load key", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return nil, false
	}
	return k, true
}

func (a *API) listKeys(w http.ResponseWriter, r *http.Request, _ *actor) {
	ks, err := a.Store.ListKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]keyJSON, 0, len(ks))
	for _, k := range ks {
		out = append(out, a.toJSON(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (a *API) getKey(w http.ResponseWriter, r *http.Request, _ *actor) {
	if k, ok := a.loadKey(w, r); ok {
		writeJSON(w, http.StatusOK, a.toJSON(k))
	}
}

var keyFields = map[string]bool{
	"name": true, "allowed_aliases": true, "expires_at": true, "log_mode": true,
	"max_concurrency": true, "rpm_limit": true, "nonstream_keepalive": true, "status": true,
}

// applyKeyInput applies a JSON object of key settings onto k. Absent fields are
// left unchanged; "expires_at": null clears the expiry. Returns changed field names.
func applyKeyInput(k *store.VirtualKey, raw map[string]json.RawMessage) ([]string, error) {
	var changed []string
	for f := range raw {
		if !keyFields[f] {
			return nil, errors.New("unknown field " + strconv.Quote(f))
		}
	}
	isNull := func(b json.RawMessage) bool { return strings.TrimSpace(string(b)) == "null" }
	for f, v := range raw {
		switch f {
		case "name":
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, errors.New("name must be a string")
			}
			s = strings.TrimSpace(s)
			if s == "" || len(s) > 200 {
				return nil, errors.New("name must be 1-200 characters")
			}
			k.Name = s
		case "allowed_aliases":
			var list []string
			if !isNull(v) {
				if err := json.Unmarshal(v, &list); err != nil {
					return nil, errors.New("allowed_aliases must be an array of strings")
				}
			}
			k.AllowedAliases = normalizeAliases(list)
		case "expires_at":
			if isNull(v) {
				k.ExpiresAt = nil
				break
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, errors.New("expires_at must be an RFC 3339 string or null")
			}
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return nil, errors.New("expires_at must be an RFC 3339 timestamp, e.g. 2027-01-01T00:00:00Z")
			}
			k.ExpiresAt = &t
		case "log_mode":
			var s string
			_ = json.Unmarshal(v, &s)
			switch s {
			case store.LogFull, store.LogMetadataOnly, store.LogNone:
				k.LogMode = s
			default:
				return nil, errors.New("log_mode must be full, metadata_only or none")
			}
		case "status":
			var s string
			_ = json.Unmarshal(v, &s)
			if s != store.KeyActive && s != store.KeyDisabled {
				return nil, errors.New("status must be active or disabled")
			}
			k.Status = s
		case "max_concurrency", "rpm_limit":
			n := 0
			if !isNull(v) {
				if err := json.Unmarshal(v, &n); err != nil || n < 0 {
					return nil, errors.New(f + " must be a non-negative integer or null")
				}
			}
			if f == "max_concurrency" {
				k.MaxConcurrency = n
			} else {
				k.RPMLimit = n
			}
		case "nonstream_keepalive":
			if err := json.Unmarshal(v, &k.NonStreamKeepalive); err != nil {
				return nil, errors.New("nonstream_keepalive must be a boolean")
			}
		}
		changed = append(changed, f)
	}
	return changed, nil
}

func normalizeAliases(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func decodeObject(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	raw := map[string]json.RawMessage{}
	if r.ContentLength == 0 {
		return raw, true
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return nil, false
	}
	return raw, true
}

func (a *API) createKey(w http.ResponseWriter, r *http.Request, act *actor) {
	raw, ok := decodeObject(w, r)
	if !ok {
		return
	}
	if _, ok := raw["name"]; !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	k := &store.VirtualKey{Status: store.KeyActive, LogMode: store.LogFull, AllowedAliases: []string{}}
	if _, err := applyKeyInput(k, raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	plain, err := a.Keys.Create(r.Context(), k)
	if err != nil {
		a.Log.Error("create key", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.audit(r, act.Name, "key.create", "key", strconv.FormatInt(k.ID, 10), map[string]any{"name": k.Name})
	j := a.toJSON(k)
	j.Key = plain
	writeJSON(w, http.StatusCreated, j)
}

func (a *API) updateKey(w http.ResponseWriter, r *http.Request, act *actor) {
	k, ok := a.loadKey(w, r)
	if !ok {
		return
	}
	raw, ok := decodeObject(w, r)
	if !ok {
		return
	}
	changed, err := applyKeyInput(k, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := a.Keys.Update(r.Context(), k); err != nil {
		a.Log.Error("update key", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.audit(r, act.Name, "key.update", "key", strconv.FormatInt(k.ID, 10), map[string]any{"name": k.Name, "fields": changed})
	k, _ = a.Store.GetKey(r.Context(), k.ID)
	writeJSON(w, http.StatusOK, a.toJSON(k))
}

func (a *API) setKeyStatus(status string) handlerFunc {
	action := map[string]string{store.KeyActive: "key.enable", store.KeyDisabled: "key.disable"}[status]
	return func(w http.ResponseWriter, r *http.Request, act *actor) {
		k, ok := a.loadKey(w, r)
		if !ok {
			return
		}
		k.Status = status
		if err := a.Keys.Update(r.Context(), k); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		a.audit(r, act.Name, action, "key", strconv.FormatInt(k.ID, 10), map[string]any{"name": k.Name})
		k, _ = a.Store.GetKey(r.Context(), k.ID)
		writeJSON(w, http.StatusOK, a.toJSON(k))
	}
}

const maxGraceMinutes = 7 * 24 * 60

func (a *API) rotateKey(w http.ResponseWriter, r *http.Request, act *actor) {
	k, ok := a.loadKey(w, r)
	if !ok {
		return
	}
	var req struct {
		GraceMinutes int `json:"grace_minutes"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	if req.GraceMinutes < 0 || req.GraceMinutes > maxGraceMinutes {
		writeError(w, http.StatusBadRequest, "invalid_request", "grace_minutes must be between 0 and 10080")
		return
	}
	plain, nk, err := a.Keys.Rotate(r.Context(), k.ID, time.Duration(req.GraceMinutes)*time.Minute)
	if err != nil {
		a.Log.Error("rotate key", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.audit(r, act.Name, "key.rotate", "key", strconv.FormatInt(k.ID, 10),
		map[string]any{"name": k.Name, "grace_minutes": req.GraceMinutes})
	j := a.toJSON(nk)
	j.Key = plain
	writeJSON(w, http.StatusOK, j)
}

func (a *API) deleteKey(w http.ResponseWriter, r *http.Request, act *actor) {
	k, ok := a.loadKey(w, r)
	if !ok {
		return
	}
	if err := a.Keys.Delete(r.Context(), k.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.audit(r, act.Name, "key.delete", "key", strconv.FormatInt(k.ID, 10), map[string]any{"name": k.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request, _ *actor) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, _ := strconv.ParseInt(r.URL.Query().Get("before_id"), 10, 64)
	entries, err := a.Store.ListAudit(r.Context(), limit, before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": entries})
}
