package keys

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"llmgw/internal/clock"
	"llmgw/internal/db"
	"llmgw/internal/store"
)

func newService(t *testing.T) (*Service, *clock.Fake) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c := clock.NewFake()
	return NewService(store.New(d, nil), c, slog.New(slog.NewTextHandler(io.Discard, nil))), c
}

func TestGenerate(t *testing.T) {
	plain, hash, display := Generate()
	if !regexp.MustCompile(`^sk-gw-[0-9A-Za-z]{40}$`).MatchString(plain) {
		t.Errorf("plain = %q", plain)
	}
	if len(hash) != 64 || hash != Hash(plain) {
		t.Errorf("hash = %q", hash)
	}
	if display != plain[:10] {
		t.Errorf("display = %q", display)
	}
	p2, _, _ := Generate()
	if p2 == plain {
		t.Error("keys must be random")
	}
}

func TestTokenFromRequest(t *testing.T) {
	cases := []struct{ hdr, val, want string }{
		{"Authorization", "Bearer sk-gw-a", "sk-gw-a"},
		{"Authorization", "bearer  sk-gw-b ", "sk-gw-b"},
		{"x-api-key", "sk-gw-c", "sk-gw-c"},
		{"Authorization", "Basic xyz", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set(c.hdr, c.val)
		if got := TokenFromRequest(r); got != c.want {
			t.Errorf("%s: %q -> %q, want %q", c.hdr, c.val, got, c.want)
		}
	}
}

func authCode(s *Service, tok string) int {
	_, ge := s.AuthenticateToken(context.Background(), tok)
	if ge == nil {
		return 200
	}
	return ge.Status
}

func TestAuthenticateLifecycle(t *testing.T) {
	s, c := newService(t)
	ctx := context.Background()
	k := &store.VirtualKey{Name: "app", AllowedAliases: []string{"a"}, LogMode: store.LogMetadataOnly}
	plain, err := s.Create(ctx, k)
	if err != nil {
		t.Fatal(err)
	}

	p, ge := s.AuthenticateToken(ctx, plain)
	if ge != nil || p.KeyID != k.ID || p.KeyName != "app" || p.LogMode != "metadata_only" || len(p.AllowedAliases) != 1 {
		t.Fatalf("p=%+v ge=%v", p, ge)
	}
	if got := authCode(s, ""); got != 401 {
		t.Errorf("missing: %d", got)
	}
	if got := authCode(s, "sk-gw-nope"); got != 401 {
		t.Errorf("invalid: %d", got)
	}

	// Disabling takes effect immediately despite the cache.
	k.Status = store.KeyDisabled
	if err := s.Update(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, ge := s.AuthenticateToken(ctx, plain); ge == nil || ge.Message != "API key is disabled" {
		t.Errorf("disabled: %v", ge)
	}
	k.Status = store.KeyActive
	exp := c.Now().Add(time.Hour)
	k.ExpiresAt = &exp
	s.Update(ctx, k)
	if got := authCode(s, plain); got != 200 {
		t.Errorf("re-enabled: %d", got)
	}
	c.Advance(time.Hour)
	if _, ge := s.AuthenticateToken(ctx, plain); ge == nil || ge.Message != "API key has expired" {
		t.Errorf("expired: %v", ge)
	}
	k.ExpiresAt = nil
	s.Update(ctx, k)

	// Rotation without grace: old key dies immediately.
	newPlain, _, err := s.Rotate(ctx, k.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if authCode(s, plain) != 401 || authCode(s, newPlain) != 200 {
		t.Error("rotation without grace")
	}
	// Rotation with grace: both work until the grace period ends.
	newer, nk, _ := s.Rotate(ctx, k.ID, 10*time.Minute)
	if nk.PrevKeyExpiresAt == nil || nk.Name != "app" || len(nk.AllowedAliases) != 1 {
		t.Errorf("rotated record = %+v", nk)
	}
	if authCode(s, newPlain) != 200 || authCode(s, newer) != 200 {
		t.Error("grace period: both keys must work")
	}
	c.Advance(10 * time.Minute)
	if authCode(s, newPlain) != 401 || authCode(s, newer) != 200 {
		t.Error("after grace: only the new key must work")
	}

	if err := s.Delete(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if authCode(s, newer) != 401 {
		t.Error("deleted key must fail immediately")
	}
}

func TestAuthCacheTTL(t *testing.T) {
	s, c := newService(t)
	ctx := context.Background()
	k := &store.VirtualKey{Name: "x"}
	plain, _ := s.Create(ctx, k)
	authCode(s, plain) // cached
	// Bypass the service (as another process would): cache still answers.
	k.Status = store.KeyDisabled
	s.Store.UpdateKey(ctx, k)
	if authCode(s, plain) != 200 {
		t.Error("expected cached result within TTL")
	}
	c.Advance(s.CacheTTL)
	if authCode(s, plain) != 401 {
		t.Error("expected refresh after TTL")
	}
}

func TestRPMLimit(t *testing.T) {
	s, c := newService(t)
	ctx := context.Background()
	plain, _ := s.Create(ctx, &store.VirtualKey{Name: "x", RPMLimit: 2})
	authCode(s, plain)
	c.Advance(10 * time.Second)
	authCode(s, plain)
	_, ge := s.AuthenticateToken(ctx, plain)
	if ge == nil || ge.Status != 429 || ge.RetryAfter != 50*time.Second {
		t.Fatalf("ge = %+v", ge)
	}
	c.Advance(50 * time.Second)
	if authCode(s, plain) != 200 {
		t.Error("window should have slid")
	}
}
