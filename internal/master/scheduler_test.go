package master

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

func TestPickCluster(t *testing.T) {
	tests := []struct {
		name       string
		clusters   []*models.Cluster
		region     string
		wantID     string
		wantErr    error
	}{
		{
			name:     "single active cluster, no region",
			clusters: []*models.Cluster{mkCluster("c1", "us-east", models.ClusterStateActive)},
			wantID:   "c1",
		},
		{
			name: "two active clusters, region match wins over lex order",
			clusters: []*models.Cluster{
				mkCluster("c1", "us-east", models.ClusterStateActive),
				mkCluster("c2", "us-west", models.ClusterStateActive),
			},
			region: "us-west",
			wantID: "c2",
		},
		{
			name: "no active cluster",
			clusters: []*models.Cluster{
				mkCluster("c1", "us-east", models.ClusterStateOffline),
			},
			wantErr: ErrNoCluster,
		},
		{
			name: "region requested but only other regions active",
			clusters: []*models.Cluster{
				mkCluster("c1", "us-east", models.ClusterStateActive),
			},
			region:  "us-west",
			wantErr: ErrNoCluster,
		},
		{
			name: "region empty falls through to any active cluster",
			clusters: []*models.Cluster{
				mkCluster("c1", "us-east", models.ClusterStateDraining),
				mkCluster("c2", "us-west", models.ClusterStateActive),
			},
			wantID: "c2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			fs.clusters.list = tt.clusters
			s := newSchedulerForTest(fs, nil)
			got, err := s.PickCluster(context.Background(), SchedRequest{Region: tt.region})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.ID != tt.wantID {
				t.Fatalf("cluster ID = %s, want %s", got.ID, tt.wantID)
			}
		})
	}
}

func TestPickNode(t *testing.T) {
	freshBeat := fixedNow.Add(-30 * time.Second) // within 90s window
	staleBeat := fixedNow.Add(-2 * time.Minute)  // outside 90s window

	t.Run("single node fits", func(t *testing.T) {
		fs := newFakeStore()
		fs.nodes.byCluster = map[string][]*models.Node{
			"c1": {mkNode("n1", "c1", models.NodeStateActive, freshBeat, 8, 16384, 200, 0, 0, 0)},
		}
		s := newSchedulerForTest(fs, nil)
		n, err := s.PickNode(context.Background(), mkCluster("c1", "us", models.ClusterStateActive),
			SchedRequest{VCPUs: 2, MemoryMB: 1024, DiskGB: 10})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if n.ID != "n1" {
			t.Fatalf("got %s, want n1", n.ID)
		}
	})

	// Brief says "higher score wins" — score is the sum of remaining
	// resources after placement. This test pins that contract: the
	// emptier node (n2) wins over the more-loaded one (n1).
	t.Run("higher score (more remaining) wins", func(t *testing.T) {
		fs := newFakeStore()
		fs.nodes.byCluster = map[string][]*models.Node{
			"c1": {
				// n1: 8 CPU total, 6 used → 2 free; tighter fit (low score).
				mkNode("n1", "c1", models.NodeStateActive, freshBeat, 8, 16384, 200, 6, 12288, 100),
				// n2: 8 CPU total, 0 used → 8 free; loose fit (high score).
				mkNode("n2", "c1", models.NodeStateActive, freshBeat, 8, 16384, 200, 0, 0, 0),
			},
		}
		s := newSchedulerForTest(fs, nil)
		n, err := s.PickNode(context.Background(), mkCluster("c1", "us", models.ClusterStateActive),
			SchedRequest{VCPUs: 2, MemoryMB: 1024, DiskGB: 10})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if n.ID != "n2" {
			t.Fatalf("expected the emptier node n2 (worst-fit / highest score) to win, got %s", n.ID)
		}
	})

	t.Run("all nodes stale heartbeat -> ErrNoCapacity", func(t *testing.T) {
		fs := newFakeStore()
		fs.nodes.byCluster = map[string][]*models.Node{
			"c1": {mkNode("n1", "c1", models.NodeStateActive, staleBeat, 8, 16384, 200, 0, 0, 0)},
		}
		s := newSchedulerForTest(fs, nil)
		_, err := s.PickNode(context.Background(), mkCluster("c1", "us", models.ClusterStateActive),
			SchedRequest{VCPUs: 2, MemoryMB: 1024, DiskGB: 10})
		if !errors.Is(err, ErrNoCapacity) {
			t.Fatalf("err = %v, want ErrNoCapacity", err)
		}
	})

	t.Run("no node has enough CPU", func(t *testing.T) {
		fs := newFakeStore()
		fs.nodes.byCluster = map[string][]*models.Node{
			"c1": {mkNode("n1", "c1", models.NodeStateActive, freshBeat, 4, 16384, 200, 4, 0, 0)},
		}
		s := newSchedulerForTest(fs, nil)
		_, err := s.PickNode(context.Background(), mkCluster("c1", "us", models.ClusterStateActive),
			SchedRequest{VCPUs: 2, MemoryMB: 1024, DiskGB: 10})
		if !errors.Is(err, ErrNoCapacity) {
			t.Fatalf("err = %v, want ErrNoCapacity", err)
		}
	})

	t.Run("non-active nodes filtered out", func(t *testing.T) {
		fs := newFakeStore()
		fs.nodes.byCluster = map[string][]*models.Node{
			"c1": {mkNode("n1", "c1", models.NodeStateDraining, freshBeat, 8, 16384, 200, 0, 0, 0)},
		}
		s := newSchedulerForTest(fs, nil)
		_, err := s.PickNode(context.Background(), mkCluster("c1", "us", models.ClusterStateActive),
			SchedRequest{VCPUs: 2, MemoryMB: 1024, DiskGB: 10})
		if !errors.Is(err, ErrNoCapacity) {
			t.Fatalf("err = %v, want ErrNoCapacity", err)
		}
	})

	t.Run("score tie broken by lex ID", func(t *testing.T) {
		fs := newFakeStore()
		// Two identical nodes — n1 should win by lex order.
		fs.nodes.byCluster = map[string][]*models.Node{
			"c1": {
				mkNode("n2", "c1", models.NodeStateActive, freshBeat, 8, 16384, 200, 0, 0, 0),
				mkNode("n1", "c1", models.NodeStateActive, freshBeat, 8, 16384, 200, 0, 0, 0),
			},
		}
		s := newSchedulerForTest(fs, nil)
		n, err := s.PickNode(context.Background(), mkCluster("c1", "us", models.ClusterStateActive),
			SchedRequest{VCPUs: 2, MemoryMB: 1024, DiskGB: 10})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if n.ID != "n1" {
			t.Fatalf("expected lex tie-break to pick n1, got %s", n.ID)
		}
	})
}

