package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"llmgw/internal/store"
)

func (a *API) registerLogs(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/logs", a.protected(a.listLogs))
	mux.HandleFunc("GET /admin/api/logs/export", a.protected(a.exportLogs))
	mux.HandleFunc("GET /admin/api/logs/{id}", a.protected(a.getLog))
}

// logFilter parses: from, to (RFC 3339), key_id, alias, status, q, before_id, limit.
func logFilter(r *http.Request) (store.LogFilter, error) {
	q := r.URL.Query()
	var f store.LogFilter
	parseTime := func(name string) (time.Time, error) {
		v := q.Get(name)
		if v == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, errors.New(name + " must be an RFC 3339 timestamp")
		}
		return t, nil
	}
	var err error
	if f.From, err = parseTime("from"); err != nil {
		return f, err
	}
	if f.To, err = parseTime("to"); err != nil {
		return f, err
	}
	if v := q.Get("key_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return f, errors.New("key_id must be an integer")
		}
		f.KeyID = &id
	}
	f.Alias, f.Status, f.Query = q.Get("alias"), q.Get("status"), q.Get("q")
	f.BeforeID, _ = strconv.ParseInt(q.Get("before_id"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	return f, store.ValidateLogFilter(f)
}

// logDetail is the full representation of a log row. Bodies are embedded as
// JSON when they are valid JSON, otherwise as strings.
type logDetail struct {
	*store.LogRow
	Attempts            json.RawMessage `json:"attempts"`
	RequestBody         json.RawMessage `json:"request_body"`
	ResponseBody        json.RawMessage `json:"response_body"`
	UpstreamRequestBody json.RawMessage `json:"upstream_request_body"`
}

func bodyJSON(s *string) json.RawMessage {
	if s == nil {
		return json.RawMessage("null")
	}
	if json.Valid([]byte(*s)) {
		return json.RawMessage(*s)
	}
	b, _ := json.Marshal(*s)
	return b
}

func detail(l *store.LogRow) logDetail {
	d := logDetail{LogRow: l, Attempts: json.RawMessage("[]"),
		RequestBody: bodyJSON(l.RequestBody), ResponseBody: bodyJSON(l.ResponseBody),
		UpstreamRequestBody: bodyJSON(l.UpstreamRequestBody)}
	if l.AttemptsJSON != "" {
		d.Attempts = json.RawMessage(l.AttemptsJSON)
	}
	return d
}

func (a *API) listLogs(w http.ResponseWriter, r *http.Request, _ *actor) {
	f, err := logFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	rows, err := a.Store.ListLogs(r.Context(), f)
	if err != nil {
		a.Log.Error("list logs", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	resp := map[string]any{"data": rows}
	if len(rows) > 0 {
		resp["next_before_id"] = rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) getLog(w http.ResponseWriter, r *http.Request, _ *actor) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "log not found")
		return
	}
	l, err := a.Store.GetLog(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "log not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, detail(l))
}

// exportLogs streams matching logs (with bodies) as JSON Lines, newest first.
// limit defaults to 10000 rows per export.
func (a *API) exportLogs(w http.ResponseWriter, r *http.Request, _ *actor) {
	f, err := logFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	total := f.Limit
	if total <= 0 {
		total = 10000
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="llmgw-logs-`+a.Clock.Now().UTC().Format("20060102-150405")+`.jsonl"`)
	enc := json.NewEncoder(w)
	written := 0
	for written < total {
		f.Limit = min(500, total-written)
		rows, err := a.Store.ListLogs(r.Context(), f)
		if err != nil {
			a.Log.Error("export logs", "err", err)
			return
		}
		for _, l := range rows {
			if enc.Encode(detail(l)) != nil {
				return
			}
		}
		written += len(rows)
		if len(rows) < f.Limit {
			break
		}
		f.BeforeID = rows[len(rows)-1].ID
	}
}
