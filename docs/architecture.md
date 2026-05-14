# Vajra — Architecture

## Overview

Vajra is a self-hosted sandbox cloud for AI agents. It creates isolated
Linux microVMs on bare-metal hosts using Cloud Hypervisor and the host
KVM, exposes them through a stateless REST control plane, and returns a
running sandbox to the caller in **~160 ms** on EC2 nested virt (target:
80–150 ms on bare metal). The platform is comparable in shape to
Daytona, E2B, and Modal — but the entire stack lives in this repo:
VMM shim, node agent, control plane, reverse proxy, guest agent,
SDKs, CLI, and React dashboard.

The repo ships four Go binaries — `vajra-master`, `vajra-agent`,
`vajra-proxy`, `vajra` — plus a static ELF guest agent and a Python
SDK / TypeScript scaffold / React dashboard.

## Design Principles

These five principles are load-bearing across every package in
`internal/`. Every file in this codebase is meant to honour them; the
review of any new change should start from this list.

### 1. The master is stateless

`vajra-master` keeps **no** sandbox state in memory. Every read goes to
Postgres (or Redis as a hint, with Postgres as truth). Multiple
replicas behind any L4 load balancer are functionally equivalent.

This shows up in concrete places:

- `internal/master/scheduler.go` is `dbScheduler{}` — every placement
  decision is computed against the live Postgres view of `nodes`,
  with no node table cached locally. Heartbeat freshness is part of
  the SQL filter, not a Go map of last-seen times.
- `internal/master/reconciler.go` reconciles drift by diffing the
  agent's view against the DB — never against a Go process map.
- `internal/master/handlers_share.go` writes share-link state to
  Postgres (table `share_links`, migration `002_share_links.up.sql`)
  rather than an in-process map, because in-memory shares would
  split-brain across replicas.
- The only "stateful" components on master are the operation
  tracker (`internal/master/operation.go`) — and even that just
  writes audit rows — and the autoscaler's pending-request queue
  (`internal/master/autoscaler.go`), which is rebuildable from the
  503 retry contract.

### 2. Event-driven agent communication

Agents push state to master, master does not poll agents.

- HTTP path: agents call `POST /internal/nodes/{id}/heartbeat` every
  5 s. The handler updates `nodes.last_heartbeat` and refreshes the
  Redis usage hint (`internal/master/handlers_internal.go`,
  `internal/master/cache_writer.go`).
- NATS path (optional): when `NATS_URL` is set, agents publish
  `vajra.node.heartbeat`, `vajra.sandbox.state_changed`, and
  `vajra.node.unhealthy` (`internal/agent/publisher.go`). The master
  subscriber writes Redis directly and batches Postgres every 30 s
  (`internal/master/subscriber.go`) — collapsing the per-second
  heartbeat into a per-batch DB write.
- The reconciler exists, but only as a slow drift detector (default
  60 s ticks). It is not the primary signal.

### 3. Content-addressable images

Templates are identified by the SHA256 of their rootfs, stored under
`/var/lib/vajra/cache/{hash}/` with a fixed layout (`rootfs.raw`,
`vmlinux`, `snapshot/`). Implementation in
`internal/agent/image.go`:

- HTTP pull writes to `<hash>.part` and renames atomically only after
  the hash matches the expected one.
- Concurrent pulls of the same hash coalesce on a per-hash inflight
  map — two callers race to pull a 500 MB rootfs, one wins, both
  get the file.
- `EvictLRU` evicts oldest-touched directories until total cache
  size is under the soft cap.

Implication: a template promoted from a snapshot at master cannot
silently change underneath running sandboxes. If the bytes change,
the hash changes, and the agent will treat it as a new template.

### 4. Two-tier scheduler

`Schedule` = `PickCluster` → `PickNode` (`internal/master/scheduler.go`).
Both layers exist even when there is a single cluster, because we
will eventually want region affinity:

- `PickCluster` honours `request.region` strictly if set, else picks
  any ACTIVE cluster.
- `PickNode` filters to ACTIVE nodes heartbeated within the last
  90 s, then scores each by *remaining capacity after the proposed
  placement* (worst-fit / spread). Lex tie-break on `id` keeps the
  test suite deterministic.
- `CheckQuota` enforces both sandbox-count and vCPU/memory sum.
  DESTROYED and ERROR sandboxes don't count.

When autoscaling is enabled and `Schedule` returns `ErrNoCapacity`,
master kicks an EC2 launch and re-runs `Schedule` once the new node
heartbeats — same scoring path
(`internal/master/autoscaler.go::HandleNoCapacity`).

### 5. Defense in depth

The threat model document
([security-threat-model.md](security-threat-model.md)) walks the full
trust chain. The short version is:

