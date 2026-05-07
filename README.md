# Vajra

A production-ready sandbox cloud platform for AI agents. Vajra creates
isolated Linux microVMs using [Cloud Hypervisor](https://www.cloudhypervisor.org/)
on bare metal, with sub-200ms boot times via snapshot restore.

Think Daytona / E2B / Modal — self-hosted, built from scratch.

## Architecture

Vajra ships four binaries:

| Binary         | Role                                                                |
|----------------|---------------------------------------------------------------------|
| `vajra-master` | Stateless control-plane API server. All state in PostgreSQL.        |
| `vajra-agent`  | Node daemon. Runs on each bare metal host. Manages local microVMs.  |
| `vajra-proxy`  | Reverse proxy for sandbox port forwarding and browser terminals.    |
| `vajra`        | CLI for users.                                                      |

### Design principles

1. **Stateless master.** All state lives in PostgreSQL — multiple replicas
   behind a load balancer just work. Sandbox state never lives in memory.
2. **Event-driven agents.** Agents push state changes to the master; the
   master never polls.
3. **Content-addressable images.** Templates are identified by SHA256 of
   the rootfs.
4. **Two-tier scheduler.** `pick_cluster()` → `pick_node()`. The interface
   exists even with a single cluster.
5. **Defense in depth.** KVM hardware isolation + seccomp + iptables + auth.

### Stack

- Go 1.22+
- Cloud Hypervisor (REST API over Unix socket)
- PostgreSQL 16
- React + TypeScript + Vite + Tailwind (dashboard)
- Agent ↔ master via gRPC / HTTP
- Guest communication via vsock (virtio-vsock)

## Benchmark numbers

Measured on EC2 `c8i.large` with nested virtualization. Bare-metal numbers
will be substantially lower once nested-virt overhead is gone.

| Path                                         | Time          |
|----------------------------------------------|---------------|
| Cloud Hypervisor snapshot restore (internal) | ~160 ms       |
| End-to-end with shell overhead               | ~312 ms       |
| Target on bare metal                         | 80 – 150 ms   |
| Reference: Incus container cold start        | ~1000 ms      |

## Project layout

```
cmd/         entry points (vajra-agent, vajra-master, vajra-proxy, vajra)
internal/
  vmm/       Cloud Hypervisor shim (HTTP-over-Unix-socket client)
  agent/     Node agent: sandbox lifecycle, pre-warm pool, image cache
  master/    Control plane: API, scheduler, reconciler, auth
  proxy/     Reverse proxy: dynamic routing, WebSocket, TLS
  models/    Shared data models (sandbox, node, cluster, …)
  store/     Database layer (PostgreSQL via sqlx)
pkg/sdk/     Public Go SDK
sdk/         Python and TypeScript SDKs
web/         React dashboard
migrations/  golang-migrate SQL migrations
scripts/     Host setup, benchmarks, demo
docs/        Architecture, threat model, deploy guide, known gaps
test/        Integration tests
```

## Quick start

```sh
make db-up        # Start PostgreSQL + Redis
make build        # Build all four binaries into ./bin
make test         # Run unit tests
make benchmark    # Run vmm benchmarks
```

## Status

Day 1 — Cloud Hypervisor validation complete (see `bible.md`).
Implementation of the Go shim and node agent is next.
