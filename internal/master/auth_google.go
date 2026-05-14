// Package master — auth_google.go owns the Google OAuth login surface.
// The flow is the textbook authorization-code dance: GET /v1/auth/google
// stamps a state cookie and 302s to Google's consent screen; Google calls
// back to GET /v1/auth/google/callback with code+state; we exchange the
// code for an access token, pull the userinfo email, find-or-create an
// account, mint a JWT, and bounce the browser to the dashboard with the
// token in the URL fragment.
//
// GET /v1/auth/config is the dashboard's "which login buttons should I
// render" probe — it always works (returns email_auth_enabled=true) and
// flips google_oauth_enabled when GoogleOAuth is fully configured.
package master

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleUserURL  = "https://www.googleapis.com/oauth2/v2/userinfo"

	oauthStateCookie  = "vajra_oauth_state"
	oauthReturnCookie = "vajra_oauth_return"
	oauthCookieTTL    = 10 * time.Minute

	googleHTTPTimeout = 10 * time.Second
)

// GoogleOAuthConfig is the optional Google-login configuration. Wired in
// from env vars by main: empty fields → endpoints respond as if OAuth
// were disabled. RedirectURL must match the URI registered with the
// Google Cloud project exactly. DashboardURL is where the browser lands
// after a successful exchange — typically the dashboard origin.
type GoogleOAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	DashboardURL string
}

// Enabled reports whether the minimum configuration for an actual
// OAuth round trip is present.
func (g GoogleOAuthConfig) Enabled() bool {
	return g.ClientID != "" && g.ClientSecret != "" && g.RedirectURL != ""
}

// authConfigResponse is the JSON body of GET /v1/auth/config.
type authConfigResponse struct {
	GoogleOAuthEnabled bool `json:"google_oauth_enabled"`
	EmailAuthEnabled   bool `json:"email_auth_enabled"`
}

// authConfig reports which login mechanisms the master will accept.
func (h *Handlers) authConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, authConfigResponse{
		GoogleOAuthEnabled: h.GoogleOAuth.Enabled(),
		EmailAuthEnabled:   true,
	})
}

// googleInitiate kicks off the OAuth handshake. The random state cookie
// is the CSRF guard for the callback; an optional return_to query param
// is stored in a sibling cookie so the callback knows where to bounce
// the user after success.
func (h *Handlers) googleInitiate(w http.ResponseWriter, r *http.Request) {
	if !h.GoogleOAuth.Enabled() {
		writeErr(w, http.StatusNotFound, "google oauth not configured")
		return
	}
	state, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	setOAuthCookie(w, oauthStateCookie, state, oauthCookieTTL)
	if rt := strings.TrimSpace(r.URL.Query().Get("return_to")); rt != "" {
		setOAuthCookie(w, oauthReturnCookie, rt, oauthCookieTTL)
	}

	q := url.Values{}
	q.Set("client_id", h.GoogleOAuth.ClientID)
	q.Set("redirect_uri", h.GoogleOAuth.RedirectURL)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("access_type", "online")
	q.Set("prompt", "select_account")
	q.Set("state", state)
	http.Redirect(w, r, googleAuthURL+"?"+q.Encode(), http.StatusFound)
}