| Layer | Mechanism |
|------|-----------|
| Guest ↔ host | KVM hardware virtualization + Cloud Hypervisor (Rust, minimal device surface) |
| Sandbox ↔ sandbox | Each VM gets a private vsock CID; no shared L3 networking |
| Caller ↔ master | JWT (HS256, 1h) or API key (`vj_live_<32 hex>`); per-account rate limit |
| Auditability | Every mutating call writes an `operations` row |
| At rest | Optional S3 SSE on archive blobs; UNIX `0o600` perms on local state |
| Operational | Token bucket per account, default 10 RPS; admin gate behind env-pinned account id |

Each line is implemented somewhere in `internal/`. See the threat
model for file references and live attack vectors.

## Technology Choices

Each non-obvious choice is documented here. Defaults that match
"what every Go shop does" are not listed.

### Cloud Hypervisor (not Firecracker / Incus / gVisor)

- **Firecracker.** Faster snapshot restore is roughly equivalent
  (low hundreds of ms). The reason Vajra picked CH over FC was
  **GPU passthrough via VFIO** — FC does not support PCI passthrough
  and the roadmap is explicit about not adding it. AI agent workloads
  want a GPU eventually; CH lets that happen without a VMM rewrite.
- **Incus / LXC containers** (the path Marko's current platform uses
  per `bible.md` Day 1). Container boot is fast on its own
  (~1 second on the Day 1 benchmark) but the isolation envelope is
  weaker, and a single bad syscall escape compromises the host.
  Vajra's measured 160 ms restore is **6× faster** than the 1000 ms
  Incus baseline for cold start, *and* runs inside KVM.
- **gVisor.** User-space syscall interposition. Strong isolation,
  but high syscall overhead — and again no GPU passthrough.

The cost of CH vs FC: slightly larger memory footprint, slightly
fewer flags. The benefit: VFIO, virtio-pci, more active
upstream (Intel-backed), Rust codebase that's easier to audit than
QEMU.

### Go (not Rust / Python)

- **Single binary** per service — no container layer required to
  ship a master replica.
- **Goroutines** match the workload: heartbeat fan-in, per-sandbox
  exec streams, reverse-proxy connections.
- **Ecosystem fit.** `golang-migrate`, `sqlx`, `chi`-style mux
  (we use the stdlib 1.22 mux, same shape — see Day 3 in
  `bible.md`), `aws-sdk-go-v2`, `nats.go`, `go-redis`. Everything we
  needed was there.

Rust was a real consideration for the agent (CH is Rust); rejected
because the agent's hot path is HTTP + Postgres + occasional
file I/O, not syscall-bashing. Python was rejected because we'd
have to ship an interpreter on every host.

### PostgreSQL + Redis + NATS

- **Postgres** is the source of truth. Tables: `accounts`,
  `api_keys`, `clusters`, `nodes`, `sandboxes`, `snapshots`,
  `templates`, `operations`, `sandbox_usage`, `share_links`.
  Migrations live in `migrations/`, applied at master startup via
  `golang-migrate`.
- **Redis** is a hint cache, not a write store
  (`internal/cache/{cache,redis,noop,keys}.go`). TTLs are tight:
  sandbox state 30 s, node usage 10 s, account quota 60 s, template
  metadata 5 min. A Redis miss is invisible to callers — the read
  falls through to Postgres.
- **NATS** carries heartbeat + state-change events
  (`internal/events/{events,nats,noop}.go`,
  `internal/master/subscriber.go`,
  `internal/agent/publisher.go`). Subjects:
  `vajra.sandbox.{created,destroyed,state_changed}`,
  `vajra.node.{heartbeat,registered,unhealthy}`.

All three layered stores are **opt-in**. `REDIS_URL` empty →
`NoopCache`. `NATS_URL` empty → `NoopBus`. Master started with
neither runs against Postgres exclusively, with byte-for-byte
identical demo-path semantics.

## System Architecture

