# Vajra — Security and Threat Model

This document walks the full trust chain from the public internet down
to the guest kernel, names the realistic threats at each boundary,
points to the file that defends against each one, and lists what is
*not* defended against today.

If you are reading this for a deployment review: skip to the
"Production security recommendations" section at the bottom for the
hardening checklist.

## Trust Boundaries

```
                          (less trusted)
   ┌───────────────────────────────────────────────────────┐
   │                       Internet                        │
   └───────────────────────────────┬───────────────────────┘
                                   │ TLS terminate at LB
                                   ▼
   ┌───────────────────────────────────────────────────────┐
   │   B1: vajra-master  (JWT/API key, rate-limit, quota)  │
   └───────────────────────────────┬───────────────────────┘
                                   │ Internal-only HTTP
                                   │ Bearer = agent_shared_secret
                                   ▼
   ┌───────────────────────────────────────────────────────┐
   │   B2: vajra-agent   (one per bare-metal host)         │
   │       owns: image cache, sandbox dirs, /dev/kvm       │
   └───────────────────────────────┬───────────────────────┘
                                   │ Unix-socket REST
                                   ▼
   ┌───────────────────────────────────────────────────────┐
   │   B3: cloud-hypervisor process (per VM)               │
   │       Rust, minimal device surface, seccomp           │
   └───────────────────────────────┬───────────────────────┘
                                   │ KVM /dev/kvm + virtio-vsock
                                   ▼
   ┌───────────────────────────────────────────────────────┐
   │   B4: guest kernel + guest-agent + user payload       │
   │       (user code lives here; untrusted)               │
   └───────────────────────────────────────────────────────┘
                          (least trusted)
```

The interesting transitions are **B4 → B3 → B2** (VM escape) and
**Internet → B1** (API abuse). The remaining transitions are simpler
in the model — they're just inside-the-perimeter HTTP.

## Threat Analysis

For each threat: the boundary it crosses, the realistic attacker
goal, the mechanism that defends against it, and the file where
that mechanism lives.

### T1 — VM escape (sandbox → host)

**Boundary:** B4 → B3 → B2.
**Attacker goal:** break out of the guest, gain code execution
on the host as the agent user; from there steal images, snoop
other tenants' sandboxes, or pivot to the master.

**Defense in depth:**

| Layer | Mechanism | Where |
|------|-----------|-------|
| Hardware | KVM hardware-assisted virtualization. Guest can't read host pages without an explicit MMIO/vsock path. | Linux kernel; `vmm.Client.bootInternal` requires `/dev/kvm`. |
| VMM | Cloud Hypervisor, written in Rust, minimal device surface (virtio-block, virtio-net optional, virtio-vsock, virtio-pci). No QEMU's legacy ISA devices. | `internal/vmm/` is the only thing that speaks to CH. |
| Seccomp | CH applies seccomp filters to its own threads (CH default policy). Vajra does not currently install an *additional* seccomp filter around the CH process. | CH binary; future enhancement, see [known-gaps.md](known-gaps.md). |
| Filesystem | Per-sandbox state lives under `/var/lib/vajra/sandboxes/{id}/` with 0o600 perms. CH process runs as the agent user, not root, in production deployments. | `internal/agent/sandbox.go::createDir`. |
| Memory | Each VM gets its own CH process and its own KVM memory slot. There is no shared-memory page across sandboxes. | CH default; not configurable from Vajra. |

**Residual risk.** CH 0-days exist (the CVE database has CH
advisories — virtio-block, virtio-fs paths have all been touched).
KVM 0-days exist. There is no defense against an unknown VMM/KVM
escape; the mitigation is keeping CH and the host kernel current
(`docs/deploy.md` records the recommended CH version).

### T2 — Cross-sandbox network access

**Boundary:** B4 ↔ B4 (between guest VMs on the same host).
**Attacker goal:** read or modify another tenant's sandbox over
the network.

