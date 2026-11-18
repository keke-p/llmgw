package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// LogRow is one request_logs row.
type LogRow struct {
	ID                  int64     `json:"id"`
	CreatedAt           time.Time `json:"created_at"`
	KeyID               *int64    `json:"key_id"`
	KeyName             string    `json:"key_name"`
	Alias               string    `json:"alias"`
	ProviderID          *int64    `json:"provider_id"`
	UpstreamModel       string    `json:"upstream_model"`
	Endpoint            string    `json:"endpoint"`
	InboundFormat       string    `json:"inbound_format"`
	UpstreamFormat      string    `json:"upstream_format"`
	Passthrough         bool      `json:"passthrough"`
	Stream              bool      `json:"stream"`
	StatusCode          int       `json:"status_code"`
	Error               string    `json:"error"`
	QueueMS             *int64    `json:"queue_ms"`
	LatencyMS           *int64    `json:"latency_ms"`
	TTFTMS              *int64    `json:"ttft_ms"`
	TimeoutPhase        string    `json:"timeout_phase"`
	CancelledByClient   bool      `json:"cancelled_by_client"`
	Interrupted         bool      `json:"interrupted"`
	AttemptsJSON        string    `json:"-"`
	PromptTokens        *int      `json:"prompt_tokens"`
	CompletionTokens    *int      `json:"completion_tokens"`
	ClientIP            string    `json:"client_ip"`
	UserAgent           string    `json:"user_agent"`
	RequestBody         *string   `json:"-"`
	ResponseBody        *string   `json:"-"`
	UpstreamRequestBody *string   `json:"-"`
	BodyTruncated       bool      `json:"body_truncated"`
}

const logCols = `id, created_at, key_id, key_name, alias, provider_id, upstream_model, endpoint,
	inbound_format, upstream_format, passthrough, stream, status_code, error, queue_ms, latency_ms,
	ttft_ms, timeout_phase, cancelled_by_client, interrupted, attempts_json, prompt_tokens,
	completion_tokens, client_ip, user_agent, request_body, response_body, upstream_request_body,
	body_truncated`

