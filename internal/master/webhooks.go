// Package master — webhooks.go: per-account outbound notifications.
//
// WebhookManager dispatches a typed event payload to every active
// webhook subscribed to that event. Each delivery is signed with
// HMAC-SHA256 over the request body using the webhook's per-row secret;
// the digest is sent in X-Vajra-Signature so the receiver can verify
// authenticity. Failures are retried up to 3 times with exponential
// backoff (1s → 2s → 4s) before giving up.
package master

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// SignatureHeader is the HTTP header carrying the HMAC-SHA256 digest.
const SignatureHeader = "X-Vajra-Signature"

// EventHeader names the firing event in plain text. Receivers can route
// on this without parsing the body.
const EventHeader = "X-Vajra-Event"

// DeliveryIDHeader carries a unique identifier per delivery attempt —
// useful for receiver-side idempotency / dedupe.
const DeliveryIDHeader = "X-Vajra-Delivery-Id"

// maxWebhookRetries bounds the number of delivery attempts per webhook
// per event. The brief asks for 3 retries; the first attempt plus 3
// retries = 4 total deliveries.
const maxWebhookRetries = 3

// webhookHTTPTimeout caps each individual delivery attempt. Receivers
// that hang would otherwise pin a goroutine indefinitely.
const webhookHTTPTimeout = 10 * time.Second

// WebhookManager owns the dispatch + retry pipeline.
type WebhookManager struct {
	store  store.Store
	http   *http.Client
	logger *slog.Logger
	now    func() time.Time

	// backoff is the per-attempt sleep schedule. Indexed by attempt
	// (0 = before first retry). Overridable for tests to skip the
	// production exponential schedule.
	backoff []time.Duration

	inflight sync.WaitGroup
}

// NewWebhookManager wires a manager. Pass nil logger for slog default.
func NewWebhookManager(st store.Store, lg *slog.Logger) *WebhookManager {
	if lg == nil {
		lg = slog.Default()
	}
	return &WebhookManager{
		store:   st,
		http:    &http.Client{Timeout: webhookHTTPTimeout},
		logger:  lg,
		now:     time.Now,
		backoff: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
	}
}

// HTTPDoer is the minimal interface the manager needs from net/http.
// Exposed so tests can drop in a recorder without spinning a real
// listener.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// WithHTTPClient overrides the default *http.Client. Tests use this to
// install an httptest.Server.Client or a counter wrapper.
func (m *WebhookManager) WithHTTPClient(c *http.Client) *WebhookManager {
	m.http = c
	return m
}

// WithBackoff overrides the retry schedule. Tests pass tight intervals
// so the table stays under a few hundred milliseconds.
func (m *WebhookManager) WithBackoff(d []time.Duration) *WebhookManager {
	m.backoff = d
	return m
}

// Dispatch fans the event out to every active subscriber, asynchronously.
// Returns the number of deliveries kicked off (not the number that
// succeeded). Best-effort: a failed lookup is logged and swallowed so
// publish-site callers never block on the bus.
func (m *WebhookManager) Dispatch(ctx context.Context, accountID, event string, payload any) int {
	if !models.ValidWebhookEvent(event) {
		m.logger.Warn("webhook: unknown event", "event", event)
		return 0
	}
	hooks, err := m.store.Webhooks().ListActiveByEvent(ctx, accountID, event)
	if err != nil {
		m.logger.Error("webhook: list active", "err", err, "account_id", accountID, "event", event)
		return 0
	}
	if len(hooks) == 0 {
		return 0
	}
	body, err := json.Marshal(map[string]any{
		"event":      event,
		"account_id": accountID,
		"timestamp":  m.now().UTC().Format(time.RFC3339Nano),
		"data":       payload,
	})
	if err != nil {
		m.logger.Error("webhook: marshal payload", "err", err)
		return 0
	}
	for _, w := range hooks {
		m.inflight.Add(1)
		go func(hook *models.Webhook) {
			defer m.inflight.Done()
			m.deliver(hook, event, body)
		}(w)
	}
	return len(hooks)
}

