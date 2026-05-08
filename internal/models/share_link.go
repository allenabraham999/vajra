package models

import "time"

// ShareLink is a tokenised pointer to a sandbox: the URL holder can
// reach the sandbox's port forward / terminal endpoints without a
// vajra account.
//
// Tokens are 32 bytes of OS randomness, hex-encoded. We never store the
// cleartext — TokenHash is the SHA256 hex digest, matching the API key
// shape. Lookups happen by hash, so token verification is constant-time
// against the indexed column.
//
// Port = nil means "any port the sandbox exposes"; setting it pins the
// share to a single TCP port (the typical case — a developer wants to
// share their app on :3000, not their database on :5432).
type ShareLink struct {
	ID         string     `db:"id" json:"id"`
	AccountID  string     `db:"account_id" json:"account_id"`
	SandboxID  string     `db:"sandbox_id" json:"sandbox_id"`
	TokenHash  string     `db:"token_hash" json:"-"` // never JSON-marshalled
	Port       *int       `db:"port" json:"port,omitempty"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
	ExpiresAt  *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	RevokedAt  *time.Time `db:"revoked_at" json:"revoked_at,omitempty"`
}

// IsActive reports whether the share is currently usable: not revoked
// and not yet expired. now is provided so tests can pin time.
func (s *ShareLink) IsActive(now time.Time) bool {
	if s.RevokedAt != nil {
		return false
	}
	if s.ExpiresAt != nil && !now.Before(*s.ExpiresAt) {
		return false
	}
	return true
}