// InsertLogs writes rows in one transaction.
func (s *Store) InsertLogs(ctx context.Context, rows []*LogRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO request_logs(created_at, key_id, key_name, alias,
		provider_id, upstream_model, endpoint, inbound_format, upstream_format, passthrough, stream,
		status_code, error, queue_ms, latency_ms, ttft_ms, timeout_phase, cancelled_by_client, interrupted,
		attempts_json, prompt_tokens, completion_tokens, client_ip, user_agent, request_body, response_body,
		upstream_request_body, body_truncated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.CreatedAt.UnixMilli(), r.KeyID, nullStr(r.KeyName), nullStr(r.Alias),
			r.ProviderID, nullStr(r.UpstreamModel), r.Endpoint, r.InboundFormat, nullStr(r.UpstreamFormat),
			boolInt(r.Passthrough), boolInt(r.Stream), r.StatusCode, nullStr(r.Error), r.QueueMS, r.LatencyMS,
			r.TTFTMS, nullStr(r.TimeoutPhase), boolInt(r.CancelledByClient), boolInt(r.Interrupted),
			nullStr(r.AttemptsJSON), r.PromptTokens, r.CompletionTokens, nullStr(r.ClientIP), nullStr(r.UserAgent),
			r.RequestBody, r.ResponseBody, r.UpstreamRequestBody, boolInt(r.BodyTruncated)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanLog(r scanner) (*LogRow, error) {
	var (
		l                                                          LogRow
		created                                                    int64
		keyName, alias, upModel, upFmt, errStr, phase, att, ip, ua sql.NullString
		pass, stream, cancelled, interrupted, truncated            int
		reqB, respB, upReqB                                        sql.NullString
	)
	if err := r.Scan(&l.ID, &created, &l.KeyID, &keyName, &alias, &l.ProviderID, &upModel, &l.Endpoint,
		&l.InboundFormat, &upFmt, &pass, &stream, &l.StatusCode, &errStr, &l.QueueMS, &l.LatencyMS,
		&l.TTFTMS, &phase, &cancelled, &interrupted, &att, &l.PromptTokens, &l.CompletionTokens,
		&ip, &ua, &reqB, &respB, &upReqB, &truncated); err != nil {
		return nil, err
	}
	l.CreatedAt = time.UnixMilli(created).UTC()
	l.KeyName, l.Alias, l.UpstreamModel, l.UpstreamFormat = keyName.String, alias.String, upModel.String, upFmt.String
	l.Error, l.TimeoutPhase, l.AttemptsJSON, l.ClientIP, l.UserAgent = errStr.String, phase.String, att.String, ip.String, ua.String
	l.Passthrough, l.Stream, l.CancelledByClient, l.Interrupted, l.BodyTruncated = pass != 0, stream != 0, cancelled != 0, interrupted != 0, truncated != 0
	str := func(v sql.NullString) *string {
		if !v.Valid {
			return nil
		}
		s := v.String
		return &s
	}
	l.RequestBody, l.ResponseBody, l.UpstreamRequestBody = str(reqB), str(respB), str(upReqB)
	return &l, nil
}

// LogFilter selects request logs.
type LogFilter struct {
	From, To time.Time // zero = unbounded
	KeyID    *int64
	Alias    string
	Status   string // "", "200", "2xx", "4xx", "5xx", "error"
	Query    string // substring of request/response body
	BeforeID int64  // paging: rows with id < BeforeID
	Limit    int
}

func (f *LogFilter) where() (string, []any, error) {
	var conds []string
	var args []any
	if !f.From.IsZero() {
		conds, args = append(conds, "created_at >= ?"), append(args, f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		conds, args = append(conds, "created_at < ?"), append(args, f.To.UnixMilli())
	}
	if f.KeyID != nil {
		conds, args = append(conds, "key_id = ?"), append(args, *f.KeyID)
	}
	if f.Alias != "" {
		conds, args = append(conds, "alias = ?"), append(args, f.Alias)
	}
	switch s := f.Status; {
	case s == "":
	case s == "error":
		conds = append(conds, "(status_code >= 400 OR error IS NOT NULL)")
	case len(s) == 3 && s[1:] == "xx" && s[0] >= '1' && s[0] <= '5':
		lo := int(s[0]-'0') * 100
		conds, args = append(conds, "status_code >= ? AND status_code < ?"), append(args, lo, lo+100)
	default:
		n, err := strconv.Atoi(s)
		if err != nil {
			return "", nil, errors.New("status must be a code, 2xx/4xx/5xx or error")
		}
		conds, args = append(conds, "status_code = ?"), append(args, n)
	}
	if f.Query != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Query)
		like := "%" + esc + "%"
		conds = append(conds, `(request_body LIKE ? ESCAPE '\' OR response_body LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	if f.BeforeID > 0 {
		conds, args = append(conds, "id < ?"), append(args, f.BeforeID)
	}
	if len(conds) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

// ListLogs returns logs (newest first) matching the filter.
func (s *Store) ListLogs(ctx context.Context, f LogFilter) ([]*LogRow, error) {
	where, args, err := f.where()
	if err != nil {
		return nil, err
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+logCols+` FROM request_logs`+where+` ORDER BY id DESC LIMIT ?`,
		append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*LogRow{}
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ValidateLogFilter reports filter errors without querying.
func ValidateLogFilter(f LogFilter) error {
	_, _, err := f.where()
	return err
}

// GetLog loads one log row.
func (s *Store) GetLog(ctx context.Context, id int64) (*LogRow, error) {
	l, err := scanLog(s.db.QueryRowContext(ctx, `SELECT `+logCols+` FROM request_logs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

// DeleteLogsBefore removes logs older than t in batches; returns rows deleted.
func (s *Store) DeleteLogsBefore(ctx context.Context, t time.Time) (int64, error) {
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE id IN
			(SELECT id FROM request_logs WHERE created_at < ? LIMIT 5000)`, t.UnixMilli())
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n == 0 {
			return total, nil
		}
	}
}
