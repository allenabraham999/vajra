package models

import "time"

// ClusterState is the lifecycle state of a cluster.
type ClusterState string

const (
	ClusterStateActive   ClusterState = "ACTIVE"
	ClusterStateDraining ClusterState = "DRAINING"
	ClusterStateOffline  ClusterState = "OFFLINE"
)

// Valid reports whether s is a known ClusterState constant.
func (s ClusterState) Valid() bool {
	switch s {
	case ClusterStateActive, ClusterStateDraining, ClusterStateOffline:
		return true
	}
	return false
}

// Cluster is a logical group of nodes, typically pinned to a region or AZ.
// The two-tier scheduler picks a cluster first, then a node within it.
type Cluster struct {
	ID        string       `db:"id" json:"id"`
	Name      string       `db:"name" json:"name"`
	Region    string       `db:"region" json:"region"`
	State     ClusterState `db:"state" json:"state"`
	CreatedAt time.Time    `db:"created_at" json:"created_at"`
}
