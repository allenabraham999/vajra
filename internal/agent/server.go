package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	mux.HandleFunc("GET /sandbox/{id}", s.handleGet)
	mux.HandleFunc("POST /sandbox/{id}/exec", s.handleExec)
	mux.HandleFunc("POST /sandbox/{id}/stop", s.handleStop)
	mux.HandleFunc("POST /sandbox/{id}/start", s.handleStart)
	mux.HandleFunc("DELETE /sandbox/{id}", s.handleDestroy)
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

// CreateRequestBody is the JSON shape accepted by POST /sandbox/create.
// FromPool=true short-circuits CreateSandbox by handing back a warm pool
// member; if the pool is empty the server falls through to a fresh
// restore.
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
	if body.FromPool && s.pool != nil {
		if id, err := s.pool.AssignFromPool(); err == nil {
			sb, err := s.sandboxes.Get(id)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			s.createdTotal.Add(1)
			writeJSON(w, http.StatusOK, sb)
			return
		}
	}
	sb, err := s.sandboxes.CreateSandbox(r.Context(), CreateRequest{
		ID:           body.ID,
		TemplateHash: body.TemplateHash,
		Config:       body.Config,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.createdTotal.Add(1)
	writeJSON(w, http.StatusCreated, sb)
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
	if err := s.sandboxes.DestroySandbox(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.pool != nil {
		s.pool.Release(id)
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
	fmt.Fprintf(w, "# HELP vajra_agent_pool_available Warm pool members ready for assignment\n")
	fmt.Fprintf(w, "# TYPE vajra_agent_pool_available gauge\n")
	fmt.Fprintf(w, "vajra_agent_pool_available %d\n", poolStats.Available)
	fmt.Fprintf(w, "vajra_agent_pool_in_use %d\n", poolStats.InUse)
	fmt.Fprintf(w, "vajra_agent_pool_creating %d\n", poolStats.Creating)
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
