// Package master — autoscaler.go: optional EC2 capacity provisioner.
// When the scheduler returns ErrNoCapacity and autoscaling is enabled,
// HandleNoCapacity queues the request, launches a fresh node via
// ec2.RunInstances with a user-data script that boots the agent, and
// retries the request once the agent registers. Background scaleDown
// terminates idle vajra:managed nodes after a cooldown.
//
// The autoscaler is OPTIONAL — nil unless VAJRA_AUTOSCALE_ENABLED=true.
// The handlers check `h.Autoscaler != nil && h.Autoscaler.Config.Enabled`
// before invoking it, so existing 503 behaviour is preserved by default.
package master

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/allenabraham999/vajra/internal/cache"
	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// EC2API is the narrow slice of *ec2.Client we depend on. Tests pass a
// fake implementing the same three calls — RunInstances to launch,
// TerminateInstances to scale down, DescribeInstances for status.
type EC2API interface {
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// AutoscaleConfig captures every knob the autoscaler reads. Enabled is
// the master switch; the rest only matters when Enabled is true.
//
// InstanceType is treated as an override: when set, every scale-up uses
// that exact type. When empty, the autoscaler picks the smallest type
// from instanceLadder that fits the requested sandbox resources, so a
// user asking for 8 vCPU on a host with only a c8i.large lights up a
// c6i.2xlarge instead of a too-small c8i.large.
type AutoscaleConfig struct {
	Enabled       bool
	AMI           string
	InstanceType  string
	Region        string
	SecurityGroup string
	KeyPair       string
	SubnetID      string
	MasterURL     string
	AgentSecret   string
	ClusterID     string
	MinNodes      int
	MaxNodes      int
	CooldownMins  int
	S3Bucket      string
	// RootVolumeGB overrides the AMI's default root volume size. 0 means
	// inherit the AMI snapshot's size; a positive value resizes the root
	// gp3 volume so sandboxes that request disk_gb > AMI-free-space have
	// somewhere to land. Filesystem growth on first boot is handled by
	// cloud-init's growpart module, which Ubuntu cloud images enable by
	// default.
	RootVolumeGB int
}

// withDefaults fills in unset numeric knobs so callers don't have to
// remember the defaults documented in the brief. InstanceType is
// intentionally left empty so scaleUp falls through to the resource-fit
// ladder; setting a default here would silently override resource-aware
// selection.
func (c AutoscaleConfig) withDefaults() AutoscaleConfig {
	if c.MinNodes == 0 {
		c.MinNodes = 1
	}
	if c.MaxNodes == 0 {
		c.MaxNodes = 50
	}
	if c.CooldownMins == 0 {
		c.CooldownMins = 15
	}
	return c
}

// instanceSpec describes one rung of the resource-fit ladder.
type instanceSpec struct {
	Type     string
	VCPUs    int
	MemoryMB int
}

// instanceLadder is the ordered list of supported EC2 types, smallest
// first. instanceTypeForResources picks the first rung that fits the
// request. The ceiling (c6i.4xlarge) caps what a single sandbox can ask
// for — requests above that are rejected at the handler with 400 since
// no single node we know how to launch could host them.
//
// The brief defines this mapping explicitly. Keep entries sorted by
// VCPUs (then MemoryMB) so the linear scan stays correct.
var instanceLadder = []instanceSpec{
	{Type: "c8i.large", VCPUs: 2, MemoryMB: 4 * 1024},
	{Type: "c8i.xlarge", VCPUs: 4, MemoryMB: 8 * 1024},
	{Type: "c8i.2xlarge", VCPUs: 8, MemoryMB: 16 * 1024},
	{Type: "c8i.4xlarge", VCPUs: 16, MemoryMB: 32 * 1024},
}

// instanceTypeForResources returns the smallest instance type in
// instanceLadder that can host a sandbox of (vcpus, memoryMB). Returns
// "" when no rung fits — callers translate that into a 400 because no
// scale-up will help.
func instanceTypeForResources(vcpus, memoryMB int) string {
	for _, spec := range instanceLadder {
		if spec.VCPUs >= vcpus && spec.MemoryMB >= memoryMB {
			return spec.Type
		}
	}
	return ""
}

// maxNodeVCPUs and maxNodeMemoryMB are the ceiling of instanceLadder.
// Used by handlers to short-circuit oversize requests before they hit
// the queue. Kept as functions so the values track the slice if anyone
// edits the ladder.
func maxNodeVCPUs() int {
	out := 0
	for _, s := range instanceLadder {
		if s.VCPUs > out {
			out = s.VCPUs
		}
	}
	return out
}

func maxNodeMemoryMB() int {
	out := 0
	for _, s := range instanceLadder {
		if s.MemoryMB > out {
			out = s.MemoryMB
		}
	}
	return out
}

// ExceedsAnyNodeCapacity reports whether (vcpus, memoryMB) is larger
// than any instance the autoscaler can launch. Exported so handlers can
// classify the request as "too big to ever fit" vs "wait for capacity".
func ExceedsAnyNodeCapacity(vcpus, memoryMB int) bool {
	return vcpus > maxNodeVCPUs() || memoryMB > maxNodeMemoryMB()
}

// Autoscaler owns the pending-request queue and the EC2 client. Single
// instance per master process; the mutex serialises scale-up so a burst
// of no-capacity requests doesn't fan out into many EC2 launches.
type Autoscaler struct {
	ec2Client EC2API
	store     store.Store
	cache     cache.Cache
	scheduler Scheduler
	Config    AutoscaleConfig
	logger    *slog.Logger

	mu           sync.Mutex
	scaling      bool
	pendingQueue []*PendingCreate
	now          func() time.Time
}

// PendingCreate is one queued sandbox-create request awaiting capacity.
// ResultCh is buffered size 1 so the scaler can drop the result without
// blocking even if the requester abandoned the wait.
type PendingCreate struct {
	Request   createSandboxRequest
	AccountID string
	ResultCh  chan PendingResult
	QueuedAt  time.Time
}

// PendingResult is the outcome the scaler hands back to the requester.
type PendingResult struct {
	NodeID string
	Error  error
}

// NewAutoscaler builds an Autoscaler. ec2Client may be nil when Enabled
// is false (e.g. tests that don't exercise the scaling paths). The
// scheduler is wired in so retries after scale-up reuse the same
// scoring logic the original create used.
func NewAutoscaler(cfg AutoscaleConfig, ec2Client EC2API, st store.Store, c cache.Cache, sched Scheduler, logger *slog.Logger) *Autoscaler {
	if logger == nil {
		logger = slog.Default()
	}
	if c == nil {
		c = cache.NewNoopCache()
	}
	return &Autoscaler{
		ec2Client: ec2Client,
		store:     st,
		cache:     c,
		scheduler: sched,
		Config:    cfg.withDefaults(),
		logger:    logger,
		now:       time.Now,
	}
}

// HandleNoCapacity is called by createSandbox when the scheduler returns
// ErrNoCapacity. If autoscaling is disabled we return ErrNoCapacity
// straight back so the handler returns 503 — current behaviour. When
// enabled, we queue and trigger scale-up.
//
// The function blocks until the launched node registers OR a 5min
// timeout fires, whichever comes first. The result tells the handler
// which node to retry on.
func (a *Autoscaler) HandleNoCapacity(ctx context.Context, req createSandboxRequest, accountID string) (string, error) {
	if a == nil || !a.Config.Enabled {
		return "", ErrNoCapacity
	}
	count, err := a.managedNodeCount(ctx)
	if err != nil {
		return "", fmt.Errorf("autoscale: count nodes: %w", err)
	}
	if count >= a.Config.MaxNodes {
		return "", fmt.Errorf("autoscale: max nodes reached (%d)", a.Config.MaxNodes)
	}

	pc := &PendingCreate{
		Request:   req,
		AccountID: accountID,
		ResultCh:  make(chan PendingResult, 1),
		QueuedAt:  a.now(),
	}
	a.mu.Lock()
	a.pendingQueue = append(a.pendingQueue, pc)
	startScale := !a.scaling
	if startScale {
		a.scaling = true
	}
	a.mu.Unlock()
	if startScale {
		// scaleUp inspects the queue under its own lock to pick a type
		// large enough for the biggest waiter; we don't pass it through
		// the channel so a late-arriving big request can still upgrade
		// the launch decision if it lands before scaleUp starts.
		go a.scaleUp(context.Background())
	}

	select {
	case res := <-pc.ResultCh:
		return res.NodeID, res.Error
	case <-time.After(5 * time.Minute):
		return "", fmt.Errorf("autoscale: timed out waiting for capacity")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// pickInstanceType decides which EC2 type to launch. Config.InstanceType
// (the operator override) wins when set; otherwise we look at the
// largest queued request and pick the smallest ladder rung that fits
// it. Picking the biggest waiter is deliberate: one too-small node
// would leave that waiter stuck even though smaller waiters would have
// been fine.
func (a *Autoscaler) pickInstanceType() string {
	if a.Config.InstanceType != "" {
		return a.Config.InstanceType
	}
	a.mu.Lock()
	maxVCPU, maxMem := 0, 0
	for _, pc := range a.pendingQueue {
		if pc.Request.VCPUs > maxVCPU {
			maxVCPU = pc.Request.VCPUs
		}
		if pc.Request.MemoryMB > maxMem {
			maxMem = pc.Request.MemoryMB
		}
	}
	a.mu.Unlock()
	if maxVCPU == 0 && maxMem == 0 {
		// No queued requests (e.g. admin-triggered scale-up). Fall back
		// to the smallest rung; operators who want a specific size set
		// InstanceType explicitly.
		return instanceLadder[0].Type
	}
	return instanceTypeForResources(maxVCPU, maxMem)
}

// scaleUp launches a single EC2 instance, waits for the agent to
// register, then drains the pending queue by handing each waiter the
// new node ID. Any error during launch or wait is broadcast to every
// waiter so they can fail fast rather than block the full 5min ceiling.
func (a *Autoscaler) scaleUp(ctx context.Context) {
	defer func() {
		a.mu.Lock()
		a.scaling = false
		a.mu.Unlock()
	}()

	count, err := a.managedNodeCount(ctx)
	if err != nil {
		a.broadcastError(fmt.Errorf("autoscale: count nodes: %w", err))
		return
	}
	if count >= a.Config.MaxNodes {
		a.broadcastError(fmt.Errorf("autoscale: max nodes reached"))
		return
	}

	instType := a.pickInstanceType()
	if instType == "" {
		a.broadcastError(fmt.Errorf("autoscale: no instance type fits queued requests"))
		return
	}
	userData := a.buildUserData()
	tagName := fmt.Sprintf("vajra-node-%d", a.now().Unix())
	in := &ec2.RunInstancesInput{
		ImageId:          aws.String(a.Config.AMI),
		InstanceType:     ec2types.InstanceType(instType),
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
		KeyName:          stringOrNil(a.Config.KeyPair),
		SecurityGroupIds: nonEmptySlice(a.Config.SecurityGroup),
		SubnetId:         stringOrNil(a.Config.SubnetID),
		UserData:         aws.String(base64.StdEncoding.EncodeToString([]byte(userData))),
		CpuOptions: &ec2types.CpuOptionsRequest{
			NestedVirtualization: ec2types.NestedVirtualizationSpecificationEnabled,
		},
		BlockDeviceMappings: a.rootVolumeMapping(),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags: []ec2types.Tag{
				{Key: aws.String("Name"), Value: aws.String(tagName)},
				{Key: aws.String("vajra:managed"), Value: aws.String("true")},
			},
		}},
	}

	a.logger.Info("autoscale: launching ec2 instance", "ami", a.Config.AMI, "type", instType)
	out, err := a.ec2Client.RunInstances(ctx, in)
	if err != nil {
		a.broadcastError(fmt.Errorf("autoscale: run instances: %w", err))
		return
	}
	if len(out.Instances) == 0 {
		a.broadcastError(fmt.Errorf("autoscale: ec2 returned no instances"))
		return
	}
	instanceID := aws.ToString(out.Instances[0].InstanceId)
	a.logger.Info("autoscale: instance launched", "instance_id", instanceID)

	nodeID, err := a.waitForRegistration(ctx, instanceID)
	if err != nil {
		a.logger.Error("autoscale: registration timeout", "instance_id", instanceID, "err", err)
		_, _ = a.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: []string{instanceID},
		})
		a.broadcastError(fmt.Errorf("autoscale: agent did not register: %w", err))
		return
	}
	a.logger.Info("autoscale: node registered", "node_id", nodeID, "instance_id", instanceID)
	a.drainQueue(nodeID)
}

