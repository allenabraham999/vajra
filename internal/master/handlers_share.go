// Package master — handlers_share.go is the shareable-link surface:
//
//	POST    /v1/sandboxes/{id}/share              create
//	GET     /v1/sandboxes/{id}/shares             list
//	DELETE  /v1/sandboxes/{id}/share/{token_id}   revoke
//	GET     /internal/proxy/route                 proxy → agent route
//	GET     /internal/proxy/validate-share        proxy share validator
//
// Tokens are 32 bytes of OS randomness, hex-encoded. Master stores only
// the SHA256 of the token; cleartext is shown to the user exactly once
// at creation time and can never be retrieved later.
package master

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// MaxShareTTL is the longest expiry we'll accept on a fresh share. 30
// days lines up with the API key cap in auth.go and is enough for any
// reasonable demo/handoff flow without bloating the share_links table
// with permanent backdoors into a sandbox.
const MaxShareTTL = 30 * 24 * time.Hour

// createShareRequest is the body of POST /v1/sandboxes/{id}/share.
// ExpiresInSeconds == 0 means "no expiry"; Port == 0 means "any port".
type createShareRequest struct {
	ExpiresInSeconds int64 `json:"expires_in_seconds,omitempty"`
	Port             int   `json:"port,omitempty"`
}

