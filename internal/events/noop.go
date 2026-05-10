package events

import "context"

// NoopBus is the zero-config EventBus used when NATS_URL is empty.
// Publish does nothing; Subscribe records nothing. Callers wired
// against EventBus stay correct — they just don't see any events.
type NoopBus struct{}

// NewNoopBus returns a NoopBus. Exists for symmetry with NewNATSBus.
func NewNoopBus() *NoopBus { return &NoopBus{} }

// Publish drops the payload on the floor.
func (n *NoopBus) Publish(ctx context.Context, subject string, payload []byte) error {
	return nil
}

// Subscribe ignores the registration.
func (n *NoopBus) Subscribe(subject string, handler Handler) error { return nil }

// Close is a no-op.
func (n *NoopBus) Close() error { return nil }
