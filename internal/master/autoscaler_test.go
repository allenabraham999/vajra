package master

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/allenabraham999/vajra/internal/cache"
	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// fakeEC2 is the test double for the autoscaler's ec2 dependency. It
// records every call so assertions can verify side-effects without
// touching real AWS.
type fakeEC2 struct {
	mu sync.Mutex

	runErr        error
	terminateErr  error
	describeErr   error
	runCalls      int32
	terminateIDs  []string
	describeCalls int32

	managed map[string]string // ip → instanceID for DescribeInstances
}

func newFakeEC2() *fakeEC2 {
	return &fakeEC2{managed: map[string]string{}}
}

func (f *fakeEC2) RunInstances(_ context.Context, _ *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	atomic.AddInt32(&f.runCalls, 1)
	if f.runErr != nil {
		return nil, f.runErr
	}
	id := "i-fake-" + time.Now().Format("150405.000000")
	return &ec2.RunInstancesOutput{
		Instances: []ec2types.Instance{{InstanceId: aws.String(id)}},
	}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	if f.terminateErr != nil {
		return nil, f.terminateErr
	}
	f.mu.Lock()
	f.terminateIDs = append(f.terminateIDs, in.InstanceIds...)
	f.mu.Unlock()
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	atomic.AddInt32(&f.describeCalls, 1)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	out := &ec2.DescribeInstancesOutput{}
	r := ec2types.Reservation{}
	for ip, id := range f.managed {
		ip, id := ip, id
		r.Instances = append(r.Instances, ec2types.Instance{
			InstanceId:       aws.String(id),
			PrivateIpAddress: aws.String(ip),
		})
	}
	out.Reservations = []ec2types.Reservation{r}
	return out, nil
}