// createShareResponse is the JSON body returned on success. Token is
// the cleartext (only ever shown here); URL is a convenience suggestion
// the SDK can copy/paste, formatted as `https://{port}-{id}.{base}/?token=…`
// when the master knows the public base domain (env var).
type createShareResponse struct {
	ID        string     `json:"id"`
	Token     string     `json:"token"`
	URL       string     `json:"url,omitempty"`
	Port      *int       `json:"port,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// createShare allocates a fresh token, hashes it, and persists.
func (h *Handlers) createShare(w http.ResponseWriter, r *http.Request) {
	accountID, sb, _, ok := h.resolveSandboxAndNode(w, r)
	if !ok {
		return
	}
	var body createShareRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ExpiresInSeconds < 0 {
		writeErr(w, http.StatusBadRequest, "expires_in_seconds must be >= 0")
		return
	}
	if body.ExpiresInSeconds > int64(MaxShareTTL.Seconds()) {
		writeErr(w, http.StatusBadRequest, "expires_in_seconds too long")
		return
	}
	if body.Port < 0 || body.Port > 65535 {
		writeErr(w, http.StatusBadRequest, "invalid port")
		return
	}

	tokenRaw, err := randomHex(32)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	tokenHash := hashToken(tokenRaw)
	id, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := h.now().UTC()
	var expiresAt *time.Time
	if body.ExpiresInSeconds > 0 {
		t := now.Add(time.Duration(body.ExpiresInSeconds) * time.Second)
		expiresAt = &t
	}
	var portPtr *int
	if body.Port > 0 {
		p := body.Port
		portPtr = &p
	}
	link := &models.ShareLink{
		ID: id, AccountID: accountID, SandboxID: sb.ID,
		TokenHash: tokenHash, Port: portPtr,
		CreatedAt: now, ExpiresAt: expiresAt,
	}
	if err := h.Store.ShareLinks().Create(r.Context(), link); err != nil {
		h.log().Error("createShare: persist", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, createShareResponse{
		ID:        id,
		Token:     tokenRaw,
		URL:       buildShareURL(h.PublicBaseDomain, sb.ID, body.Port, tokenRaw),
		Port:      portPtr,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	})
}

// listShares returns active and revoked shares for a sandbox.
func (h *Handlers) listShares(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing sandbox id")
		return
	}
	if _, err := h.Store.Sandboxes().GetByID(r.Context(), accountID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeErr(w, translateStoreErr(err), "lookup failed")
		return
	}
	out, err := h.Store.ShareLinks().ListBySandbox(r.Context(), accountID, id, parseListOpts(r))
	if err != nil {
		h.log().Error("listShares", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// revokeShare marks the named share revoked. URL: /v1/sandboxes/{id}/share/{token_id}.
// We don't actually need the sandbox ID from the path to revoke (token
// IDs are globally unique) — but enforcing the route shape lets us 404
// on cross-account attempts at the top of the handler.
func (h *Handlers) revokeShare(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	tokenID := r.PathValue("token_id")
	if tokenID == "" {
		writeErr(w, http.StatusBadRequest, "missing token id")
		return
	}
	if err := h.Store.ShareLinks().Revoke(r.Context(), accountID, tokenID, h.now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "share not found")
			return
		}
		h.log().Error("revokeShare", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// proxyRouteResponse is the JSON master returns to vajra-proxy when it
// asks how to reach a sandbox. The proxy uses it as the upstream URL.
type proxyRouteResponse struct {
	SandboxID    string `json:"sandbox_id"`
	AccountID    string `json:"account_id"`
	AgentBaseURL string `json:"agent_base_url"`
	AgentSecret  string `json:"agent_secret"`
	State        string `json:"state"`
}

// proxyRoute is the internal endpoint the proxy hits to resolve a
// sandbox to its agent. We don't gate this by share token here — the
// proxy does that separately via /internal/proxy/validate-share.
func (h *Handlers) proxyRoute(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.URL.Query().Get("sandbox_id")
	if sandboxID == "" {
		writeErr(w, http.StatusBadRequest, "sandbox_id is required")
		return
	}
	// Cross-account-by-design: proxy doesn't know which account the
	// caller belongs to.
	sb, err := h.findSandbox(r, sandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.log().Error("proxyRoute: lookup", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if sb.NodeID == nil || *sb.NodeID == "" {
		writeErr(w, http.StatusServiceUnavailable, "sandbox has no placement")
		return
	}
	node, err := h.Store.Nodes().GetByID(r.Context(), *sb.NodeID)
	if err != nil {
		h.log().Error("proxyRoute: node lookup", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, proxyRouteResponse{
		SandboxID:    sb.ID,
		AccountID:    sb.AccountID,
		AgentBaseURL: fmt.Sprintf("http://%s:%d", node.IP, DefaultAgentPort),
		AgentSecret:  h.AgentSharedSecret,
		State:        string(sb.State),
	})
}

// validateShare confirms a token is valid for a given sandbox + port.
// 200 = OK, 404 = unknown token, 403 = wrong sandbox or wrong port,
// 410 = revoked / expired. The status code is what vajra-proxy keys on.
func (h *Handlers) validateShare(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sandboxID := q.Get("sandbox_id")
	token := q.Get("token")
	if sandboxID == "" || token == "" {
		writeErr(w, http.StatusBadRequest, "sandbox_id and token are required")
		return
	}
	tokenHash := hashToken(token)
	link, err := h.Store.ShareLinks().GetByHash(r.Context(), tokenHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "unknown token")
			return
		}
		h.log().Error("validateShare", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if link.SandboxID != sandboxID {
		writeErr(w, http.StatusForbidden, "token not for this sandbox")
		return
	}
	if !link.IsActive(h.now().UTC()) {
		writeErr(w, http.StatusGone, "token revoked or expired")
		return
	}
	if link.Port != nil {
		// If the share was scoped to one port, reject any other port.
		if portStr := q.Get("port"); portStr != "" {
			if p, err := strconv.Atoi(portStr); err != nil || p != *link.Port {
				writeErr(w, http.StatusForbidden, "token not for this port")
				return
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// hashToken returns the SHA256 hex digest of a cleartext token. We
// store this rather than the cleartext so a leaked DB doesn't leak
// usable tokens.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// findSandbox is the cross-account lookup used by proxyRoute. The
// caller has already authenticated as a system-internal client (proxy)
// via InternalAuthMiddleware, so account scoping is intentionally
// dropped here.
func (h *Handlers) findSandbox(r *http.Request, sandboxID string) (*models.Sandbox, error) {
	return h.Store.Sandboxes().GetByIDUnscoped(r.Context(), sandboxID)
}

// buildShareURL composes the suggested user-facing URL. Returns "" when
// the master doesn't know its public base domain (no env config).
func buildShareURL(base, sandboxID string, port int, token string) string {
	if base == "" {
		return ""
	}
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("https://%d-%s.%s/?token=%s", port, sandboxID, base, token)
}