```
                                    ┌──────────────┐
                                    │  React UI    │
                                    └──────┬───────┘
                                           │ /v1/*
        ┌──────────────┐   ┌───────────────┴──────────────┐
        │ Python SDK   │   │  vajra (cobra CLI)            │
        └──────┬───────┘   └────────────────┬──────────────┘
               │                            │
               │            HTTP (JWT/API key)             │ token bucket
               ▼                            ▼              │ per account
        ╔═══════════════════════════════════════════════╗  │
        ║                vajra-master                   ║◄─┘
        ║  ───────────────────────────────────────────  ║
        ║   handlers │ scheduler │ reconciler │ ops     ║
        ║   auth     │ dispatch  │ autoscaler │ subs    ║
        ╚═══╤═══════════════╤════════════════════╤══════╝
            │ Postgres      │ Redis (optional)   │ NATS (optional)
            ▼               ▼                    ▼
        ┌─────────┐    ┌─────────┐         ┌─────────┐
        │ RDS     │    │ Redis   │         │ NATS    │
        │ (truth) │    │ (hints) │         │ (events)│
        └─────────┘    └─────────┘         └─────────┘

        ╔═════════════════════════════════════════════════╗
        ║ vajra-proxy (host-based subdomain dispatcher)   ║
        ║   <port>-<sandbox>.<base-domain> → agent tunnel ║
        ╚════════════════════╤════════════════════════════╝
                             │ HTTP / WebSocket / upgrades
                             ▼
   ┌───────────────────────────────────────────────────────────┐
   │              Bare-metal host (one or many)                │
   │  ┌─────────────────────────────────────────────────────┐  │
   │  │              vajra-agent                            │  │
   │  │  sandbox  │  pool   │ image cache │ archive/migrate │  │
   │  │  health   │ files   │ snapshots   │ vmm shim        │  │
   │  └────────────────────────┬────────────────────────────┘  │
   │                           │ Unix-socket REST              │
   │            ┌──────────────┴──────────────┐                │
   │            ▼                             ▼                │
   │     ┌────────────┐                 ┌────────────┐         │
   │     │ Cloud      │  ... per VM ... │ Cloud      │         │
   │     │ Hypervisor │                 │ Hypervisor │         │
   │     └─────┬──────┘                 └─────┬──────┘         │
   │           │ KVM /dev/kvm                 │                │
   │           ▼                              ▼                │
   │      ┌─────────┐                     ┌─────────┐          │
   │      │ guest   │  vsock 5252..5255   │ guest   │          │
   │      │ (Ubuntu │  exec/files/term/   │ (Ubuntu │          │
   │      │  Noble) │  forward            │  Noble) │          │
   │      └─────────┘                     └─────────┘          │
   └───────────────────────────────────────────────────────────┘
```

## Component Detail

### `vajra-master`

Stateless REST control plane (`cmd/vajra-master`,
`internal/master/`). Single Go process. Multiple replicas behind a
load balancer is the expected production shape.

Responsibilities:

- AuthN/AuthZ. JWT (HS256, 1h, `internal/master/auth.go`) and API
  keys (`vj_live_<32 hex>`, SHA256-hashed before storage).
  `AuthMiddleware` dispatches Bearer tokens to either path; account
  ID is threaded through `context.Context`.
- Scheduling. `dbScheduler` in `internal/master/scheduler.go`.
- Reconciliation. 60 s loop in `internal/master/reconciler.go`
  resolves orphan / ghost / state-mismatch drift between agents and
  Postgres.
- Dispatch. `internal/master/dispatcher.go` and
  `dispatcher_pool.go` hold the per-node HTTP client cache. Retry is
  exponential-backoff, 3 attempts default, 4xx not retried.
- Operations. Every mutating request opens an `operations` row in
  IN_PROGRESS and closes it COMPLETED/FAILED
  (`internal/master/operation.go`).
- Rate limit. Per-account token bucket
  (`internal/master/ratelimit.go`), default 10 RPS.
- Optional autoscaler (`internal/master/autoscaler.go`) and NATS
  subscriber (`internal/master/subscriber.go`).

Key endpoints (full list across `handlers_*.go`):

- Auth: `POST /v1/auth/register|login`, `POST /v1/api-keys`,
  `GET /v1/api-keys`, `DELETE /v1/api-keys/{id}`
- Sandboxes: `POST|GET /v1/sandboxes`, `GET|DELETE /v1/sandboxes/{id}`,
  `POST /v1/sandboxes/{id}/{stop,start,exec,snapshot,archive,rehydrate,migrate,share}`
- Files: `POST /v1/sandboxes/{id}/files/upload`,
  `GET /v1/sandboxes/{id}/files/download`,
  `GET /v1/sandboxes/{id}/files/list`
- Templates / snapshots: standard CRUD plus
  `POST /v1/snapshots/{id}/promote`
- Admin: `GET /v1/admin/clusters|nodes`, `POST /v1/admin/nodes/{id}/drain`,
  `GET|POST /v1/admin/autoscale/...`
- Internal (agent-only, `InternalAuthMiddleware`):
  `/internal/nodes/{register,heartbeat,event}`,
  `/internal/proxy/{route,validate-share}`
- Ops: `GET /health`, `GET /v1/version`

### `vajra-agent`

Per-host daemon (`cmd/vajra-agent`, `internal/agent/`). One per
bare-metal node. Manages the local sandbox population, the image
cache, the pre-warm pool, and the agent ↔ guest vsock traffic.

