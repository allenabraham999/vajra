package models

import "time"

// Snapshot is a saved memory + disk state of a sandbox at a point in time.
// StoragePath is local to the node that produced the snapshot.
type Snapshot struct {
	ID          string    `db:"id" json:"id"`
	SandboxID   string    `db:"sandbox_id" json:"sandbox_id"`
	AccountID   string    `db:"account_id" json:"account_id"`
	NodeID      string    `db:"node_id" json:"node_id"`
	StoragePath string    `db:"storage_path" json:"storage_path"`
	SizeBytes   int64     `db:"size_bytes" json:"size_bytes"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}
