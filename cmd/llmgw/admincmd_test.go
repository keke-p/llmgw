package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"llmgw/internal/api/admin"
	"llmgw/internal/auth"
	"llmgw/internal/db"
	"llmgw/internal/store"
)

func TestResetAdminPasswordAndBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llmgw.db")
	t.Setenv("LLMGW_DB_PATH", path)
	ctx := context.Background()
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(d, nil)
	if err := admin.EnsureAdmin(ctx, st, "original-password-1", slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetAdminByUsername(ctx, "admin")
	st.CreateSession(ctx, &store.Session{ID: "s1", AdminID: a.ID, CSRFToken: "c", ExpiresAt: time.Now().Add(time.Hour), LastSeenAt: time.Now()})

	if err := resetAdminPassword([]string{"-password", "short"}); err == nil {
		t.Error("short password must be rejected")
	}
	if err := resetAdminPassword([]string{"-password", "brand-new-password"}); err != nil {
		t.Fatal(err)
	}
	a, _ = st.GetAdminByUsername(ctx, "admin")
	if ok, _ := auth.VerifyPassword(a.PasswordHash, "brand-new-password"); !ok {
		t.Error("password not changed")
	}
	if _, err := st.GetSession(ctx, "s1", time.Now()); err != store.ErrNotFound {
		t.Error("sessions must be revoked")
	}
	// Unknown user is created.
	if err := resetAdminPassword([]string{"-username", "ops", "-password", "another-password-2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAdminByUsername(ctx, "ops"); err != nil {
		t.Error("account must be created")
	}
	entries, _ := st.ListAudit(ctx, 10, 0)
	if len(entries) == 0 || entries[0].Action != "password.reset" || entries[0].Actor != "cli" {
		t.Errorf("audit = %+v", entries)
	}
	d.Close()

	out := filepath.Join(dir, "backup.db")
	if err := backup([]string{out}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("backup file: %v", err)
	}
	if err := backup([]string{out}); err == nil {
		t.Error("existing target must be refused")
	}
}
