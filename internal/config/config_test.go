package config

import (
	"testing"
	"time"
)

func mapEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(mapEnv(map[string]string{"LLMGW_SECRET_KEY": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":4000" || c.DBPath != "data/llmgw.db" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.ReadTimeout != 120*time.Second || c.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("unexpected timeouts: %v %v", c.ReadTimeout, c.ReadHeaderTimeout)
	}
	if !c.CookieSecure {
		t.Error("cookie secure should default to true")
	}
}

func TestLoadValues(t *testing.T) {
	c, err := load(mapEnv(map[string]string{
		"LLMGW_LISTEN":          "127.0.0.1:8080",
		"LLMGW_SECRET_KEY":      "s3cret",
		"LLMGW_READ_TIMEOUT":    "300",
		"LLMGW_TRUSTED_PROXIES": "10.0.0.0/8, 192.168.1.10 ,::1",
		"LLMGW_COOKIE_SECURE":   "false",
		"LLMGW_LOG_LEVEL":       "DEBUG",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != "127.0.0.1:8080" {
		t.Errorf("listen = %q", c.ListenAddr)
	}
	if len(c.SecretKey) != 32 {
		t.Errorf("secret key len = %d", len(c.SecretKey))
	}
	if c.ReadTimeout != 300*time.Second {
		t.Errorf("read timeout = %v", c.ReadTimeout)
	}
	if len(c.TrustedProxies) != 3 || c.TrustedProxies[1].Bits() != 32 || c.TrustedProxies[2].Bits() != 128 {
		t.Errorf("trusted proxies = %v", c.TrustedProxies)
	}
	if c.CookieSecure || c.LogLevel != "debug" {
		t.Errorf("cookie=%v level=%q", c.CookieSecure, c.LogLevel)
	}
}

func TestLoadErrors(t *testing.T) {
	_, err := load(mapEnv(map[string]string{
		"LLMGW_READ_TIMEOUT":    "abc",
		"LLMGW_TRUSTED_PROXIES": "not-an-ip",
		"LLMGW_LOG_LEVEL":       "verbose",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSecretKeyRequired(t *testing.T) {
	if _, err := load(mapEnv(nil)); err == nil {
		t.Fatal("expected error when LLMGW_SECRET_KEY is unset")
	}
}