**Defense:** Vajra VMs **do not share a network**. There is no
bridge, no tap, no NAT. Each VM speaks vsock to the host — that's
it. The proxy reaches into a sandbox by opening an agent CONNECT
tunnel and using vsock port 5255 (`scripts/guest-agent/forward.go`,
`internal/agent/forward.go`), so the host is the only path in.

| Layer | Mechanism | Where |
|------|-----------|-------|
| L2/L3 | No virtio-net; no shared bridge. | `internal/vmm/types.go` VM config; no net device added. |
| vsock | CIDs are allocated per-sandbox (`atomic.Uint32` starting at 3 for cold creates, 100 for pool members). Each CH process has its own `/tmp/vsock-<id>.sock` Unix socket. | `internal/agent/sandbox.go::AllocateCID`, `internal/agent/pool.go::cidAllocator`. |
| Process | Per-sandbox CH process, per-sandbox Unix socket — no shared object across CH processes. | One CH process per VM by design. |

**Residual risk.** None at the network layer for the default
deployment. A future feature that wires virtio-net into a shared
bridge **must** add tenant network policy (eBPF, ipset, or
hypervisor-level micro-segmentation). See production
recommendations.

### T3 — API abuse (caller → master)

**Boundary:** Internet → B1.
**Attacker goal:** unauthenticated abuse (mass-create sandboxes,
DoS the master), authenticated abuse (one tenant exhausting
another's quota or the global capacity).

**Defense:**

| Layer | Mechanism | Where |
|------|-----------|-------|
| Authentication | JWT HS256 (1h TTL, `RegisteredClaims`, alg pinned at parse). API keys (`vj_live_<32 hex>`, SHA256-hashed before storage; constant-time compare via the SQL lookup). | `internal/master/auth.go`. |
| Authorization | Account scoping. Every sandbox read/write is filtered by `account_id` from the request context; cross-account 404 is regression-tested (`TestGetSandboxOtherAccount`). | `internal/master/handlers_sandbox.go` + `handlers_test.go`. |
| Rate limit | Per-account token bucket, default 10 RPS (`VAJRA_RATE_LIMIT_RPS`). Anonymous bucket shared by `/v1/auth/login|register` so unauthenticated brute force is bounded. | `internal/master/ratelimit.go`. |
| Quota | `Scheduler.CheckQuota` enforces both sandbox count and vCPU/memory sums per account. DESTROYED and ERROR sandboxes don't count. | `internal/master/scheduler.go::CheckQuota`. |
| Internal endpoints | `/internal/*` is behind a separate middleware (`InternalAuthMiddleware`) that does constant-time compare against `AGENT_SHARED_SECRET`. | `internal/master/auth.go::InternalAuthMiddleware`. |
| Audit | Every mutating handler writes an `operations` row in IN_PROGRESS then COMPLETED/FAILED; error messages truncated at 1 KB to prevent log poisoning. | `internal/master/operation.go`. |

**Residual risk.** Rate-limit buckets are per-replica in-memory.
With >1 master replica, a tenant can saturate the per-replica
bucket and effectively get `N × 10 RPS`. Redis-backed token
buckets are the fix; see [known-gaps.md](known-gaps.md). The
admin gate is a single `ADMIN_ACCOUNT_ID` env var equality check
rather than a real role column on `accounts`.

### T4 — Snapshot / archive data theft

**Boundary:** B2 (agent host) → external.
**Attacker goal:** read sandbox snapshot bytes from disk or S3;
exfiltrate the memory image of a running workload.

**Defense:**

| Layer | Mechanism | Where |
|------|-----------|-------|
| File perms | Sandbox dirs created with 0o700, files 0o600. Snapshot dirs hardlink the template snapshot; hardlinks preserve permissions. | `internal/agent/sandbox.go`, `internal/agent/snapshot.go`. |
| S3 transport | `aws-sdk-go-v2` uses HTTPS by default. | `internal/agent/archive.go`. |
| S3 at-rest | Server-side encryption is **not** explicitly enabled in code today. Bucket policy / SSE-S3 / SSE-KMS must be configured on the bucket. | Production recommendation; gap. |
| Archive extraction | Path-traversal sanitisation rejects `..` components before `os.OpenFile`. | `internal/agent/archive.go::extractTar`. |
| Migrate transport | Plain HTTP today, behind `agent_shared_secret`. Live deployments should run agents on a private VPC subnet; cross-VPC migrate requires a TLS / mTLS layer. | `internal/agent/migrate.go`. |

**Residual risk.** At-rest encryption depends on bucket
configuration (not enforced in code). Local NVMe is unencrypted
unless the operator turns on dm-crypt / LUKS. The agent
process can read any sandbox's snapshot on the host it owns — by
design — so a compromised agent host exposes every sandbox on
that host.

### T5 — Side-channel attacks (Spectre, MDS, etc.)

**Boundary:** B4 → B4 (cross-tenant via shared CPU cache).
**Attacker goal:** infer bytes of another tenant's memory via
speculative execution side channels.

**Defense:**

| Layer | Mechanism | Where |
|------|-----------|-------|
| Kernel mitigations | Linux's standard mitigations (KPTI, retpoline, IBRS, MDS clear) are enabled by default on Ubuntu 22.04+ host kernels. | Host configuration. |
| Microcode | Up-to-date CPU microcode required (Intel `intel-microcode`, AMD `amd64-microcode`). | Host configuration. |
| Co-residency | No explicit core-pinning today. Two tenants can land on hyperthreads of the same physical core. | Production recommendation: disable SMT for high-isolation deployments. |

**Residual risk.** Single-tenant high-security deployments should
disable SMT (`smt=off` on the kernel command line) and pin
per-tenant cores. Vajra does not orchestrate that today.

### T6 — Denial of service

**Boundary:** various.
**Attacker goal:** exhaust master, agent, or host capacity to
deny service to other tenants.

**Defense:**

| Vector | Mechanism | Where |
|--------|-----------|-------|
| API floods | Per-account token bucket (T3). | `internal/master/ratelimit.go`. |
| Resource exhaustion via creates | `Scheduler.CheckQuota` per account; `Scheduler.PickNode` worst-fit bin-pack prevents one tenant filling one node entirely. | `internal/master/scheduler.go`. |
| Memory blow-up on the pool | Replenish checks `/proc/meminfo`; refuses to warm if `MemAvailable / MemTotal < 20 %`. Pool capped at `maxSize`. | `internal/agent/pool.go::adjustTargetSize`, `replenishOne`. |
| Snapshot blow-up | Archive on-disk staging under `/var/lib/vajra/archives/`. No size cap today — production should set a disk quota. | Gap; see known-gaps.md. |
| Long-running idle sandboxes | Auto-stop is **not** wired today (an idle sandbox stays RUNNING until the tenant stops it). | Gap. |
| Connection floods | Agent's HTTP server uses the stdlib `http.Server`; default IdleTimeout / ReadTimeout apply. No explicit per-IP concurrency limit. | Could add `golang.org/x/net/netutil.LimitListener` if pressure shows. |

### T7 — Supply chain

**Boundary:** Build host / dependency tree → all production hosts.
**Attacker goal:** inject malicious code via a compromised
dependency or CI step.

**Defense:**

| Layer | Mechanism | Where |
|------|-----------|-------|
| Dependency surface | Small. Runtime Go deps: `cobra`, `pflag`, `mousetrap`, `fatih/color`, `mattn/*` (CLI only); `jmoiron/sqlx`, `lib/pq`, `golang-migrate`, `klauspost/compress`, `aws-sdk-go-v2`, `nats.go`, `go-redis`, `golang.org/x/sys`. No transitive web of leftpad-style packages. | `go.mod`. |
| Reproducibility | `go.sum` pins every transitive dependency by hash. `go mod verify` checks. | `go.sum`. |
| Static binary | `CGO_ENABLED=0 go build` produces a static binary for the guest agent (4.3 MB). Static binaries don't depend on host glibc / dynamic loader. | `scripts/guest-agent/`, agent/master use CGO indirectly via `lib/pq` — switch to `pgx` is documented as a hardening step. |
| Build trust | No CI configured in this repo yet; production builds should sign artefacts with cosign or similar. | Gap. |
| SDK | Python SDK has one runtime dep (`requests`). No pydantic, no async stacks. | `sdk/python/pyproject.toml`. |

**Residual risk.** No SBOM emission today. No cosign signing on
release artefacts. No `dependabot` / `renovate` configured.

## Implemented Security Features

The table below is a one-shot map from "the brief said this" to
"the code does this, here". Anything not in this table is documented
as a gap in [known-gaps.md](known-gaps.md).

| Feature | Status | File(s) |
|---------|--------|---------|
| JWT auth (HS256, 1h TTL, alg-pinned) | Implemented | `internal/master/auth.go::IssueJWT`, `ParseJWT` |
| API keys (`vj_live_<32 hex>`, SHA256 hash storage) | Implemented | `internal/master/auth.go::GenerateAPIKey`, `HashAPIKey` |
| bcrypt password hash (cost 12) | Implemented | `internal/master/auth.go::HashPassword` |
| Account-scoped sandbox access (cross-account 404) | Implemented + regression-tested | `internal/master/handlers_sandbox.go`, `handlers_test.go::TestGetSandboxOtherAccount` |
| Per-account rate limit (token bucket, 10 RPS default) | Implemented | `internal/master/ratelimit.go` |
| Quota enforcement (count + vCPU + memory) | Implemented | `internal/master/scheduler.go::CheckQuota` |
| Audit trail (operations table) | Implemented | `internal/master/operation.go`, `migrations/001_init.up.sql` |
| Internal-only endpoints (`InternalAuthMiddleware`) | Implemented | `internal/master/auth.go::InternalAuthMiddleware` |
| KVM hardware isolation | Required by VMM layer | `internal/vmm/vmm.go` (CH spawn) |
| Per-VM vsock CID + Unix socket | Implemented | `internal/agent/sandbox.go::AllocateCID`, `internal/agent/pool.go` |
| Content-addressable image store (SHA256) | Implemented | `internal/agent/image.go::Pull` |
| qcow2 overlay (no rootfs mutation across sandboxes) | Implemented | `internal/agent/sandbox.go::createDisk` |
| Path-traversal sanitisation in archive extraction | Implemented | `internal/agent/archive.go::extractTar` |
| Memory headroom check before pool warming | Implemented | `internal/agent/pool.go::replenishOne` |
| Share-link tokens stored as SHA256 only | Implemented | `internal/master/handlers_share.go`, `migrations/002_share_links.up.sql` |
| Edge-triggered health notification | Implemented | `internal/agent/health.go::HealthChecker` |
| Operation error truncation (1 KB) | Implemented | `internal/master/operation.go::Complete` |
| Constant-time admin-secret compare | Implemented | `internal/master/auth.go::InternalAuthMiddleware` (uses `subtle.ConstantTimeCompare`) |
| Vsock AF_VSOCK net.Conn adapter (no `getsockname` leak) | Implemented (Day 7 fix) | `scripts/guest-agent/vsock.go::vsockNetConn` |
| Migrate sanitises path traversal | Implemented (shared `extractTar`) | `internal/agent/migrate.go::ReceiveSandbox` |
| Pool members excluded from health-check probing | Implemented (Day-9 pool rewrite) | `internal/agent/pool.go` (ownership rule) |

## Production Security Recommendations

The defaults are intended to be safe enough for the demo + early
production. Hardening for serious multi-tenant deployments should
do all of the below, none of which are wired in code today.

1. **TLS everywhere.**
   - Terminate TLS at the load balancer in front of master.
   - For master ↔ agent and master ↔ proxy, enable TLS on the
     listeners. The agent's HTTP server is stdlib `http.Server`;
     adding `TLSConfig` + a cert / key flag is straightforward.
   - Mutual TLS between master and agent is the right shape for
     anything past pilot — replaces `AGENT_SHARED_SECRET` with
     short-lived agent certs from an internal CA.
2. **Disk encryption at rest.**
   - `dm-crypt` / LUKS on the host NVMe holding
     `/var/lib/vajra/`.
   - S3 SSE-KMS on the archive bucket. Set as a bucket policy so
     the bucket *cannot* accept an unencrypted PUT; the Vajra
     archive code passes through whatever the bucket enforces.
3. **MFA on the dashboard.**
   - Today `vajra-master` accepts username/password + JWT only.
     Add TOTP at the `/v1/auth/login` boundary; the bcrypt path
     in `internal/master/auth.go` already separates the password
     check from token issuance.
4. **Key rotation.**
   - `JWT_SECRET` rotation: support two secrets (current +
     previous), accept tokens signed by either, drop the previous
     on the next rotation. Today the secret is a single env var.
   - API key rotation: surface `POST /v1/api-keys/{id}/rotate`
     that issues a new key under the same `name` and revokes the
     old one at the next reaper pass. Today an operator does
     `create new` → `revoke old` manually.
5. **Audit log shipping.**
   - The `operations` table is the audit log. Ship it to an
     append-only store (CloudWatch, Splunk, Loki) so a master
     compromise can't rewrite the trail.
6. **eBPF network policy.**
   - When/if Vajra grows virtio-net + a shared bridge, gate the
     bridge with Cilium / native eBPF so cross-sandbox traffic is
     dropped by default. The current vsock-only design dodges
     this, but it's the right design for the future.
7. **Seccomp around CH.**
   - CH applies an internal seccomp filter. An additional
     host-level filter via `bwrap` or `systemd-nspawn` reduces the
     attack surface on `clone`, `bpf`, `mount`, etc.
8. **Host kernel hardening.**
   - Boot with `mitigations=auto,nosmt` for high-isolation
     deployments.
   - Disable kernel modules that are not needed (`kexec_load`,
     `unshare`).
   - Pin CH and the kernel to the latest stable; subscribe to
     `oss-security` for CVE alerts.
9. **Out-of-band image signing.**
   - Templates are content-addressed by SHA256. The next step is
     signing the SHA256 — `cosign sign` on the image digest, and
     have the agent verify the signature before
     `ImageCache.Pull` completes. Today the agent trusts whatever
     bytes match the SHA256 it was given.
10. **Rate-limit shared across replicas.**
    - Move `internal/master/ratelimit.go` to Redis-backed buckets
      (atomic INCR + EXPIRE, or a Lua refill script). Required
      once master runs with > 1 replica.

## What This Threat Model Does *Not* Cover

For clarity, here is what is explicitly out of scope for the
current design:

- **Insider threats at the cloud provider.** AWS / GCP /
  bare-metal hosting providers have physical access to the
  hardware. There is no defense in this codebase against a
  compromised hypervisor host operator. SGX / SEV-SNP attestation
  would be the right path; not built today.
- **Cryptographic erasure of deleted snapshots.** When a sandbox
  is destroyed, files are unlinked. The blocks remain
  recoverable on the NVMe until overwritten. For deployments
  where this matters, mount `/var/lib/vajra/` on an encrypted
  volume whose key is destroyed with the host.
- **Per-sandbox network egress controls.** The guest can make
  arbitrary outbound TCP/UDP connections via `forward.go` plus
  any TCP stack inside the guest. There is no egress firewall
  today. For models like "agents may not reach the public
  internet", a host-level egress policy (iptables / nftables /
  eBPF) is required.
- **Side-channel-resistant scheduling.** The scheduler does not
  consider co-residency. Two tenants can land on the same
  physical host with hyperthread siblings, exposing classic
  side-channel risk. High-isolation deployments must disable
  SMT.
- **Replay protection on heartbeats.** Internal endpoints are
  protected by a static shared secret. A compromised
  intermediary on the agent → master link could replay
  heartbeats. mTLS solves this; see recommendation 1.
