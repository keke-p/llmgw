package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenAndMigrate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "test.db")

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := SchemaVersion(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := loadMigrations()
	if v != ms[len(ms)-1].version {
		t.Errorf("schema version = %d, want %d", v, ms[len(ms)-1].version)
	}

	var fk int
	if err := d.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, err = %v", fk, err)
	}
	for _, tbl := range []string{"admins", "providers", "aliases", "alias_targets", "virtual_keys", "request_logs", "audit_logs", "settings"} {
		var n int
		if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
			t.Errorf("table %s: %v", tbl, err)
		}
	}
	d.Close()

	// Re-opening must be idempotent.
	d, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var count int
	if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(ms) {
		t.Errorf("schema_migrations rows = %d, want %d", count, len(ms))
	}
}

func TestForeignKeyRestrict(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "fk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO providers(id, name, type, base_url, created_at, updated_at) VALUES (1, 'p', 'ollama', 'http://x', 0, 0)`)
	mustExec(`INSERT INTO aliases(id, name, created_at, updated_at) VALUES (1, 'a', 0, 0)`)
	mustExec(`INSERT INTO alias_targets(alias_id, provider_id, upstream_model) VALUES (1, 1, 'm')`)
	if _, err := d.ExecContext(ctx, `DELETE FROM providers WHERE id = 1`); err == nil {
		t.Error("deleting a provider referenced by a target should fail")
	}
	mustExec(`DELETE FROM aliases WHERE id = 1`)
	var n int
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM alias_targets`).Scan(&n)
	if n != 0 {
		t.Errorf("targets should cascade, got %d", n)
	}
}