Major pieces:

- `sandbox.go` — `SandboxManager`. Owns the lifecycle
  (CREATING → RUNNING → STOPPED → DESTROYED). CoW disk via
  qcow2 overlay (the Day 8 ~30× speedup over `cp --reflink=auto`).
- `image.go` — content-addressable `ImageCache`.
- `pool.go` — dynamic pre-warm pool (see below).
- `archive.go` — tar+zstd, optional S3 upload via `aws-sdk-go-v2`.
- `migrate.go` — agent-to-agent uncompressed tar streaming.
- `health.go` — edge-triggered vsock health probe; `NotifyUnhealthy`
  fires once per healthy→unhealthy transition.
- `snapshot.go`, `files.go`, `forward.go`, `exec.go` — host-side
  vsock clients for the four guest ports.
- `server.go` + `server_*.go` — HTTP surface for master
  (auth via shared secret) and for the proxy.
- `master_client.go` — outbound `Register / Heartbeat / NotifyUnhealthy`.
- `publisher.go` — optional NATS publisher; falls back to HTTP only.

### Guest agent

`scripts/guest-agent/` — Linux-only ELF (`CGO_ENABLED=0`, ~4.3 MB
static, x/sys/unix only). Runs inside every Vajra VM under systemd.

Listens on four AF_VSOCK ports:

| Port | Purpose | Protocol |
|------|---------|---|
| 5252 | exec    | JSON-line `{command, timeout_ms}` → `{exit_code, stdout, stderr}` |
| 5253 | files   | JSON envelopes for upload / download / list (`X-Vajra-Path`-style header in the body) |
| 5254 | terminal | PTY allocated via direct ioctls (`TIOCSPTLCK`, `TIOCGPTN`, `TIOCSWINSZ`); resize frames carried in the protocol |
| 5255 | forward  | Tunnel for arbitrary localhost ports inside the guest (used by `vajra-proxy`) |

The vsock connection uses a custom `vsockNetConn` (Day 7 fix in
`scripts/guest-agent/vsock.go`) because `net.FileConn` rejects
AF_VSOCK with "address family not supported by protocol". The
adapter wraps `*os.File` directly and implements `net.Conn` with
stub addresses.

### `vajra-proxy`

Reverse proxy for sandbox port forwarding and browser terminals
(`cmd/vajra-proxy`, `internal/proxy/`).

- Host-aware top-level dispatch in `proxy.go`. Apex host →
  proxy's own routes (`/healthz`, terminal endpoint). Subdomain
  `<port>-<sandbox-id>.<base>` → forwarded to the agent that owns
  the sandbox, via a master lookup at
  `/internal/proxy/route`.
- Custom `http.Transport.DialContext` that opens an
  agent-CONNECT tunnel per dial, so `httputil.ReverseProxy`
  transparently handles plain HTTP, WebSocket, and protocol
  upgrades with no per-protocol code.
- In-tree minimal WebSocket implementation
  (`websocket.go`, ~200 LoC, stdlib only) — accept-key, masked
  client frames, 16-bit extended length.
- `terminal.go` bridges browser WebSocket ↔ agent's terminal
  vsock; binary frames map to data, JSON `{"resize":[r,c]}` text
  frames map to PTY resize.
- `share.go` validates share tokens via
  `/internal/proxy/validate-share` — only the SHA256 hash of the
  token is persisted in `share_links`.

### Storage layer

`internal/store/`:

- `store.go` — interface definitions
- `postgres.go` — `*Postgres` wrapping `*sqlx.DB`
- `pg_*.go` — per-table implementations
- `migrations.go` — `golang-migrate` driver

Per Day 6, migrator and server use **separate** `*sql.DB` handles.
The migrator's `Close()` shuts down its own database driver, so
sharing the pool caused the server to crash with `sql: database is
closed` after migrations ran.

## Data Flow

### Sandbox creation (cold path)

```
client → POST /v1/sandboxes
  master  : authn → rate-limit → quota check
          : Schedule = PickCluster → PickNode
          : INSERT sandboxes row (state=CREATING)
          : Operation.Start
          : dispatcher.CreateSandbox → agent
            ├── 202 + sandbox payload
  agent   : ImageCache.GetTemplate (pull if not cached)
          : sandbox dir: qcow2 overlay over template raw
          : hardlink snapshot dir (no 513 MB copy)
          : rewrite config.json (per-sandbox vsock socket)
          : spawn cloud-hypervisor
          : restore from snapshot, wait for Paused
          : Resume
          : state → RUNNING, push event
  master  : poll (or NATS subscribe) → DB state=RUNNING
          : Operation.Complete
          : Usage.RecordStart (open billing interval)
```

