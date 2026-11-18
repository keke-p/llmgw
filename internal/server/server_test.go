package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/db"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(&config.Config{}, d, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["db"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

func TestHealthzDBDown(t *testing.T) {
	s := newTestServer(t)
	s.db.Close()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServerTimeouts(t *testing.T) {
	s := newTestServer(t)
	s.cfg.ReadTimeout = 120e9
	s.cfg.ReadHeaderTimeout = 10e9
	srv := s.HTTPServer()
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout must be 0 for streaming, got %v", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout != 10e9 || srv.ReadTimeout != 120e9 {
		t.Errorf("read timeouts = %v / %v", srv.ReadHeaderTimeout, srv.ReadTimeout)
	}
}
