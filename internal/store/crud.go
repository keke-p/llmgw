package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"llmgw/internal/secret"
)

// ErrConflict is returned for unique-constraint violations (duplicate names).
var ErrConflict = errors.New("conflict")

// ErrInUse is returned when deleting a provider still referenced by aliases.
var ErrInUse = errors.New("in use")

func classify(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		return ErrConflict
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return ErrInUse
	}
	return err
}

func durMs(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return d.Milliseconds()
}

// ---- providers -------------------------------------------------------------

// ListProviders returns all providers ordered by name.
func (s *Store) ListProviders(ctx context.Context) ([]*Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+providerCols+` FROM providers p ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Provider{}
	for rows.Next() {
		p, dest := s.providerDest()
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if err := p.finish(s.secretKey); err != nil {
			// A key encrypted with another LLMGW_SECRET_KEY must not hide the provider.
			p.APIKey = ""
			p.HealthError = err.Error()
		}
		out = append(out, p.Provider)
	}
	return out, rows.Err()
}

func (s *Store) encryptKey(k string) (any, error) {
	if k == "" {
		return nil, nil
	}
	return secret.Encrypt(s.secretKey, k)
}

// CreateProvider inserts a provider (APIKey is encrypted) and sets p.ID.
func (s *Store) CreateProvider(ctx context.Context, p *Provider) error {
	enc, err := s.encryptKey(p.APIKey)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `INSERT INTO providers(name, type, base_url, api_key_enc, supports_anthropic,
		connect_timeout_ms, queue_timeout_ms, ttft_timeout_ms, idle_timeout_ms, max_duration_ms, max_concurrency,
		enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Type, p.BaseURL, enc, boolInt(p.SupportsAnthropic), durMs(p.ConnectTimeout), durMs(p.QueueTimeout),
		durMs(p.TTFTTimeout), durMs(p.IdleTimeout), durMs(p.MaxDuration), zeroNull(p.MaxConcurrency),
		boolInt(p.Enabled), now, now)
	if err != nil {
		return classify(err)
	}
	p.ID, err = res.LastInsertId()
	return err
}

// UpdateProvider saves provider settings. When setKey is false the stored API
// key is left unchanged; otherwise it is replaced by p.APIKey ("" clears it).
func (s *Store) UpdateProvider(ctx context.Context, p *Provider, setKey bool) error {
	q := `UPDATE providers SET name = ?, type = ?, base_url = ?, supports_anthropic = ?, connect_timeout_ms = ?,
		queue_timeout_ms = ?, ttft_timeout_ms = ?, idle_timeout_ms = ?, max_duration_ms = ?, max_concurrency = ?,
		enabled = ?, updated_at = ?`
	args := []any{p.Name, p.Type, p.BaseURL, boolInt(p.SupportsAnthropic), durMs(p.ConnectTimeout), durMs(p.QueueTimeout),
		durMs(p.TTFTTimeout), durMs(p.IdleTimeout), durMs(p.MaxDuration), zeroNull(p.MaxConcurrency),
		boolInt(p.Enabled), time.Now().UnixMilli()}
	if setKey {
		enc, err := s.encryptKey(p.APIKey)
		if err != nil {
			return err
		}
		q += `, api_key_enc = ?`
		args = append(args, enc)
	}
	res, err := s.db.ExecContext(ctx, q+` WHERE id = ?`, append(args, p.ID)...)
	if err != nil {
		return classify(err)
	}
	return mustAffect(res)
}

// DeleteProvider deletes a provider; ErrInUse if aliases still target it.
func (s *Store) DeleteProvider(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		return classify(err)
	}
	return mustAffect(res)
}

// SetProviderHealth records the result of a connectivity check. models is a
// JSON array (kept unchanged when empty).
func (s *Store) SetProviderHealth(ctx context.Context, id int64, status, errMsg, models string, at time.Time) error {
	q := `UPDATE providers SET health_status = ?, health_error = ?, health_checked_at = ?`
	args := []any{status, nullStr(errMsg), at.UnixMilli()}
	if models != "" {
		q += `, models_json = ?`
		args = append(args, models)
	}
	_, err := s.db.ExecContext(ctx, q+` WHERE id = ?`, append(args, id)...)
	return err
}

