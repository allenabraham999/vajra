package events

import (
	"context"
	"testing"
)

// TestNoopBusContract confirms NoopBus.Subscribe never delivers, but
// also never errors — the back-compat guarantee.
func TestNoopBusContract(t *testing.T) {
	b := NewNoopBus()
	called := false
	if err := b.Subscribe("vajra.test", func(string, []byte) { called = true }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := b.Publish(context.Background(), "vajra.test", []byte("hi")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if called {
		t.Fatal("noop bus must not deliver to subscribers")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
