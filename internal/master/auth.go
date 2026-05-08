// Package master is the vajra control plane. auth.go is the
// authentication and middleware layer: HS256 JWTs for browser sessions,
// long-lived API keys (vj_live_* prefix) for SDK calls, bcrypt password
// hashing, plus the HTTP middleware that resolves an Authorization header
// into an account_id on the request context.
//
// A separate InternalAuthMiddleware guards /internal/nodes/* with a
// pre-shared secret instead of user credentials — node agents are
// peers of the control plane, not customers, so they don't have an
// account.
package master

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/allenabraham999/vajra/internal/store"
)

// JWTTTL is how long a freshly minted browser-session token stays valid.
// One hour matches typical dashboard activity windows; SDKs should use
// API keys instead.
const JWTTTL = time.Hour

// BcryptCost is the bcrypt work factor for stored password hashes.
// Higher than the library default (10) — auth is not a hot path and the
// extra ~4x cost meaningfully slows offline brute-force.
const BcryptCost = 12

// APIKeyPrefix is the literal prefix every issued key carries. Keeping
// it visible in raw keys lets log scrubbers and secret-scanners catch
// leaked credentials without needing to know our hashing scheme.
const APIKeyPrefix = "vj_live_"

// apiKeyTotalLen is the exact length of a well-formed key:
// len("vj_live_") + 32 hex chars = 40.
const apiKeyTotalLen = len(APIKeyPrefix) + 32

// ctxKey is unexported so callers outside this package can't collide
// with our context entries by guessing the key.
type ctxKey int

const ctxKeyAccountID ctxKey = 1

// ---------- JWT ----------

// JWTSigner wraps the HMAC secret used to sign and verify session
// tokens. Constructed once at server startup and shared across requests.
type JWTSigner struct {
	secret []byte
	now    func() time.Time // overridable for tests
}

// NewJWTSigner builds a signer from the configured HMAC secret. The
// caller owns the secret bytes — we don't copy.
func NewJWTSigner(secret []byte) *JWTSigner {
	return &JWTSigner{secret: secret, now: time.Now}
}

// Sign issues a fresh HS256 JWT for the given account, valid for
// JWTTTL. The only claim we populate beyond the registered set is
// Subject; nothing else needs to ride in the token.
func (s *JWTSigner) Sign(accountID string) (string, error) {
	return s.signWithTTL(accountID, JWTTTL)
}

