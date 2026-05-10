// Package events is the async event bus that decouples agents from
// master. Master subscribes to vajra.* subjects and reacts to whatever
// agents publish (heartbeats, state changes, unhealthy reports);
// agents subscribe to nothing today but the symmetry is left in.
//
// Implementations are picked at startup from NATS_URL: set →
// NATSBus, unset → NoopBus. With NoopBus every Publish/Subscribe is a
// no-op and the existing HTTP heartbeat path stays in charge — full
// backward compatibility.
package events

import "context"

// Subjects we publish on. Centralised so misspellings are caught at
// compile time and operators can grep one file to see the wire surface.
const (
	SubjectSandboxCreated      = "vajra.sandbox.created"
	SubjectSandboxDestroyed    = "vajra.sandbox.destroyed"
	SubjectSandboxStateChanged = "vajra.sandbox.state_changed"
	SubjectNodeHeartbeat       = "vajra.node.heartbeat"
	SubjectNodeRegistered      = "vajra.node.registered"
	SubjectNodeUnhealthy       = "vajra.node.unhealthy"
)

// Handler is invoked by the bus whenever a subscribed subject arrives.
// Implementations must be safe for concurrent use.
type Handler func(subject string, payload []byte)

// EventBus is the narrow interface every consumer depends on.
// Publish is the hot path; Subscribe is called once at startup.
type EventBus interface {
	Publish(ctx context.Context, subject string, payload []byte) error
	Subscribe(subject string, handler Handler) error
	Close() error
}

// SandboxCreatedEvent is the payload for SubjectSandboxCreated.
type SandboxCreatedEvent struct {
	SandboxID  string `json:"sandbox_id"`
	AccountID  string `json:"account_id"`
	NodeID     string `json:"node_id"`
	TemplateID string `json:"template_id"`
	Timestamp  int64  `json:"timestamp"`
}

// SandboxDestroyedEvent is the payload for SubjectSandboxDestroyed.
type SandboxDestroyedEvent struct {
	SandboxID string `json:"sandbox_id"`
	AccountID string `json:"account_id"`
	NodeID    string `json:"node_id"`
	Timestamp int64  `json:"timestamp"`
}

// SandboxStateChangedEvent is the payload for SubjectSandboxStateChanged.
type SandboxStateChangedEvent struct {
	SandboxID string `json:"sandbox_id"`
	AccountID string `json:"account_id"`
	OldState  string `json:"old_state"`
	NewState  string `json:"new_state"`
	Timestamp int64  `json:"timestamp"`
}

// NodeHeartbeatEvent is the payload for SubjectNodeHeartbeat. Same
// shape as the existing HTTP heartbeat body so we can reuse handler
// logic on the receive side.
type NodeHeartbeatEvent struct {
	NodeID       string `json:"node_id"`
	UsedCPU      int    `json:"used_cpu"`
	UsedMemoryMB int    `json:"used_mem_mb"`
	UsedDiskGB   int    `json:"used_disk_gb"`
	SandboxCount int    `json:"sandbox_count"`
	Version      string `json:"version,omitempty"`
	Timestamp    int64  `json:"timestamp"`
}

// NodeRegisteredEvent is the payload for SubjectNodeRegistered.
type NodeRegisteredEvent struct {
	NodeID    string `json:"node_id"`
	Hostname  string `json:"hostname"`
	IP        string `json:"ip"`
	ClusterID string `json:"cluster_id"`
	Timestamp int64  `json:"timestamp"`
}

// NodeUnhealthyEvent is the payload for SubjectNodeUnhealthy.
type NodeUnhealthyEvent struct {
	NodeID    string `json:"node_id"`
	SandboxID string `json:"sandbox_id"`
	Error     string `json:"error"`
	Timestamp int64  `json:"timestamp"`
}
