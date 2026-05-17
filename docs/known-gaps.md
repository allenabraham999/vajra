# Vajra — Known Gaps

Marko's spec asked for an honest assessment of what is and isn't
built. The two earlier docs ([architecture.md](architecture.md) and
[security-threat-model.md](security-threat-model.md)) describe what
the system *is*. This document describes what it *isn't yet*.

The repo is end-to-end functional — `bible.md` Day 8 captures a full
e2e lifecycle pass against EC2 nested virt — but several features are
either partial, deferred, or design-only. Hand-waving past those would
be worse than calling them out.

## Legend

- **P0** — required before any external user load (security or
  correctness gap).
- **P1** — required for production / pricing-bearing usage.
- **P2** — nice-to-have, real demand-driven.

All effort estimates are rough engineer-hours by someone already
familiar with the codebase.

## Fully Implemented

Each line below is something the spec explicitly asked for and the
code actually does. Evidence is a test name, benchmark number, or
file reference.

| Feature | Evidence |
|---------|----------|
| Cloud Hypervisor shim with snapshot/restore | `internal/vmm/`; benchmark `min 152.75ms / avg 161.03ms / p95 175.82ms` over 10 EC2 c8i.large runs (bible.md Day 2 + Go Shim section) |
| Content-addressable image cache with concurrent-pull coalescing and LRU eviction | `internal/agent/image.go` + `image_test.go` (pull verify, hash mismatch rollback, concurrent coalescing, LRU eviction) |
| Sandbox lifecycle (CREATING → RUNNING → STOPPED → DESTROYED, plus ARCHIVED) | `internal/agent/sandbox.go` + `sandbox_test.go` (full lifecycle, template-not-found, restore-failure rollback, snapshot-failure → ERROR, exec round-trip) |
| qcow2 overlay disk (no full rootfs copy per sandbox) | `internal/agent/sandbox.go::createDisk`; Day 8 measured ~30× speedup on disk-prep (33s → 1s) |
| Hardlink snapshot dir (no 513 MB copy per sandbox) | `internal/agent/snapshot.go`; Day 8 |
| Dynamic pre-warm pool (min/max, hit-driven sizing, stale rotation, memory guard, CID recycling) | `internal/agent/pool.go` + `pool_test.go` (12 tests: warm-up, assign, miss, replenish, grow, shrink, max-cap, concurrent-assign, stale-rotation, startup race, shutdown-cleanup, CID recycling) |
| Edge-triggered guest health probe | `internal/agent/health.go` + `health_test.go` (single notification across sustained unhealthy) |
| Master API: auth, sandboxes, snapshots, templates, files, share-links, admin | `internal/master/handlers_*.go`; 17+ handler tests in `handlers_test.go` |
| JWT (HS256, 1h, alg-pinned) | `internal/master/auth.go` + `auth_test.go` (round-trip, tampered, expired, wrong-secret) |
| API keys (`vj_live_<32 hex>`, SHA256-hashed) | `internal/master/auth.go::GenerateAPIKey/HashAPIKey` |
| Two-tier scheduler (PickCluster → PickNode, worst-fit, lex tie-break) | `internal/master/scheduler.go` + `scheduler_test.go` (single fit, worst-fit, stale heartbeat, no CPU, non-active filter, lex tie-break, quota, region match/mismatch/fallthrough) |
| Quota enforcement (count + vCPU + memory) | `internal/master/scheduler.go::CheckQuota` + `scheduler_test.go` |
| Reconciliation (orphan / ghost / state-mismatch resolution) | `internal/master/reconciler.go` + `reconciler_test.go` (six drift cases) |
| Dispatcher (per-node HTTP client cache, exp-backoff retry, IP-change replacement) | `internal/master/dispatcher.go`, `dispatcher_pool.go` + `dispatcher_test.go` |
| Operations audit trail (1 KB truncation) | `internal/master/operation.go` + `operation_test.go` |
| Per-account rate limit (token bucket, default 10 RPS, anonymous bucket for `/v1/auth/*`) | `internal/master/ratelimit.go` + `ratelimit_test.go` |
| Cost tracking (sandbox_usage, vCPU/mem/disk seconds, rate pinned $0.06 vCPU·hr / $0.01 GB·hr / $0.005 GB-storage·hr) | `internal/store/pg_usage.go` + `usage_test.go` (cost-rate pinning) |
| Archive (tar+zstd → local FS or S3) with progress logging every 10 MB | `internal/agent/archive.go` + `archive_test.go`; live-tested on EC2 against `s3://vajra-archive/archives/...` (179 MB upload in ~970 ms) |
| Rehydrate (S3 download via parallel ranged GETs, restore to STOPPED, follow-up Start works) | `internal/agent/archive.go::RehydrateSandbox`; live-tested end-to-end (Day N) |
| Sandbox migration (uncompressed tar over agent → agent) | `internal/agent/migrate.go` + `migrate_test.go` (httptest, round-trip, rejects-existing, rejects-empty) |
| Vsock-based exec / files / terminal / forward (guest agent on 5252/3/4/5) | `scripts/guest-agent/`; bible.md Day 7 e2e (exec round-trip) and Day 8 e2e (full lifecycle including files + terminal) |
| `vajra-proxy` with host-aware subdomain dispatch + in-tree WebSocket | `internal/proxy/` + `proxy_test.go`, `websocket_test.go` |
| Share links (SHA256-hashed token storage, optional port + expiry) | `internal/master/handlers_share.go` + `handlers_share_test.go` (create/list/revoke/validate/proxy-lookup) |
| Redis cache (sandbox-state hint 30 s, node usage 10 s, account count 60 s, template 5 min) | `internal/cache/` + tests; integration test behind `-tags=integration` |
| NATS event bus (heartbeat, state-change, unhealthy on `vajra.*` subjects) | `internal/events/` + `internal/master/subscriber.go` + `internal/agent/publisher.go` |
| Autoscaler (EC2 launch on ErrNoCapacity, idle scale-down, admin endpoints) | `internal/master/autoscaler.go` + `autoscaler_test.go` (seven tests: scale-up, max-nodes-limit, duplicate scale-up, scale-down-idle, protects-min-nodes, skips-unmanaged, disabled passthrough) |
| CLI (`vajra`) with cobra, `--json`, `--no-color`, table renderer, config at `~/.vajra/config.json` (0o600) | `cmd/vajra/`; bible.md Day 9 |
| Python SDK (sync, `requests`-backed, dataclasses with `from_dict` unknown-field tolerance) | `sdk/python/vajra/` |
| React dashboard (Login, Overview, Sandboxes, Sandbox detail with xterm.js Terminal + Exec + Files + Snapshots, Templates, Nodes, API Keys, Usage, Admin, Metrics with pool stats) | `web/`; clean `npm run build` (633 KB chunk, 174 KB gzip) |
| End-to-end lifecycle verification on EC2 nested virt | bible.md Day 8: create → exec → stop → start → snapshot → destroy all pass |
| S3 archive integration with parallel-download rehydrate | bible.md Day N: live archive + rehydrate + start + exec round-trip on EC2 |
| AF_VSOCK net.Conn adapter (Day 7 fix: `getsockname` doesn't support AF_VSOCK) | `scripts/guest-agent/vsock.go::vsockNetConn` |
| Migrator/server pool isolation (Day 6 fix: dedicated `*sql.DB` for golang-migrate) | `cmd/vajra-master/main.go::runMigrations` |

## Partially Implemented

These features exist in some form but are missing important pieces.
The "what's missing" column is what would need to land before
calling the feature done.

| Feature | What works | What's missing | Effort | Priority |
|---------|-----------|-----------------|--------|----------|
| **Custom template builds** | `POST /v1/templates/build` is real: `scripts/build-custom-template.sh` copies the Ubuntu base rootfs, runs the caller's setup script in a chroot, boots + snapshots the VM, and lays the triple into the agent image cache. Sandboxes boot from the result. | Templates derive from the Ubuntu 24.04 base only — no arbitrary base images (the agent restores snapshots, never cold-boots, so a from-scratch image would not match the project kernel/cmdline). The build also lays the triple only into the build host's cache; multi-node distribution of custom templates isn't wired (the download endpoint serves `rootfs.raw`, builds produce `rootfs.qcow2`). | 20–40 h (arbitrary bases); 4–6 h (distribution) | P2 |
| **Snapshot promotion to template** | `POST /v1/snapshots/{id}/promote` inserts a `templates` row with the caller's name/version | The agent doesn't promote the on-disk blob into the content-addressable image cache. Promoted templates are unusable until that's wired. | 4–6 h | P1 |
| **Usage rollups (`/v1/usage`)** | Endpoint returns a synthesised approximation; dashboard's Usage page falls back to client-side synthesis | Real rollup from `sandbox_usage` table (the data is being written; the reader isn't built). Day 10's `RecordStart/RecordStop` are in place. | 6–10 h | P1 |
| **Async create (202 + operation polling)** | Pool-hit path returns 200 immediately. Cold path on a busy host still holds the request for ~1–2 s. | Move cold creates to 202 + operation polling so the client never waits on the wire. Plumbing exists (`Operation` rows already drive completion); needs a worker pool on the master side. | 8–12 h | P1 |
| **Admin role check** | Admin endpoints + dashboard tab + `vajra admin *` CLI are wired and tested | All gated by env-pinned `ADMIN_ACCOUNT_ID` equality check, not a real `accounts.role` column. The localStorage `vajra.admin=1` flag in the dashboard mirrors that. | 3–4 h (column + migration + middleware) | P1 |
| **Reconciler ↔ agent listing** | Reconciler resolves orphan/ghost via `AgentLister`; master ships `GET /sandbox/list` on the agent | `dispatcher.SnapshotSandbox` and `dispatcher.ListSandboxes` targets exist on the master; the agent's `server.go` ships the list endpoint but snapshot-via-master still has rough edges. | 2–3 h | P2 |
| **Cost ledger finalisation on stale sandboxes** | `UsageStore.RecordStart/RecordStop` close intervals on lifecycle events; reconciler can call `FinalizeOpenIntervals` | Not wired into a periodic sweep. A Postgres outage between `UpdateState(RUNNING)` and `RecordStart` would lose a sandbox's billing start. | 1–2 h (hook into reconciler tick) | P1 |
| **Multi-master rate limit** | Per-replica in-memory bucket via `sync.Map` of atomics | Buckets sharded across replicas — a tenant gets `N × 10 RPS` with N replicas. Needs Redis-backed bucket (atomic INCR + Lua refill). | 4–6 h | P1 (when running >1 master) |
| **Pool live verification** | Pool code, tests, benchmark harness flags (`--pool --agent-url --template`), `/pool/stats`, dashboard tile all done | Live EC2 verification was deferred when the test host was unreachable. Build / vet / unit tests / web tsc all green. | 1–2 h on a reachable EC2 host | P2 |
| **Live (in-flight) migration** | Offline migrate works end-to-end (stop → tar stream → register → start). | True live migration would need CH's checkpoint-stream mode and a longer conversation across the agent's vsock socket. | 12–20 h | P2 |
| **Archive S3 CI fixture** | S3 path live-tested on EC2 with `vajra-archive` bucket. Local filesystem path covered by unit tests. | No `localstack`/`minio` integration test in CI for the S3 branch — works in practice, not asserted on every commit. | 2–4 h | P2 |
| **TypeScript SDK (`sdk/typescript/`)** | Directory scaffolded. React dashboard has an in-tree axios client typed against the Go models. | Empty package — needs `package.json`, exports, build, types extracted from `web/src/api/client.ts`. | 4–8 h | P1 |
| **Async Python SDK** | Sync client done | No `httpx.AsyncClient` variant; concurrent fan-out requires user-side threading. | 3–6 h | P2 |
| **CLI integration tests** | All commands wired and manually verified | The cobra wiring + HTTP shim has no test file. `httptest`-backed master fake is the right shape. | 3–5 h | P2 |
| **Per-share auth** | Share token validates against SHA256 hash; optional port + expires_at | No password-protected links, no IP allowlists, no view-count caps. | 2–4 h per knob | P2 |
| **File-upload curl examples** | API expects `POST /v1/sandboxes/{id}/files/upload` with `X-Vajra-Path` header + `application/octet-stream`. Dashboard and SDK send this. | Older notes / SDK READMEs still reference a multipart form. The master no longer accepts that. | 30 min cleanup | P1 |
| **Agent restart resilience for billing** | Lifecycle events close intervals. | Agent crash between `RUNNING` and graceful stop leaves intervals open. Reconciler hammer covers it once wired. | 1–2 h (same fix as cost ledger finalisation) | P1 |
| **CH process re-parenting** | Day 9 SIGTERM in `cmd/vajra-agent/main.go` cleanly shuts the pool down. | If the agent crashes without SIGTERM, CH processes linger as orphans. They get reaped by `Reconciler` only after master sees the node go stale (90 s). | 2–3 h (track CH PID in a per-node state file, scan on agent restart) | P2 |
| **`VAJRA_S3_PREFIX` doc** | Code honours it (`.env.example` comments) | Not documented in `docs/deploy.md`. | 15 min | P2 |
| **Heartbeat batching observability** | NATS subscriber batches; toggle is `NATS_URL` | No metric on how many heartbeats fold into one DB write. Useful for capacity tuning. | 1 h (add gauge to `/metrics`) | P2 |
| **Per-account roles for templates** | Templates are global. | A team posting a private template to share with only their org has no isolation today — every account sees every template. | 6–10 h | P1 |

