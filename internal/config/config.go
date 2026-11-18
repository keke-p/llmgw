// Package config loads the static (environment-based) configuration of llmgw.
//
// Only a minimal set of settings lives in the environment. Everything that can be
// changed at runtime (timeouts, retention, health-check interval, ...) is stored in
// the database and edited from the admin UI.
package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the process-level configuration read from environment variables.
type Config struct {
	// ListenAddr is the address the HTTP server binds to (LLMGW_LISTEN).
	ListenAddr string
	// DBPath is the SQLite database file path (LLMGW_DB_PATH).
	DBPath string
	// SecretKey is the 32-byte key derived from LLMGW_SECRET_KEY (required), used to
	// encrypt provider API keys with AES-GCM.
	SecretKey []byte
	// AdminPassword is the initial admin password (LLMGW_ADMIN_PASSWORD). Only used
	// on first start when no admin exists.
	AdminPassword string
	// AdminTrustHeader, when non-empty, makes the admin UI treat any request
	// carrying this header as authenticated (LLMGW_ADMIN_TRUST_HEADER).
	AdminTrustHeader string
	// TrustedProxies lists the proxies whose X-Forwarded-For is trusted
	// (LLMGW_TRUSTED_PROXIES, comma separated IPs or CIDRs).
	TrustedProxies []netip.Prefix
	// ReadTimeout bounds reading the request body (LLMGW_READ_TIMEOUT).
	ReadTimeout time.Duration
	// ReadHeaderTimeout bounds reading request headers (LLMGW_READ_HEADER_TIMEOUT).
	ReadHeaderTimeout time.Duration
	// ShutdownTimeout is how long graceful shutdown waits for in-flight requests
	// (LLMGW_SHUTDOWN_TIMEOUT).
	ShutdownTimeout time.Duration
	// LogLevel is debug, info, warn or error (LLMGW_LOG_LEVEL).
	LogLevel string
	// OllamaVersion is reported by GET /api/version (LLMGW_OLLAMA_VERSION).
	OllamaVersion string
	// MaxBodyBytes limits API request bodies (LLMGW_MAX_BODY_BYTES).
	MaxBodyBytes int64
	// CookieSecure controls the Secure attribute of the admin session cookie
	// (LLMGW_COOKIE_SECURE). Defaults to true; set to false for plain-HTTP local use.
	CookieSecure bool
}

// Load reads configuration from the environment.
func Load() (*Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (*Config, error) {
	c := &Config{
		ListenAddr:        envOr(getenv, "LLMGW_LISTEN", ":4000"),
		DBPath:            envOr(getenv, "LLMGW_DB_PATH", "data/llmgw.db"),
		AdminPassword:     getenv("LLMGW_ADMIN_PASSWORD"),
		AdminTrustHeader:  strings.TrimSpace(getenv("LLMGW_ADMIN_TRUST_HEADER")),
		LogLevel:          strings.ToLower(envOr(getenv, "LLMGW_LOG_LEVEL", "info")),
		ReadTimeout:       120 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		ShutdownTimeout:   30 * time.Second,
		CookieSecure:      true,
		MaxBodyBytes:      64 << 20,
		OllamaVersion:     envOr(getenv, "LLMGW_OLLAMA_VERSION", "0.13.0"),
	}

	var errs []error

	if s := getenv("LLMGW_SECRET_KEY"); s != "" {
		c.SecretKey = DeriveKey(s)
	} else {
		errs = append(errs, errors.New("LLMGW_SECRET_KEY is required (used to encrypt provider API keys)"))
	}

	var err error
	if c.ReadTimeout, err = envDuration(getenv, "LLMGW_READ_TIMEOUT", c.ReadTimeout); err != nil {
		errs = append(errs, err)
	}
	if c.ReadHeaderTimeout, err = envDuration(getenv, "LLMGW_READ_HEADER_TIMEOUT", c.ReadHeaderTimeout); err != nil {
		errs = append(errs, err)
	}
	if c.ShutdownTimeout, err = envDuration(getenv, "LLMGW_SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
		errs = append(errs, err)
	}
	if v := getenv("LLMGW_COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("LLMGW_COOKIE_SECURE: %w", err))
		} else {
			c.CookieSecure = b
		}
	}
	if v := getenv("LLMGW_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("LLMGW_MAX_BODY_BYTES: invalid value %q", v))
		} else {
			c.MaxBodyBytes = n
		}
	}
	if v := getenv("LLMGW_TRUSTED_PROXIES"); v != "" {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			p, err := parsePrefix(part)
			if err != nil {
				errs = append(errs, fmt.Errorf("LLMGW_TRUSTED_PROXIES: %q: %w", part, err))
				continue
			}
			c.TrustedProxies = append(c.TrustedProxies, p)
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("LLMGW_LOG_LEVEL: unknown level %q", c.LogLevel))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// DeriveKey turns an arbitrary secret string into a 32-byte AES-256 key.
func DeriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte("llmgw-secret-key-v1:" + secret))
	return sum[:]
}

func envOr(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}

// envDuration accepts Go duration syntax ("90s", "2m") or a bare number of seconds.
func envDuration(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("%s: must not be negative", key)
		}
		return time.Duration(n * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: must not be negative", key)
	}
	return d, nil
}

func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}
