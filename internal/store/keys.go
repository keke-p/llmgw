package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Key statuses and log modes.
const (
	KeyActive   = "active"
	KeyDisabled = "disabled"

	LogFull         = "full"
	LogMetadataOnly = "metadata_only"
	LogNone         = "none"
)

// VirtualKey is a client API key record. Only the SHA-256 hash of the key is stored.
type VirtualKey struct {
	ID                 int64
	Name               string
	KeyPrefix          string
	KeyHash            string
	PrevKeyHash        string     // valid until PrevKeyExpiresAt (rotation grace)
	PrevKeyExpiresAt   *time.Time // nil when there is no previous key
	Status             string
	AllowedAliases     []string // empty = all
	ExpiresAt          *time.Time
	LogMode            string
	MaxConcurrency     int // 0 = unlimited
	RPMLimit           int // 0 = unlimited
	NonStreamKeepalive bool
	Imported           bool
	LastUsedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

const keyCols = `id, name, key_prefix, key_hash, prev_key_hash, prev_key_expires_at, status,
	allowed_aliases_json, expires_at, log_mode, max_concurrency, rpm_limit, nonstream_keepalive,
	imported, last_used_at, created_at, updated_at`

func scanKey(r scanner) (*VirtualKey, error) {
	var (
		k                   VirtualKey
		prevHash            sql.NullString
		prevExp, exp, used  sql.NullInt64
		allowed             string
		maxcon, rpm         sql.NullInt64
		keepalive, imported int
		created, updated    int64
	)
	if err := r.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.KeyHash, &prevHash, &prevExp, &k.Status,
		&allowed, &exp, &k.LogMode, &maxcon, &rpm, &keepalive, &imported, &used, &created, &updated); err != nil {
		return nil, err
	}
	k.PrevKeyHash = prevHash.String
	k.PrevKeyExpiresAt = msTime(prevExp)
	k.ExpiresAt = msTime(exp)
	k.LastUsedAt = msTime(used)
	if err := json.Unmarshal([]byte(allowed), &k.AllowedAliases); err != nil {
		return nil, err
	}
	if k.AllowedAliases == nil {
		k.AllowedAliases = []string{}
	}
	k.MaxConcurrency, k.RPMLimit = int(maxcon.Int64), int(rpm.Int64)
	k.NonStreamKeepalive, k.Imported = keepalive != 0, imported != 0
	k.CreatedAt, k.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return &k, nil
}

// ListKeys returns all keys ordered by id.
func (s *Store) ListKeys(ctx context.Context) ([]*VirtualKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyCols+` FROM virtual_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*VirtualKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetKey loads a key by id.
func (s *Store) GetKey(ctx context.Context, id int64) (*VirtualKey, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM virtual_keys WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

// FindKeyByHash finds a key by its current or previous (rotation grace) hash.
// Validity (status, expiry, grace period) is checked by the caller.
func (s *Store) FindKeyByHash(ctx context.Context, hash string) (*VirtualKey, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx,
		`SELECT `+keyCols+` FROM virtual_keys WHERE key_hash = ? OR prev_key_hash = ? LIMIT 1`, hash, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

func keyArgs(k *VirtualKey) (allowed string, exp, maxcon, rpm any) {
	aa := k.AllowedAliases
	if aa == nil {
		aa = []string{}
	}
	b, _ := json.Marshal(aa)
	return string(b), timeMs(k.ExpiresAt), zeroNull(k.MaxConcurrency), zeroNull(k.RPMLimit)
}

// CreateKey inserts a key and sets k.ID.
func (s *Store) CreateKey(ctx context.Context, k *VirtualKey) error {
	now := time.Now()
	allowed, exp, maxcon, rpm := keyArgs(k)
	res, err := s.db.ExecContext(ctx, `INSERT INTO virtual_keys(name, key_prefix, key_hash, status,
		allowed_aliases_json, expires_at, log_mode, max_concurrency, rpm_limit, nonstream_keepalive, imported,
		created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.Name, k.KeyPrefix, k.KeyHash, k.Status, allowed, exp, k.LogMode, maxcon, rpm,
		boolInt(k.NonStreamKeepalive), boolInt(k.Imported), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return err
	}
	k.ID, err = res.LastInsertId()
	k.CreatedAt, k.UpdatedAt = now, now
	return err
}

// UpdateKey saves the mutable settings of a key (not its hash).
func (s *Store) UpdateKey(ctx context.Context, k *VirtualKey) error {
	allowed, exp, maxcon, rpm := keyArgs(k)
	res, err := s.db.ExecContext(ctx, `UPDATE virtual_keys SET name = ?, status = ?, allowed_aliases_json = ?,
		expires_at = ?, log_mode = ?, max_concurrency = ?, rpm_limit = ?, nonstream_keepalive = ?, updated_at = ?
		WHERE id = ?`, k.Name, k.Status, allowed, exp, k.LogMode, maxcon, rpm, boolInt(k.NonStreamKeepalive),
		time.Now().UnixMilli(), k.ID)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// RotateKey replaces the key hash. When graceUntil is non-nil the old hash stays
// valid until then; otherwise it is invalidated immediately.
func (s *Store) RotateKey(ctx context.Context, id int64, newHash, newPrefix string, graceUntil *time.Time) error {
	var res sql.Result
	var err error
	now := time.Now().UnixMilli()
	if graceUntil != nil {
		res, err = s.db.ExecContext(ctx, `UPDATE virtual_keys SET prev_key_hash = key_hash, prev_key_expires_at = ?,
			key_hash = ?, key_prefix = ?, imported = 0, updated_at = ? WHERE id = ?`,
			graceUntil.UnixMilli(), newHash, newPrefix, now, id)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE virtual_keys SET prev_key_hash = NULL, prev_key_expires_at = NULL,
			key_hash = ?, key_prefix = ?, imported = 0, updated_at = ? WHERE id = ?`, newHash, newPrefix, now, id)
	}
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// DeleteKey physically deletes a key (request logs keep the name snapshot).
func (s *Store) DeleteKey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM virtual_keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// TouchKey updates last_used_at.
func (s *Store) TouchKey(ctx context.Context, id int64, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE virtual_keys SET last_used_at = ? WHERE id = ?`, t.UnixMilli(), id)
	return err
}

func mustAffect(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func msTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64)
	return &t
}

func timeMs(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func zeroNull(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