## Design-Only Features

Documented in `bible.md` / `CLAUDE.md` / spec and *not* built. These
are the items where saying "we shipped this" would be hand-waving.

| Feature | Why it matters | Steps to complete | Effort | Priority |
|---------|----------------|-------------------|--------|----------|
| **Live migration** | Currently offline only — sandbox is stopped during the tar stream. For workloads that can't tolerate a stop, live migration is the SLO-relevant feature. | Wire CH's checkpoint-stream mode; agent on the target opens a CH process in restore-from-stream mode; agent on the source pipes CH checkpoint output over a TLS socket; on cutover, source pauses, last delta streams, target resumes. | 30–40 h | P2 |
| **Multi-region** | Single-cluster today. Operationally, master + RDS + agents all live in one region. | Region column on `clusters` (column exists; not respected end-to-end). Per-region master deployment, per-region RDS, GeoDNS or per-region API endpoints. Two-tier scheduler already filters by region at `PickCluster`. | 20–40 h | P1 (for any GA deploy) |
| **P2P image distribution** | Today every agent pulls templates from the master / S3 / template registry directly. At 1000+ agents this saturates the origin. | Dragonfly / Kraken / IPFS overlay; the SHA256 in the image cache is already a swarm key. Each agent acts as a peer; first agent to fetch seeds neighbours. | 40–80 h | P2 |
| **TLS everywhere** | Master ↔ agent traffic is plain HTTP behind a shared secret. Proxy ↔ client is HTTP unless terminated upstream. | (a) Add `TLSConfig` + cert/key flags to the agent + proxy listeners. (b) Switch master ↔ agent to mTLS with an internal CA. (c) Document the upstream-LB TLS pattern in `deploy.md`. | 12–20 h | P0 for production |
| **Full billing / Stripe integration** | Cost is computed in `internal/store/pg_usage.go`; no one is being charged today. | Stripe (or Lago) integration in master: monthly usage rollup → invoice. Credit card on file via Stripe Elements on the dashboard. Free-credits banner on dashboard already shows $200; the credit ledger needs a real table. | 30–60 h | P1 |
| **GPU passthrough testing** | CH was specifically chosen over Firecracker for VFIO/GPU support; not exercised today. | Pin a node with a GPU; add `gpu` to `nodes.capacity`; teach `PickNode` to filter on requested GPUs; add `gpu_count` to `sandboxes.config`; verify CH VFIO config; smoke-test an Ubuntu rootfs that loads the NVIDIA driver from inside. | 16–30 h (assuming a GPU host is available) | P2 |
| **Production observability (Prometheus + Grafana)** | Agent exposes `/metrics` (Prometheus text-format) with pool gauges + counters. Master has `/health` + `/v1/version` only. | Add `/metrics` on master with: request rate, p95 by route, schedule attempts, schedule failures by reason, Postgres pool stats, Redis hit rate, NATS lag, autoscaler launches. Ship a Grafana dashboard JSON under `docs/`. | 12–20 h | P1 |
| **eBPF network isolation** | Vajra is vsock-only today, so cross-sandbox network isolation is trivially correct. The moment we add virtio-net + a shared bridge, eBPF/Cilium becomes mandatory. | Pin Cilium as the CNI; per-tenant network policy generated from the `accounts` table; default-deny ingress on the bridge. | 20–30 h | P2 (conditional on virtio-net) |
| **MFA (TOTP) on the dashboard** | bcrypt + JWT today. | Add a `users` table or column on `accounts` for `totp_secret`; `/v1/auth/login` returns "totp_required" if set; `/v1/auth/login-totp` accepts the code. The dashboard prompts for the 6-digit. | 6–10 h | P1 |
| **API key + JWT secret rotation** | Static `JWT_SECRET`, no rotation. API keys revoked manually. | (a) JWT: accept current + previous secret; rotate by promoting "next" → "current" → "previous". (b) API key: `POST /v1/api-keys/{id}/rotate` issues new key under same name, revokes old at next reaper pass. | 8–12 h | P1 |
| **Audit log shipping** | `operations` table is the audit log; no export. | Add a sink (CloudWatch Logs / Loki / Splunk) on operation INSERT. Append-only so a compromised master can't rewrite history. | 6–10 h | P1 |
| **CH process supervision** | Today `vajra-agent` spawns CH via `os/exec`; if the agent crashes ungracefully, CH processes linger. | Track CH PID per sandbox in a per-node state file; on agent startup, scan and re-adopt or kill orphans. | 4–6 h | P1 |
| **JetStream-backed NATS** | Core NATS today; cold-start drops events published before `Subscribe` runs. | Convert subjects to JetStream streams; durable consumers per master replica; ack semantics on heartbeat → DB write. | 8–12 h | P2 |
| **Sandbox auto-stop / idle timer** | A sandbox stays RUNNING until the tenant stops it — no idle-timeout safety net. | Add `idle_timeout_minutes` to `sandboxes.config`; reconciler sees no exec / network activity for N minutes → issues Stop. Requires per-sandbox last-activity tracking. | 8–12 h | P1 |
| **Cosign signing on templates** | Templates are content-addressed by SHA256; integrity is verified but provenance is not. | Sign the SHA256 with `cosign sign`; agent verifies signature before `ImageCache.Pull` returns. Signing key on the master / template publisher. | 4–8 h | P2 |
| **Egress controls** | Sandboxes can make arbitrary outbound connections. | Host-level nftables / eBPF egress rules generated from per-account policy. | 12–20 h | P2 |
| **SMT-off / core-pinning for high isolation** | Today, two tenants can share hyperthread siblings of one physical core. | Add `isolation_class` to `accounts` (e.g. "shared", "isolated"). Isolated tenants get exclusive physical-core pinning; scheduler considers SMT siblings. Requires host kernel boot with `nosmt`. | 16–24 h | P2 |
| **SBOM emission on release** | No CI configured in this repo. | Add a release workflow that runs `syft` against the build artefacts and uploads SBOMs alongside binaries. | 2–4 h | P1 |

