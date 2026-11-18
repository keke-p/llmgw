package admin

import (
	"context"
	"log/slog"

	"llmgw/internal/auth"
	"llmgw/internal/chat"
	"llmgw/internal/store"
)

// DefaultUsername is the initial administrator account name.
const DefaultUsername = "admin"

// EnsureAdmin creates the initial administrator on first start. When password is
// empty a random one is generated and logged exactly once.
func EnsureAdmin(ctx context.Context, st *store.Store, password string, log *slog.Logger) error {
	n, err := st.CountAdmins(ctx)
	if err != nil || n > 0 {
		return err
	}
	generated := password == ""
	if generated {
		password = chat.RandomString(24)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := st.CreateAdmin(ctx, DefaultUsername, hash); err != nil {
		return err
	}
	if generated {
		log.Warn("created initial admin account with a generated password; it is shown only once, change it after login",
			"username", DefaultUsername, "password", password)
	} else {
		log.Info("created initial admin account from LLMGW_ADMIN_PASSWORD", "username", DefaultUsername)
	}
	return nil
}