// signWithTTL is exposed for tests that need to mint already-expired
// tokens without monkey-patching time.Now.
func (s *JWTSigner) signWithTTL(accountID string, ttl time.Duration) (string, error) {
	now := s.now()
	claims := jwt.RegisteredClaims{
		Subject:   accountID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signed, nil
}

// Validate verifies the token's signature and expiry and returns the
// subject (account ID). All failures collapse to a single error so
// the auth middleware can return a uniform 401 without leaking which
// validation step failed.
func (s *JWTSigner) Validate(token string) (string, error) {
	parsed, err := jwt.ParseWithClaims(token, &jwt.RegisteredClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil {
		return "", fmt.Errorf("parse jwt: %w", err)
	}
	claims, ok := parsed.Claims.(*jwt.RegisteredClaims)
	if !ok || !parsed.Valid {
		return "", errors.New("invalid jwt claims")
	}
	if claims.Subject == "" {
		return "", errors.New("jwt missing subject")
	}
	return claims.Subject, nil
}

// ---------- Passwords ----------

// HashPassword bcrypts the plaintext at BcryptCost. The result is the
// canonical string form, safe to store directly in a database column.
func HashPassword(plain string) (string, error) {
	out, err := bcrypt.GenerateFromPassword([]byte(plain), BcryptCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt: %w", err)
	}
	return string(out), nil
}

// VerifyPassword returns nil iff plain matches the stored bcrypt hash.
// Mismatches and decoding errors both surface as non-nil; callers
// should treat them identically.
func VerifyPassword(hash, plain string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
}

// ---------- API keys ----------

// GenerateAPIKey produces a fresh `vj_live_<32 hex>` key plus its
// SHA256 hex digest. The raw key is shown to the user exactly once at
// creation time; only the hash is persisted, so a database leak does
// not expose live credentials.
func GenerateAPIKey() (raw string, hash string, err error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", fmt.Errorf("read random bytes: %w", err)
	}
	raw = APIKeyPrefix + hex.EncodeToString(buf[:])
	hash = HashAPIKey(raw)
	return raw, hash, nil
}

// HashAPIKey returns the SHA256 hex of raw. Exported so handlers can
// hash an inbound key the same way the issuer did before doing a
// database lookup.
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// IsAPIKeyFormat is a cheap shape check — prefix + total length only.
// Real validation happens by hashing and looking up the row.
func IsAPIKeyFormat(token string) bool {
	return len(token) == apiKeyTotalLen && strings.HasPrefix(token, APIKeyPrefix)
}

// ---------- Middleware ----------

// AuthMiddleware authenticates user requests. It accepts either an
// API key (vj_live_…) or an HS256 JWT in the standard
// "Authorization: Bearer …" header, and on success injects the
// account ID into the request context for downstream handlers.
func AuthMiddleware(signer *JWTSigner, keys store.APIKeyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				slog.Default().Debug("auth: missing or malformed Authorization header")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			accountID, status := resolveToken(r.Context(), signer, keys, token)
			if status != 0 {
				http.Error(w, http.StatusText(status), status)
				return
			}

			ctx := WithAccountID(r.Context(), accountID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveToken returns the account ID on success, or an HTTP status
// code on failure (0 means "no error"). Splitting this out keeps
// AuthMiddleware short and gives tests a way to exercise individual
// branches without spinning up an HTTP server.
func resolveToken(ctx context.Context, signer *JWTSigner, keys store.APIKeyStore, token string) (string, int) {
	if IsAPIKeyFormat(token) {
		key, err := keys.GetByHash(ctx, HashAPIKey(token))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				slog.Default().Debug("auth: api key not found")
				return "", http.StatusUnauthorized
			}
			slog.Default().Error("auth: api key lookup failed", "err", err)
			return "", http.StatusInternalServerError
		}
		if key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now()) {
			slog.Default().Debug("auth: api key expired", "key_id", key.ID)
			return "", http.StatusUnauthorized
		}
		return key.AccountID, 0
	}

	accountID, err := signer.Validate(token)
	if err != nil {
		slog.Default().Debug("auth: jwt validation failed", "err", err)
		return "", http.StatusUnauthorized
	}
	return accountID, 0
}

// InternalAuthMiddleware guards endpoints meant for node agents. It
// requires an exact match against a pre-shared secret; constant-time
// compare to avoid leaking the secret via timing attacks.
func InternalAuthMiddleware(sharedSecret string) func(http.Handler) http.Handler {
	expected := []byte(sharedSecret)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				slog.Default().Debug("internal auth: missing or malformed Authorization header")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if subtle.ConstantTimeCompare([]byte(token), expected) != 1 {
				slog.Default().Debug("internal auth: shared secret mismatch")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken pulls the token out of an "Authorization: Bearer …"
// header. The check is case-insensitive on the scheme but not the
// token (which is opaque to us).
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// ---------- Context helpers ----------

// AccountIDFromContext returns the authenticated account ID, or "" if
// the request was not authenticated. Handlers that called through
// AuthMiddleware can rely on this being non-empty.
func AccountIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyAccountID).(string); ok {
		return v
	}
	return ""
}

// WithAccountID stamps an account ID onto a context. Exported for
// tests that need to inject one without going through the middleware.
func WithAccountID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyAccountID, id)
}

// RequireAccount returns the authenticated account ID and writes 401
// if it's missing. AuthMiddleware should have already filtered those
// out, so a missing ID here is an internal wiring bug — log loudly.
func RequireAccount(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := AccountIDFromContext(r.Context())
	if id == "" {
		slog.Default().Error("RequireAccount called without AuthMiddleware in chain", "path", r.URL.Path)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	return id, true
}
