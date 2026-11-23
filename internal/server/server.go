// Package server wires up the HTTP server: routing, timeouts and graceful shutdown.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"llmgw/internal/api/admin"
	"llmgw/internal/api/anthropic"
	"llmgw/internal/api/lmstudio"
	"llmgw/internal/api/ollama"
	"llmgw/internal/api/openai"
	"llmgw/internal/clock"
	"llmgw/internal/config"
	"llmgw/internal/gateway"
	"llmgw/internal/gwerr"
	"llmgw/internal/health"
	"llmgw/internal/keys"
	"llmgw/internal/limits"
	"llmgw/internal/reqlog"
	"llmgw/internal/store"
	"llmgw/internal/version"
)

// Server is the llmgw HTTP server.
type Server struct {
	cfg    *config.Config
	db     *sql.DB
	log    *slog.Logger
	mux    *http.ServeMux
	store  *store.Store
	engine *gateway.Engine
	keys   *keys.Service
	reqlog *reqlog.Writer
	health *health.Checker
}

// Option customizes a Server (used by tests).
type Option func(*Server)

// WithClock replaces the engine clock.
func WithClock(c clock.Clock) Option { return func(s *Server) { s.engine.Clock = c } }

// WithHTTPClient replaces the upstream HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(s *Server) { s.engine.Client = c } }

// WithRecorder sets the request-log sink.
func WithRecorder(f func(*gateway.Record)) Option { return func(s *Server) { s.engine.Record = f } }

// New builds a Server and registers its routes.
func New(cfg *config.Config, db *sql.DB, log *slog.Logger, opts ...Option) *Server {
	s := &Server{cfg: cfg, db: db, log: log, mux: http.NewServeMux()}
	s.store = store.New(db, cfg.SecretKey)
	s.engine = &gateway.Engine{
		Store:  s.store,
		Client: gateway.NewHTTPClient(),
		Clock:  clock.Real{},
		Log:    log,
	}
	s.engine.Record = func(rec *gateway.Record) {
		s.logRecord(rec)
		s.reqlog.Record(rec)
	}
	for _, o := range opts {
		o(s)
	}
	s.engine.Limits = limits.NewRegistry(s.engine.Clock)
	s.keys = keys.NewService(s.store, s.engine.Clock, log)
	s.reqlog = reqlog.NewWriter(s.store, log, s.engine.Clock)
	s.health = &health.Checker{Store: s.store, Client: s.engine.Client, Clock: s.engine.Clock, Log: log}
	s.routes()
	return s
}

// Engine returns the request engine.
func (s *Server) Engine() *gateway.Engine { return s.engine }

// Health returns the provider health checker.
func (s *Server) Health() *health.Checker { return s.health }

// Store returns the server's store.
func (s *Server) Store() *store.Store { return s.store }

// StartBackground starts background jobs (request log writer, retention).
// The returned stop function flushes pending logs; call it after the HTTP
// server has stopped.
func (s *Server) StartBackground(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	s.reqlog.Start(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.health.Run(ctx) }()
	return func() {
		cancel()
		<-done
		s.reqlog.Stop()
	}
}

// Keys returns the virtual key service.
func (s *Server) Keys() *keys.Service { return s.keys }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	oa := &openai.Handler{
		Engine:       s.engine,
		Auth:         s.authenticate,
		MaxBodyBytes: s.cfg.MaxBodyBytes,
		ClientIP:     func(r *http.Request) string { return clientIP(r, s.cfg.TrustedProxies) },
	}
	oa.Register(s.mux)

	ol := &ollama.Handler{
		Engine:       s.engine,
		Auth:         s.authenticate,
		MaxBodyBytes: s.cfg.MaxBodyBytes,
		ClientIP:     func(r *http.Request) string { return clientIP(r, s.cfg.TrustedProxies) },
		Version:      s.cfg.OllamaVersion,
	}
	ol.Register(s.mux)

	an := &anthropic.Handler{
		Engine:       s.engine,
		Auth:         s.authenticate,
		MaxBodyBytes: s.cfg.MaxBodyBytes,
		ClientIP:     func(r *http.Request) string { return clientIP(r, s.cfg.TrustedProxies) },
	}
	an.Register(s.mux)

	// LM Studio compatible model list (model discovery only; inference uses
	// the OpenAI compatible API).
	lm := &lmstudio.Handler{Engine: s.engine, Auth: s.authenticate}
	lm.Register(s.mux)

	ad := &admin.API{
		Store:        s.store,
		Keys:         s.keys,
		Clock:        s.engine.Clock,
		Log:          s.log,
		CookieSecure: s.cfg.CookieSecure,
		TrustHeader:  s.cfg.AdminTrustHeader,
		PeerTrusted:  func(r *http.Request) bool { return peerTrusted(r, s.cfg.TrustedProxies) },
		ClientIP:     func(r *http.Request) string { return clientIP(r, s.cfg.TrustedProxies) },
		HTTPClient:   s.engine.Client,
		Dispatch:     s.dispatchAs,
		BackupDir:    filepath.Dir(s.cfg.DBPath),
	}
	ad.Register(s.mux)
}

