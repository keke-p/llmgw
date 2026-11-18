package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"llmgw/internal/api/admin"
	"llmgw/internal/auth"
	"llmgw/internal/chat"
	"llmgw/internal/db"
	"llmgw/internal/store"
)

func dbPath() string {
	if p := os.Getenv("LLMGW_DB_PATH"); p != "" {
		return p
	}
	return "data/llmgw.db"
}

// resetAdminPassword sets a new admin password (creating the account if it
// does not exist) and revokes the account's sessions. It works while the
// server is running.
func resetAdminPassword(args []string) error {
	fs := flag.NewFlagSet("reset-admin-password", flag.ContinueOnError)
	username := fs.String("username", admin.DefaultUsername, "admin account name")
	password := fs.String("password", "", "new password (default: $LLMGW_NEW_ADMIN_PASSWORD, or a generated one)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pw := *password
	if pw == "" {
		pw = os.Getenv("LLMGW_NEW_ADMIN_PASSWORD")
	}
	generated := pw == ""
	if generated {
		pw = chat.RandomString(24)
	} else if len(pw) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dbPath())
	if err != nil {
		return err
	}
	defer d.Close()
	st := store.New(d, nil)
	a, err := st.GetAdminByUsername(ctx, *username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if _, err := st.CreateAdmin(ctx, *username, hash); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "created admin account %q\n", *username)
	case err != nil:
		return err
	default:
		// "" keeps no session: every session of this account is revoked.
		if err := st.SetAdminPassword(ctx, a.ID, hash, ""); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "password of %q has been reset; existing sessions were revoked\n", *username)
	}
	_ = st.AddAudit(ctx, "cli", "password.reset", "admin", "", map[string]any{"username": *username}, "")
	if generated {
		fmt.Println(pw)
	}
	return nil
}

// backup writes an online backup of the database to path.
func backup(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: llmgw backup <path>")
	}
	if _, err := os.Stat(args[0]); err == nil {
		return fmt.Errorf("%s already exists", args[0])
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dbPath())
	if err != nil {
		return err
	}
	defer d.Close()
	if err := store.New(d, nil).Backup(ctx, args[0]); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "backup written to %s\n", args[0])
	return nil
}
