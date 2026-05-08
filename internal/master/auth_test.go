package master

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// fakeAPIKeyStore is a minimal in-memory APIKeyStore for the
// middleware tests. Only GetByHash is exercised; the rest are no-ops
// so we still satisfy the interface.
type fakeAPIKeyStore struct {
	byHash map[string]*models.APIKey
	err    error // override for non-NotFound failure paths
}

func (f *fakeAPIKeyStore) Create(context.Context, *models.APIKey) error { return nil }
func (f *fakeAPIKeyStore) GetByID(context.Context, string, string) (*models.APIKey, error) {
	return nil, store.ErrNotFound
}
func (f *fakeAPIKeyStore) GetByHash(_ context.Context, hash string) (*models.APIKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	if k, ok := f.byHash[hash]; ok {
		return k, nil
	}
	return nil, store.ErrNotFound
}
func (f *fakeAPIKeyStore) ListByAccount(context.Context, string, store.ListOpts) ([]*models.APIKey, error) {
	return nil, nil
}
func (f *fakeAPIKeyStore) Delete(context.Context, string, string) error { return nil }

// ---------- JWT ----------

func TestJWTRoundTrip(t *testing.T) {
	signer := NewJWTSigner([]byte("test-secret"))

	tok, err := signer.Sign("acct_123")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	got, err := signer.Validate(tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got != "acct_123" {
		t.Fatalf("got %q, want %q", got, "acct_123")
	}
}

func TestJWTTamperedSignatureRejected(t *testing.T) {
	signer := NewJWTSigner([]byte("test-secret"))
	tok, err := signer.Sign("acct_123")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Flip a byte in the signature segment (last segment after final '.').
	idx := strings.LastIndex(tok, ".")
	if idx < 0 {
		t.Fatalf("no signature segment in %q", tok)
	}
	tampered := tok[:idx+1] + "AAAA" + tok[idx+5:]

	if _, err := signer.Validate(tampered); err == nil {
		t.Fatalf("tampered token validated, expected error")
	}
}

func TestJWTExpiredRejected(t *testing.T) {
	signer := NewJWTSigner([]byte("test-secret"))
	tok, err := signer.signWithTTL("acct_123", -time.Minute)
	if err != nil {
		t.Fatalf("signWithTTL: %v", err)
	}
	if _, err := signer.Validate(tok); err == nil {
		t.Fatalf("expired token validated, expected error")
	}
}

func TestJWTWrongSecretRejected(t *testing.T) {
	a := NewJWTSigner([]byte("secret-a"))
	b := NewJWTSigner([]byte("secret-b"))
	tok, err := a.Sign("acct_x")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := b.Validate(tok); err == nil {
		t.Fatalf("token from secret-a validated under secret-b")
	}
}

// ---------- Passwords ----------

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("hunter2")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "hunter2" {
		t.Fatalf("hash equals plaintext")
	}
	if err := VerifyPassword(hash, "hunter2"); err != nil {
		t.Fatalf("VerifyPassword (correct): %v", err)
	}
	if err := VerifyPassword(hash, "wrong"); err == nil {
		t.Fatalf("VerifyPassword (wrong) returned nil, expected error")
	}
}

// ---------- API keys ----------

func TestGenerateAPIKeyFormat(t *testing.T) {
	rawRe := regexp.MustCompile(`^vj_live_[0-9a-f]{32}$`)
	for i := 0; i < 5; i++ {
		raw, hash, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		if !rawRe.MatchString(raw) {
			t.Fatalf("raw %q doesn't match expected format", raw)
		}
		if len(raw) != 40 {
			t.Fatalf("raw len = %d, want 40", len(raw))
		}
		if hash == raw {
			t.Fatalf("hash equals raw key")
		}
		// SHA256 hex is 64 chars.
		if len(hash) != 64 {
			t.Fatalf("hash len = %d, want 64", len(hash))
		}
		if _, err := hex.DecodeString(hash); err != nil {
			t.Fatalf("hash %q is not hex: %v", hash, err)
		}
		if HashAPIKey(raw) != hash {
			t.Fatalf("HashAPIKey is not deterministic with GenerateAPIKey")
		}
	}
}

func TestHashAPIKeyDeterministic(t *testing.T) {
	raw := "vj_live_" + strings.Repeat("a", 32)
	if HashAPIKey(raw) != HashAPIKey(raw) {
		t.Fatalf("HashAPIKey not deterministic")
	}
}