// waitForRegistration polls the nodes table every 5s for up to 5min
// looking for a node that registered after we kicked off the launch.
// We can't directly correlate ec2 instance ID → node row (the agent
// chooses its own ID from hostname), so we look for the most-recently
// heartbeat-fresh active node that wasn't there before. In practice
// the new agent's first heartbeat is the only thing landing in this
// window; if multiple registrations race they'll all serve the queue.
func (a *Autoscaler) waitForRegistration(ctx context.Context, instanceID string) (string, error) {
	deadline := a.now().Add(5 * time.Minute)
	known := map[string]struct{}{}
	if existing, err := a.store.Nodes().List(ctx, store.ListOpts{Limit: 1000}); err == nil {
		for _, n := range existing {
			known[n.ID] = struct{}{}
		}
	}
	for a.now().Before(deadline) {
		nodes, err := a.store.Nodes().List(ctx, store.ListOpts{Limit: 1000})
		if err == nil {
			for _, n := range nodes {
				if _, seen := known[n.ID]; seen {
					continue
				}
				if n.State == models.NodeStateActive {
					return n.ID, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return "", fmt.Errorf("registration timeout for instance %s", instanceID)
}

// drainQueue hands the new node ID to every queued waiter. Since the
// scheduler will re-evaluate the request, we just unblock the waiter —
// the handler reissues the request through Schedule.
func (a *Autoscaler) drainQueue(nodeID string) {
	a.mu.Lock()
	queue := a.pendingQueue
	a.pendingQueue = nil
	a.mu.Unlock()
	for _, pc := range queue {
		select {
		case pc.ResultCh <- PendingResult{NodeID: nodeID}:
		default:
		}
	}
}

// broadcastError fans the same error out to every queued waiter and
// clears the queue so the next no-capacity event starts fresh.
func (a *Autoscaler) broadcastError(err error) {
	a.mu.Lock()
	queue := a.pendingQueue
	a.pendingQueue = nil
	a.mu.Unlock()
	for _, pc := range queue {
		select {
		case pc.ResultCh <- PendingResult{Error: err}:
		default:
		}
	}
}

// staleHeartbeatThreshold is how long a node can go without a heartbeat
// before the autoscaler stops counting it toward MaxNodes. Stale rows
// linger after EC2 terminations or agent crashes, so without this filter
// a few zombie rows can pin the autoscaler at capacity and silently
// block legitimate scale-ups.
const staleHeartbeatThreshold = 5 * time.Minute

// managedNodeCount returns the count of live nodes against which the
// MaxNodes cap is enforced. "Live" means state=ACTIVE and a heartbeat
// within staleHeartbeatThreshold; anything older is treated as a
// zombie row that should not be allowed to block scale-up.
func (a *Autoscaler) managedNodeCount(ctx context.Context) (int, error) {
	nodes, err := a.store.Nodes().List(ctx, store.ListOpts{Limit: 1000})
	if err != nil {
		return 0, err
	}
	cutoff := a.now().Add(-staleHeartbeatThreshold)
	count := 0
	for _, n := range nodes {
		if n.State != models.NodeStateActive {
			continue
		}
		if n.LastHeartbeat.Before(cutoff) {
			continue
		}
		count++
	}
	return count, nil
}

// RunScaleDown is the long-running goroutine — call once from main and
// it blocks until ctx is cancelled. Every 5min it scans for idle
// vajra:managed nodes whose last sandbox was destroyed at least
// CooldownMins ago, drains them, and terminates the EC2 instance.
func (a *Autoscaler) RunScaleDown(ctx context.Context) {
	if a == nil || !a.Config.Enabled {
		return
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.scaleDown(ctx)
		}
	}
}

// scaleDown is one pass of the periodic scaler. Each idle managed node
// past the cooldown is drained and terminated. Errors are logged and
// we keep moving — one bad node should not block reaping the others.
func (a *Autoscaler) scaleDown(ctx context.Context) {
	nodes, err := a.store.Nodes().List(ctx, store.ListOpts{Limit: 1000})
	if err != nil {
		a.logger.Error("autoscale: list nodes", "err", err)
		return
	}
	if len(nodes) <= a.Config.MinNodes {
		return
	}

	managed, err := a.listManagedInstances(ctx)
	if err != nil {
		a.logger.Error("autoscale: list managed", "err", err)
		return
	}
	if len(managed) == 0 {
		return
	}
	cooldown := time.Duration(a.Config.CooldownMins) * time.Minute

	for _, n := range nodes {
		if len(nodes) <= a.Config.MinNodes {
			return
		}
		instID, ok := managed[n.IP]
		if !ok {
			continue
		}
		sandboxes, err := a.store.Sandboxes().ListByNode(ctx, n.ID, store.ListOpts{Limit: 1000})
		if err != nil {
			a.logger.Error("autoscale: list sandboxes", "node_id", n.ID, "err", err)
			continue
		}
		if hasActiveSandbox(sandboxes) {
			continue
		}
		idleSince := lastActivityForNode(n, sandboxes)
		if a.now().Sub(idleSince) < cooldown {
			continue
		}
		a.logger.Info("autoscale: terminating idle node", "node_id", n.ID, "instance_id", instID)
		if err := a.store.Nodes().UpdateState(ctx, n.ID, models.NodeStateDraining); err != nil {
			a.logger.Error("autoscale: drain", "node_id", n.ID, "err", err)
			continue
		}
		if _, err := a.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: []string{instID},
		}); err != nil {
			a.logger.Error("autoscale: terminate", "instance_id", instID, "err", err)
			continue
		}
		if err := a.store.Nodes().Delete(ctx, n.ID); err != nil {
			a.logger.Error("autoscale: delete node", "node_id", n.ID, "err", err)
		}
	}
}

// listManagedInstances returns IP→instance_id map for instances tagged
// vajra:managed=true. We key on IP because that's what matches the
// agent's registration row in Postgres.
func (a *Autoscaler) listManagedInstances(ctx context.Context) (map[string]string, error) {
	out, err := a.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:vajra:managed"), Values: []string{"true"}},
			{Name: aws.String("instance-state-name"), Values: []string{"running", "pending"}},
		},
	})
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			ip := aws.ToString(inst.PrivateIpAddress)
			id := aws.ToString(inst.InstanceId)
			if ip != "" && id != "" {
				result[ip] = id
			}
		}
	}
	return result, nil
}

