// Package master — handlers_auth.go owns the unauthenticated registration
// and login endpoints plus the authenticated API-key CRUD. These all
// produce or consume credentials, so they are kept close together.
package master

import (
	"errors"
	"net/http"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// minPasswordLen is the floor for newly-created passwords. Eight bytes is
// the conventional bcrypt minimum and still accepts passphrases.
const minPasswordLen = 8

// registerRequest is the body of POST /v1/auth/register.
type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// registerResponse is the success body of POST /v1/auth/register. The
// raw API key is shown to the user exactly once.
type registerResponse struct {
	AccountID string `json:"account_id"`
	APIKey    string `json:"api_key"`
}

// register creates a new account and a first API key in a single tx.
func (h *Handlers) register(w http.ResponseWriter, r *http.Request) {
	var body registerRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validEmail(body.Email) {
		writeErr(w, http.StatusBadRequest, "invalid email")
		return
	}
	if len(body.Password) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	hash, err := HashPassword(body.Password)
	if err != nil {
		h.log().Error("register: hash password", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	accountID, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	keyID, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	rawKey, keyHash, err := GenerateAPIKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	now := h.now().UTC()
	account := &models.Account{
		ID: accountID, Email: body.Email,
		PasswordHash: hash, CreatedAt: now,
	}
	apiKey := &models.APIKey{
		ID: keyID, AccountID: accountID,
		KeyHash: keyHash, Name: "default",
		Permissions: models.Permissions{}, CreatedAt: now,
	}

	err = h.Store.WithTx(r.Context(), func(s store.Store) error {
		if err := s.Accounts().Create(r.Context(), account); err != nil {
			return err
		}
		return s.APIKeys().Create(r.Context(), apiKey)
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, http.StatusConflict, "email already registered")
			return
		}
		h.log().Error("register: persist", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusCreated, registerResponse{
		AccountID: accountID, APIKey: rawKey,
	})
}

// loginRequest is the body of POST /v1/auth/login.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// loginResponse is the success body of POST /v1/auth/login.
type loginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// login validates credentials and returns a fresh JWT.
func (h *Handlers) login(w http.ResponseWriter, r *http.Request) {
	var body loginRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Email == "" || body.Password == "" {
		writeErr(w, http.StatusBadRequest, "email and password required")
		return
	}

	account, err := h.Store.Accounts().GetByEmail(r.Context(), body.Email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		h.log().Error("login: lookup", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := VerifyPassword(account.PasswordHash, body.Password); err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	token, err := h.Signer.Sign(account.ID)
	if err != nil {
		h.log().Error("login: sign jwt", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{
		Token:     token,
		ExpiresAt: h.now().Add(JWTTTL),
	})
}

// refreshSession re-issues a JWT for the already-authenticated caller,
// extending the browser session by another JWTTTL. The dashboard polls
// this proactively before its current token expires, so a tab left open
// through a long operation (e.g. an autoscale wait) keeps a valid token
// and its background polling never starts 401-ing.
func (h *Handlers) refreshSession(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	token, err := h.Signer.Sign(accountID)
	if err != nil {
		h.log().Error("refreshSession: sign jwt", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{
		Token:     token,
		ExpiresAt: h.now().Add(JWTTTL),
	})
}

// createAPIKeyRequest is the body of POST /v1/api-keys.
type createAPIKeyRequest struct {
	Name string `json:"name"`
}

// apiKeyView is the row shape returned by listAPIKeys. KeyHash is never
// surfaced; we cannot reconstruct the raw secret.
type apiKeyView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// createAPIKeyResponse extends apiKeyView with the freshly-issued raw
// secret.
type createAPIKeyResponse struct {
	apiKeyView
	Key string `json:"key"`
}

// createAPIKey issues a new API key for the calling account.
func (h *Handlers) createAPIKey(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	var body createAPIKeyRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}

	keyID, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	raw, hash, err := GenerateAPIKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	now := h.now().UTC()
	key := &models.APIKey{
		ID: keyID, AccountID: accountID,
		KeyHash: hash, Name: body.Name,
		Permissions: models.Permissions{}, CreatedAt: now,
	}
	if err := h.Store.APIKeys().Create(r.Context(), key); err != nil {
		h.log().Error("createAPIKey: persist", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusCreated, createAPIKeyResponse{
		apiKeyView: apiKeyView{ID: keyID, Name: body.Name, CreatedAt: now},
		Key:        raw,
	})
}

// listAPIKeys returns the calling account's keys. We deliberately omit
// any prefix/suffix of the raw key — only the hash is stored, so we
// have nothing to reconstruct it from. Callers track keys by ID/name.
func (h *Handlers) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	keys, err := h.Store.APIKeys().ListByAccount(r.Context(), accountID, parseListOpts(r))
	if err != nil {
		h.log().Error("listAPIKeys", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	views := make([]apiKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, apiKeyView{ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt})
	}
	writeJSON(w, http.StatusOK, views)
}

// deleteAPIKey removes the named key from the calling account.
func (h *Handlers) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing key id")
		return
	}
	if err := h.Store.APIKeys().Delete(r.Context(), accountID, id); err != nil {
		if status := translateStoreErr(err); status != http.StatusInternalServerError {
			writeErr(w, status, http.StatusText(status))
			return
		}
		h.log().Error("deleteAPIKey", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
