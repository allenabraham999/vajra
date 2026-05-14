# Vajra — Deployment Guide

End-to-end production deployment guide. Reads top-to-bottom: every
section depends on the one above. The "Local development" section
at the bottom is for `git clone` → working master on a laptop in
under five minutes.

The architecture rationale, threat model, and known gaps live in
[architecture.md](architecture.md), [security-threat-model.md](security-threat-model.md),
and [known-gaps.md](known-gaps.md).

## Prerequisites

### Bare-metal host (one or more)

- Ubuntu 22.04 LTS or 24.04 LTS, x86_64.
- `/dev/kvm` present and writable by the agent user. On a fresh
  Ubuntu install: `sudo apt-get install -y qemu-kvm` (we don't run
  qemu — this is the easiest way to get the udev rules + the
  `kvm` group + permissions right) then `sudo usermod -aG kvm
  $USER`.
- Hardware virtualization enabled in BIOS (`grep -c
  'vmx\|svm' /proc/cpuinfo` returns > 0).
- At least 16 GB RAM and 200 GB NVMe per host for the demo. For
  production, size to the per-tenant sandbox profile + 20 %
  headroom (the pool memory-guard refuses to warm below
  `MemAvailable / MemTotal = 20 %`).
- Outbound HTTPS to the master endpoint and (if archives ship to
  S3) to `s3.<region>.amazonaws.com`.

### Master host

- Ubuntu 22.04+ or any Linux capable of running a single Go
  binary. No KVM required on the master.
- Postgres 16 reachable from master (RDS, ElastiCache-co-located
  RDS, or a Postgres container).
- Optional: Redis 7 reachable from master. Optional: NATS
  reachable from master + every agent.

### Cloud Hypervisor

Cloud Hypervisor is required on every bare-metal host. It is the
VMM the agent shells out to via the Unix-socket REST API
(`internal/vmm/`).

```bash
# pick the latest CH release that has snapshot/restore support
# (Vajra's benchmark numbers were measured on CH v43+)
CH_VERSION=v43.0
curl -L -o /tmp/cloud-hypervisor \
    "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor"
sudo install -m 0755 /tmp/cloud-hypervisor /usr/local/bin/cloud-hypervisor
cloud-hypervisor --version
```

Vajra picks CH up from `$PATH` by default. Override with
`VAJRA_CH_BIN=/path/to/cloud-hypervisor` if you ship multiple
versions side-by-side.