// hasActiveSandbox reports whether any sandbox on this node is still
// occupying capacity. DESTROYED and ERROR don't count.
func hasActiveSandbox(sandboxes []*models.Sandbox) bool {
	for _, sb := range sandboxes {
		if sb.State == models.SandboxStateDestroyed || sb.State == models.SandboxStateError {
			continue
		}
		return true
	}
	return false
}

// lastActivityForNode returns the most recent UpdatedAt across the
// node's sandboxes; falls back to the node's last heartbeat when there
// are no sandboxes at all.
func lastActivityForNode(n *models.Node, sandboxes []*models.Sandbox) time.Time {
	latest := n.LastHeartbeat
	for _, sb := range sandboxes {
		if sb.UpdatedAt.After(latest) {
			latest = sb.UpdatedAt
		}
	}
	return latest
}

// buildUserData assembles the cloud-init bash script that gets the
// agent running on a freshly launched instance. Binaries are pulled
// from master's /internal/binaries/{name} endpoint (Bearer-authed with
// the shared agent secret) — this avoids needing public S3 or an IAM
// instance profile on every node. CPU/MEM/DISK are computed inline
// so the rendered systemd unit holds concrete numbers, not literal
// $(nproc) which neither bash heredocs (single-quoted) nor systemd's
// Environment= will expand.
func (a *Autoscaler) buildUserData() string {
	return fmt.Sprintf(`#!/bin/bash
set -eux
apt-get update && apt-get install -y wget
AUTH="Authorization: Bearer %[2]s"
wget -q --header="$AUTH" %[1]s/internal/binaries/cloud-hypervisor -O /usr/local/bin/cloud-hypervisor
chmod +x /usr/local/bin/cloud-hypervisor
wget -q --header="$AUTH" %[1]s/internal/binaries/vajra-agent -O /usr/local/bin/vajra-agent
chmod +x /usr/local/bin/vajra-agent
mkdir -p /var/lib/vajra/cache /var/lib/vajra/sandboxes /tmp/vajra/sockets
CPU=$(nproc)
MEM=$(free -m | awk '/Mem:/{print $2}')
DISK=$(df -BG / | awk 'NR==2{print $4}' | tr -d 'G')
cat > /etc/systemd/system/vajra-agent.service <<SVCEOF
[Unit]
Description=Vajra Agent
After=network.target
[Service]
ExecStart=/usr/local/bin/vajra-agent
Environment=VAJRA_AGENT_MASTER_URL=%[1]s
Environment=VAJRA_AGENT_API_KEY=%[2]s
Environment=VAJRA_AGENT_CLUSTER_ID=%[3]s
Environment=VAJRA_AGENT_TOTAL_CPU=${CPU}
Environment=VAJRA_AGENT_TOTAL_MEMORY_MB=${MEM}
Environment=VAJRA_AGENT_TOTAL_DISK_GB=${DISK}
Restart=always
[Install]
WantedBy=multi-user.target
SVCEOF
systemctl daemon-reload
systemctl enable --now vajra-agent
`, a.Config.MasterURL, a.Config.AgentSecret, a.Config.ClusterID)
}

