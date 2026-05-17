package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"
)

// Account is a customer of the platform. PasswordHash is never serialized.
// CreditsRemaining is the prepaid USD balance the billing meter decrements
// every tick; new accounts start with the demo grant (DB column default).
//
// IsAdmin, Suspended and LastLogin back the cluster admin panel. IsAdmin is
// one of two ways an account reaches the /v1/admin/* surface — the other is
// having its email in the VAJRA_ADMIN_EMAIL list — and the login handler
// back-fills this column for env-listed admins so the Accounts tab is
// accurate. LastLogin is nil for an account that has never logged in.
type Account struct {
	ID               string     `db:"id" json:"id"`
	Email            string     `db:"email" json:"email"`
	PasswordHash     string     `db:"password_hash" json:"-"`
	CreatedAt        time.Time  `db:"created_at" json:"created_at"`
	CreditsRemaining float64    `db:"credits_remaining" json:"credits_remaining"`
	IsAdmin          bool       `db:"is_admin" json:"is_admin"`
	Suspended        bool       `db:"suspended" json:"suspended"`
	LastLogin        *time.Time `db:"last_login" json:"last_login,omitempty"`
}

// Permissions is a list of permission strings granted to an APIKey.
// Persisted as a JSONB column so we don't pin the DB driver to one of the
// many incompatible Postgres array packages (lib/pq StringArray vs
// pgx pgtype.TextArray vs raw text[]).
type Permissions []string

// Value implements driver.Valuer.
func (p Permissions) Value() (driver.Value, error) {
	if p == nil {
		return json.Marshal([]string{})
	}
	return json.Marshal(p)
}

// Scan implements sql.Scanner.
func (p *Permissions) Scan(src any) error { return scanJSON(src, p) }

// APIKey is a long-lived credential used by SDKs and automation to
// authenticate to the API. KeyHash stores a hash of the secret, not the
// secret itself. ExpiresAt is nil for keys that never expire.
type APIKey struct {
	ID          string      `db:"id" json:"id"`
	AccountID   string      `db:"account_id" json:"account_id"`
	KeyHash     string      `db:"key_hash" json:"-"`
	Name        string      `db:"name" json:"name"`
	Permissions Permissions `db:"permissions" json:"permissions"`
	CreatedAt   time.Time   `db:"created_at" json:"created_at"`
	ExpiresAt   *time.Time  `db:"expires_at" json:"expires_at,omitempty"`
}