// logRecord writes a request summary to the structured log. Persistent request
// logs (request_logs) come in phase 6.
func (s *Server) logRecord(rec *gateway.Record) {
	attrs := []any{
		"endpoint", rec.Endpoint, "alias", rec.Alias, "key", rec.KeyName,
		"inbound", rec.InboundFormat, "upstream", rec.UpstreamFormat, "passthrough", rec.Passthrough,
		"stream", rec.Stream, "status", rec.StatusCode, "latency_ms", rec.LatencyMS,
		"attempts", len(rec.Attempts), "client_ip", rec.ClientIP,
	}
	if rec.TTFTMS != nil {
		attrs = append(attrs, "ttft_ms", *rec.TTFTMS)
	}
	if rec.TimeoutPhase != "" {
		attrs = append(attrs, "timeout_phase", rec.TimeoutPhase)
	}
	if rec.CancelledByClient {
		attrs = append(attrs, "cancelled_by_client", true)
	}
	if rec.PromptTokens != nil {
		attrs = append(attrs, "prompt_tokens", *rec.PromptTokens, "completion_tokens", *rec.CompletionTokens)
	}
	if rec.Error != "" {
		attrs = append(attrs, "error", rec.Error)
	}
	level := slog.LevelInfo
	if rec.StatusCode >= 500 {
		level = slog.LevelWarn
	}
	s.log.Log(context.Background(), level, "request", attrs...)
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.mux)
}

// HTTPServer returns an *http.Server configured per the spec:
//   - WriteTimeout is disabled (0) because streaming responses may legitimately last
//     a long time; per-request limits are enforced with contexts and
//     http.ResponseController instead.
//   - ReadHeaderTimeout is short, ReadTimeout is long enough for large bodies.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	srv := s.HTTPServer()
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	s.log.Info("listening", "addr", ln.Addr().String(), "version", version.Version)

	stopBG := s.StartBackground(context.WithoutCancel(ctx))
	defer stopBG() // flush request logs after in-flight requests finished

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout.String())
	shCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		// In-flight streams did not finish in time; force close them.
		s.log.Warn("graceful shutdown timed out; closing connections", "err", err)
		srv.Close()
	}
	return nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status, code := "ok", http.StatusOK
	dbStatus := "ok"
	if err := s.db.PingContext(ctx); err != nil {
		status, code, dbStatus = "error", http.StatusServiceUnavailable, "error"
		s.log.Error("healthz: db ping failed", "err", err)
	}
	writeJSON(w, code, map[string]any{
		"status":  status,
		"db":      dbStatus,
		"version": version.Version,
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic in handler", "panic", v, "method", r.Method, "path", r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type principalKey struct{}

// authenticate resolves the caller: a principal injected by the admin
// Playground (internal only, never from client input) or a virtual key.
func (s *Server) authenticate(r *http.Request) (*gateway.Principal, *gwerr.Error) {
	if p, ok := r.Context().Value(principalKey{}).(*gateway.Principal); ok && p != nil {
		return p, nil
	}
	return s.keys.Authenticate(r)
}

// dispatchAs serves an API path as the given principal (admin Playground).
func (s *Server) dispatchAs(w http.ResponseWriter, r *http.Request, p *gateway.Principal, path string) {
	r2 := r.Clone(context.WithValue(r.Context(), principalKey{}, p))
	r2.URL.Path, r2.URL.RawPath, r2.RequestURI = path, "", path
	r2.Header.Del("Authorization")
	r2.Header.Del("x-api-key")
	s.mux.ServeHTTP(w, r2)
}
