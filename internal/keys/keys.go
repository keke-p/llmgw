// Package keys manages virtual keys: generation, authentication (with a short
// in-memory cache), rpm limiting and lifecycle operations. All mutations go
// through Service so the auth cache is invalidated immediately.
package keys

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"llmgw/internal/auth"
	"llmgw/internal/chat"
	"llmgw/internal/clock"
	"llmgw/internal/gateway"
	"llmgw/internal/gwerr"
	"llmgw/internal/store"
)

// Prefix is the prefix of every generated key.
const Prefix = "sk-gw-"

const (
	randomLen     = 40
	displayLen    = len(Prefix) + 4
	defaultTTL    = 30 * time.Second
	maxCacheSize  = 10000
	touchInterval = time.Minute
	rpmWindow     = time.Minute
)

// Generate returns a new plaintext key, its SHA-256 hash and display prefix.
func Generate() (plain, hash, display string) {
	plain = Prefix + chat.RandomString(randomLen)
	return plain, Hash(plain), plain[:displayLen]
}

// Hash returns the stored representation of a key (SHA-256 hex).
func Hash(plain string) string { return auth.SHA256Hex(plain) }

// TokenFromRequest extracts the key from "Authorization: Bearer" or "x-api-key".
func TokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, tok, ok := strings.Cut(h, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(tok)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

type cacheEntry struct {
	key *store.VirtualKey // nil = no such key (negative cache)
	at  time.Time
}

// Service authenticates and manages virtual keys.
type Service struct {
	Store    *store.Store
	Clock    clock.Clock
	Log      *slog.Logger
	CacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
	gen   uint64

	touchMu   sync.Mutex
	lastTouch map[int64]time.Time

	rpm rpmLimiter
}

// NewService returns a Service.
func NewService(st *store.Store, c clock.Clock, log *slog.Logger) *Service {
	return &Service{Store: st, Clock: c, Log: log, CacheTTL: defaultTTL,
		cache: map[string]cacheEntry{}, lastTouch: map[int64]time.Time{},
		rpm: rpmLimiter{hits: map[int64][]time.Time{}}}
}

// Invalidate drops the auth cache (called after every key mutation).
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.cache = map[string]cacheEntry{}
	s.gen++
	s.mu.Unlock()
}

func (s *Service) lookup(ctx context.Context, hash string) (*store.VirtualKey, error) {
	now := s.Clock.Now()
	s.mu.Lock()
	if e, ok := s.cache[hash]; ok && now.Sub(e.at) < s.CacheTTL {
		s.mu.Unlock()
		return e.key, nil
	}
	gen := s.gen
	s.mu.Unlock()

	k, err := s.Store.FindKeyByHash(ctx, hash)
	if err == store.ErrNotFound {
		k, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.gen == gen { // do not cache results that raced with an invalidation
		if len(s.cache) >= maxCacheSize {
			s.cache = map[string]cacheEntry{}
		}
		s.cache[hash] = cacheEntry{key: k, at: now}
	}
	s.mu.Unlock()
	return k, nil
}

func authErr(msg string) *gwerr.Error {
	return &gwerr.Error{Status: http.StatusUnauthorized, Kind: gwerr.Authentication, Message: msg, Code: "invalid_api_key"}
}

// Authenticate validates a request's key and returns the principal.
func (s *Service) Authenticate(r *http.Request) (*gateway.Principal, *gwerr.Error) {
	return s.AuthenticateToken(r.Context(), TokenFromRequest(r))
}

// AuthenticateToken validates a plaintext key.
func (s *Service) AuthenticateToken(ctx context.Context, token string) (*gateway.Principal, *gwerr.Error) {
	if token == "" {
		return nil, authErr("missing API key (use Authorization: Bearer <key> or x-api-key)")
	}
	hash := Hash(token)
	k, err := s.lookup(ctx, hash)
	if err != nil {
		s.Log.Error("key lookup", "err", err)
		return nil, gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error")
	}
	if k == nil {
		return nil, authErr("invalid API key")
	}
	now := s.Clock.Now()
	current := subtle.ConstantTimeCompare([]byte(k.KeyHash), []byte(hash)) == 1
	prev := k.PrevKeyHash != "" && subtle.ConstantTimeCompare([]byte(k.PrevKeyHash), []byte(hash)) == 1 &&
		k.PrevKeyExpiresAt != nil && now.Before(*k.PrevKeyExpiresAt)
	if !current && !prev {
		return nil, authErr("invalid API key")
	}
	if k.Status != store.KeyActive {
		return nil, authErr("API key is disabled")
	}
	if k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) {
		return nil, authErr("API key has expired")
	}
	if k.RPMLimit > 0 {
		if wait, ok := s.rpm.allow(k.ID, k.RPMLimit, now); !ok {
			e := gwerr.New(http.StatusTooManyRequests, gwerr.RateLimit, "rate limit exceeded: %d requests per minute", k.RPMLimit)
			e.Code, e.RetryAfter = "rate_limit_exceeded", wait
			return nil, e
		}
	}
	s.touch(k.ID, now)
	return &gateway.Principal{
		KeyID: k.ID, KeyName: k.Name, AllowedAliases: k.AllowedAliases, LogMode: k.LogMode,
		NonStreamKeepalive: k.NonStreamKeepalive, MaxConcurrency: k.MaxConcurrency,
	}, nil
}

