package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Settings are the dynamic, DB-stored global settings.
type Settings struct {
	ConnectTimeout        time.Duration
	QueueTimeout          time.Duration
	TTFTTimeout           time.Duration
	IdleTimeout           time.Duration
	MaxDuration           time.Duration
	FallbackOnTTFTTimeout bool
	KeepaliveInterval     time.Duration
	LogRetentionDays      int
	LogBodyMaxBytes       int
	LogUpstreamRequest    bool
	HealthCheckInterval   time.Duration
}

// DefaultSettings are used for keys missing from the settings table.
func DefaultSettings() Settings {
	return Settings{
		ConnectTimeout:      10 * time.Second,
		QueueTimeout:        120 * time.Second,
		TTFTTimeout:         300 * time.Second,
		IdleTimeout:         120 * time.Second,
		MaxDuration:         1800 * time.Second,
		KeepaliveInterval:   15 * time.Second,
		LogRetentionDays:    30,
		LogBodyMaxBytes:     1 << 20,
		HealthCheckInterval: 60 * time.Second,
	}
}

// settingFields maps setting keys to Settings fields. Durations are stored in ms.
func settingFields(s *Settings) map[string]any {
	return map[string]any{
		"connect_timeout_ms":       &s.ConnectTimeout,
		"queue_timeout_ms":         &s.QueueTimeout,
		"ttft_timeout_ms":          &s.TTFTTimeout,
		"idle_timeout_ms":          &s.IdleTimeout,
		"max_duration_ms":          &s.MaxDuration,
		"fallback_on_ttft_timeout": &s.FallbackOnTTFTTimeout,
		"keepalive_interval_ms":    &s.KeepaliveInterval,
		"log_retention_days":       &s.LogRetentionDays,
		"log_body_max_bytes":       &s.LogBodyMaxBytes,
		"log_upstream_request":     &s.LogUpstreamRequest,
		"health_check_interval_ms": &s.HealthCheckInterval,
	}
}

// Settings loads the global settings, filling defaults for missing keys.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	out := DefaultSettings()
	fields := settingFields(&out)
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return out, err
		}
		f, ok := fields[k]
		if !ok {
			continue
		}
		if err := decodeSetting(f, v); err != nil {
			return out, fmt.Errorf("setting %s: %w", k, err)
		}
	}
	return out, rows.Err()
}

// SetSetting stores one setting value (JSON encoded). Durations are given in ms.
func (s *Store) SetSetting(ctx context.Context, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var probe Settings
	f, ok := settingFields(&probe)[key]
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	if err := decodeSetting(f, string(b)); err != nil {
		return fmt.Errorf("setting %s: %w", key, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(b), time.Now().UnixMilli())
	return err
}

func decodeSetting(field any, raw string) error {
	switch f := field.(type) {
	case *time.Duration:
		var ms int64
		if err := json.Unmarshal([]byte(raw), &ms); err != nil {
			return err
		}
		if ms < 0 {
			return fmt.Errorf("must not be negative")
		}
		*f = time.Duration(ms) * time.Millisecond
	default:
		return json.Unmarshal([]byte(raw), field)
	}
	return nil
}

// SettingsMap renders settings as {key: value} with durations in ms.
func SettingsMap(s Settings) map[string]any {
	out := map[string]any{}
	for k, f := range settingFields(&s) {
		switch v := f.(type) {
		case *time.Duration:
			out[k] = v.Milliseconds()
		case *bool:
			out[k] = *v
		case *int:
			out[k] = *v
		}
	}
	return out
}

// ValidateSetting checks a JSON value for a setting key without storing it.
func ValidateSetting(key string, raw json.RawMessage) error {
	var probe Settings
	f, ok := settingFields(&probe)[key]
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	if err := decodeSetting(f, string(raw)); err != nil {
		return fmt.Errorf("setting %s: %w", key, err)
	}
	return nil
}