## Notes on Honesty

A few specifics that are worth not papering over:

- **Pool live verification was not run** — bible.md "Dynamic Pre-warm
  Pool" section flags this. The pool's code, tests, dashboard tile,
  benchmark flags, and Prometheus metrics all exist. The
  "this number on EC2 is X ms" line is *not* recorded because the
  EC2 host was unreachable at build time. Numbers will land in the
  next session.
- **Bare-metal benchmarks are projections** — every "80–100 ms" or
  "300 ms cold create" number in `docs/architecture.md` projected to
  bare metal is exactly that: a projection. The measured numbers are
  EC2 nested virt. Bare-metal hardware was not part of this build.
- **The `/v1/usage` endpoint is a stub** — the dashboard's
  Usage/Billing page synthesises numbers client-side from a sandbox
  list when the stub returns. The cost-rate math is pinned by unit
  test, but the rollup query isn't written.
- **No CI pipeline** — `go test ./...`, `go vet ./...`, `npm run
  build`, and the local docker-compose are all invoked by hand or by
  `Makefile`. There's no GitHub Actions / Buildkite / CircleCI in
  the repo. Reviews catch what tests don't.
- **Snapshot promotion is metadata-only** — promoting a snapshot to
  a template inserts a `templates` row but the agent does not
  promote the on-disk blob into the content-addressable cache.
  Promoted templates are not yet bootable until that wiring lands.
- **Multi-replica master has not been run** — single-master
  deployment is exercised continuously. The "stateless" claim is
  load-bearing on the design but not on operations. Rate limit and
  in-flight operation tracking would behave differently across
  replicas; both are listed above as P1 gaps.
- **No GPU was tested** — Cloud Hypervisor was picked specifically
  for VFIO/GPU support. That decision is documented and the
  scheduler scaffolding is general enough to handle it, but a real
  GPU passthrough test is on the design-only list.

This is the honest picture of where the system stands. The
demo-path works end-to-end (Day 8 e2e: create → exec → stop → start
→ snapshot → destroy on a deployed EC2 master + agent + guest;
Day N e2e: archive → S3 round-trip → rehydrate → start → exec); the
production-shape path has the gaps above.