// SetProviderAnthropic updates supports_anthropic only.
func (s *Store) SetProviderAnthropic(ctx context.Context, id int64, v bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE providers SET supports_anthropic = ? WHERE id = ?`, boolInt(v), id)
	return err
}

// ProviderAliases returns the names of aliases targeting a provider.
func (s *Store) ProviderAliases(ctx context.Context, id int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT a.name FROM alias_targets t JOIN aliases a ON a.id = t.alias_id
		WHERE t.provider_id = ? ORDER BY a.name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---- aliases ---------------------------------------------------------------

// GetAlias loads an alias by id with all targets (including disabled providers).
func (s *Store) GetAlias(ctx context.Context, id int64) (*Alias, error) {
	a, err := scanAlias(s.db.QueryRowContext(ctx, `SELECT `+aliasCols+` FROM aliases WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Targets, err = s.targets(ctx, a.ID, false)
	return a, err
}

// ListAliasesFull returns all aliases with all targets.
func (s *Store) ListAliasesFull(ctx context.Context) ([]*Alias, error) {
	list, err := s.ListAliases(ctx, false)
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if a.Targets, err = s.targets(ctx, a.ID, false); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func fallbackArg(b *bool) any {
	if b == nil {
		return nil
	}
	return boolInt(*b)
}

func params(a *Alias) string {
	if len(a.Params) == 0 {
		return "{}"
	}
	return string(a.Params)
}

func writeTargets(ctx context.Context, tx *sql.Tx, aliasID int64, targets []Target) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM alias_targets WHERE alias_id = ?`, aliasID); err != nil {
		return err
	}
	for i, t := range targets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO alias_targets(alias_id, provider_id, upstream_model, priority)
			VALUES (?, ?, ?, ?)`, aliasID, t.Provider.ID, t.UpstreamModel, i); err != nil {
			return classify(err)
		}
	}
	return nil
}

// CreateAlias inserts an alias with its targets (priority = slice order).
func (s *Store) CreateAlias(ctx context.Context, a *Alias) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO aliases(name, enabled, description, ttft_timeout_ms, idle_timeout_ms,
		max_duration_ms, fallback_on_ttft_timeout, params_json, model_info_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Name, boolInt(a.Enabled), a.Description, durMs(a.TTFTTimeout), durMs(a.IdleTimeout), durMs(a.MaxDuration),
		fallbackArg(a.FallbackOnTTFTTimeout), params(a), a.ModelInfo.encode(), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return classify(err)
	}
	if a.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	if err := writeTargets(ctx, tx, a.ID, a.Targets); err != nil {
		return err
	}
	a.CreatedAt = now
	return tx.Commit()
}

// UpdateAlias saves an alias; targets are replaced when replaceTargets is true.
func (s *Store) UpdateAlias(ctx context.Context, a *Alias, replaceTargets bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE aliases SET name = ?, enabled = ?, description = ?, ttft_timeout_ms = ?,
		idle_timeout_ms = ?, max_duration_ms = ?, fallback_on_ttft_timeout = ?, params_json = ?, model_info_json = ?,
		updated_at = ? WHERE id = ?`, a.Name, boolInt(a.Enabled), a.Description, durMs(a.TTFTTimeout), durMs(a.IdleTimeout),
		durMs(a.MaxDuration), fallbackArg(a.FallbackOnTTFTTimeout), params(a), a.ModelInfo.encode(), time.Now().UnixMilli(), a.ID)
	if err != nil {
		return classify(err)
	}
	if err := mustAffect(res); err != nil {
		return err
	}
	if replaceTargets {
		if err := writeTargets(ctx, tx, a.ID, a.Targets); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteAlias deletes an alias (targets cascade).
func (s *Store) DeleteAlias(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM aliases WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// ResolveAliasAny loads an alias by exact name, enabled or not, with all targets.
func (s *Store) ResolveAliasAny(ctx context.Context, name string) (*Alias, error) {
	a, err := scanAlias(s.db.QueryRowContext(ctx, `SELECT `+aliasCols+` FROM aliases WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Targets, err = s.targets(ctx, a.ID, false)
	return a, err
}
