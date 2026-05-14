// Package master — server.go is the HTTP entry point. It wires the
// Handlers struct into a mux of method+path patterns and applies a
// short middleware chain (request ID → logging → recovery → auth) to
// every route.
package master

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// shutdownTimeout is the drain budget after Run() observes a context
// cancellation. 30s is the conventional graceful-shutdown window for
// HTTP services and matches the brief.
const shutdownTimeout = 30 * time.Second

// ServerConfig is the bind + secrets bundle for NewServer.
type ServerConfig struct {
	Addr           string
	Logger         *slog.Logger
	InternalSecret string
	AdminAccountID string
	// RateLimitRPS is the per-account ceiling for the authed surface; 0
	// falls back to DefaultRateLimitRPS. Anonymous traffic (login,
	// register) shares a single bucket at the same rate.
	RateLimitRPS int
}

// Server bundles a configured *Handlers with the http.Server it serves
// from. It is constructed once by main.go and never mutated.
type Server struct {
	cfg      ServerConfig
	handlers *Handlers
	limiter  *RateLimiter
	http     *http.Server
}

// NewServer returns a configured Server. The handlers' AdminAccountID
// is overwritten with cfg.AdminAccountID so callers don't have to set
// it twice.
func NewServer(cfg ServerConfig, h *Handlers) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	h.AdminAccountID = cfg.AdminAccountID
	if h.Logger == nil {
		h.Logger = cfg.Logger
	}
	limiter := NewRateLimiter(RateLimitConfig{RPS: cfg.RateLimitRPS})
	return &Server{cfg: cfg, handlers: h, limiter: limiter}
}

// ctxKeyRequestID is the context key for the per-request ID injected by
// requestIDMiddleware. Unexported so callers outside the package can't
// guess it.
const ctxKeyRequestID ctxKey = 2

// Routes builds the http.Handler the server will serve. Exported so
// tests can hit it via httptest without spinning a real listener.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	h := s.handlers

	// Public routes — no auth.
	mux.HandleFunc("GET /health", h.getHealth)
	mux.HandleFunc("GET /version", h.getVersion)
	mux.HandleFunc("GET /v1/docs", h.docsSwaggerUI)
	mux.HandleFunc("GET /v1/docs/openapi.yaml", h.docsOpenAPISpec)
	// Rate-limit auth endpoints under the shared "anonymous" bucket so a
	// brute-force login loop can't outpace a single tenant's quota.
	mux.Handle("POST /v1/auth/register", s.limiter.Middleware(http.HandlerFunc(h.register)))
	mux.Handle("POST /v1/auth/login", s.limiter.Middleware(http.HandlerFunc(h.login)))
	// Google OAuth surface. /config is unauthenticated probe used by the
	// dashboard; /google starts the handshake; /google/callback finishes
	// it and 302s the browser back with a JWT in the URL fragment.
	mux.HandleFunc("GET /v1/auth/config", h.authConfig)
	mux.Handle("GET /v1/auth/google", s.limiter.Middleware(http.HandlerFunc(h.googleInitiate)))
	mux.Handle("GET /v1/auth/google/callback", s.limiter.Middleware(http.HandlerFunc(h.googleCallback)))

	// Authed routes — wrap each with AuthMiddleware + the per-account
	// rate limiter. The limiter is applied AFTER auth so the bucket is
	// keyed on account_id rather than IP; anonymous spam is bounded
	// further upstream by the (login|register) middleware below.
	auth := AuthMiddleware(h.Signer, s.handlers.Store.APIKeys())
	for pattern, hf := range s.authedRoutes() {
		mux.Handle(pattern, auth(s.limiter.Middleware(http.HandlerFunc(hf))))
	}

	// Internal routes — pre-shared secret only.
	internal := InternalAuthMiddleware(s.cfg.InternalSecret)
	for pattern, hf := range s.internalRoutes() {
		mux.Handle(pattern, internal(http.HandlerFunc(hf)))
	}

	return s.middleware(mux)
}