func TestIsAPIKeyFormat(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"valid", "vj_live_" + strings.Repeat("a", 32), true},
		{"too short", "vj_live_abc", false},
		{"too long", "vj_live_" + strings.Repeat("a", 33), false},
		{"wrong prefix", "vj_test_" + strings.Repeat("a", 32), false},
		{"empty", "", false},
		{"jwt-shaped", "eyJhbGciOi.JSUzI1NiIsInR.5cCI6IkpXVCJ9", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAPIKeyFormat(tc.in); got != tc.want {
				t.Fatalf("IsAPIKeyFormat(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ---------- AuthMiddleware ----------

func newAuthHarness(t *testing.T, keys *fakeAPIKeyStore) (*JWTSigner, http.Handler, *string) {
	t.Helper()
	signer := NewJWTSigner([]byte("test-secret"))
	seen := new(string)
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = AccountIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	return signer, AuthMiddleware(signer, keys)(final), seen
}

func TestAuthMiddleware_MissingHeader(t *testing.T) {
	_, h, _ := newAuthHarness(t, &fakeAPIKeyStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_BadSchemeRejected(t *testing.T) {
	_, h, _ := newAuthHarness(t, &fakeAPIKeyStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_JWTValid(t *testing.T) {
	signer, h, seen := newAuthHarness(t, &fakeAPIKeyStore{})
	tok, err := signer.Sign("acct_jwt")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if *seen != "acct_jwt" {
		t.Fatalf("handler saw account_id = %q, want acct_jwt", *seen)
	}
}

func TestAuthMiddleware_JWTExpired(t *testing.T) {
	signer, h, _ := newAuthHarness(t, &fakeAPIKeyStore{})
	tok, err := signer.signWithTTL("acct_jwt", -time.Minute)
	if err != nil {
		t.Fatalf("signWithTTL: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_APIKeyValid(t *testing.T) {
	raw, hash, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	keys := &fakeAPIKeyStore{
		byHash: map[string]*models.APIKey{
			hash: {ID: "key_1", AccountID: "acct_apikey", KeyHash: hash},
		},
	}
	_, h, seen := newAuthHarness(t, keys)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if *seen != "acct_apikey" {
		t.Fatalf("handler saw account_id = %q, want acct_apikey", *seen)
	}
}

func TestAuthMiddleware_APIKeyNotFound(t *testing.T) {
	raw, _, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	keys := &fakeAPIKeyStore{byHash: map[string]*models.APIKey{}}
	_, h, _ := newAuthHarness(t, keys)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_APIKeyExpired(t *testing.T) {
	raw, hash, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	keys := &fakeAPIKeyStore{
		byHash: map[string]*models.APIKey{
			hash: {ID: "key_1", AccountID: "acct_x", KeyHash: hash, ExpiresAt: &past},
		},
	}
	_, h, _ := newAuthHarness(t, keys)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_APIKeyStoreError(t *testing.T) {
	raw, _, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	keys := &fakeAPIKeyStore{err: errors.New("db down")}
	_, h, _ := newAuthHarness(t, keys)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// ---------- InternalAuthMiddleware ----------

func TestInternalAuthMiddleware(t *testing.T) {
	const secret = "shared-secret-xyz"
	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := InternalAuthMiddleware(secret)(final)

	cases := []struct {
		name       string
		header     string
		wantStatus int
		wantCalled bool
	}{
		{"missing", "", http.StatusUnauthorized, false},
		{"wrong scheme", "Basic " + secret, http.StatusUnauthorized, false},
		{"wrong secret", "Bearer not-the-secret", http.StatusUnauthorized, false},
		{"valid", "Bearer " + secret, http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/internal/x", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if called != tc.wantCalled {
				t.Fatalf("called = %v, want %v", called, tc.wantCalled)
			}
		})
	}
}

// ---------- Context helpers ----------

func TestAccountIDFromContext(t *testing.T) {
	if got := AccountIDFromContext(context.Background()); got != "" {
		t.Fatalf("empty ctx returned %q, want \"\"", got)
	}
	ctx := WithAccountID(context.Background(), "acct_42")
	if got := AccountIDFromContext(ctx); got != "acct_42" {
		t.Fatalf("got %q, want acct_42", got)
	}
}

func TestRequireAccount(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req = req.WithContext(WithAccountID(req.Context(), "acct_ok"))
		got, ok := RequireAccount(rec, req)
		if !ok || got != "acct_ok" {
			t.Fatalf("RequireAccount = (%q, %v), want (acct_ok, true)", got, ok)
		}
		if rec.Code != http.StatusOK { // no Write happened, default is 200
			t.Fatalf("unexpected status %d", rec.Code)
		}
	})
	t.Run("absent", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		got, ok := RequireAccount(rec, req)
		if ok || got != "" {
			t.Fatalf("RequireAccount = (%q, %v), want (\"\", false)", got, ok)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}
