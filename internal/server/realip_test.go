package server

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	cases := []struct {
		remote, xff, want string
	}{
		{"203.0.113.5:1234", "1.2.3.4", "203.0.113.5"},             // untrusted peer: XFF ignored
		{"10.0.0.2:1234", "1.2.3.4", "1.2.3.4"},                    // trusted proxy
		{"10.0.0.2:1234", "6.6.6.6, 1.2.3.4, 10.0.0.9", "1.2.3.4"}, // spoofed left entry skipped
		{"10.0.0.2:1234", "", "10.0.0.2"},
		{"[::1]:80", "", "::1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := clientIP(r, trusted); got != c.want {
			t.Errorf("remote=%s xff=%q: got %s want %s", c.remote, c.xff, got, c.want)
		}
	}
}
