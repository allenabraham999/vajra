package models

import "time"

// Template is an immutable, content-addressable VM image used to launch
// sandboxes. Hash is the SHA256 of the rootfs and uniquely identifies the
// template's contents across nodes.
//
// Public templates are visible to every account in list/get queries but
// remain writable only by their owner — the flag is a read-side ACL, not
// a transfer of ownership.
type Template struct {
	ID           string    `db:"id" json:"id"`
	AccountID    string    `db:"account_id" json:"account_id"`
	Name         string    `db:"name" json:"name"`
	Version      string    `db:"version" json:"version"`
	Hash         string    `db:"hash" json:"hash"`
	RootfsPath   string    `db:"rootfs_path" json:"rootfs_path"`
	KernelPath   string    `db:"kernel_path" json:"kernel_path"`
	SnapshotPath string    `db:"snapshot_path" json:"snapshot_path"`
	Public       bool      `db:"public" json:"public"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}
