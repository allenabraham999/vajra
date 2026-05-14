// Package models — webhook.go defines per-account outbound webhook
// targets. The master dispatches signed POSTs to each Active webhook
// whose Events slice contains the firing event name.
package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"
)

// WebhookEvent is the symbolic name of a notification topic. New events
// are added by extending this set and updating the dispatch sites.
type WebhookEvent string

const (
	WebhookEventSandboxCreated   WebhookEvent = "sandbox.created"
	WebhookEventSandboxRunning   WebhookEvent = "sandbox.running"
	WebhookEventSandboxStopped   WebhookEvent = "sandbox.stopped"
	WebhookEventSandboxDestroyed WebhookEvent = "sandbox.destroyed"
	WebhookEventSandboxError     WebhookEvent = "sandbox.error"
	WebhookEventSandboxArchived  WebhookEvent = "sandbox.archived"
)

// AllWebhookEvents is the canonical list every Webhook may subscribe to.
// Kept here so handlers can validate inbound subscription lists without
// hard-coding strings.
var AllWebhookEvents = []WebhookEvent{
	WebhookEventSandboxCreated,
	WebhookEventSandboxRunning,
	WebhookEventSandboxStopped,
	WebhookEventSandboxDestroyed,
	WebhookEventSandboxError,
	WebhookEventSandboxArchived,
}

// ValidWebhookEvent reports whether s is a known WebhookEvent.
func ValidWebhookEvent(s string) bool {
	for _, e := range AllWebhookEvents {
		if string(e) == s {
			return true
		}
	}
	return false
}

// WebhookEvents is a string slice persisted as JSONB. Wrapping it lets us
// reuse one Scan/Value pair for the events column on the webhooks table.
type WebhookEvents []string

// Value implements driver.Valuer.
func (e WebhookEvents) Value() (driver.Value, error) {
	if e == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(e)
}

// Scan implements sql.Scanner for JSONB reads.
func (e *WebhookEvents) Scan(src any) error {
	return scanJSON(src, e)
}

// Has reports whether the slice contains the given event name.
func (e WebhookEvents) Has(name string) bool {
	for _, ev := range e {
		if ev == name {
			return true
		}
	}
	return false
}

// Webhook is the persisted record of a single notification target.
// Secret is the per-webhook HMAC key used to sign outbound payloads —
// it is returned exactly once at creation time and never again, so the
// caller must store it on their side.
type Webhook struct {
	ID        string        `db:"id" json:"id"`
	AccountID string        `db:"account_id" json:"account_id"`
	URL       string        `db:"url" json:"url"`
	Secret    string        `db:"secret" json:"secret,omitempty"`
	Events    WebhookEvents `db:"events" json:"events"`
	Active    bool          `db:"active" json:"active"`
	CreatedAt time.Time     `db:"created_at" json:"created_at"`
}