// googleCallback handles Google's redirect: validate state, exchange
// code, fetch userinfo, find-or-create the account, mint a JWT, and 302
// the browser to the dashboard with the token in the URL fragment so it
// never appears in server-side logs or Referer headers.
func (h *Handlers) googleCallback(w http.ResponseWriter, r *http.Request) {
	if !h.GoogleOAuth.Enabled() {
		writeErr(w, http.StatusNotFound, "google oauth not configured")
		return
	}
	q := r.URL.Query()
	if errMsg := q.Get("error"); errMsg != "" {
		writeErr(w, http.StatusUnauthorized, "google: "+errMsg)
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		writeErr(w, http.StatusBadRequest, "missing code or state")
		return
	}

	stateCookie, err := r.Cookie(oauthStateCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(state)) != 1 {
		writeErr(w, http.StatusBadRequest, "invalid oauth state")
		return
	}
	clearOAuthCookie(w, oauthStateCookie)

	tok, err := exchangeGoogleCode(r.Context(), h.GoogleOAuth, code)
	if err != nil {
		h.log().Error("googleCallback: exchange", "err", err)
		writeErr(w, http.StatusBadGateway, "google token exchange failed")
		return
	}

	info, err := fetchGoogleUserInfo(r.Context(), tok.AccessToken)
	if err != nil {
		h.log().Error("googleCallback: userinfo", "err", err)
		writeErr(w, http.StatusBadGateway, "google userinfo failed")
		return
	}
	if !info.VerifiedEmail {
		writeErr(w, http.StatusForbidden, "google email not verified")
		return
	}

	accountID, err := h.findOrCreateGoogleAccount(r.Context(), info.Email)
	if err != nil {
		h.log().Error("googleCallback: account", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	jwtTok, err := h.Signer.Sign(accountID)
	if err != nil {
		h.log().Error("googleCallback: sign jwt", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	expires := h.now().Add(JWTTTL).Unix()

	returnURL := h.GoogleOAuth.DashboardURL
	if c, err := r.Cookie(oauthReturnCookie); err == nil && c.Value != "" {
		returnURL = c.Value
	}
	if returnURL == "" {
		returnURL = "/"
	}
	clearOAuthCookie(w, oauthReturnCookie)

	sep := "#"
	if strings.Contains(returnURL, "#") {
		sep = "&"
	}
	final := fmt.Sprintf("%s%stoken=%s&expires=%d&email=%s",
		returnURL, sep,
		url.QueryEscape(jwtTok),
		expires,
		url.QueryEscape(info.Email),
	)
	http.Redirect(w, r, final, http.StatusFound)
}

// setOAuthCookie writes an HttpOnly, SameSite=Lax cookie. SameSite must
// stay Lax (not Strict) so the cookie survives the cross-site redirect
// from accounts.google.com back to our callback.
func setOAuthCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  time.Now().Add(ttl),
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearOAuthCookie expires a previously set OAuth cookie.
func clearOAuthCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// googleTokenResponse is the shape of the token-endpoint reply.
type googleTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	IDToken     string `json:"id_token,omitempty"`
}

// exchangeGoogleCode trades the one-shot authorization code for an
// access token. We do not persist the token; userinfo is the only call
// we make against it before discarding.
func exchangeGoogleCode(ctx context.Context, cfg GoogleOAuthConfig, code string) (*googleTokenResponse, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	form.Set("redirect_uri", cfg.RedirectURL)
	form.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: googleHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var out googleTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if out.AccessToken == "" {
		return nil, errors.New("token endpoint returned empty access_token")
	}
	return &out, nil
}

// googleUserInfo is the subset of /oauth2/v2/userinfo we consume.
type googleUserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
}

// fetchGoogleUserInfo pulls the authenticated user's profile.
func fetchGoogleUserInfo(ctx context.Context, accessToken string) (*googleUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleUserURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: googleHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("userinfo returned %d", resp.StatusCode)
	}
	var out googleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if out.Email == "" {
		return nil, errors.New("userinfo returned empty email")
	}
	return &out, nil
}

// findOrCreateGoogleAccount returns the account ID for email, creating a
// new row when none exists. Google-sourced accounts are stamped with a
// random unguessable password hash so the email/password login path is
// inert against them until the user explicitly sets one.
func (h *Handlers) findOrCreateGoogleAccount(ctx context.Context, email string) (string, error) {
	existing, err := h.Store.Accounts().GetByEmail(ctx, email)
	if err == nil {
		return existing.ID, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("lookup: %w", err)
	}

	accountID, err := randomHex(16)
	if err != nil {
		return "", err
	}
	sentinel, err := randomHex(32)
	if err != nil {
		return "", err
	}
	hash, err := HashPassword(sentinel)
	if err != nil {
		return "", err
	}
	acc := &models.Account{
		ID:           accountID,
		Email:        email,
		PasswordHash: hash,
		CreatedAt:    h.now().UTC(),
	}
	if err := h.Store.Accounts().Create(ctx, acc); err != nil {
		if errors.Is(err, store.ErrConflict) {
			again, err2 := h.Store.Accounts().GetByEmail(ctx, email)
			if err2 == nil {
				return again.ID, nil
			}
			return "", err2
		}
		return "", err
	}
	return accountID, nil
}
