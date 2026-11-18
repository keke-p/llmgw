package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Admin is an administrator account.
type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
}

// CountAdmins returns the number of admin accounts.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n)
	return n, err
}

// CreateAdmin inserts an admin.
func (s *Store) CreateAdmin(ctx context.Context, username, passwordHash string) (int64, error) {
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `INSERT INTO admins(username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		username, passwordHash, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetAdminByUsername loads an admin by name.
func (s *Store) GetAdminByUsername(ctx context.Context, username string) (*Admin, error) {
	var a Admin
	err := s.db.QueryRowContext(ctx, `SELECT id, username, password_hash FROM admins WHERE username = ?`, username).
		Scan(&a.ID, &a.Username, &a.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

// GetAdmin loads an admin by id.
func (s *Store) GetAdmin(ctx context.Context, id int64) (*Admin, error) {
	var a Admin
	err := s.db.QueryRowContext(ctx, `SELECT id, username, password_hash FROM admins WHERE id = ?`, id).
		Scan(&a.ID, &a.Username, &a.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

// SetAdminPassword updates the password hash and revokes the admin's other sessions.
func (s *Store) SetAdminPassword(ctx context.Context, id int64, hash, keepSessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE admins SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE admin_id = ? AND id <> ?`, id, keepSessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// Session is an admin login session. ID is the SHA-256 of the cookie token.
type Session struct {
	ID         string
	AdminID    int64
	Username   string
	CSRFToken  string
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// CreateSession stores a session.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin_sessions(id, admin_id, csrf_token, created_at, expires_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)`, sess.ID, sess.AdminID, sess.CSRFToken, now, sess.ExpiresAt.UnixMilli(), sess.LastSeenAt.UnixMilli())
	return err
}

// GetSession loads a session (expired sessions are reported as ErrNotFound).
func (s *Store) GetSession(ctx context.Context, id string, now time.Time) (*Session, error) {
	var sess Session
	var exp, seen int64
	err := s.db.QueryRowContext(ctx, `SELECT s.id, s.admin_id, a.username, s.csrf_token, s.expires_at, s.last_seen_at
		FROM admin_sessions s JOIN admins a ON a.id = s.admin_id WHERE s.id = ?`, id).
		Scan(&sess.ID, &sess.AdminID, &sess.Username, &sess.CSRFToken, &exp, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess.ExpiresAt, sess.LastSeenAt = time.UnixMilli(exp), time.UnixMilli(seen)
	if !now.Before(sess.ExpiresAt) {
		return nil, ErrNotFound
	}
	return &sess, nil
}

// TouchSession extends a session.
func (s *Store) TouchSession(ctx context.Context, id string, seen, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE admin_sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		seen.UnixMilli(), expires.UnixMilli(), id)
	return err
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE id = ?`, id)
	return err
}

// DeleteExpiredSessions removes expired sessions.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at <= ?`, now.UnixMilli())
	return err
}

// ---- audit log -------------------------------------------------------------

// AuditEntry is one audit log row.
type AuditEntry struct {
	ID         int64           `json:"id"`
	CreatedAt  time.Time       `json:"created_at"`
	Actor      string          `json:"actor"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type,omitempty"`
	TargetID   string          `json:"target_id,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	ClientIP   string          `json:"client_ip,omitempty"`
}

// AddAudit records an admin action. detail is marshalled to JSON (may be nil).
func (s *Store) AddAudit(ctx context.Context, actor, action, targetType, targetID string, detail any, clientIP string) error {
	var d sql.NullString
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		d = sql.NullString{String: string(b), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_logs(created_at, actor, action, target_type, target_id, detail_json, client_ip)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, time.Now().UnixMilli(), actor, action,
		nullStr(targetType), nullStr(targetID), d, nullStr(clientIP))
	return err
}

// ListAudit returns audit entries, newest first. beforeID > 0 pages backwards.
func (s *Store) ListAudit(ctx context.Context, limit int, beforeID int64) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, created_at, actor, action, target_type, target_id, detail_json, client_ip FROM audit_logs`
	args := []any{}
	if beforeID > 0 {
		q += ` WHERE id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var created int64
		var tt, tid, detail, ip sql.NullString
		if err := rows.Scan(&e.ID, &created, &e.Actor, &e.Action, &tt, &tid, &detail, &ip); err != nil {
			return nil, err
		}
		e.CreatedAt = time.UnixMilli(created).UTC()
		e.TargetType, e.TargetID, e.ClientIP = tt.String, tid.String, ip.String
		if detail.Valid {
			e.Detail = json.RawMessage(detail.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// Backup writes a consistent copy of the database to path (which must not
// exist) using VACUUM INTO. It is safe while the server is running.
func (s *Store) Backup(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}
