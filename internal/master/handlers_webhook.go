// Package master — handlers_webhook.go: webhook CRUD + test-fire.
//
// Webhooks live per-account; on creation we mint a fresh HMAC secret
// and return it exactly once (mirroring the API-key contract). All
// subsequent reads omit the secret.
package master

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// createWebhookRequest is the body of POST /v1/webhooks.
type createWebhookRequest struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

// createWebhook persists a fresh webhook + secret and returns the row
// including the raw secret. Returning the secret here is the only chance
// the caller has to record it; subsequent reads omit it.
func (h *Handlers) createWebhook(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	var body createWebhookRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	if body.URL == "" || (!strings.HasPrefix(body.URL, "http://") && !strings.HasPrefix(body.URL, "https://")) {
		writeErr(w, http.StatusBadRequest, "url must be http(s)://...")
		return
	}
	if len(body.Events) == 0 {
		writeErr(w, http.StatusBadRequest, "events is required")
		return
	}
	for _, e := range body.Events {
		if !models.ValidWebhookEvent(e) {
			writeErr(w, http.StatusBadRequest, "unknown event: "+e)
			return
		}
	}
	id, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	secret, err := newWebhookSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	hook := &models.Webhook{
		ID: id, AccountID: accountID,
		URL: body.URL, Secret: secret,
		Events:    models.WebhookEvents(body.Events),
		Active:    true,
		CreatedAt: h.now().UTC(),
	}
	if err := h.Store.Webhooks().Create(r.Context(), hook); err != nil {
		h.log().Error("createWebhook", "err", err)
		writeErr(w, translateStoreErr(err), "create failed")
		return
	}
	// Return the secret exactly once.
	writeJSON(w, http.StatusCreated, hook)
}

// listWebhooks returns the calling account's webhooks. Secrets are
// stripped from the response — they were shown at creation time only.
func (h *Handlers) listWebhooks(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	out, err := h.Store.Webhooks().ListByAccount(r.Context(), accountID, parseListOpts(r))
	if err != nil {
		h.log().Error("listWebhooks", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, hk := range out {
		hk.Secret = ""
	}
	writeJSON(w, http.StatusOK, out)
}

// getWebhook returns one webhook by ID. Secret is omitted.
func (h *Handlers) getWebhook(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	hook, err := h.Store.Webhooks().GetByID(r.Context(), accountID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "webhook not found")
			return
		}
		h.log().Error("getWebhook", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	hook.Secret = ""
	writeJSON(w, http.StatusOK, hook)
}

// deleteWebhook removes a webhook permanently.
func (h *Handlers) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	if err := h.Store.Webhooks().Delete(r.Context(), accountID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "webhook not found")
			return
		}
		h.log().Error("deleteWebhook", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testWebhook fires a synthetic event at the named webhook so users can
// verify their receiver is wired up. We send a synthetic payload with
// event="sandbox.created" and an obviously-fake sandbox stub.
func (h *Handlers) testWebhook(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	hook, err := h.Store.Webhooks().GetByID(r.Context(), accountID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "webhook not found")
			return
		}
		h.log().Error("testWebhook", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if h.Webhooks == nil {
		writeErr(w, http.StatusServiceUnavailable, "webhook dispatcher not configured")
		return
	}
	ok2, err := h.Webhooks.DeliverOne(hook, string(models.WebhookEventSandboxCreated), map[string]any{
		"id":   "sb-test-delivery",
		"name": "test",
		"note": "synthetic payload from POST /v1/webhooks/{id}/test",
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"webhook_id": hook.ID,
		"delivered":  ok2,
	})
}

// newWebhookSecret returns a fresh 32-byte hex string used as the
// HMAC key. 256 bits matches the SHA-256 output width.
func newWebhookSecret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