// authedRoutes returns the (pattern → handler) map for everything
// behind AuthMiddleware. Returning a map keeps registration tidy and
// makes the route surface easy to enumerate in tests.
func (s *Server) authedRoutes() map[string]http.HandlerFunc {
	h := s.handlers
	return map[string]http.HandlerFunc{
		// API keys
		"POST /v1/api-keys":        h.createAPIKey,
		"GET /v1/api-keys":         h.listAPIKeys,
		"DELETE /v1/api-keys/{id}": h.deleteAPIKey,

		// Sandboxes
		"POST /v1/sandboxes":                    h.createSandbox,
		"GET /v1/sandboxes":                     h.listSandboxes,
		"GET /v1/sandboxes/{id}":                h.getSandbox,
		"POST /v1/sandboxes/{id}/exec":          h.execSandbox,
		"POST /v1/sandboxes/{id}/stop":          h.stopSandbox,
		"POST /v1/sandboxes/{id}/start":         h.startSandbox,
		"DELETE /v1/sandboxes/{id}":             h.destroySandbox,
		"POST /v1/sandboxes/{id}/snapshot":      h.snapshotSandbox,
		"GET /v1/sandboxes/{id}/snapshots":      h.listSandboxSnapshots,
		"POST /v1/sandboxes/{id}/archive":       h.archiveSandbox,
		"POST /v1/sandboxes/{id}/rehydrate":     h.rehydrateSandbox,
		"POST /v1/sandboxes/{id}/migrate":       h.migrateSandbox,

		// Files (proxy through agent)
		"POST /v1/sandboxes/{id}/files/upload":   h.uploadFile,
		"GET /v1/sandboxes/{id}/files/download":  h.downloadFile,
		"GET /v1/sandboxes/{id}/files/list":      h.listFiles,

		// Shares
		"POST /v1/sandboxes/{id}/share":                  h.createShare,
		"GET /v1/sandboxes/{id}/shares":                  h.listShares,
		"DELETE /v1/sandboxes/{id}/share/{token_id}":     h.revokeShare,

		// Snapshots
		"POST /v1/snapshots/{id}/restore": h.restoreSnapshot,
		"POST /v1/snapshots/{id}/clone":   h.cloneSnapshot,
		"POST /v1/snapshots/{id}/promote": h.promoteSnapshot,

		// Templates
		"GET /v1/templates":               h.listTemplates,
		"POST /v1/templates":              h.createTemplate,
		"POST /v1/templates/build":        h.buildTemplate,
		"GET /v1/templates/builds":        h.listBuilds,
		"GET /v1/templates/builds/{id}":   h.getBuild,

		// Webhooks
		"POST /v1/webhooks":          h.createWebhook,
		"GET /v1/webhooks":           h.listWebhooks,
		"GET /v1/webhooks/{id}":      h.getWebhook,
		"DELETE /v1/webhooks/{id}":   h.deleteWebhook,
		"POST /v1/webhooks/{id}/test": h.testWebhook,

		// Admin
		"GET /v1/clusters":                 h.listClusters,
		"GET /v1/nodes":                    h.listNodes,
		"POST /v1/nodes/{id}/drain":        h.drainNode,
		"PATCH /v1/admin/templates/{id}":   h.setTemplatePublic,

		// Admin: autoscaler
		"GET /v1/admin/autoscale":          h.getAutoscaleStatus,
		"POST /v1/admin/autoscale/trigger": h.triggerAutoscale,

		// Usage
		"GET /v1/usage": h.getUsage,
	}
}

// internalRoutes returns the (pattern → handler) map for endpoints
// guarded by the agent's pre-shared secret.
func (s *Server) internalRoutes() map[string]http.HandlerFunc {
	h := s.handlers
	return map[string]http.HandlerFunc{
		"POST /internal/nodes/register":          h.registerNode,
		"POST /internal/nodes/{id}/heartbeat":    h.nodeHeartbeat,
		"POST /internal/nodes/{id}/event":        h.nodeEvent,
		"POST /internal/sandboxes/{id}/unhealthy": h.sandboxUnhealthyAlias,
		"GET /internal/proxy/route":               h.proxyRoute,
		"GET /internal/proxy/validate-share":      h.validateShare,
		"GET /internal/binaries/{name}":           h.serveBinary,
	}
}

// ListenAndServe binds the configured address and serves until ctx is
// cancelled. On cancellation we drain for shutdownTimeout before
// forcing a close; if the listener fails for any other reason the
// error is returned.
func (s *Server) ListenAndServe(ctx context.Context) error {
	s.http = &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.cfg.Logger.Info("vajra-master listening", "addr", s.cfg.Addr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// middleware wraps the mux with request-ID, logging, and recovery
// layers. CORS is intentionally noop here — the dashboard talks to
// master via a same-origin proxy in production.
func (s *Server) middleware(next http.Handler) http.Handler {
	logger := s.cfg.Logger
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			var buf [4]byte
			_, _ = rand.Read(buf[:])
			reqID = hex.EncodeToString(buf[:])
		}
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, reqID)
		w.Header().Set("X-Request-ID", reqID)

		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		// Recovery: a panic inside a handler must not crash the
		// process. Log + 500.
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered",
					"request_id", reqID,
					"path", r.URL.Path,
					"panic", rec,
				)
				if !rw.written {
					http.Error(rw, "internal error", http.StatusInternalServerError)
				}
			}
			accountID := AccountIDFromContext(ctx)
			logger.Info("http",
				"request_id", reqID,
				"account_id", accountID,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.status,
				"latency_ms", time.Since(start).Milliseconds(),
			)
		}()

		next.ServeHTTP(rw, r.WithContext(ctx))
	})
}

// statusRecorder captures the response code so the logging middleware
// can record it. WriteHeader is the only point at which the code
// reaches the wire, so we observe it there.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

// WriteHeader records the status before delegating.
func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.written = true
	r.ResponseWriter.WriteHeader(code)
}

// Write marks the response as written so a defer-recovered panic does
// not double-write.
func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}