Measured cold-path latency on EC2 nested virt: ~1–2 s (Day 8 e2e
test). The bulk of that is *not* CH itself — see the next section.

### Sandbox creation (pool hit)

```
client → POST /v1/sandboxes
  master  : authn → rate-limit → quota check
          : Schedule
          : dispatcher.CreateSandbox(from_pool=true)
  agent   : PoolManager.AssignFromPool   (single mutex op)
          : pop warm[0]
          : vmm.ResumeVM(socketPath)     (one CH API round-trip)
          : SandboxManager.AdoptSandbox
          : return 200 with RUNNING sandbox
  master  : Operation.Complete, Usage.RecordStart
          : (background) pool refills toward target
```

Expected pool-hit latency: **5–15 ms** at the agent boundary;
end-to-end master → agent → 200 is bounded by network RTT, not VMM
work.

### Exec

```
client → POST /v1/sandboxes/{id}/exec
  master  : authn → rate-limit
          : load sandbox, find owning node
          : dispatcher.ExecCommand(node, id, cmd, timeout)
  agent   : SandboxManager.ExecCommand
          : dialer.Dial(VsockSocketPath, CID=3, port=5252)
          : write JSON request line, read JSON response line
          : return {exit_code, stdout, stderr}
  master  : marshal response, return 200
```

Day 8 e2e: single-digit-ms guest round-trip on a hot VM.

### Stop / start

```
stop:                                 start:
  master  : state → STOPPING            master  : state → STARTING
  dispatch: agent.StopSandbox           dispatch: agent.StartSandbox
  agent   : vmm.Snapshot into            agent   : vmm.RestoreVM from
            <sandbox>/state/                       <sandbox>/state/
          : DestroyVM (host frees                : wait for Paused, Resume
            CH process)                          : state → RUNNING
          : state → STOPPED                       : Usage.RecordStart
          : Usage.RecordStop
```

Both branches are idempotent. Re-issuing a stop on a STOPPED sandbox
returns 200 without error.

### Snapshot (out-of-band)

```
client → POST /v1/sandboxes/{id}/snapshot
  master  : authn → rate-limit
          : Operation.Start (OperationTypeSnapshot)
          : dispatcher.SnapshotSandbox
  agent   : SnapshotIntoDir (CH vm.pause → vm.snapshot → vm.resume)
          : write snapshot row
  master  : Operation.Complete
```

The sandbox stays RUNNING across the snapshot. Promote-to-template is
a separate `POST /v1/snapshots/{id}/promote` — currently metadata-only;
see [known-gaps.md](known-gaps.md).

### Archive

```
client → POST /v1/sandboxes/{id}/archive
  master  : state → ARCHIVING
          : Operation.Start (OperationTypeArchive)
  agent   : if RUNNING, snapshot first
          : tar+zstd <sandbox-dir> →
            /var/lib/vajra/archives/{id}.tar.zst
          : if VAJRA_S3_BUCKET set:
              upload (progress logged every 10 MB)
              delete local copy
          : remove sandbox dir + manager entry
  master  : state → ARCHIVED
          : Operation.Complete
```

Rehydrate inverts the trip — STOPPED row reappears, follow-up
`/start` re-restores from the embedded snapshot. The agent's
`RehydrateSandbox` sets `VsockSocketPath = <sandboxDir>/vsock.sock`
on the adopted sandbox — without this, the first `ExecCommand` after
rehydrate fails with `dial unix : missing address` (Day N bug).

## Storage Tiers

| Tier | Where | What | Latency |
|------|-------|------|---------|
| Hot | Local NVMe under `/var/lib/vajra/` | qcow2 overlays, CH snapshot blobs, image cache, in-progress sandbox state | µs–ms |
| Warm | Postgres (`sandboxes`, `nodes`, `operations`, ...) | All control-plane records; truth | ms |
| Warm-cached | Redis | Sandbox state (30s), node usage (10s), template meta (5min) | µs–sub-ms |
| Cold | S3 (`archives/{id}.tar.zst`) | Archived sandboxes | seconds (parallel ranged GETs) |

Hot-tier paths:

- `/var/lib/vajra/cache/{hash}/{rootfs.raw,vmlinux,snapshot/}` — image cache
- `/var/lib/vajra/sandboxes/{id}/{disk.qcow2,snapshot/,state/}` — per-sandbox
- `/var/lib/vajra/archives/{id}.tar.zst` — local archive staging

The hardlink trick in Day 8 means a new sandbox does **not** copy the
513 MB CH memory-ranges file; it hardlinks it into the sandbox
directory. CoW fallback only kicks in across a filesystem boundary.

## Pre-warm Pool

Implementation: `internal/agent/pool.go` (rewritten from the static
Day 9 version into a dynamic-sized pool).

