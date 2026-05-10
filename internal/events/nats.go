package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// NATSBus is the NATS-backed EventBus. It auto-reconnects with
// exponential backoff and logs every connect/disconnect/reconnect so
// operators can spot a flaky bus. The underlying nats.Conn is goroutine-
// safe so we share one across the whole process.
type NATSBus struct {
	conn   *nats.Conn
	logger *slog.Logger

	mu   sync.Mutex
	subs []*nats.Subscription
}

// NewNATSBus dials natsURL (e.g. "nats://localhost:4222") with sensible
// reconnect defaults. Returns a ready-to-use bus or a wrapped error.
func NewNATSBus(natsURL string, logger *slog.Logger) (*NATSBus, error) {
	if logger == nil {
		logger = slog.Default()
	}
	opts := []nats.Option{
		nats.Name("vajra"),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
		nats.PingInterval(20 * time.Second),
		nats.MaxPingsOutstanding(3),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("nats disconnected", "err", err)
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			logger.Info("nats reconnected", "url", c.ConnectedUrl())
		}),
		nats.ClosedHandler(func(c *nats.Conn) {
			logger.Info("nats connection closed")
		}),
	}
	conn, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	logger.Info("nats connected", "url", conn.ConnectedUrl())
	return &NATSBus{conn: conn, logger: logger}, nil
}

// Publish ships payload on subject. NATS publish is fire-and-forget, so
// the only error path is a closed connection.
func (n *NATSBus) Publish(ctx context.Context, subject string, payload []byte) error {
	if n == nil || n.conn == nil {
		return nil
	}
	if err := n.conn.Publish(subject, payload); err != nil {
		return fmt.Errorf("nats publish %s: %w", subject, err)
	}
	return nil
}

// Subscribe wires handler to subject. The subscription is stored so
// Close can drain it on shutdown. NATS itself dispatches each message
// on a goroutine, so handler must be safe for concurrent use.
func (n *NATSBus) Subscribe(subject string, handler Handler) error {
	if n == nil || n.conn == nil {
		return fmt.Errorf("nats: not connected")
	}
	sub, err := n.conn.Subscribe(subject, func(msg *nats.Msg) {
		handler(msg.Subject, msg.Data)
	})
	if err != nil {
		return fmt.Errorf("nats subscribe %s: %w", subject, err)
	}
	n.mu.Lock()
	n.subs = append(n.subs, sub)
	n.mu.Unlock()
	return nil
}

// Close drains every subscription and tears the connection down. Safe
// to call multiple times.
func (n *NATSBus) Close() error {
	if n == nil || n.conn == nil {
		return nil
	}
	n.mu.Lock()
	subs := n.subs
	n.subs = nil
	n.mu.Unlock()
	for _, s := range subs {
		_ = s.Drain()
	}
	n.conn.Close()
	return nil
}
