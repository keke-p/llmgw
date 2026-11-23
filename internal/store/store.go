// Package store provides typed access to providers, aliases and settings.
//
// Alias resolution reads the DB on every request (SQLite is local and the
// queries are indexed), so changes to an alias take effect immediately without
// any cache invalidation.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"llmgw/internal/secret"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Provider types.
const (
	ProviderOllama           = "ollama"
	ProviderOpenAICompatible = "openai_compatible"
)

// Provider is an upstream LLM server.
type Provider struct {
	ID                int64
	Name              string
	Type              string
	BaseURL           string
	APIKey            string // decrypted; empty if none
	SupportsAnthropic bool
	ConnectTimeout    *time.Duration
	QueueTimeout      *time.Duration
	TTFTTimeout       *time.Duration
	IdleTimeout       *time.Duration
	MaxDuration       *time.Duration
	MaxConcurrency    int
	Enabled           bool

	// Health / discovery state (written by connectivity checks).
	HealthStatus    string // ok | error | "" (unknown)
	HealthError     string
	HealthCheckedAt *time.Time
	ModelsJSON      string // last fetched upstream model list (JSON array of names)
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Alias maps a public model name to upstream targets.
type Alias struct {
	ID                    int64
	Name                  string
	Enabled               bool
	Description           string
	TTFTTimeout           *time.Duration
	IdleTimeout           *time.Duration
	MaxDuration           *time.Duration
	FallbackOnTTFTTimeout *bool
	Params                json.RawMessage
	ModelInfo             ModelInfo // manual metadata overrides for /api/v1/models
	CreatedAt             time.Time
	// Targets are ordered by priority. Only targets whose provider is enabled
	// are included by ResolveAlias.
	Targets []Target
}

// ModelInfo overrides the model metadata that llmgw reports in the LM Studio
// compatible model list. A nil field means "detect from the upstream".
type ModelInfo struct {
	ContextLength *int  `json:"context_length,omitempty"`
	Tools         *bool `json:"tools,omitempty"`
	Vision        *bool `json:"vision,omitempty"`
}

// IsZero reports whether no override is set.
func (m ModelInfo) IsZero() bool {
	return m.ContextLength == nil && m.Tools == nil && m.Vision == nil
}

func (m ModelInfo) encode() string {
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Target is one upstream for an alias.
type Target struct {
	ID            int64
	Provider      *Provider
	UpstreamModel string
	Priority      int
}

// Store wraps the database.
type Store struct {
	db        *sql.DB
	secretKey []byte
}

// New returns a Store. secretKey decrypts provider API keys.
func New(db *sql.DB, secretKey []byte) *Store {
	return &Store{db: db, secretKey: secretKey}
}

// DB exposes the underlying database.
func (s *Store) DB() *sql.DB { return s.db }

// CandidateNames returns the names to try for a requested model, handling the
// Ollama ":latest" convention: "foo" also matches "foo:latest" and vice versa.
func CandidateNames(name string) []string {
	if strings.HasSuffix(name, ":latest") {
		return []string{name, strings.TrimSuffix(name, ":latest")}
	}
	if !strings.Contains(name, ":") {
		return []string{name, name + ":latest"}
	}
	return []string{name}
}

const aliasCols = `id, name, enabled, description, ttft_timeout_ms, idle_timeout_ms,
	max_duration_ms, fallback_on_ttft_timeout, params_json, model_info_json, created_at`

// ResolveAlias finds an enabled alias by public name and loads its targets with
// enabled providers. It returns ErrNotFound for unknown or disabled aliases.
func (s *Store) ResolveAlias(ctx context.Context, name string) (*Alias, error) {
	var a *Alias
	for _, cand := range CandidateNames(name) {
		row := s.db.QueryRowContext(ctx, `SELECT `+aliasCols+` FROM aliases WHERE name = ?`, cand)
		got, err := scanAlias(row)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		a = got
		break
	}
	if a == nil || !a.Enabled {
		return nil, ErrNotFound
	}
	targets, err := s.targets(ctx, a.ID, true)
	if err != nil {
		return nil, err
	}
	a.Targets = targets
	return a, nil
}

// ListAliases returns all aliases (without targets), ordered by name.
func (s *Store) ListAliases(ctx context.Context, enabledOnly bool) ([]*Alias, error) {
	q := `SELECT ` + aliasCols + ` FROM aliases`
	if enabledOnly {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Alias
	for rows.Next() {
		a, err := scanAlias(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanAlias(r scanner) (*Alias, error) {
	var (
		a                Alias
		enabled          int
		ttft, idle, maxd sql.NullInt64
		fallback         sql.NullInt64
		params           string
		modelInfo        string
		createdAt        int64
	)
	if err := r.Scan(&a.ID, &a.Name, &enabled, &a.Description, &ttft, &idle, &maxd,
		&fallback, &params, &modelInfo, &createdAt); err != nil {
		return nil, err
	}
	a.Enabled = enabled != 0
	a.TTFTTimeout = msPtr(ttft)
	a.IdleTimeout = msPtr(idle)
	a.MaxDuration = msPtr(maxd)
	if fallback.Valid {
		b := fallback.Int64 != 0
		a.FallbackOnTTFTTimeout = &b
	}
	a.Params = json.RawMessage(params)
	_ = json.Unmarshal([]byte(modelInfo), &a.ModelInfo) // invalid JSON = no overrides
	a.CreatedAt = time.UnixMilli(createdAt)
	return &a, nil
}

const providerCols = `p.id, p.name, p.type, p.base_url, p.api_key_enc, p.supports_anthropic,
	p.connect_timeout_ms, p.queue_timeout_ms, p.ttft_timeout_ms, p.idle_timeout_ms,
	p.max_duration_ms, p.max_concurrency, p.enabled, p.health_status, p.health_error,
	p.health_checked_at, p.models_json, p.created_at, p.updated_at`

func (s *Store) targets(ctx context.Context, aliasID int64, enabledProvidersOnly bool) ([]Target, error) {
	q := `SELECT t.id, t.upstream_model, t.priority, ` + providerCols + `
		FROM alias_targets t JOIN providers p ON p.id = t.provider_id
		WHERE t.alias_id = ?`
	if enabledProvidersOnly {
		q += ` AND p.enabled = 1`
	}
	q += ` ORDER BY t.priority, t.id`
	rows, err := s.db.QueryContext(ctx, q, aliasID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		var t Target
		dest := []any{&t.ID, &t.UpstreamModel, &t.Priority}
		p, pdest := s.providerDest()
		if err := rows.Scan(append(dest, pdest...)...); err != nil {
			return nil, err
		}
		if err := p.finish(s.secretKey); err != nil {
			return nil, err
		}
		t.Provider = p.Provider
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetProvider loads one provider.
func (s *Store) GetProvider(ctx context.Context, id int64) (*Provider, error) {
	p, dest := s.providerDest()
	err := s.db.QueryRowContext(ctx, `SELECT `+providerCols+` FROM providers p WHERE p.id = ?`, id).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := p.finish(s.secretKey); err != nil {
		return nil, err
	}
	return p.Provider, nil
}

type providerScan struct {
	*Provider
	apiKeyEnc                                []byte
	supportsAnthropic, enabled               int
	connect, queue, ttft, idle, maxd, maxcon sql.NullInt64
	hStatus, hError, models                  sql.NullString
	hChecked                                 sql.NullInt64
	created, updated                         int64
}

func (s *Store) providerDest() (*providerScan, []any) {
	p := &providerScan{Provider: &Provider{}}
	return p, []any{&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.apiKeyEnc, &p.supportsAnthropic,
		&p.connect, &p.queue, &p.ttft, &p.idle, &p.maxd, &p.maxcon, &p.enabled,
		&p.hStatus, &p.hError, &p.hChecked, &p.models, &p.created, &p.updated}
}

func (p *providerScan) finish(key []byte) error {
	p.SupportsAnthropic = p.supportsAnthropic != 0
	p.Enabled = p.enabled != 0
	p.ConnectTimeout = msPtr(p.connect)
	p.QueueTimeout = msPtr(p.queue)
	p.TTFTTimeout = msPtr(p.ttft)
	p.IdleTimeout = msPtr(p.idle)
	p.MaxDuration = msPtr(p.maxd)
	p.MaxConcurrency = int(p.maxcon.Int64)
	p.HealthStatus, p.HealthError, p.ModelsJSON = p.hStatus.String, p.hError.String, p.models.String
	p.HealthCheckedAt = msTime(p.hChecked)
	p.CreatedAt, p.UpdatedAt = time.UnixMilli(p.created), time.UnixMilli(p.updated)
	if len(p.apiKeyEnc) > 0 {
		k, err := secret.Decrypt(key, p.apiKeyEnc)
		if err != nil {
			return fmt.Errorf("provider %q api key: %w", p.Name, err)
		}
		p.APIKey = k
	}
	return nil
}

func msPtr(v sql.NullInt64) *time.Duration {
	if !v.Valid {
		return nil
	}
	d := time.Duration(v.Int64) * time.Millisecond
	return &d
}