// AutoscaleStatus is the body of GET /v1/admin/autoscale.
type AutoscaleStatus struct {
	Enabled      bool `json:"enabled"`
	Scaling      bool `json:"scaling"`
	PendingCount int  `json:"pending_count"`
	NodeCount    int  `json:"node_count"`
	MinNodes     int  `json:"min_nodes"`
	MaxNodes     int  `json:"max_nodes"`
}

// Status returns a snapshot of the autoscaler state. Cheap; safe to
// call from an HTTP handler.
func (a *Autoscaler) Status(ctx context.Context) (AutoscaleStatus, error) {
	if a == nil {
		return AutoscaleStatus{}, errors.New("autoscaler not configured")
	}
	a.mu.Lock()
	st := AutoscaleStatus{
		Enabled:      a.Config.Enabled,
		Scaling:      a.scaling,
		PendingCount: len(a.pendingQueue),
		MinNodes:     a.Config.MinNodes,
		MaxNodes:     a.Config.MaxNodes,
	}
	a.mu.Unlock()
	count, err := a.managedNodeCount(ctx)
	if err != nil {
		return st, err
	}
	st.NodeCount = count
	return st, nil
}

// Trigger forces a scale-up immediately. Used by the admin-only
// trigger endpoint and by tests. No-op if a scale-up is already in
// flight.
func (a *Autoscaler) Trigger(ctx context.Context) error {
	if a == nil || !a.Config.Enabled {
		return errors.New("autoscaler not enabled")
	}
	a.mu.Lock()
	if a.scaling {
		a.mu.Unlock()
		return errors.New("scale-up already in progress")
	}
	a.scaling = true
	a.mu.Unlock()
	go a.scaleUp(context.Background())
	return nil
}