// DispatchSync delivers inline rather than via goroutines. Tests use
// this to assert on outcomes without orchestrating WaitGroups.
func (m *WebhookManager) DispatchSync(ctx context.Context, accountID, event string, payload any) (int, error) {
	if !models.ValidWebhookEvent(event) {
		return 0, fmt.Errorf("unknown event %q", event)
	}
	hooks, err := m.store.Webhooks().ListActiveByEvent(ctx, accountID, event)
	if err != nil {
		return 0, err
	}
	body, err := json.Marshal(map[string]any{
		"event":      event,
		"account_id": accountID,
		"timestamp":  m.now().UTC().Format(time.RFC3339Nano),
		"data":       payload,
	})
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, w := range hooks {
		if m.deliver(w, event, body) {
			sent++
		}
	}
	return sent, nil
}

// DeliverOne delivers a single payload to a specific webhook. Used by
// the test-fire endpoint (POST /v1/webhooks/{id}/test).
func (m *WebhookManager) DeliverOne(w *models.Webhook, event string, payload any) (bool, error) {
	body, err := json.Marshal(map[string]any{
		"event":      event,
		"account_id": w.AccountID,
		"timestamp":  m.now().UTC().Format(time.RFC3339Nano),
		"data":       payload,
	})
	if err != nil {
		return false, err
	}
	return m.deliver(w, event, body), nil
}

// Wait blocks until every in-flight delivery finishes. Used during
// shutdown so payloads aren't dropped mid-flight.
func (m *WebhookManager) Wait() { m.inflight.Wait() }

// deliver runs the attempt loop for one webhook. Returns true on a
// 2xx from any attempt.
func (m *WebhookManager) deliver(w *models.Webhook, event string, body []byte) bool {
	sig := signBody(w.Secret, body)
	deliveryID, _ := randomHex(8)
	for attempt := 0; attempt <= maxWebhookRetries; attempt++ {
		if attempt > 0 {
			d := m.attemptDelay(attempt - 1)
			time.Sleep(d)
		}
		req, err := http.NewRequest(http.MethodPost, w.URL, bytes.NewReader(body))
		if err != nil {
			m.logger.Error("webhook: build request", "err", err, "webhook_id", w.ID)
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(SignatureHeader, "sha256="+sig)
		req.Header.Set(EventHeader, event)
		req.Header.Set(DeliveryIDHeader, deliveryID)
		req.Header.Set("User-Agent", "vajra-webhooks/1.0")
		resp, err := m.http.Do(req)
		if err != nil {
			m.logger.Warn("webhook: transport failed",
				"err", err, "webhook_id", w.ID, "attempt", attempt+1)
			continue
		}
		// Drain so the connection can be reused; cap the read so a
		// pathological receiver can't OOM us.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			m.logger.Info("webhook: delivered",
				"webhook_id", w.ID, "event", event, "status", resp.StatusCode, "attempt", attempt+1)
			return true
		}
		m.logger.Warn("webhook: non-2xx",
			"webhook_id", w.ID, "event", event, "status", resp.StatusCode, "attempt", attempt+1)
	}
	m.logger.Error("webhook: gave up after retries",
		"webhook_id", w.ID, "event", event, "attempts", maxWebhookRetries+1)
	return false
}

// attemptDelay returns the configured backoff for the given retry. Beyond
// the configured slice we reuse the last entry; the loop bounds attempts
// at maxWebhookRetries anyway.
func (m *WebhookManager) attemptDelay(retry int) time.Duration {
	if len(m.backoff) == 0 {
		return 0
	}
	if retry >= len(m.backoff) {
		return m.backoff[len(m.backoff)-1]
	}
	return m.backoff[retry]
}

// signBody returns the lowercase-hex HMAC-SHA256 of body under secret.
// The literal "sha256=" prefix is added by the caller so a future
// versioning of the signature is straightforward.
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature returns true iff sig (without prefix) matches the
// computed HMAC. Provided so SDK/tests can verify the same way
// receivers will.
func VerifySignature(secret string, body []byte, sig string) bool {
	expected := signBody(secret, body)
	return hmac.Equal([]byte(expected), []byte(sig))
}
