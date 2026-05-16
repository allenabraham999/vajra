package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// DefaultListenAddr is the agent's HTTP bind address when nothing is set.
const DefaultListenAddr = ":9000"

// Server is the HTTP control surface vajra-master and operators talk to.
// All handlers are stateless; concurrent state lives behind the manager
// pointers below.
type Server struct {
	addr      string
	sandboxes *SandboxManager
	pool      *PoolManager
	archives  *ArchiveManager
	logger    *slog.Logger
	http      *http.Server

	// metrics — kept tiny and exposed via /metrics in Prometheus text
	// format. Not a replacement for a real client library; just enough
	// for liveness dashboards during the demo.
	requests       atomic.Int64
	requestErrors  atomic.Int64
	createdTotal   atomic.Int64
	destroyedTotal atomic.Int64
}

// NewServer wires the manager pointers into a configured *Server. addr
// defaults to DefaultListenAddr when empty.
func NewServer(addr string, sandboxes *SandboxManager, pool *PoolManager, logger *slog.Logger) *Server {
	if addr == "" {
		addr = DefaultListenAddr
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		addr:      addr,
		sandboxes: sandboxes,
		pool:      pool,
		logger:    logger,
	}
}

// SetArchiveManager swaps in the archive manager used by the
// /sandbox/{id}/archive and /sandbox/{id}/rehydrate handlers. Wired from
// main so the server constructor remains stable for tests.
func (s *Server) SetArchiveManager(a *ArchiveManager) { s.archives = a }

