// Package proxy implements vajra-proxy: a stateless reverse proxy that
// fronts every sandbox. Two responsibilities live here:
//
//  1. Sandbox-port forwarding. A request for `8080-{sandbox-id}.vajra.dev`
//     is routed to TCP port 8080 inside the named sandbox via a
//     hijacked HTTP CONNECT-style tunnel terminating at vajra-agent.
//  2. Browser terminal endpoint (see terminal.go) — a WebSocket bridge
//     into the sandbox's PTY.
//
// The proxy is stateless: every routing decision is resolved by calling
// SandboxResolver.Resolve, which the production wiring backs with HTTP
// calls into vajra-master. Tests inject an in-memory resolver.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default tunables. Override at construction by mutating Server.Config.
const (
	DefaultBaseDomain        = "vajra.dev"
	DefaultListenAddr        = ":8443"
	DefaultDialTimeout       = 10 * time.Second
	DefaultUpstreamUserAgent = "vajra-proxy/0.1"
)

// SandboxRoute is everything the proxy needs to forward a request: the
// sandbox ID (used in the agent URL), the agent URL itself
// (http://nodeIP:9000), and the agent shared secret used as the Bearer
// token. The resolver also returns the AccountID so the share-token
// validator can confirm scope.
type SandboxRoute struct {
	SandboxID    string
	AccountID    string
	AgentBaseURL string
	AgentSecret  string
	// State is the master's current view of the sandbox; the proxy
	// rejects non-RUNNING sandboxes with 503 instead of silently
	// hanging on the agent dial.
	State string
}

// SandboxResolver maps a sandbox ID to its current placement. The proxy
// consults this on every inbound request — there is no caching at this
// layer (master's cache is sufficient and avoids stale-routing pain).
type SandboxResolver interface {
	Resolve(ctx context.Context, sandboxID string) (*SandboxRoute, error)
}

// ShareValidator authorises a share token against a sandbox + optional
// port. Returning a non-nil error tells the proxy to 401/403; returning
// nil means the request may proceed.
type ShareValidator interface {
	ValidateShare(ctx context.Context, sandboxID, token string, port int) error
}

// noopShareValidator approves every request. Used when the operator
// hasn't configured a master URL yet (typically in dev).
type noopShareValidator struct{}

// ValidateShare always succeeds.
func (noopShareValidator) ValidateShare(context.Context, string, string, int) error { return nil }

// Config bundles the proxy's runtime options. BaseDomain controls the
// subdomain parser; DialTimeout caps per-request agent dials; Insecure
// skips share-token validation entirely (for local dev — never set in
// production).
type Config struct {
	ListenAddr  string
	BaseDomain  string
	DialTimeout time.Duration
	Logger      *slog.Logger
	Resolver    SandboxResolver
	Shares      ShareValidator
	// HTTPClient is the client used for upstream agent calls. Tests
	// inject a custom Transport whose RoundTrip is wired to a fake
	// agent. Production wiring leaves this nil and gets a default.
	HTTPClient *http.Client
}

// Server is the proxy. A single instance handles every request through
// its single ServeHTTP entry point. State stored here is immutable
// after NewServer returns.
type Server struct {
	cfg    Config
	logger *slog.Logger
	mux    *http.ServeMux
	// reverse is the shared httputil.ReverseProxy. Director swaps in
	// the per-request upstream URL before each call.
	reverse *httputil.ReverseProxy
}

// NewServer constructs a Server with sensible defaults and wires the
// HTTP routes. Returns an error if the config rejects validation.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Resolver == nil {
		return nil, errors.New("proxy: Resolver is required")
	}
	if cfg.BaseDomain == "" {
		cfg.BaseDomain = DefaultBaseDomain
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListenAddr
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Shares == nil {
		cfg.Shares = noopShareValidator{}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 0} // streaming bodies, no global timeout
	}

	s := &Server{cfg: cfg, logger: cfg.Logger, mux: http.NewServeMux()}
	s.reverse = &httputil.ReverseProxy{
		Director:       func(req *http.Request) { /* set per-request below */ },
		ErrorLog:       nil,
		FlushInterval:  100 * time.Millisecond,
		ModifyResponse: nil,
		Transport:      s.upstreamTransport(),
	}
	s.routes()
	return s, nil
}

// ListenAndServe binds the configured address and serves until ctx is
// cancelled. We do not configure TLS here; operators terminate TLS at
// a real LB or front the proxy with caddy/nginx.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("vajra-proxy listening",
			"addr", s.cfg.ListenAddr, "domain", s.cfg.BaseDomain)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// routes wires the HTTP surface. Host-based routing has priority over