### Why

Cold create on a sandbox does these steps before `Resume`:

1. qcow2 overlay over the template raw
2. hardlink the CH snapshot dir
3. rewrite `config.json` (per-sandbox vsock socket path)
4. spawn `cloud-hypervisor`
5. CH restore from snapshot
6. wait for `state=Paused`
7. `vm.resume`

Even on bare metal where (5) is ~150 ms, the wall-clock budget is
dominated by (1)–(4) — CH process start, file plumbing, socket
poll. **Pool members are pre-restored and paused**, so the only
work at assignment time is step (7) — one CH API round-trip,
typically 5–15 ms.

### Design

- **Ownership.** Pool members are *not* in `SandboxManager` until
  `AssignFromPool` calls `AdoptSandbox`. The earlier draft registered
  them as `FromPool=true` and was rejected: the paused guest can't
  answer the vsock health probe, so the health checker would demote
  every pool member to ERROR.
- **Restore path.** `vmm.RestoreVMPaused` is factored out of
  `RestoreVM` — shares `restoreInternal`, skips the trailing
  `client.Resume`. The pool member sits in `Paused` until assigned.
- **Dynamic sizing.** `targetSize` lives between `minSize` and
  `maxSize`. Every 30 s, `adjustTargetSize` walks the recent
  window:
  - misses > 0 → target up by `misses + 2`
  - hits == misses == 0 → target down by 1 (toward minSize)
  - hits > 0, misses == 0 → hold
  Hard cap at `maxSize` so the pool can't drown the host.
- **Replenish.** Single goroutine, 1 s tick. Spawns up to
  `DefaultPoolWarmConcurrent = 3` warm-ups in flight. Each warm-up
  reads `/proc/meminfo` and aborts if `MemAvailable / MemTotal <
  20 %`. On non-Linux dev hosts the check is skipped.
- **Stale rotation.** Members older than 10 minutes are destroyed
  on a 60 s loop — keeps dirty kernel state (timer drift, RNG
  state) from accumulating.
- **CID management.** Pool runs its own `atomic.Uint32` from 100
  with a `freeCIDs` free-list; cold sandboxes use the manager's
  allocator starting at 3. Recycled on every destroy path.
- **Concurrent assign.** `AssignFromPool` pops `warm[0]` under the
  mutex, increments `recentHits`, returns. Two simultaneous calls
  cannot return the same member — `TestConcurrentAssign` races 10
  goroutines and asserts distinct sandboxes.

### Config

```
VAJRA_AGENT_POOL_TEMPLATE=<sha256>   # required; empty disables the pool
VAJRA_AGENT_POOL_MIN_SIZE=3          # default 2
VAJRA_AGENT_POOL_MAX_SIZE=10         # default 20
```

### Observability

- `GET /pool/stats` on the agent: `{min_size, max_size, target_size,
  available, warming, total_hits, total_misses, total_created,
  hit_rate_pct, template}`
- `GET /metrics` (Prometheus text): `vajra_pool_available`,
  `vajra_pool_warming`, `vajra_pool_target`,
  `vajra_pool_hits_total`, `vajra_pool_misses_total`,
  `vajra_pool_hit_rate`
- CLI: `vajra pool stats --agent-url http://<node>:9000 [--json]`
- Dashboard: `web/src/pages/Metrics.tsx` Pre-warm Pool section
  (green ≥80 %, yellow ≥50 %, red <50 %)

## Benchmarks

All numbers below are reproducible — `scripts/benchmark.go` is the
driver, `bible.md` Day 1, Day 2, and Mega Build sections are the
log.

### Snapshot restore (Cloud Hypervisor, in isolation)

EC2 c8i.large, nested virtualization, Ubuntu Noble rootfs, 2 vCPU /
512 MB RAM, 10 consecutive `restore → destroy` cycles via the Go
shim:

| metric | value |
|--------|-------|
| min | 152.75 ms |
| avg | 161.03 ms |
| p50 | 157.54 ms |
| p95 | 175.82 ms |
| p99 | 175.82 ms |
| max | 175.82 ms |

Zero failures over 10 runs. The Go shim eliminated ~300 ms of
shell-prototype overhead (`vmm.PollSocketReady` dials the CH API
socket every 5 ms instead of `sleep 0.05`).

### End-to-end create (cold, through master)

Day 8 e2e on EC2 nested virt: `POST /v1/sandboxes` → state RUNNING
in **~1–2 s** (cold cache). The non-CH cost is in qcow2 overlay,
hardlink snapshot, config rewrite, agent ↔ master HTTP, DB writes.

Pre-Day-8 baseline (full `cp --reflink=auto`, full snapshot copy):
**~33 s** for disk-prep alone — qcow2 overlay + hardlink dropped
that to **~1 s** (≈30× speedup).

