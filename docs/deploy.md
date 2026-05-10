# Deployment Guide

## Local development

The default `docker-compose.yml` brings up the three datastores Vajra
needs:

```bash
docker compose up -d postgres redis nats
make build
source .env.example
./bin/vajra-master
```

Redis and NATS are optional in local dev — leave `REDIS_URL` and
`NATS_URL` unset and master falls back to Postgres-only behaviour.

## Production: separate datastores

Production runs Postgres on AWS RDS and Redis/NATS on dedicated hosts
(or ElastiCache + a managed NATS). The compute side stays on bare
metal so we keep the 80–150ms restore target.

```bash
# bring up redis + nats only — postgres is RDS
docker compose -f docker-compose.prod.yml up -d
```

### RDS Postgres

1. Create an RDS PostgreSQL 16 instance in the same VPC as the
   master nodes.
2. Security group must allow inbound 5432 from the master security
   group (and ONLY that group).
3. Multi-AZ: enable for prod. db.t4g.medium is enough for the demo.
4. Update `DATABASE_URL` to the RDS endpoint:
   ```
   DATABASE_URL=postgres://vajra:PASSWORD@vajra-db.xxxxx.ap-south-1.rds.amazonaws.com:5432/vajra?sslmode=require
   ```
5. Run migrations once. They are idempotent so re-running them on a
   second master replica is safe:
   ```bash
   ./bin/vajra-master   # migrations run on startup
   ```

No code changes are required — the master process already accepts
any libpq-compatible DSN.

### Redis

Set `REDIS_URL=redis://...:6379/0`. With Redis enabled:

- Sandbox-state lookups skip Postgres on cache hit (TTL 30s).
- Node-capacity reads skip Postgres on cache hit (TTL 10s, refreshed
  on every heartbeat).
- Account quota checks read a cached count (TTL 60s) instead of a
  COUNT(*) on the sandboxes table.
- Template metadata is cached for 5min.

If Redis is unreachable at startup, master logs a warning and
continues with NoopCache — every read falls through to Postgres.

### NATS

Set `NATS_URL=nats://nats.internal:4222`. With NATS enabled:

- Agents publish `vajra.node.heartbeat` to NATS every 5s. Master's
  subscriber writes the usage straight to Redis and batches Postgres
  writes every 30s — no more heartbeat-per-second hitting the DB.
- Sandbox state changes flow over `vajra.sandbox.state_changed`.
- `vajra.node.unhealthy` triggers a single reconciliation pass on
  the affected sandbox.

Agents also keep the HTTP heartbeat path active. NATS is best-effort;
the HTTP write is the canonical truth.

### Autoscaler

Enable with:

```
VAJRA_AUTOSCALE_ENABLED=true
VAJRA_AUTOSCALE_AMI=ami-0abcdef0123456789
VAJRA_AUTOSCALE_REGION=ap-south-1
VAJRA_AUTOSCALE_SECURITY_GROUP=sg-vajra-agents
VAJRA_AUTOSCALE_SUBNET_ID=subnet-xxxxx
VAJRA_AUTOSCALE_KEY_PAIR=vajra-prod
VAJRA_AUTOSCALE_S3_BUCKET=vajra-binaries
VAJRA_AUTOSCALE_MASTER_URL=http://master.internal:8080
VAJRA_AUTOSCALE_MIN_NODES=1
VAJRA_AUTOSCALE_MAX_NODES=50
VAJRA_AUTOSCALE_COOLDOWN_MINS=15
```

When the scheduler returns ErrNoCapacity, master queues the request,
launches one EC2 instance via `RunInstances`, waits up to 5 minutes
for the new agent to register, then retries the schedule. Idle
managed nodes are scaled down every 5 min once they've been idle for
`VAJRA_AUTOSCALE_COOLDOWN_MINS`.

The autoscaler only reaps instances tagged `vajra:managed=true`;
manually-registered nodes are never terminated.

Inspect:

```
vajra admin autoscale status
vajra admin autoscale trigger
```

## Backward compatibility

Every new dependency is opt-in:

- `REDIS_URL` unset → NoopCache, every read hits Postgres.
- `NATS_URL` unset → NoopBus, HTTP heartbeats continue.
- `VAJRA_AUTOSCALE_ENABLED` unset/false → 503 on no-capacity, current
  behaviour.

Run master with none of those set and behaviour is identical to the
pre-mega-build version.