// asMaster returns a logger that swallows everything; tests don't read
// log output and a nil logger crashes Slog.
func asMaster() *slog.Logger {
	return slog.New(slog.NewTextHandler(noopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// newTestStore returns an empty handlerStore with a single ACTIVE
// cluster for the tests to schedule against.
func newTestStore(t *testing.T) *handlerStore {
	t.Helper()
	st := newHandlerStore()
	_ = st.Clusters().Create(context.Background(), &models.Cluster{
		ID: "c1", Region: "us-east-1", State: models.ClusterStateActive,
	})
	return st
}

// stubScheduler is a Scheduler whose Schedule returns ErrNoCapacity by
// default; used so HandleNoCapacity has something to call.
type stubScheduler struct{}

func (stubScheduler) Schedule(context.Context, SchedRequest) (*models.Cluster, *models.Node, error) {
	return nil, nil, ErrNoCapacity
}
func (stubScheduler) PickCluster(context.Context, SchedRequest) (*models.Cluster, error) {
	return nil, ErrNoCapacity
}
func (stubScheduler) PickNode(context.Context, *models.Cluster, SchedRequest) (*models.Node, error) {
	return nil, ErrNoCapacity
}
func (stubScheduler) CheckQuota(context.Context, string, SchedRequest) error { return nil }

// TestScaleUpOnNoCapacity: enable, queue, register a fresh node mid-
// flight, verify the waiter unblocks with the new node ID and exactly
// one EC2 launch happened.
func TestScaleUpOnNoCapacity(t *testing.T) {
	st := newTestStore(t)
	fec2 := newFakeEC2()
	cfg := AutoscaleConfig{Enabled: true, AMI: "ami-test", InstanceType: "c5.large", Region: "us-east-1"}
	a := NewAutoscaler(cfg, fec2, st, cache.NewNoopCache(), stubScheduler{}, asMaster())

	// Background goroutine: simulate the new agent registering after a
	// short delay. The autoscaler's wait loop sees a fresh node and
	// returns its ID.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = st.Nodes().Create(context.Background(), &models.Node{
			ID: "n-new", ClusterID: "c1", State: models.NodeStateActive,
			LastHeartbeat: time.Now().UTC(),
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := a.HandleNoCapacity(ctx, createSandboxRequest{Name: "x", Source: "image", VCPUs: 1, MemoryMB: 256, DiskGB: 1, TemplateID: "t1"}, "acc1")
	if err != nil {
		t.Fatalf("HandleNoCapacity: %v", err)
	}
	if id != "n-new" {
		t.Fatalf("got node %q, want n-new", id)
	}
	if got := atomic.LoadInt32(&fec2.runCalls); got != 1 {
		t.Fatalf("runInstances called %d times, want 1", got)
	}
}

// TestMaxNodesLimit: when the node count is already at MaxNodes,
// HandleNoCapacity must short-circuit without ever calling EC2.
func TestMaxNodesLimit(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < 3; i++ {
		_ = st.Nodes().Create(context.Background(), &models.Node{
			ID: "n" + string(rune('0'+i)), ClusterID: "c1", State: models.NodeStateActive,
		})
	}
	fec2 := newFakeEC2()
	cfg := AutoscaleConfig{Enabled: true, MaxNodes: 3, AMI: "ami-test"}
	a := NewAutoscaler(cfg, fec2, st, cache.NewNoopCache(), stubScheduler{}, asMaster())

	_, err := a.HandleNoCapacity(context.Background(),
		createSandboxRequest{Name: "x", Source: "image", VCPUs: 1, MemoryMB: 256, DiskGB: 1, TemplateID: "t1"}, "acc1")
	if err == nil {
		t.Fatal("expected error at max nodes, got nil")
	}
	if atomic.LoadInt32(&fec2.runCalls) != 0 {
		t.Fatalf("EC2 launched while at max — runCalls=%d", fec2.runCalls)
	}
}

// TestDuplicateScaleUp: many concurrent HandleNoCapacity calls must
// fan into a single EC2 launch (the queue is what amortises bursts).
func TestDuplicateScaleUp(t *testing.T) {
	st := newTestStore(t)
	fec2 := newFakeEC2()
	cfg := AutoscaleConfig{Enabled: true, AMI: "ami-test"}
	a := NewAutoscaler(cfg, fec2, st, cache.NewNoopCache(), stubScheduler{}, asMaster())

	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = st.Nodes().Create(context.Background(), &models.Node{
			ID: "n-fresh", ClusterID: "c1", State: models.NodeStateActive,
			LastHeartbeat: time.Now().UTC(),
		})
	}()

	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, errs[i] = a.HandleNoCapacity(ctx,
				createSandboxRequest{Name: "x", Source: "image", VCPUs: 1, MemoryMB: 256, DiskGB: 1, TemplateID: "t1"}, "acc1")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("waiter %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&fec2.runCalls); got != 1 {
		t.Fatalf("runInstances called %d times, want 1 (deduped)", got)
	}
}

// TestScaleDownIdleNode: an idle managed node past cooldown must be
// terminated. Postgres delete is best-effort in our fake but the EC2
// terminate call is the assertion.
func TestScaleDownIdleNode(t *testing.T) {
	st := newTestStore(t)
	fec2 := newFakeEC2()

	// Two nodes — one idle past cooldown, one fresh.
	idleAt := time.Now().Add(-30 * time.Minute)
	_ = st.Nodes().Create(context.Background(), &models.Node{
		ID: "n-idle", ClusterID: "c1", IP: "10.0.0.10",
		State: models.NodeStateActive, LastHeartbeat: idleAt,
	})
	_ = st.Nodes().Create(context.Background(), &models.Node{
		ID: "n-fresh", ClusterID: "c1", IP: "10.0.0.11",
		State: models.NodeStateActive, LastHeartbeat: time.Now(),
	})
	fec2.managed["10.0.0.10"] = "i-idle"
	fec2.managed["10.0.0.11"] = "i-fresh"

	cfg := AutoscaleConfig{Enabled: true, MinNodes: 1, CooldownMins: 15}
	a := NewAutoscaler(cfg, fec2, st, cache.NewNoopCache(), stubScheduler{}, asMaster())

	a.scaleDown(context.Background())

	fec2.mu.Lock()
	defer fec2.mu.Unlock()
	if len(fec2.terminateIDs) != 1 || fec2.terminateIDs[0] != "i-idle" {
		t.Fatalf("terminateIDs = %v, want [i-idle]", fec2.terminateIDs)
	}
}

// TestScaleDownProtectsMinNodes: at MinNodes, scaleDown must not
// terminate even an idle node.
func TestScaleDownProtectsMinNodes(t *testing.T) {
	st := newTestStore(t)
	fec2 := newFakeEC2()
	idleAt := time.Now().Add(-30 * time.Minute)
	_ = st.Nodes().Create(context.Background(), &models.Node{
		ID: "n-idle", ClusterID: "c1", IP: "10.0.0.10",
		State: models.NodeStateActive, LastHeartbeat: idleAt,
	})
	fec2.managed["10.0.0.10"] = "i-idle"

	cfg := AutoscaleConfig{Enabled: true, MinNodes: 1, CooldownMins: 15}
	a := NewAutoscaler(cfg, fec2, st, cache.NewNoopCache(), stubScheduler{}, asMaster())

	a.scaleDown(context.Background())
	if len(fec2.terminateIDs) != 0 {
		t.Fatalf("min-node protection failed; terminated %v", fec2.terminateIDs)
	}
}

// TestScaleDownSkipsUnmanagedNodes: a node not in the EC2 managed-tag
// listing must never be terminated, even when long idle.
func TestScaleDownSkipsUnmanagedNodes(t *testing.T) {
	st := newTestStore(t)
	fec2 := newFakeEC2()
	idleAt := time.Now().Add(-30 * time.Minute)
	_ = st.Nodes().Create(context.Background(), &models.Node{
		ID: "n-bare-metal", ClusterID: "c1", IP: "192.168.0.5",
		State: models.NodeStateActive, LastHeartbeat: idleAt,
	})
	_ = st.Nodes().Create(context.Background(), &models.Node{
		ID: "n-managed", ClusterID: "c1", IP: "10.0.0.10",
		State: models.NodeStateActive, LastHeartbeat: idleAt,
	})
	// Only the managed one shows up in EC2's tag-filtered describe.
	fec2.managed["10.0.0.10"] = "i-managed"

	cfg := AutoscaleConfig{Enabled: true, MinNodes: 1, CooldownMins: 15}
	a := NewAutoscaler(cfg, fec2, st, cache.NewNoopCache(), stubScheduler{}, asMaster())
	a.scaleDown(context.Background())

	fec2.mu.Lock()
	defer fec2.mu.Unlock()
	if len(fec2.terminateIDs) != 1 || fec2.terminateIDs[0] != "i-managed" {
		t.Fatalf("terminateIDs = %v, want [i-managed]", fec2.terminateIDs)
	}
}

// TestHandleNoCapacityDisabled: when Enabled=false, HandleNoCapacity
// must return ErrNoCapacity untouched and never reach EC2.
func TestHandleNoCapacityDisabled(t *testing.T) {
	st := newTestStore(t)
	fec2 := newFakeEC2()
	a := NewAutoscaler(AutoscaleConfig{Enabled: false}, fec2, st, cache.NewNoopCache(), stubScheduler{}, asMaster())
	_, err := a.HandleNoCapacity(context.Background(),
		createSandboxRequest{Name: "x"}, "acc1")
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("want ErrNoCapacity passthrough, got %v", err)
	}
	if atomic.LoadInt32(&fec2.runCalls) != 0 {
		t.Fatalf("disabled autoscaler called EC2 — runCalls=%d", fec2.runCalls)
	}
}

// TestInstanceTypeForResources locks the ladder mapping from the brief
// so a future edit can't silently shrink what we'll launch.
func TestInstanceTypeForResources(t *testing.T) {
	cases := []struct {
		vcpu, mem int
		want      string
	}{
		{1, 1024, "c8i.large"},
		{2, 2048, "c8i.large"},
		{2, 4096, "c8i.large"},
		{3, 4097, "c8i.xlarge"},
		{4, 8192, "c8i.xlarge"},
		{5, 8192, "c8i.2xlarge"},
		{8, 16384, "c8i.2xlarge"},
		{9, 16384, "c8i.4xlarge"},
		{16, 32768, "c8i.4xlarge"},
		{17, 1024, ""},        // exceeds ceiling on vCPU
		{1, 32769, ""},        // exceeds ceiling on memory
		{128, 256 * 1024, ""}, // way past the ceiling
	}
	for _, c := range cases {
		got := instanceTypeForResources(c.vcpu, c.mem)
		if got != c.want {
			t.Errorf("instanceTypeForResources(%d, %d) = %q, want %q",
				c.vcpu, c.mem, got, c.want)
		}
	}
}

// TestExceedsAnyNodeCapacity verifies the handler-side check the brief
// asks for: 128 vCPU is rejected up front, whereas anything inside the
// ladder ceiling is allowed.
func TestExceedsAnyNodeCapacity(t *testing.T) {
	if ExceedsAnyNodeCapacity(16, 32*1024) {
		t.Fatal("ladder ceiling reported as exceeding capacity")
	}
	if !ExceedsAnyNodeCapacity(128, 1024) {
		t.Fatal("128 vCPU request should be flagged as oversize")
	}
	if !ExceedsAnyNodeCapacity(2, 256*1024) {
		t.Fatal("256 GB request should be flagged as oversize")
	}
}

// TestPickInstanceTypeFromQueue covers the resource-aware ladder pick:
// the autoscaler must launch a node large enough for the biggest
// queued waiter, not just the most recent one.
func TestPickInstanceTypeFromQueue(t *testing.T) {
	a := &Autoscaler{Config: AutoscaleConfig{}, logger: asMaster(), now: time.Now}
	a.pendingQueue = []*PendingCreate{
		{Request: createSandboxRequest{VCPUs: 2, MemoryMB: 2 * 1024}},
		{Request: createSandboxRequest{VCPUs: 8, MemoryMB: 8 * 1024}},
		{Request: createSandboxRequest{VCPUs: 1, MemoryMB: 512}},
	}
	if got := a.pickInstanceType(); got != "c8i.2xlarge" {
		t.Fatalf("biggest waiter is 8/8 — want c8i.2xlarge, got %q", got)
	}

	a.Config.InstanceType = "c5.large"
	if got := a.pickInstanceType(); got != "c5.large" {
		t.Fatalf("operator override should win, got %q", got)
	}
}

// Stop bound for the wait loop so tests don't run for 5 minutes when
// the goroutine that creates the node never fires. We close ctx in
// the test path, so this is just defence in depth.
var _ = time.Now
var _ = store.ListOpts{}