### Pool-hit assignment

Expected at the agent boundary: **5–15 ms** (single CH API
round-trip via `vmm.ResumeVM`). End-to-end through master adds
network RTT only — no disk, no CH spawn. (Live benchmark via the
new `scripts/benchmark.go --pool` flag is the next session's
work — see [known-gaps.md](known-gaps.md).)

### Projected bare-metal numbers

Nested virt on EC2 adds a non-trivial KVM hop. On bare metal we
expect:

| operation | EC2 nested virt | bare metal (projected) |
|----|----|----|
| CH snapshot restore | 161 ms avg | **80–100 ms** |
| Cold e2e create | 1–2 s | **~300 ms** |
| Pool-hit assign | n/a measured | **5–15 ms** |
| Exec round-trip | <10 ms | **<5 ms** |

### Comparison

| | Vajra | Daytona / E2B | CubeSandbox | Firecracker (raw) |
|---|---|---|---|---|
| Isolation | KVM microVM | Container (E2B) / VM (Daytona) | Container | KVM microVM |
| Cold create | ~1–2 s (EC2) / ~300 ms (bare metal projected) | sub-second | seconds | ~125 ms |
| Pool-hit assign | 5–15 ms | not exposed | n/a | not built in |
| GPU passthrough | Yes (CH + VFIO) | No (E2B) / Limited (Daytona) | No | No |
| Self-hosted | Yes | Cloud-only | Yes | Library, not a platform |
| Stateless master | Yes | Vendor | Yes | n/a |
| Source-available | Yes (this repo) | No | Yes | Yes |

Vajra is firmly in the "self-hosted, microVM, GPU-capable" cell;
Firecracker is faster on raw restore but ships as a library, not a
platform. Daytona/E2B are turnkey but cloud-only.

## Scaling Roadmap

The repo as it stands handles single-node and small multi-node
deployments without modification. The bottlenecks at each scale
tier are documented below.

### 1–10 nodes

Shape: one master replica, Postgres on the same VPC, optional
Redis + NATS. No autoscaling.

- Bottleneck: agent → master heartbeats. At 10 nodes × 1 hb/5 s,
  the master writes 2 rows/s to Postgres. Trivial.
- Solution: nothing — this is the demo / dev / pilot tier.

### 10–100 nodes

Shape: 2–3 master replicas behind a load balancer, RDS Multi-AZ,
ElastiCache Redis, managed NATS, autoscaler enabled.

- Bottleneck: per-second heartbeat writes (100 nodes × 1/5 s = 20
  writes/s) start contending with mutating-handler writes during
  load spikes.
- Solution: **NATS subscriber batches Postgres**
  (`internal/master/subscriber.go`). With NATS on, heartbeats hit
  Redis immediately and flush to Postgres every 30 s — collapsing
  the per-second write into a per-batch write.
- Bottleneck: per-node `dispatcher` HTTP client cache lives in
  process memory and is rebuilt when a node's IP changes
  (`internal/master/dispatcher_pool.go`). With 100 nodes this is
  fine; the cache is bounded by the node count.

### 100–1000 nodes

Shape: 5+ master replicas, sharded autoscaler groups per region.

- Bottleneck: scheduler full-table scans on `nodes` for every
  placement decision. Postgres handles it, but tail latency
  drifts up.
- Solution: index `nodes (cluster_id, state, last_heartbeat)`
  (already present from `001_init.up.sql` covering equality
  predicates); add a `nodes_with_capacity` materialised view
  refreshed by the heartbeat subscriber.
- Bottleneck: reconciler walks every ACTIVE node every 60 s. At
  1000 nodes and 30 s per pass we have 30 dispatch RPCs in
  flight.
- Solution: partition the reconciler by cluster (one goroutine
  per cluster, each does its own 60 s sweep). The
  `internal/master/reconciler.go::Run` loop is single-purpose
  enough that this is a 50-line change.
- Bottleneck: rate-limit buckets are per-replica in-memory
  (`internal/master/ratelimit.go`). At >1 master, a tenant's 10
  RPS is multiplied by the replica count.
- Solution: Redis-backed token buckets. Atomic `INCR` + TTL or
  `Lua` script for the refill math. Bible.md flags this as
  "defer until needed"; this is when it stops being deferrable.

### 1000–10000 nodes

Shape: regional master deployments. Single Postgres becomes a
write bottleneck.

- Bottleneck: Postgres write fan-in. 10k nodes × 1 hb/5 s = 2k
  writes/s baseline; mutating-handler writes on top.
