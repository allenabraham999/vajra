package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// DefaultPoolMinSize is the steady-state warm pool target if nothing is
// configured.
const DefaultPoolMinSize = 2

// DefaultPoolReplenishTimeout bounds how long the background goroutine
// will wait on a single CreateSandbox call before declaring it failed and
// trying again on the next tick.
const DefaultPoolReplenishTimeout = 30 * time.Second

// PoolStats is the snapshot returned by GET /pool/stats.
type PoolStats struct {
	Total     int    `json:"total"`
	Available int    `json:"available"`
	InUse     int    `json:"in_use"`
	Creating  int    `json:"creating"`
	Template  string `json:"template"`
}

// PoolManager keeps a small pool of pre-restored sandboxes ready for
// instant assignment. Each pool member is a real, fully-restored sandbox
// in the SandboxManager — the pool is only an index of which ones haven't
// been claimed yet.
type PoolManager struct {
	minSize  int
	template string
	config   SandboxConfig
	sandboxes *SandboxManager
	logger   *slog.Logger
	timeout  time.Duration

	mu        sync.Mutex
	available []string
	assigned  map[string]struct{}
	creating  int

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

// NewPoolManager builds a pool that targets minSize warm VMs cloned from
// the named template. Pass minSize <= 0 for DefaultPoolMinSize.
func NewPoolManager(
	minSize int,
	templateHash string,
	cfg SandboxConfig,
	sandboxes *SandboxManager,
	logger *slog.Logger,
) *PoolManager {
	if minSize <= 0 {
		minSize = DefaultPoolMinSize
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PoolManager{
		minSize:   minSize,
		template:  templateHash,
		config:    cfg,
		sandboxes: sandboxes,
		logger:    logger,
		timeout:   DefaultPoolReplenishTimeout,
		assigned:  map[string]struct{}{},
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Start launches the background replenish goroutine. The pool is empty
// until the goroutine catches up; pass a long-lived context that lives for
// the duration of the daemon. Calling Start more than once panics.
func (p *PoolManager) Start(ctx context.Context) {
	go p.run(ctx)
	p.poke()
}

// Stop signals the background goroutine to exit and waits for it. Safe
// against double-stop.
func (p *PoolManager) Stop() {
	select {
	case <-p.stop:
		return
	default:
		close(p.stop)
	}
	<-p.done
}

// AssignFromPool returns the ID of an available pool sandbox and marks it
// assigned. ErrPoolEmpty is returned when no warm VM is ready; callers
// should fall back to a synchronous CreateSandbox in that case.
func (p *PoolManager) AssignFromPool() (string, error) {
	p.mu.Lock()
	if len(p.available) == 0 {
		p.mu.Unlock()
		p.poke()
		return "", ErrPoolEmpty
	}
	id := p.available[0]
	p.available = p.available[1:]
	p.assigned[id] = struct{}{}
	p.mu.Unlock()
	p.poke()
	return id, nil
}

// Release tells the pool that a previously-assigned sandbox has been
// destroyed by its owner. The pool only uses this for stats; it does not
// re-add the sandbox to the available list.
func (p *PoolManager) Release(id string) {
	p.mu.Lock()
	delete(p.assigned, id)
	p.mu.Unlock()
}

// Stats returns a point-in-time snapshot.
func (p *PoolManager) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{
		Total:     len(p.available) + len(p.assigned) + p.creating,
		Available: len(p.available),
		InUse:     len(p.assigned),
		Creating:  p.creating,
		Template:  p.template,
	}
}

// ErrPoolEmpty is returned by AssignFromPool when no warm sandbox is ready.
var ErrPoolEmpty = errors.New("pool: no warm sandbox available")

func (p *PoolManager) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *PoolManager) run(ctx context.Context) {
	defer close(p.done)
	// Even without explicit pokes, tick periodically so a transient
	// replenish failure doesn't strand the pool below target forever.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		p.replenishOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

func (p *PoolManager) replenishOnce(ctx context.Context) {
	for {
		p.mu.Lock()
		needed := p.minSize - len(p.available) - p.creating
		if needed <= 0 {
			p.mu.Unlock()
			return
		}
		p.creating++
		p.mu.Unlock()

		if err := p.createOne(ctx); err != nil {
			p.mu.Lock()
			p.creating--
			p.mu.Unlock()
			p.logger.Warn("pool replenish failed", "err", err)
			return
		}
	}
}

func (p *PoolManager) createOne(ctx context.Context) error {
	createCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	sb, err := p.sandboxes.CreateSandbox(createCtx, CreateRequest{
		TemplateHash: p.template,
		Config:       p.config,
	})
	if err != nil {
		return fmt.Errorf("pool create: %w", err)
	}
	// Mark the sandbox as pool-owned so observers know it's idle until
	// assigned. The manager already adopted it in CreateSandbox.
	p.sandboxes.mu.Lock()
	if cur, ok := p.sandboxes.sandboxes[sb.ID]; ok {
		cur.FromPool = true
	}
	p.sandboxes.mu.Unlock()

	p.mu.Lock()
	p.available = append(p.available, sb.ID)
	p.creating--
	p.mu.Unlock()
	p.logger.Info("pool sandbox ready", "id", sb.ID)
	return nil
}
