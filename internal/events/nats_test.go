//go:build integration

package events

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

func natsURL() string {
	if v := os.Getenv("NATS_TEST_URL"); v != "" {
		return v
	}
	return "nats://localhost:4222"
}

// TestNATSPublishSubscribe exercises the round trip against a real
// NATS server. Build-tagged so unit pipelines don't need NATS:
//
//	go test -tags=integration ./internal/events/...
func TestNATSPublishSubscribe(t *testing.T) {
	bus, err := NewNATSBus(natsURL(), nil)
	if err != nil {
		t.Skipf("nats unreachable: %v", err)
	}
	defer bus.Close()

	subject := "vajra.test." + time.Now().Format("20060102150405.000")

	var (
		mu       sync.Mutex
		received [][]byte
	)
	if err := bus.Subscribe(subject, func(_ string, payload []byte) {
		mu.Lock()
		received = append(received, append([]byte{}, payload...))
		mu.Unlock()
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := bus.Publish(context.Background(), subject, []byte("hello")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(received)
		mu.Unlock()
		if got >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || string(received[0]) != "hello" {
		t.Fatalf("received = %v, want [hello]", received)
	}
}