// path-based: a request whose Host parses as `<port>-<sandbox-id>.<base>`
// is forwarded to the sandbox regardless of path, so user apps see
// their own URL. Requests on the apex (no port-id prefix) match the
// proxy's own routes (terminal, healthz).
func (s *Server) routes() {
	apex := http.NewServeMux()
	apex.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	apex.HandleFunc("GET /v1/sandboxes/{id}/terminal", s.handleTerminal)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isSandboxHost(stripPort(r.Host), s.cfg.BaseDomain) {
			s.handleForward(w, r)
			return
		}
		apex.ServeHTTP(w, r)
	})
}

// isSandboxHost returns true when host parses as a sandbox subdomain.
// Used by the host-based dispatcher above so the proxy can distinguish
// "request for the proxy itself" vs "request for a sandbox app".
func isSandboxHost(host, base string) bool {
	_, _, err := parseSubdomain(host, base)
	return err == nil
}

// handleForward is the catch-all for sandbox port forwarding. The host
// header carries the routing decision: `{port}-{sandbox-id}.{base}` →
// CONNECT-tunnel into the agent on the sandbox's node, terminating at
// the guest service on `127.0.0.1:{port}`.
//
// The reverse-proxy machinery here uses a custom Transport whose
// DialContext opens the agent CONNECT tunnel; once dialed, the conn
// behaves like a regular TCP connection to the in-VM service, so plain
// HTTP and WebSocket upgrades both Just Work without per-protocol code
// in the proxy.
func (s *Server) handleForward(w http.ResponseWriter, r *http.Request) {
	host := stripPort(r.Host)
	port, sandboxID, err := parseSubdomain(host, s.cfg.BaseDomain)
	if err != nil {
		http.Error(w, "vajra: "+err.Error(), http.StatusNotFound)
		return
	}
	route, err := s.cfg.Resolver.Resolve(r.Context(), sandboxID)
	if err != nil {
		s.logger.Warn("resolve failed", "sandbox", sandboxID, "err", err)
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if route.State != "" && route.State != "RUNNING" {
		http.Error(w, "sandbox not running", http.StatusServiceUnavailable)
		return
	}
	// Optional share-token gate. The query string carries the token to
	// keep it out of cookies & logs; production callers will ride a
	// signed cookie set by the dashboard.
	if token := r.URL.Query().Get("token"); token != "" {
		if err := s.cfg.Shares.ValidateShare(r.Context(), sandboxID, token, port); err != nil {
			http.Error(w, "share rejected: "+err.Error(), http.StatusForbidden)
			return
		}
	}

	upstream, err := url.Parse(route.AgentBaseURL)
	if err != nil {
		s.logger.Error("bad upstream", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	target := *upstream
	director := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		// The agent doesn't actually inspect the path on tunneled
		// requests — they're consumed inside the bridge — but we set
		// it anyway for clean logs.
	}
	rp := &httputil.ReverseProxy{
		Director:      director,
		Transport:     s.tunnelTransport(route, sandboxID, port),
		FlushInterval: s.reverse.FlushInterval,
	}
	rp.ServeHTTP(w, r)
}

// tunnelTransport returns an http.Transport whose DialContext opens a
// fresh agent CONNECT tunnel for every dial. Connection: keep-alive
// reuse is intentionally disabled (DisableKeepAlives) so that one
// hijacked tunnel never serves two unrelated user requests.
func (s *Server) tunnelTransport(route *SandboxRoute, sandboxID string, port int) *http.Transport {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return openAgentTunnel(ctx, route, sandboxID, port, s.cfg.DialTimeout)
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
		IdleConnTimeout:       0,
		ResponseHeaderTimeout: 0, // streaming bodies allowed
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// openAgentTunnel dials the agent's TCP socket and performs the
// CONNECT-style HTTP/1.1 Upgrade handshake. The returned net.Conn is
// the post-101 raw byte stream — i.e. a TCP connection straight to the
// user's app inside the sandbox.
func openAgentTunnel(ctx context.Context, route *SandboxRoute, sandboxID string, port int, dialTimeout time.Duration) (net.Conn, error) {
	if dialTimeout <= 0 {
		dialTimeout = DefaultDialTimeout
	}
	u, err := url.Parse(route.AgentBaseURL)
	if err != nil {
		return nil, err
	}
	addr := u.Host
	if !hostHasPort(addr) {
		addr = addr + ":80"
	}
	d := &net.Dialer{Timeout: dialTimeout}
	tcp, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = tcp.SetDeadline(dl)
	}
	path := "/sandbox/" + url.PathEscape(sandboxID) + "/forward/" + strconv.Itoa(port)
	req := "POST " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: vajra-tcp\r\n" +
		"Authorization: Bearer " + route.AgentSecret + "\r\n" +
		"User-Agent: " + DefaultUpstreamUserAgent + "\r\n\r\n"
	if _, err := tcp.Write([]byte(req)); err != nil {
		_ = tcp.Close()
		return nil, fmt.Errorf("agent connect write: %w", err)
	}
	br := bufio.NewReader(tcp)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = tcp.Close()
		return nil, fmt.Errorf("agent connect read: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = tcp.Close()
		return nil, fmt.Errorf("agent did not upgrade: %s", resp.Status)
	}
	// Clear deadline so the streaming body has no time pressure.
	_ = tcp.SetDeadline(time.Time{})
	return &bufConn{Conn: tcp, r: br}, nil
}

// upstreamTransport returns an *http.Transport that pools per-host
// connections sensibly for proxying. We keep timeouts loose because
// CONNECT tunnels stream forever.
func (s *Server) upstreamTransport() http.RoundTripper {
	if t := s.cfg.HTTPClient.Transport; t != nil {
		return t
	}
	d := &net.Dialer{Timeout: s.cfg.DialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext:           d.DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// parseSubdomain pulls (port, sandboxID) out of `<port>-<id>.<base>`.
// Any other shape is rejected with a descriptive error so curl users
// see what went wrong.
func parseSubdomain(host, base string) (int, string, error) {
	host = strings.ToLower(host)
	base = strings.ToLower(strings.TrimPrefix(base, "."))
	suffix := "." + base
	if !strings.HasSuffix(host, suffix) {
		return 0, "", fmt.Errorf("host %q is not under base domain %q", host, base)
	}
	prefix := strings.TrimSuffix(host, suffix)
	dash := strings.IndexByte(prefix, '-')
	if dash <= 0 || dash == len(prefix)-1 {
		return 0, "", fmt.Errorf("malformed subdomain %q (expected <port>-<sandbox-id>)", prefix)
	}
	portStr, id := prefix[:dash], prefix[dash+1:]
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return 0, "", fmt.Errorf("invalid port in %q", prefix)
	}
	return port, id, nil
}

// stripPort drops the trailing :port from a Host header if present.
// Plain net.SplitHostPort errors on host without port; this helper
// tolerates both.
func stripPort(h string) string {
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

// HTTPResolver implements SandboxResolver by querying a master endpoint
// that returns the agent base URL. Production wiring uses this; tests
// usually inject an in-memory resolver.
//
// Master must expose `GET /internal/proxy/route?sandbox_id=...` (or a
// token-validating variant) — defining that surface in master is
// covered by handlers_share.go.
type HTTPResolver struct {
	BaseURL string
	Token   string // shared internal secret
	Client  *http.Client
}

// Resolve issues GET /internal/proxy/route?sandbox_id=… and decodes the
// response into a SandboxRoute. Network errors propagate as-is so the
// proxy can return 502.
func (r *HTTPResolver) Resolve(ctx context.Context, sandboxID string) (*SandboxRoute, error) {
	c := r.Client
	if c == nil {
		c = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.BaseURL+"/internal/proxy/route?sandbox_id="+url.QueryEscape(sandboxID), nil)
	if err != nil {
		return nil, err
	}
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("sandbox not found")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("master responded %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		SandboxID    string `json:"sandbox_id"`
		AccountID    string `json:"account_id"`
		AgentBaseURL string `json:"agent_base_url"`
		AgentSecret  string `json:"agent_secret"`
		State        string `json:"state"`
	}
	if err := decodeJSON(resp.Body, &out); err != nil {
		return nil, err
	}
	return &SandboxRoute{
		SandboxID:    out.SandboxID,
		AccountID:    out.AccountID,
		AgentBaseURL: out.AgentBaseURL,
		AgentSecret:  out.AgentSecret,
		State:        out.State,
	}, nil
}

// staticResolverEntry pairs a sandbox ID with its route. Used by the
// in-memory StaticResolver below.
type staticResolverEntry struct{ route SandboxRoute }

// StaticResolver is a tiny in-memory SandboxResolver used by tests and
// demos. Add entries via Set; lookups are O(1).
type StaticResolver struct {
	mu      sync.RWMutex
	entries map[string]staticResolverEntry
}

// NewStaticResolver builds an empty static resolver.
func NewStaticResolver() *StaticResolver {
	return &StaticResolver{entries: map[string]staticResolverEntry{}}
}

// Set adds or replaces the route for sandboxID.
func (r *StaticResolver) Set(sandboxID string, route SandboxRoute) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[sandboxID] = staticResolverEntry{route: route}
}

// Resolve implements SandboxResolver.
func (r *StaticResolver) Resolve(_ context.Context, sandboxID string) (*SandboxRoute, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[sandboxID]
	if !ok {
		return nil, errors.New("sandbox not found")
	}
	out := entry.route
	return &out, nil
}