Why CH instead of Firecracker: see
[architecture.md → Technology Choices](architecture.md#cloud-hypervisor-not-firecracker--incus--gvisor).
Short version: VFIO/GPU passthrough, Rust upstream, more permissive
roadmap.

### Postgres

Either of:

**Local docker-compose:**

```bash
docker compose up -d postgres
# default DSN: postgres://vajra:vajra@localhost:5432/vajra
```

**AWS RDS PostgreSQL 16:**

1. Create the instance in the same VPC as the master nodes.
2. Security group inbound 5432 from the master SG **only**.
3. Multi-AZ on for production. `db.t4g.medium` is sufficient for
   the demo.
4. Set the DSN:

   ```
   DATABASE_URL=postgres://vajra:PASSWORD@vajra-db.xxxxx.ap-south-1.rds.amazonaws.com:5432/vajra?sslmode=require
   ```

Migrations are idempotent — running `vajra-master` against a fresh
RDS instance applies them on startup; running a second master
against the same DB is a no-op.

### Redis (optional)

Set `REDIS_URL=redis://hostname:6379/0`. Effect on master
(`internal/cache/keys.go`):

- Sandbox state cached 30 s
- Node usage cached 10 s (refreshed on every heartbeat)
- Account sandbox-count cached 60 s
- Template metadata cached 5 min

Master started with no `REDIS_URL` falls through to
`NoopCache` — every read hits Postgres. No reduction in
correctness, only throughput.

### NATS (optional)

Set `NATS_URL=nats://nats.internal:4222`. Effect:

- Agents publish `vajra.node.heartbeat` / `vajra.sandbox.state_changed`
  / `vajra.node.unhealthy` (`internal/agent/publisher.go`).
- Master subscriber writes Redis directly and batches Postgres every
  30 s (`internal/master/subscriber.go`), eliminating the per-second
  DB heartbeat write at >10 nodes.

Master started with no `NATS_URL` falls through to `NoopBus` and
the HTTP heartbeat path remains canonical.

## Deploy: `vajra-master`

Build:

```bash
make build
# produces ./bin/vajra-master (~11.6 MB stripped)
```

### Environment variables

Required:

| Var | Notes |
|-----|-------|
| `DATABASE_URL` | libpq-compatible DSN. RDS: `?sslmode=require`. |
| `JWT_SECRET` | ≥ 32 bytes. Rotation strategy: see `known-gaps.md`. |
| `AGENT_SHARED_SECRET` | Internal endpoints use constant-time compare against this. Same value on every agent's `MASTER_SHARED_SECRET`. |
| `LISTEN_ADDR` | e.g. `:8080`. |

Optional:

| Var | Default | Purpose |
|-----|---------|---------|
| `MIGRATIONS_DIR` | `./migrations` | Path passed to golang-migrate. |
| `RECONCILE_INTERVAL` | `60s` | Drift reconciliation cadence. |
| `ADMIN_ACCOUNT_ID` | unset | Account ID that gets admin routes; placeholder until role column lands. |
| `VAJRA_RATE_LIMIT_RPS` | `10` | Per-account token-bucket rate. |
| `REDIS_URL` | unset → NoopCache | See above. |
| `NATS_URL` | unset → NoopBus | See above. |
| `PUBLIC_BASE_DOMAIN` | unset | Shareable URL suffix. Used by `handlers_share.go` to build `<port>-<id>.<base>` URLs and by the proxy host parser. |
| `VAJRA_VERSION` / `VAJRA_COMMIT` / `VAJRA_BUILT_AT` | from ldflags | Override the build stamp for dev. |

Autoscaler (only read when `VAJRA_AUTOSCALE_ENABLED=true`):

| Var | Notes |
|-----|-------|
| `VAJRA_AUTOSCALE_AMI` | AMI ID for fresh agent hosts. |
| `VAJRA_AUTOSCALE_REGION` | EC2 region. |
| `VAJRA_AUTOSCALE_INSTANCE_TYPE` | Default `c8i.large`. |
| `VAJRA_AUTOSCALE_SECURITY_GROUP` | SG that allows master ↔ agent traffic. |
| `VAJRA_AUTOSCALE_SUBNET_ID` | Subnet for new agent instances. |
| `VAJRA_AUTOSCALE_KEY_PAIR` | EC2 key pair name (for SSH troubleshooting). |
| `VAJRA_AUTOSCALE_S3_BUCKET` | Bucket where the agent + CH binaries live; user-data pulls from here. |
| `VAJRA_AUTOSCALE_MASTER_URL` | Master URL the new agent registers against. |
| `VAJRA_AUTOSCALE_MIN_NODES` | Default `1`. Never scales below. |
| `VAJRA_AUTOSCALE_MAX_NODES` | Default `50`. Never scales above. |
| `VAJRA_AUTOSCALE_COOLDOWN_MINS` | Default `15`. Idle minutes before scale-down candidate. |

### Run

```bash
export DATABASE_URL=postgres://vajra:vajra@localhost:5432/vajra?sslmode=disable
export JWT_SECRET=$(openssl rand -hex 32)
export AGENT_SHARED_SECRET=$(openssl rand -hex 32)
export LISTEN_ADDR=:8080
./bin/vajra-master
```

The process prints `migrations applied` then `vajra-master
listening`. The migrator and server use **separate** Postgres pool
handles — sharing them was the Day 6 bug. If you see `sql: database
is closed` after a successful start, you are running an older
binary.

### Migrations

Migrations live in `migrations/`:

```
001_init.up.sql / 001_init.down.sql
002_share_links.up.sql / 002_share_links.down.sql
```

Tables created: `accounts`, `api_keys`, `clusters`, `nodes`,
`sandboxes`, `snapshots`, `templates`, `operations`,
`sandbox_usage`, `share_links`.

Master applies migrations at startup via `golang-migrate`. To
preview SQL without starting master:

```bash
docker run --rm -v $PWD/migrations:/m migrate/migrate \
    -path=/m -database=$DATABASE_URL up
```

### Health verification

```bash
curl http://localhost:8080/health
# {"status":"ok","db":"ok"}
curl http://localhost:8080/v1/version
# {"version":"...","commit":"...","built_at":"..."}
```

If `/health` shows `db: failed` for more than the reconciler
interval, the migrator may have closed the shared pool — see
"Run" above.

## Deploy: `vajra-agent`

Build (on a Linux build host, or cross-compile from a Mac):

```bash
make build       # produces ./bin/vajra-agent (~9.6 MB stripped)
# or for a cross-compile from Mac:
GOOS=linux GOARCH=amd64 go build -o bin/vajra-agent ./cmd/vajra-agent
```

### Environment variables

Required:

| Var | Notes |
|-----|-------|
| `MASTER_URL` | `http://master.internal:8080` |
| `MASTER_SHARED_SECRET` | Must match master's `AGENT_SHARED_SECRET`. |
| `AGENT_LISTEN_ADDR` | e.g. `:9000`. Master dispatches to this. |
| `NODE_ID` | Stable per host. UUID is fine. |
| `CLUSTER_ID` | Cluster this node belongs to (admin must create the cluster row before the agent registers). |
| `HOSTNAME` | Hostname master logs as. |
| `NODE_IP` | IP that master + proxy will dial. |

Capacity (override if hardware differs from the host's actual numbers):

| Var | Default |
|-----|---------|
| `NODE_VCPU` | `runtime.NumCPU()` |
| `NODE_MEMORY_MB` | from `/proc/meminfo` |
| `NODE_DISK_GB` | from `df` on `/var/lib/vajra` |

Image / sandbox state:

| Var | Default |
|-----|---------|
| `VAJRA_AGENT_STATE_DIR` | `/var/lib/vajra` |
| `VAJRA_CH_BIN` | `cloud-hypervisor` from `$PATH` |
| `VAJRA_AGENT_IMAGE_CACHE_BYTES` | soft cap on `/var/lib/vajra/cache/`, see `internal/agent/image.go::EvictLRU` |

Pool (optional):

| Var | Default | Purpose |
|-----|---------|---------|
| `VAJRA_AGENT_POOL_TEMPLATE` | unset → pool disabled | SHA256 of the template to keep warm. |
| `VAJRA_AGENT_POOL_MIN_SIZE` | `2` | Minimum warm members. |
| `VAJRA_AGENT_POOL_MAX_SIZE` | `20` | Hard cap. |

Archive / S3 (optional):

| Var | Default | Purpose |
|-----|---------|---------|
| `VAJRA_S3_BUCKET` | unset → archives stay on local FS | Archive destination bucket. |
| `VAJRA_S3_REGION` | unset | Region of the archive bucket. |
| `VAJRA_S3_PREFIX` | `archives/` | Key prefix; objects land at `<prefix><id>.tar.zst`. |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | unset | Explicit creds; if unset, SDK's `LoadDefaultConfig` picks up IAM roles / IRSA. |

Bus / cache (optional):

| Var | Default |
|-----|---------|
| `NATS_URL` | unset → publisher disabled |
| `REDIS_URL` | unset (agent reads no Redis today, but the var is reserved) |

### systemd unit

```ini
# /etc/systemd/system/vajra-agent.service
[Unit]
Description=Vajra Node Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=vajra
Group=kvm
EnvironmentFile=/etc/vajra/agent.env
ExecStart=/usr/local/bin/vajra-agent
Restart=on-failure
RestartSec=5s
KillSignal=SIGTERM
TimeoutStopSec=30s

[Install]
WantedBy=multi-user.target
```

The `vajra` user must be in the `kvm` group for `/dev/kvm`
access. `/etc/vajra/agent.env` holds the env vars above.

```bash
sudo useradd -r -s /usr/sbin/nologin -G kvm vajra
sudo mkdir -p /var/lib/vajra && sudo chown vajra:vajra /var/lib/vajra
sudo install -m 0755 ./bin/vajra-agent /usr/local/bin/vajra-agent
sudo systemctl daemon-reload
sudo systemctl enable --now vajra-agent
sudo journalctl -u vajra-agent -f
```

The first log line you should see is `agent: registering with
master`. The master logs `internal: node registered` on the
matching side.

## Deploy: `vajra-proxy`

Build:

```bash
make build       # produces ./bin/vajra-proxy
```

### Environment variables

| Var | Notes |
|-----|-------|
| `PROXY_LISTEN_ADDR` | e.g. `:443` (with TLS) or `:8081` (behind an upstream LB doing TLS). |
| `MASTER_URL` | Used to call `/internal/proxy/route` for sandbox lookups and `/internal/proxy/validate-share` for shareable links. |
| `MASTER_SHARED_SECRET` | Same secret as `AGENT_SHARED_SECRET` on master. |
| `PUBLIC_BASE_DOMAIN` | The proxy parses subdomains as `<port>-<sandbox-id>.<base>`. Must match the master's `PUBLIC_BASE_DOMAIN`. |
| `PROXY_TLS_CERT` / `PROXY_TLS_KEY` | Optional. If unset, proxy serves plain HTTP — assume TLS termination upstream. |

### DNS

Point a wildcard `*.<base-domain>` at the proxy. Each running
sandbox is reachable at `<port>-<sandbox-id>.<base-domain>`. The
proxy's `internal/proxy/proxy.go` parses that and asks the
master for the owning node.

### TLS

For production, terminate TLS either at the proxy (cert/key
flags) or at an upstream LB (NLB + ACM, Cloudflare, etc.). The
proxy speaks WebSockets transparently through the custom
`http.Transport.DialContext`, so a TLS-terminating LB does not
need any per-protocol config.

## Verify the install

### 1. Register a first account

```bash
curl -X POST http://master.internal:8080/v1/auth/register \
    -H 'Content-Type: application/json' \
    -d '{"email":"you@example.com","password":"hunter2hunter2"}'
# {"account_id":"...", "api_key":"vj_live_..."}
```

The raw `api_key` is shown once. Persist it. After this, master
only stores the SHA256 hash.

### 2. Configure the CLI

```bash
./bin/vajra login --email you@example.com --password hunter2hunter2 \
    --api-url http://master.internal:8080
# writes ~/.vajra/config.json (0o600)
./bin/vajra version
```

### 3. Register a template

```bash
./bin/vajra template list
# initially empty
```

Templates are content-addressable (`/var/lib/vajra/cache/<hash>/`).
The simplest path is to register from a pre-built rootfs the agent
already has:

```bash
# on the agent host, pre-stage the template files:
sudo mkdir -p /var/lib/vajra/cache/<sha256>
sudo cp ubuntu-noble-rootfs.raw /var/lib/vajra/cache/<sha256>/rootfs.raw
sudo cp vmlinux /var/lib/vajra/cache/<sha256>/vmlinux
sudo cp -r snapshot /var/lib/vajra/cache/<sha256>/snapshot
# then register the metadata via master:
curl -X POST http://master.internal:8080/v1/templates \
    -H "Authorization: Bearer vj_live_..." \
    -H 'Content-Type: application/json' \
    -d '{"name":"ubuntu-noble","image_hash":"<sha256>","vcpu":1,"memory_mb":512}'
```

For the Ubuntu Noble template recipe (libguestfs + virt-customize),
see the Day 7 entry in `bible.md` — `virt-customize` injects the
guest agent and systemd unit without loop-mounting the rootfs.

### 4. Create a sandbox and exec into it

```bash
./bin/vajra sandbox create --template <sha256> --vcpu 1 --memory 512
# {"id":"...", "state":"CREATING", ...}

./bin/vajra sandbox list
# state goes CREATING → RUNNING in ~1–2s on EC2 nested virt
#                                  (~300ms projected on bare metal)

./bin/vajra sandbox exec <id> -- 'hostname && uname -a'
# {"exit_code":0,"stdout":"cloud\nLinux cloud 6.12.8+ ...","stderr":""}
```

### 5. Verify the lifecycle

The Day 8 end-to-end:

```bash
./bin/vajra sandbox stop <id>      # → STOPPED
./bin/vajra sandbox start <id>     # → RUNNING (re-restored from saved state)
./bin/vajra snapshot create --sandbox <id>
./bin/vajra sandbox archive <id>   # → ARCHIVED (S3 if VAJRA_S3_BUCKET set)
./bin/vajra sandbox rehydrate <id> # → STOPPED
./bin/vajra sandbox destroy <id>   # → DESTROYED
```

If any step returns a non-2xx, `vajra` prints the master's
`{error, status}` body verbatim — that's the message to grep for
in the master log.

## Multi-node setup

Once the single-node deployment passes the lifecycle check, adding
a second node is mostly a copy of the agent steps.

### Steps

1. **Provision the second host** — same prerequisites: Ubuntu,
   `/dev/kvm`, CH installed, `vajra` user in `kvm` group.
2. **Create the cluster row** if it doesn't exist yet (the master
   admin endpoint or a direct SQL INSERT works; today there is no
   `cluster create` CLI command — admin operations land in the
   admin role rework). The cluster is the unit `PickCluster`
   filters on.
3. **Set distinct `NODE_ID`s** — both agents must use the same
   `CLUSTER_ID` but different `NODE_ID`s. UUIDs.
4. **Match `MASTER_SHARED_SECRET`** with master's
   `AGENT_SHARED_SECRET` on both hosts.
5. **Verify registration**:

   ```bash
   curl -H "Authorization: Bearer vj_live_..." \
       http://master.internal:8080/v1/admin/nodes
   ```
   You should see both nodes ACTIVE with `last_heartbeat` < 90 s
   ago. Master only schedules onto nodes that are ACTIVE *and*
   heartbeated within `heartbeatStaleAfter = 90s`
   (`internal/master/scheduler.go::PickNode`).
6. **Drive a few creates and watch placement spread.**
   `Scheduler.PickNode` is worst-fit (highest remaining capacity
   wins). With two empty nodes of equal capacity, the lex tie-break
   on `id` is deterministic; once one is loaded, the other becomes
   the winner of subsequent placements.

   `scheduler_test.go::TestPickNode_*` exercises this with table
   fixtures; the real-world test is just creating sandboxes and
   reading `/v1/admin/nodes` between calls.

### Drain a node

```bash
./bin/vajra node drain <node-id>
# moves node to DRAINING; scheduler stops considering it for new placements;
# existing sandboxes keep running until stopped/destroyed by the operator
```

Drain is admin-gated (`ADMIN_ACCOUNT_ID` env on master). See
[known-gaps.md](known-gaps.md) for the planned move to a real role
column.

### Optional: enable the autoscaler

When the cluster is busy enough that capacity becomes the limiter,
turn on the autoscaler (master env vars under
"Deploy: `vajra-master`"). Master then launches additional
EC2 instances tagged `vajra:managed=true` when `Schedule` returns
`ErrNoCapacity`, and reaps idle managed nodes every 5 min once they
pass `VAJRA_AUTOSCALE_COOLDOWN_MINS`.

Inspect:

```bash
./bin/vajra admin autoscale status
./bin/vajra admin autoscale trigger      # force a launch
```

Manually-registered (non-managed) nodes are never terminated.

## Local development

For a local single-process loop:

```bash
docker compose up -d postgres redis nats
make build
source .env.example     # fills in DATABASE_URL, JWT_SECRET, etc.
./bin/vajra-master
```

`docker-compose.yml` brings up Postgres + Redis + NATS together.
Leave `REDIS_URL` / `NATS_URL` unset to fall back to NoopCache /
NoopBus.

The bare-metal agent doesn't run on a Mac (no `/dev/kvm`). For
agent code development on macOS:

- Unit tests work — they use a `fakeVMM` that doesn't call CH.
  Run `go test ./internal/agent/...`.
- Integration tests against CH need a Linux KVM host. The
  `13.126.126.66` EC2 host (from `CLAUDE.md`) is the project's
  reference.

The React dashboard:

```bash
cd web
npm install
npm run dev   # proxies /v1/* to http://localhost:8080
```

## Production: separate datastores

`docker-compose.prod.yml` brings up Redis + NATS only — Postgres is
externalised to RDS:

```bash
docker compose -f docker-compose.prod.yml up -d
```

Set the master's `DATABASE_URL` to the RDS endpoint and start
master normally. The compute side (agents + CH) stays on bare
metal so the 80–150 ms restore target holds.

### Backward compatibility

Every new dependency is opt-in:

- `REDIS_URL` unset → NoopCache, every read hits Postgres.
- `NATS_URL` unset → NoopBus, HTTP heartbeats only.
- `VAJRA_AUTOSCALE_ENABLED` unset/false → 503 on no-capacity,
  current behaviour.

Master started with none of those set is behaviourally identical
to the pre-mega-build version.
