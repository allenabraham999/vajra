package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"
)

// NodeState is the lifecycle state of a node in a cluster.
type NodeState string

const (
	NodeStateRegistering    NodeState = "REGISTERING"
	NodeStateActive         NodeState = "ACTIVE"
	NodeStateDraining       NodeState = "DRAINING"
	NodeStateCordoned       NodeState = "CORDONED"
	NodeStateQuarantined    NodeState = "QUARANTINED"
	NodeStateOffline        NodeState = "OFFLINE"
	NodeStateDecommissioned NodeState = "DECOMMISSIONED"
)

// Valid reports whether s is a known NodeState constant.
func (s NodeState) Valid() bool {
	switch s {
	case NodeStateRegistering, NodeStateActive, NodeStateDraining,
		NodeStateCordoned, NodeStateQuarantined, NodeStateOffline,
		NodeStateDecommissioned:
		return true
	}
	return false
}

// Schedulable reports whether the scheduler may place new sandboxes on a
// node in this state. Only ACTIVE nodes accept new work; DRAINING and
// CORDONED nodes keep their existing sandboxes running but take no more.
func (s NodeState) Schedulable() bool { return s == NodeStateActive }

// NodeCapacity is the total physical resources a node has reported.
// Persisted as a JSONB column.
type NodeCapacity struct {
	TotalCPU      int `json:"total_cpu"`
	TotalMemoryMB int `json:"total_memory_mb"`
	TotalDiskGB   int `json:"total_disk_gb"`
}

// Value implements driver.Valuer.
func (c NodeCapacity) Value() (driver.Value, error) { return json.Marshal(c) }

// Scan implements sql.Scanner.
func (c *NodeCapacity) Scan(src any) error { return scanJSON(src, c) }

// NodeUsage is the resources currently allocated to running sandboxes on a
// node. Persisted as a JSONB column.
type NodeUsage struct {
	UsedCPU      int `json:"used_cpu"`
	UsedMemoryMB int `json:"used_memory_mb"`
	UsedDiskGB   int `json:"used_disk_gb"`
}

// Value implements driver.Valuer.
func (u NodeUsage) Value() (driver.Value, error) { return json.Marshal(u) }

// Scan implements sql.Scanner.
func (u *NodeUsage) Scan(src any) error { return scanJSON(src, u) }

// Node represents a bare-metal or VM host that runs sandboxes for a
// cluster. IP is stored as text and validated at API boundaries via
// ValidateNodeIP.
type Node struct {
	ID            string       `db:"id" json:"id"`
	ClusterID     string       `db:"cluster_id" json:"cluster_id"`
	Hostname      string       `db:"hostname" json:"hostname"`
	IP            string       `db:"ip" json:"ip"`
	State         NodeState    `db:"state" json:"state"`
	Capacity      NodeCapacity `db:"capacity" json:"capacity"`
	UsedResources NodeUsage    `db:"used_resources" json:"used_resources"`
	LastHeartbeat time.Time    `db:"last_heartbeat" json:"last_heartbeat"`
}

// ValidateNodeIP parses ip and returns an error if it is not a valid IPv4
// or IPv6 address. Use this at the registration boundary so malformed
// addresses never reach the database.
func ValidateNodeIP(ip string) error {
	if ip == "" {
		return fmt.Errorf("node IP is empty")
	}
	if _, err := netip.ParseAddr(ip); err != nil {
		return fmt.Errorf("invalid node IP %q: %w", ip, err)
	}
	return nil
}