// ListenAndServe binds the configured address and serves until ctx is
// cancelled. It returns the first non-shutdown error from http.Server.
func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	s.routes(mux)
	s.http = &http.Server{
		Addr:              s.addr,
		Handler:           s.middleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("agent server listening", "addr", s.addr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /sandbox/create", s.handleCreate)
	mux.HandleFunc("GET /sandbox/list", s.handleSandboxList)
	mux.HandleFunc("GET /sandbox/{id}", s.handleGet)
	mux.HandleFunc("POST /sandbox/{id}/exec", s.handleExec)
	mux.HandleFunc("POST /sandbox/{id}/stop", s.handleStop)
	mux.HandleFunc("POST /sandbox/{id}/start", s.handleStart)
	mux.HandleFunc("DELETE /sandbox/{id}", s.handleDestroy)
	mux.HandleFunc("POST /sandbox/{id}/snapshot", s.handleSandboxSnapshot)
	mux.HandleFunc("POST /sandbox/{id}/archive", s.handleArchive)
	mux.HandleFunc("POST /sandbox/{id}/rehydrate", s.handleRehydrate)
	mux.HandleFunc("POST /sandbox/{id}/migrate", s.handleMigrate)
	mux.HandleFunc("POST /sandbox/receive", s.handleReceive)
	mux.HandleFunc("POST /sandbox/{id}/files/upload", s.handleFileUpload)
	mux.HandleFunc("GET /sandbox/{id}/files/download", s.handleFileDownload)
	mux.HandleFunc("GET /sandbox/{id}/files/list", s.handleFileList)
	mux.HandleFunc("DELETE /sandbox/{id}/files", s.handleFileDelete)
	mux.HandleFunc("POST /sandbox/{id}/forward/{port}", s.handleForward)
	mux.HandleFunc("GET /sandbox/{id}/terminal", s.handleTerminal)
	mux.HandleFunc("GET /pool/stats", s.handlePoolStats)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		if rw.status >= 400 {
			s.requestErrors.Add(1)
		}
		s.logger.Debug("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack passes through to the underlying ResponseWriter so the bridge
// handlers (forward / terminal) can take over the raw connection.
// Without this the statusRecorder wrapper hides the http.Hijacker
// interface and every hijack attempt fails with a 500.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter is not a Hijacker")
	}
	return hj.Hijack()
}

// CreateRequestBody is the JSON shape accepted by POST /sandbox/create.
// The pool is consulted on every request when configured; FromPool is
// kept for wire-compatibility with the master dispatcher but currently
// ignored — the agent always prefers a warm member to a cold restore.
type CreateRequestBody struct {
	ID           string        `json:"id,omitempty"`
	TemplateHash string        `json:"template_hash"`
	Config       SandboxConfig `json:"config"`
	FromPool     bool          `json:"from_pool,omitempty"`
}

// ExecRequestBody is the JSON shape accepted by POST /sandbox/{id}/exec.
type ExecRequestBody struct {
	Command   string `json:"command"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body CreateRequestBody
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.pool != nil {
		t0 := time.Now()
		if sb, ok := s.tryAssignFromPool(r.Context(), body); ok {
			s.logger.Info("pool hit",
				"id", sb.ID,
				"elapsed_ms", time.Since(t0).Milliseconds(),
			)
			s.createdTotal.Add(1)
			writeJSON(w, http.StatusOK, sb)
			return
		}
		s.logger.Info("pool miss: cold create", "template", body.TemplateHash)
	}
	// Async create: register the placeholder synchronously (fast,
	// validation only) so master's ListSandboxes / GetSandbox sees it
	// immediately. The CoW + CH restore work runs in a goroutine on a
	// detached context — r.Context() is cancelled the moment we return
	// 202, so we must not pass it through. createdTotal is incremented
	// in the goroutine on success.
	sb, err := s.sandboxes.BeginCreate(CreateRequest{
		ID:           body.ID,
		TemplateHash: body.TemplateHash,
		Config:       body.Config,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	go func(id string) {
		ctx := context.Background()
		if err := s.sandboxes.FinishCreate(ctx, id); err != nil {
			// FinishCreate has already marked the sandbox ERROR with
			// the cause. Just log here; master will pick up the
			// terminal state on its next poll.
			s.logger.Warn("async create failed", "id", id, "err", err)
			return
		}
		s.createdTotal.Add(1)
	}(sb.ID)
	writeJSON(w, http.StatusAccepted, sb)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, err := s.sandboxes.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body ExecRequestBody
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Command == "" {
		writeErr(w, http.StatusBadRequest, "command is required")
		return
	}
	timeout := time.Duration(body.TimeoutMS) * time.Millisecond
	res, err := s.sandboxes.ExecCommand(r.Context(), id, body.Command, timeout)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.sandboxes.StopSandbox(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.sandboxes.StartSandbox(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Recycle the CID back to the pool BEFORE destroy so a fast-follow
	// pool warm-up can reuse it. We snapshot CID under the manager's
	// lock; if the sandbox is gone already the destroy is a no-op.
	var cid uint32
	if sb, err := s.sandboxes.Get(id); err == nil {
		cid = sb.VsockCID
	}
	if err := s.sandboxes.DestroySandbox(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.pool != nil && cid >= DefaultPoolFirstCID {
		s.pool.Release(cid)
	}
	s.destroyedTotal.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePoolStats(w http.ResponseWriter, _ *http.Request) {
	if s.pool == nil {
		writeJSON(w, http.StatusOK, PoolStats{})
		return
	}
	writeJSON(w, http.StatusOK, s.pool.Stats())
}

// tryAssignFromPool attempts to claim a warm member matching the
// request's template, resume it, and adopt it into the sandbox manager.
// Returns ok=false when the pool is empty or the requested template
// doesn't match what the pool was started for, so the caller can fall
// through to cold create. On resume failure the member is destroyed and
// ok=false is returned — never propagate a half-baked pool sandbox to
// the API surface.
func (s *Server) tryAssignFromPool(ctx context.Context, body CreateRequestBody) (*Sandbox, bool) {
	if body.TemplateHash != "" {
		stats := s.pool.Stats()
		if stats.Template != "" && stats.Template != body.TemplateHash {
			return nil, false
		}
	}
	ps, err := s.pool.AssignFromPool()
	if err != nil {
		return nil, false
	}
	resumeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.sandboxes.VMM().ResumeVM(resumeCtx, ps.APISocket); err != nil {
		s.logger.Warn("pool resume failed; destroying pool member", "id", ps.ID, "err", err)
		// Best-effort cleanup: bring the host back to a known state. The
		// pool will replenish itself on the next tick.
		_ = s.sandboxes.VMM().DestroyVM(context.WithoutCancel(ctx), ps.APISocket)
		s.pool.Release(ps.CID)
		return nil, false
	}
	sb := s.pool.MakeSandbox(ps)
	if body.ID != "" {
		sb.ID = body.ID
	}
	if cfg := body.Config; cfg.VCPUs != 0 || cfg.MemoryMB != 0 || cfg.DiskGB != 0 {
		sb.Config = cfg
	}
	s.sandboxes.AdoptSandbox(sb)
	return sb, true
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var poolStats PoolStats
	if s.pool != nil {
		poolStats = s.pool.Stats()
	}
	sandboxes := s.sandboxes.List()
	stateCounts := map[SandboxState]int{}
	for _, sb := range sandboxes {
		stateCounts[sb.State]++
	}
	fmt.Fprintf(w, "# HELP vajra_agent_requests_total HTTP requests served\n")
	fmt.Fprintf(w, "# TYPE vajra_agent_requests_total counter\n")
	fmt.Fprintf(w, "vajra_agent_requests_total %d\n", s.requests.Load())
	fmt.Fprintf(w, "# HELP vajra_agent_request_errors_total 4xx/5xx responses\n")
	fmt.Fprintf(w, "# TYPE vajra_agent_request_errors_total counter\n")
	fmt.Fprintf(w, "vajra_agent_request_errors_total %d\n", s.requestErrors.Load())
	fmt.Fprintf(w, "# HELP vajra_agent_sandboxes_created_total sandboxes successfully created\n")
	fmt.Fprintf(w, "# TYPE vajra_agent_sandboxes_created_total counter\n")
	fmt.Fprintf(w, "vajra_agent_sandboxes_created_total %d\n", s.createdTotal.Load())
	fmt.Fprintf(w, "# HELP vajra_agent_sandboxes_destroyed_total sandboxes destroyed\n")
	fmt.Fprintf(w, "# TYPE vajra_agent_sandboxes_destroyed_total counter\n")
	fmt.Fprintf(w, "vajra_agent_sandboxes_destroyed_total %d\n", s.destroyedTotal.Load())
	fmt.Fprintf(w, "# HELP vajra_agent_sandboxes Sandboxes currently registered, by state\n")
	fmt.Fprintf(w, "# TYPE vajra_agent_sandboxes gauge\n")
	for state, n := range stateCounts {
		fmt.Fprintf(w, "vajra_agent_sandboxes{state=%q} %d\n", string(state), n)
	}
	fmt.Fprintf(w, "# HELP vajra_pool_available Warm pool members ready for assignment\n")
	fmt.Fprintf(w, "# TYPE vajra_pool_available gauge\n")
	fmt.Fprintf(w, "vajra_pool_available %d\n", poolStats.Available)
	fmt.Fprintf(w, "# HELP vajra_pool_warming Pool members currently being pre-warmed\n")
	fmt.Fprintf(w, "# TYPE vajra_pool_warming gauge\n")
	fmt.Fprintf(w, "vajra_pool_warming %d\n", poolStats.Warming)
	fmt.Fprintf(w, "# HELP vajra_pool_target Dynamic target pool size\n")
	fmt.Fprintf(w, "# TYPE vajra_pool_target gauge\n")
	fmt.Fprintf(w, "vajra_pool_target %d\n", poolStats.TargetSize)
	fmt.Fprintf(w, "# HELP vajra_pool_hits_total Pool assignments served from a warm member\n")
	fmt.Fprintf(w, "# TYPE vajra_pool_hits_total counter\n")
	fmt.Fprintf(w, "vajra_pool_hits_total %d\n", poolStats.TotalHits)
	fmt.Fprintf(w, "# HELP vajra_pool_misses_total Pool assignments that fell back to a cold create\n")
	fmt.Fprintf(w, "# TYPE vajra_pool_misses_total counter\n")
	fmt.Fprintf(w, "vajra_pool_misses_total %d\n", poolStats.TotalMisses)
	fmt.Fprintf(w, "# HELP vajra_pool_hit_rate Rolling pool hit-rate (0-100)\n")
	fmt.Fprintf(w, "# TYPE vajra_pool_hit_rate gauge\n")
	fmt.Fprintf(w, "vajra_pool_hit_rate %.2f\n", poolStats.HitRatePct)
}

func decodeBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{
		"error":  msg,
		"status": strconv.Itoa(status),
	})
}