- Solution: shard by cluster_id. The two-tier scheduler is
  exactly the seam — `PickCluster` lives entirely above any
  shard. Move `nodes`, `sandboxes`, and `operations` writes
  into per-cluster databases; keep `accounts`, `api_keys`,
  `templates` global.
- Bottleneck: cross-region image distribution. 10k nodes pulling
  a template from one origin saturates the origin's bandwidth.
- Solution: P2P image distribution (BitTorrent / IPFS / Dragonfly).
  Content-addressable design already supports this — the SHA256
  is the swarm key. Currently a known gap.
- Bottleneck: vajra-proxy is a single binary. At 10k nodes,
  per-sandbox subdomain traffic concentrates on the proxy fleet.
- Solution: deploy proxy alongside each cluster; DNS hashes
  `<sandbox-id>` into a regional proxy. The proxy's master
  lookup at `/internal/proxy/route` already includes the node
  IP — no code change.

### 10000+ nodes

Shape: hyperscaler regime. Most of the bottlenecks become
external-system problems (DNS, anycast, BGP) rather than
control-plane code.

- Bottleneck: NATS as a single bus. JetStream durable consumers
  are the conventional answer for replay-across-restarts and
  fan-out at this scale; bible.md flags the current core-NATS
  use as "defer JetStream until we need replay".
- Bottleneck: snapshot blob storage. 10k nodes × thousands of
  archives = petabytes.
- Solution: S3 with lifecycle rules (Standard → IA → Glacier).
  The current archive code uses `aws-sdk-go-v2` and respects
  `VAJRA_S3_PREFIX`, so per-tenant prefix isolation drops out
  for free.
- Bottleneck: control-plane Postgres becomes too hot even
  per-cluster.
- Solution: time-series rollups for `sandbox_usage` into
  Clickhouse / TimescaleDB; keep Postgres for the live truth
  rows only.

## Key Design Decisions

### vsock over SSH

Bible.md Day 1. SSH would have required guest network
configuration, key distribution, and a TCP listener inside every
sandbox. vsock is faster (no IP stack), needs no key management
(CH controls the CID namespace), and the CH host socket gives the
agent a single Unix-domain mount point per VM.

Trade-off paid: writing a guest agent. The win turned out to be
larger than expected — the guest now multiplexes exec, files,
terminal, and forward over four cheap vsock ports
(`scripts/guest-agent/`), each with its own tiny protocol. The
same machinery would have been a TCP+TLS+auth puzzle over SSH.

### qcow2 overlays (Day 8)

The original sandbox disk path was `cp --reflink=auto rootfs.raw
<sandbox>/disk.raw`. On a real filesystem the reflink succeeded
and creates were fast; on ext4-on-EC2 the reflink path silently
fell through to a full copy and a sandbox took 33 s to provision
just for disk prep.

Switching to a thin qcow2 overlay backed by the immutable template
raw means the per-sandbox disk is megabytes, not gigabytes. Disk
prep dropped from 33 s → 1 s, and steady-state RSS per sandbox
dropped because the overlay only stores divergent blocks. CH
accepts qcow2 as a disk image with no flag change.

### Async create

Cold create takes ~1–2 s on EC2 nested virt. Synchronous request
holding for that long is fine for the demo, brittle in
production (timeouts, client retries, connection pool
exhaustion). The handler in `internal/master/handlers_sandbox.go`
already supports the 202 + poll pattern; the pool-hit path
short-circuits to 200 because it's fast enough.

### Dynamic pool (instead of fixed N)

Bible.md "Dynamic Pre-warm Pool" section. The earlier Day 9
implementation kept exactly `POOL_MIN_SIZE` warm members. Burst
workloads either drained the pool (every burst sandbox paid the
cold cost) or wasted memory (set N high enough for bursts, idle
hosts keep N members live indefinitely).

The current pool tracks recent hits / misses and adjusts target
size between `minSize` and `maxSize`. A run of misses lifts the
target by `misses + 2`; an idle window pulls it back to
`minSize`. Stale rotation evicts members older than 10 minutes so
kernel-state drift doesn't accumulate. The pool reads
`/proc/meminfo` before warming and refuses to warm when
MemAvailable / MemTotal < 20 % — defense against thrashing the
host.

### Hybrid HTTP / NATS heartbeat

Bible.md Mega Build. NATS-only would be lower-latency and
lower-load on Postgres, but a NATS cold-start drops every event
published before `Subscribe` runs, and JetStream isn't enabled.
HTTP-only puts a per-second write on Postgres per agent — fine at
10 nodes, a bottleneck at 1000.

The current design: agents publish to both. NATS is best-effort
(Redis is the immediate consumer). HTTP is canonical (the
heartbeat handler refreshes Redis after the DB write, so
cross-master replication is bounded by 5 s regardless of NATS
health).