// touch updates last_used_at at most once per touchInterval per key.
func (s *Service) touch(id int64, now time.Time) {
	s.touchMu.Lock()
	if last, ok := s.lastTouch[id]; ok && now.Sub(last) < touchInterval {
		s.touchMu.Unlock()
		return
	}
	s.lastTouch[id] = now
	s.touchMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Store.TouchKey(ctx, id, now); err != nil {
			s.Log.Warn("update last_used_at", "err", err, "key_id", id)
		}
	}()
}

// ---- lifecycle -------------------------------------------------------------

// Create issues a new key. The plaintext is returned once and never stored.
func (s *Service) Create(ctx context.Context, k *store.VirtualKey) (string, error) {
	plain, hash, display := Generate()
	k.KeyHash, k.KeyPrefix = hash, display
	if k.Status == "" {
		k.Status = store.KeyActive
	}
	if k.LogMode == "" {
		k.LogMode = store.LogFull
	}
	if err := s.Store.CreateKey(ctx, k); err != nil {
		return "", err
	}
	s.Invalidate()
	return plain, nil
}

// Update saves key settings.
func (s *Service) Update(ctx context.Context, k *store.VirtualKey) error {
	defer s.Invalidate()
	return s.Store.UpdateKey(ctx, k)
}

// Rotate replaces the key while keeping the record. With grace > 0 the old key
// stays valid for that long; otherwise it stops working immediately.
func (s *Service) Rotate(ctx context.Context, id int64, grace time.Duration) (string, *store.VirtualKey, error) {
	plain, hash, display := Generate()
	var until *time.Time
	if grace > 0 {
		t := s.Clock.Now().Add(grace)
		until = &t
	}
	defer s.Invalidate()
	if err := s.Store.RotateKey(ctx, id, hash, display, until); err != nil {
		return "", nil, err
	}
	k, err := s.Store.GetKey(ctx, id)
	return plain, k, err
}

// Delete removes a key.
func (s *Service) Delete(ctx context.Context, id int64) error {
	defer s.Invalidate()
	s.rpm.reset(id)
	return s.Store.DeleteKey(ctx, id)
}

// ---- rpm -------------------------------------------------------------------

// rpmLimiter is a per-key sliding-window request counter.
type rpmLimiter struct {
	mu   sync.Mutex
	hits map[int64][]time.Time
}

func (l *rpmLimiter) allow(id int64, limit int, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-rpmWindow)
	h := l.hits[id]
	i := 0
	for i < len(h) && !h[i].After(cutoff) {
		i++
	}
	h = h[i:]
	if len(h) >= limit {
		l.hits[id] = h
		return h[0].Add(rpmWindow).Sub(now), false
	}
	l.hits[id] = append(h, now)
	return 0, true
}

func (l *rpmLimiter) reset(id int64) {
	l.mu.Lock()
	delete(l.hits, id)
	l.mu.Unlock()
}

// PrincipalFor builds the principal of a stored key (used by the admin
// Playground to run requests as a key). Disabled or expired keys are refused.
func (s *Service) PrincipalFor(ctx context.Context, id int64) (*gateway.Principal, *gwerr.Error) {
	k, err := s.Store.GetKey(ctx, id)
	if err == store.ErrNotFound {
		return nil, gwerr.New(http.StatusNotFound, gwerr.NotFound, "key not found")
	}
	if err != nil {
		return nil, gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error")
	}
	if k.Status != store.KeyActive {
		return nil, authErr("API key is disabled")
	}
	if k.ExpiresAt != nil && !s.Clock.Now().Before(*k.ExpiresAt) {
		return nil, authErr("API key has expired")
	}
	return &gateway.Principal{KeyID: k.ID, KeyName: k.Name, AllowedAliases: k.AllowedAliases, LogMode: k.LogMode,
		NonStreamKeepalive: k.NonStreamKeepalive, MaxConcurrency: k.MaxConcurrency}, nil
}
