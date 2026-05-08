// Package master — handlers.go is the shared base for the HTTP handler
// surface. It declares the Handlers struct, version metadata, and the
// JSON/error helpers every handler reuses. Concrete endpoints live in
// the sibling handlers_*.go files; the split keeps each file under the
// 300-line cap and groups handlers by their auth/feature boundary.
package master

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/allenabraham999/vajra/internal/store"
)

// VersionInfo is the build provenance surfaced at GET /version. main()
// fills it in from build-time env vars (VAJRA_VERSION etc).
type VersionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"built_at"`
}

// Handlers is the dependency bundle every HTTP handler receives. The
// struct is constructed once at startup and shared across goroutines —
// every field is either immutable, internally synchronised, or stateless,
// because master must remain horizontally scalable.
type Handlers struct {
	Store     store.Store
	Signer    *JWTSigner
	Scheduler Scheduler
	Pool      *AgentPool
	Tracker   *OperationTracker
	Logger    *slog.Logger
	Version   VersionInfo

	// AdminAccountID is the placeholder admin gate: an account whose ID
	// matches this string is treated as administrator. Until accounts
	// grow a role column, this is the simplest mechanism for the
	// /v1/clusters, /v1/nodes, and /v1/nodes/*/drain surface.
	AdminAccountID string

	// AgentSharedSecret is the Bearer token vajra-proxy and the agents
	// expect on internal calls. Master returns it to the proxy as part
	// of the route response so the proxy can authorize against the
	// agent's CONNECT endpoints.
	AgentSharedSecret string

	// PublicBaseDomain is the apex domain the proxy is reachable on
	// (e.g. "vajra.dev"). Used to suggest user-friendly share URLs;
	// empty means the master will return a token but not a URL.
	PublicBaseDomain string

	// Now is overridable in tests so JWT expiry and operation timestamps
	// are deterministic. Production wires this to time.Now.
	Now func() time.Time
}

// NewHandlers wires a Handlers value with safe defaults. Callers may
// freely tweak fields on the returned struct before handing it to a
// Server. Logger and Now fall back to standard values when zero.
func NewHandlers(s store.Store, signer *JWTSigner, sched Scheduler, pool *AgentPool, tracker *OperationTracker) *Handlers {
	return &Handlers{
		Store:     s,
		Signer:    signer,
		Scheduler: sched,
		Pool:      pool,
		Tracker:   tracker,
		Logger:    slog.Default(),
		Now:       time.Now,
	}
}

// now is the internal clock. Returns time.Now when the override is nil so
// every handler can call h.now() without nil checks.
func (h *Handlers) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// log returns the configured logger or slog.Default — never nil.
func (h *Handlers) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// writeJSON encodes v as JSON with the given status. Encoder errors are
// logged but cannot be surfaced to the client (headers already written).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Error("writeJSON: encode failed", "err", err)
	}
}

// writeErr renders an error body matching the agent's shape:
// {"error": "...", "status": "<code>"}. Keeping the shape identical
// across processes simplifies SDK-side error handling.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{
		"error":  msg,
		"status": strconv.Itoa(status),
	})
}

// decodeBody pulls a JSON body into dst, rejecting unknown fields so a
// typo in a client SDK surfaces immediately. Body is closed on return.
func decodeBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

// parseListOpts pulls limit/offset query params. Invalid values silently
// fall back to the store's defaults — paginating an admin UI shouldn't
// 400 on a stray "limit=" string.
func parseListOpts(r *http.Request) store.ListOpts {
	q := r.URL.Query()
	opts := store.ListOpts{}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			opts.Offset = n
		}
	}
	return opts
}

// randomHex returns n bytes of OS randomness, hex encoded. Used as the
// primary key for accounts, sandboxes, snapshots, and so on. We don't
// pull in a UUID dependency just for this — 16 bytes of randomness is
// more than enough collision space.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// validEmail is a deliberately loose check: non-empty, contains "@", no
// whitespace. Real validation is the user clicking a link in their inbox;
// we only stop obvious garbage here.
func validEmail(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || !strings.Contains(s, "@") {
		return false
	}
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			return false
		}
	}
	return true
}

// translateStoreErr maps store-layer errors to HTTP status codes. Used
// at the boundary between handler and store; handlers never need to
// import store error sentinels themselves.
func translateStoreErr(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// pathID extracts {id} from a Go 1.22 method+path pattern. Returns "" if
// the path didn't actually carry one — handlers should validate.
func pathID(r *http.Request) string {
	return strings.TrimSpace(r.PathValue("id"))
}

// requireAdmin enforces the placeholder admin gate. The check is a
// straightforward equality against Handlers.AdminAccountID; if that
// field is empty no account can be admin (admin endpoints are then
// fully locked down). Returns the resolved account ID on success.
//
// TODO: replace with a real role column on accounts once we have one.
func (h *Handlers) requireAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return "", false
	}
	if h.AdminAccountID == "" || accountID != h.AdminAccountID {
		writeErr(w, http.StatusForbidden, "admin access required")
		return "", false
	}
	return accountID, true
}

// requestIDFromContext pulls the request ID out of the context — used
// only by handlers that want to log it.
func requestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// requestContextWithTimeout returns a child context of r.Context() with
// the given timeout. Wrapping http.Request's context this way keeps
// cancellation semantics intact even when the handler doesn't need the
// full client deadline.
func requestContextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