func TestCheckQuota(t *testing.T) {
	tightQuota := func(string) Quota {
		return Quota{MaxSandboxes: 2, MaxVCPUs: 4, MaxMemoryMB: 4096}
	}

	t.Run("at sandbox count -> exceeded", func(t *testing.T) {
		fs := newFakeStore()
		fs.sandboxes.byAccount = map[string][]*models.Sandbox{
			"acct": {
				mkSandbox("s1", "acct", models.SandboxStateRunning, 1, 512),
				mkSandbox("s2", "acct", models.SandboxStateRunning, 1, 512),
			},
		}
		s := newSchedulerForTest(fs, tightQuota)
		err := s.CheckQuota(context.Background(), "acct", SchedRequest{VCPUs: 1, MemoryMB: 256})
		if !errors.Is(err, ErrQuotaExceeded) {
			t.Fatalf("err = %v, want ErrQuotaExceeded", err)
		}
	})

	t.Run("vCPU sum exceeded", func(t *testing.T) {
		fs := newFakeStore()
		fs.sandboxes.byAccount = map[string][]*models.Sandbox{
			"acct": {
				mkSandbox("s1", "acct", models.SandboxStateRunning, 3, 512),
			},
		}
		s := newSchedulerForTest(fs, tightQuota)
		err := s.CheckQuota(context.Background(), "acct", SchedRequest{VCPUs: 2, MemoryMB: 256})
		if !errors.Is(err, ErrQuotaExceeded) {
			t.Fatalf("err = %v, want ErrQuotaExceeded (vCPU sum)", err)
		}
	})

	t.Run("memory sum exceeded", func(t *testing.T) {
		fs := newFakeStore()
		fs.sandboxes.byAccount = map[string][]*models.Sandbox{
			"acct": {
				mkSandbox("s1", "acct", models.SandboxStateRunning, 1, 3000),
			},
		}
		s := newSchedulerForTest(fs, tightQuota)
		err := s.CheckQuota(context.Background(), "acct", SchedRequest{VCPUs: 1, MemoryMB: 2000})
		if !errors.Is(err, ErrQuotaExceeded) {
			t.Fatalf("err = %v, want ErrQuotaExceeded (memory)", err)
		}
	})

	t.Run("destroyed sandboxes don't count", func(t *testing.T) {
		fs := newFakeStore()
		fs.sandboxes.byAccount = map[string][]*models.Sandbox{
			"acct": {
				mkSandbox("s1", "acct", models.SandboxStateDestroyed, 100, 100000),
				mkSandbox("s2", "acct", models.SandboxStateError, 100, 100000),
			},
		}
		s := newSchedulerForTest(fs, tightQuota)
		if err := s.CheckQuota(context.Background(), "acct", SchedRequest{VCPUs: 1, MemoryMB: 256}); err != nil {
			t.Fatalf("destroyed/error sandboxes should not block; got %v", err)
		}
	})

	t.Run("nil quotaProvider falls back to DefaultQuota", func(t *testing.T) {
		fs := newFakeStore()
		s := newSchedulerForTest(fs, nil)
		if err := s.CheckQuota(context.Background(), "acct", SchedRequest{VCPUs: 1, MemoryMB: 256}); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
	})
}

func TestSchedule_RegionMismatch(t *testing.T) {
	fs := newFakeStore()
	fs.clusters.list = []*models.Cluster{mkCluster("c1", "us-east", models.ClusterStateActive)}
	s := newSchedulerForTest(fs, nil)
	_, _, err := s.Schedule(context.Background(), SchedRequest{
		AccountID: "acct", Region: "us-west", VCPUs: 1, MemoryMB: 256, DiskGB: 1,
	})
	if !errors.Is(err, ErrNoCluster) {
		t.Fatalf("err = %v, want ErrNoCluster", err)
	}
}

func TestSchedule_HappyPath(t *testing.T) {
	fs := newFakeStore()
	fs.clusters.list = []*models.Cluster{mkCluster("c1", "us-east", models.ClusterStateActive)}
	fresh := fixedNow.Add(-30 * time.Second)
	fs.nodes.byCluster = map[string][]*models.Node{
		"c1": {mkNode("n1", "c1", models.NodeStateActive, fresh, 8, 16384, 200, 0, 0, 0)},
	}
	s := newSchedulerForTest(fs, nil)
	cluster, node, err := s.Schedule(context.Background(), SchedRequest{
		AccountID: "acct", VCPUs: 2, MemoryMB: 1024, DiskGB: 10,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cluster.ID != "c1" || node.ID != "n1" {
		t.Fatalf("got cluster=%s node=%s", cluster.ID, node.ID)
	}
}