// rootVolumeMapping returns the BlockDeviceMappings RunInstances input
// needed to resize the AMI's root device. Returns nil when RootVolumeGB
// is zero so the AMI's default volume sizing wins. Hardcodes /dev/sda1
// + gp3 because every Ubuntu-cloud AMI we ship from uses that device
// name, and gp3 gives 3000 baseline IOPS at no extra cost vs gp2 — the
// cold-snapshot read path is bandwidth-bound so this materially shrinks
// first-restore latency.
func (a *Autoscaler) rootVolumeMapping() []ec2types.BlockDeviceMapping {
	if a.Config.RootVolumeGB <= 0 {
		return nil
	}
	return []ec2types.BlockDeviceMapping{{
		DeviceName: aws.String("/dev/sda1"),
		Ebs: &ec2types.EbsBlockDevice{
			VolumeSize:          aws.Int32(int32(a.Config.RootVolumeGB)),
			VolumeType:          ec2types.VolumeTypeGp3,
			DeleteOnTermination: aws.Bool(true),
		},
	}}
}

// stringOrNil returns nil for "" so the AWS SDK doesn't reject empty
// strings on InstanceInput parameters that allow defaults.
func stringOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

// nonEmptySlice returns nil for "" so we don't pass [""] to EC2 and
// trip a validation error.
func nonEmptySlice(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
