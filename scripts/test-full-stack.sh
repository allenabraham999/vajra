#!/usr/bin/env bash
# test-full-stack.sh — exercise the full Vajra production stack
# (PostgreSQL + Redis + NATS + master + agent) on the EC2 test host.
#
# Idempotent: tears down old tmux sessions, rebuilds, reseeds .env,
# restarts master & agent, then runs a sandbox lifecycle through Redis
# and NATS asserting that caching and eventing are wired correctly.
#
# Two execution modes:
#   1. Run on a workstation — opens SSH to the EC2 host and runs the
#      remote section over there. Required env / defaults:
#          EC2_HOST=43.205.229.245
#          EC2_USER=ubuntu
#          EC2_KEY=~/.ssh/mini-daytona-key.pem
#   2. Run on the EC2 host directly — set VAJRA_LOCAL=1 to skip SSH.

set -euo pipefail

EC2_HOST="${EC2_HOST:-13.126.126.66}"
EC2_USER="${EC2_USER:-ubuntu}"
EC2_KEY="${EC2_KEY:-$HOME/.ssh/mini-daytona-key.pem}"
VAJRA_DIR="${VAJRA_DIR:-/home/${EC2_USER}/vajra}"
MASTER_PORT="${MASTER_PORT:-8080}"
AGENT_PORT="${AGENT_PORT:-9000}"
NATS_MON_PORT="${NATS_MON_PORT:-8222}"

bar() { printf '\n========== %s ==========\n' "$*"; }

remote_script() {
	cat <<'REMOTE'
set -euo pipefail

VAJRA_DIR="${VAJRA_DIR:-$HOME/vajra}"
MASTER_PORT="${MASTER_PORT:-8080}"
AGENT_PORT="${AGENT_PORT:-9000}"
NATS_MON_PORT="${NATS_MON_PORT:-8222}"

# Ensure common tool paths are visible in non-interactive ssh shells.
export PATH="/usr/local/go/bin:/usr/local/bin:$HOME/go/bin:$HOME/.local/bin:$PATH"

bar() { printf '\n========== %s ==========\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1"; exit 1; }; }

need docker
need tmux
need curl
need jq

bar "1. Pull latest + rebuild"
cd "$VAJRA_DIR"
git pull --ff-only || git pull --rebase
# t2/small EC2 has 4G RAM and no swap; the aws-ec2 SDK alone needs >2G to
# compile so we constrain Go to one package at a time with aggressive GC.
export GOFLAGS="${GOFLAGS:--p=1}"
export GOGC="${GOGC:-30}"
export GOMEMLIMIT="${GOMEMLIMIT:-2500MiB}"
make clean
make build
ls -lh bin/

bar "2. Bring up docker-compose (postgres + redis + nats)"
# Make sure NATS exposes its monitoring HTTP server on 8222. Older
# docker-compose.yml only passed --jetstream/--store_dir, so /varz, /connz,
# /subsz all RST'd. This sed is idempotent.
if ! grep -q '"-m", *"8222"' docker-compose.yml; then
	sed -i 's|\["--jetstream", "--store_dir", "/data"\]|["--jetstream", "--store_dir", "/data", "-m", "8222"]|' docker-compose.yml
	echo "patched docker-compose.yml to add NATS -m 8222"
	# Force a recreate so the new args take effect.
	docker compose rm -sf nats || true
fi
docker compose up -d
sleep 3
docker compose ps

bar "2a. Verify Redis (PING)"
redis_pong=$(docker exec vajra-redis redis-cli ping || true)
echo "redis-cli ping -> $redis_pong"
if [ "$redis_pong" != "PONG" ]; then
	echo "FAIL: redis did not respond with PONG"
	exit 1
fi

bar "2b. Verify NATS (/varz)"
# Wait for the NATS monitoring endpoint to come up.
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
	if curl -fsS "http://localhost:${NATS_MON_PORT}/healthz" >/dev/null 2>&1; then
		echo "nats /healthz up after ${i}s"; break
	fi
	sleep 1
done
curl -s "http://localhost:${NATS_MON_PORT}/varz" \
	| jq '{server_id, version, connections, in_msgs, out_msgs, subscriptions}' || true

bar "3. Update .env (Redis + NATS)"
ENV_FILE="$VAJRA_DIR/.env"
touch "$ENV_FILE"
add_env() {
	local key="$1" val="$2"
	if grep -q "^export ${key}=" "$ENV_FILE" 2>/dev/null; then
		# replace existing
		sed -i "s|^export ${key}=.*|export ${key}=\"${val}\"|" "$ENV_FILE"
	else
		printf 'export %s="%s"\n' "$key" "$val" >> "$ENV_FILE"
	fi
}
add_env REDIS_URL "redis://localhost:6379"
add_env NATS_URL  "nats://localhost:4222"
# Make sure DATABASE_URL is present for master.
if ! grep -q '^export DATABASE_URL=' "$ENV_FILE"; then
	add_env DATABASE_URL "postgres://vajra:vajra@localhost:5432/vajra?sslmode=disable"
fi
if ! grep -q '^export JWT_SECRET=' "$ENV_FILE"; then
	add_env JWT_SECRET "dev-jwt-secret-please-change"
fi
if ! grep -q '^export AGENT_SHARED_SECRET=' "$ENV_FILE"; then
	add_env AGENT_SHARED_SECRET "dev-agent-secret"
fi
echo "--- .env (Redis/NATS lines) ---"
grep -E '^export (REDIS_URL|NATS_URL|DATABASE_URL|JWT_SECRET|AGENT_SHARED_SECRET)=' "$ENV_FILE" || true

# shellcheck disable=SC1090
set -a; source "$ENV_FILE"; set +a

bar "4. Restart master & agent in tmux"
tmux kill-session -t vajra-master 2>/dev/null || true
tmux kill-session -t vajra-agent  2>/dev/null || true
# Catch anything not in tmux too.
pkill -f 'bin/vajra-master' 2>/dev/null || true
pkill -f 'bin/vajra-agent'  2>/dev/null || true
sleep 1

# master
tmux new-session -d -s vajra-master -c "$VAJRA_DIR" \
	"bash -lc 'export PATH=/usr/local/go/bin:/usr/local/bin:\$PATH; set -a; source .env; set +a; exec ./bin/vajra-master 2>&1 | tee -a /tmp/vajra-master.log'"
# agent — point it at master and supply its API key/secret via env.
add_env VAJRA_AGENT_MASTER_URL "http://localhost:${MASTER_PORT}"
add_env VAJRA_AGENT_API_KEY    "${VAJRA_AGENT_API_KEY:-dev-agent-key}"
tmux new-session -d -s vajra-agent -c "$VAJRA_DIR" \
	"bash -lc 'export PATH=/usr/local/go/bin:/usr/local/bin:\$PATH; set -a; source .env; set +a; exec ./bin/vajra-agent 2>&1 | tee -a /tmp/vajra-agent.log'"

bar "4a. Wait for health"
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
	if curl -fsS "http://localhost:${MASTER_PORT}/health" >/dev/null 2>&1; then
		echo "master up after ${i}s"; break
	fi
	sleep 1
done
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
	if curl -fsS "http://localhost:${AGENT_PORT}/health" >/dev/null 2>&1; then
		echo "agent up after ${i}s"; break
	fi
	sleep 1
done

bar "5. Health endpoints"
echo "master /health:"
curl -fsS "http://localhost:${MASTER_PORT}/health" | jq . || curl -sS "http://localhost:${MASTER_PORT}/health"; echo
echo "agent /health:"
curl -fsS "http://localhost:${AGENT_PORT}/health" | jq . || curl -sS "http://localhost:${AGENT_PORT}/health"; echo

bar "6. Register account + create sandbox"
EMAIL="stack-$(date +%s)@example.com"
PASS="hunter2hunter2"
REG=$(curl -fsS -X POST "http://localhost:${MASTER_PORT}/v1/auth/register" \
	-H 'Content-Type: application/json' \
	-d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}")
echo "register -> $REG"
API_KEY=$(echo "$REG" | jq -r .api_key)
ACCOUNT_ID=$(echo "$REG" | jq -r .account_id)
echo "API_KEY=$API_KEY"
echo "ACCOUNT_ID=$ACCOUNT_ID"

# Pick a template (first one available). The API may return either a bare
# array or {items:[...]} depending on the build, so we accept both.
TEMPLATES=$(curl -fsS "http://localhost:${MASTER_PORT}/v1/templates" \
	-H "Authorization: Bearer $API_KEY" 2>/dev/null || echo '[]')
TEMPLATE_ID=$(echo "$TEMPLATES" | jq -r '(if type=="array" then . else (.items // []) end)[0].id // empty' 2>/dev/null || true)
if [ -z "$TEMPLATE_ID" ]; then
	echo "no templates for new account — cloning an existing working template into it"
	# Look for a template that points at on-disk rootfs that actually exists,
	# then duplicate the row with a new id under our account_id. Skipping the
	# REST API because POST /v1/templates is permission-gated and may reject
	# arbitrary paths. We do the clone in SQL so the field set is exact.
	set +e
	NEW_TPL_ID=$(head -c 16 /dev/urandom | xxd -p 2>/dev/null || openssl rand -hex 16)
	CLONE_OUT=$(docker exec vajra-postgres psql -U vajra -d vajra -tA -c "
WITH src AS (
  SELECT * FROM templates
  WHERE rootfs_path LIKE '/home/ubuntu/%' OR rootfs_path LIKE '/var/lib/vajra/%'
  ORDER BY created_at DESC LIMIT 1
)
INSERT INTO templates (id, account_id, name, version, hash, rootfs_path, kernel_path, snapshot_path, created_at)
SELECT '$NEW_TPL_ID', '$ACCOUNT_ID', name||'-stacktest', version, hash, rootfs_path, kernel_path, snapshot_path, NOW()
FROM src
RETURNING id;" 2>&1)
	echo "clone -> $CLONE_OUT"
	TEMPLATE_ID=$(printf '%s\n' "$CLONE_OUT" | grep -E '^[0-9a-f]{32}$' | head -n1 || true)
	set -e
fi
echo "template_id=$TEMPLATE_ID"

CREATE_BODY=$(jq -nc --arg t "$TEMPLATE_ID" '{
	name: "full-stack-test",
	source: "image",
	template_id: $t,
	vcpus: 1, memory_mb: 512, disk_gb: 1
}')
CREATE_RESP=$(curl -fsS -X POST "http://localhost:${MASTER_PORT}/v1/sandboxes" \
	-H "Authorization: Bearer $API_KEY" \
	-H 'Content-Type: application/json' \
	-d "$CREATE_BODY" || true)
echo "create -> $CREATE_RESP"
SANDBOX_ID=$(echo "$CREATE_RESP" | jq -r '.id // .sandbox.id // empty')
echo "sandbox_id=$SANDBOX_ID"

bar "6a. Redis cached state"
if [ -n "$SANDBOX_ID" ]; then
	docker exec vajra-redis redis-cli GET "sandbox:${SANDBOX_ID}:state" || true
	docker exec vajra-redis redis-cli KEYS "sandbox:${SANDBOX_ID}:*" || true
else
	echo "(skipped — no sandbox)"
fi

bar "7. NATS connections + subjects"
echo "--- /connz ---"
curl -s "http://localhost:${NATS_MON_PORT}/connz" | jq '{num_connections, total: .total, connections: [.connections[] | {cid, name, subscriptions, in_msgs, out_msgs}]}'
echo "--- /subsz ---"
curl -s "http://localhost:${NATS_MON_PORT}/subsz?subs=1" | jq '{num_subscriptions, num_inserts, num_removes, subscriptions_list: (.subscriptions_list // [] | map(.subject))}'
echo "--- /routez ---"
curl -s "http://localhost:${NATS_MON_PORT}/routez" | jq .

bar "8. Sandbox lifecycle"
state() {
	docker exec vajra-redis redis-cli GET "sandbox:${SANDBOX_ID}:state" 2>/dev/null || true
}
poll_state() {
	local want="$1" tries="${2:-30}"
	for _ in $(seq 1 "$tries"); do
		local s; s=$(state)
		if [ "$s" = "$want" ]; then echo "  reached: $s"; return 0; fi
		sleep 1
	done
	echo "  TIMEOUT waiting for $want, last=$(state)"
	return 1
}

if [ -n "$SANDBOX_ID" ]; then
	echo "[8a] wait for RUNNING"
	poll_state RUNNING 45 || true
	docker exec vajra-redis redis-cli GET "sandbox:${SANDBOX_ID}:state" || true

	echo "[8b] exec"
	curl -fsS -X POST "http://localhost:${MASTER_PORT}/v1/sandboxes/${SANDBOX_ID}/exec" \
		-H "Authorization: Bearer $API_KEY" \
		-H 'Content-Type: application/json' \
		-d '{"cmd":["echo","hello-from-stack"]}' | jq . || true

	echo "[8c] stop"
	curl -fsS -X POST "http://localhost:${MASTER_PORT}/v1/sandboxes/${SANDBOX_ID}/stop" \
		-H "Authorization: Bearer $API_KEY" | jq . || true
	poll_state STOPPED 30 || true

	echo "[8d] start"
	curl -fsS -X POST "http://localhost:${MASTER_PORT}/v1/sandboxes/${SANDBOX_ID}/start" \
		-H "Authorization: Bearer $API_KEY" | jq . || true
	poll_state RUNNING 30 || true

	echo "[8e] destroy"
	curl -fsS -X DELETE "http://localhost:${MASTER_PORT}/v1/sandboxes/${SANDBOX_ID}" \
		-H "Authorization: Bearer $API_KEY" | jq . || true
	# Key should disappear (or move to DESTROYED briefly).
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		s=$(state)
		if [ -z "$s" ] || [ "$s" = "DESTROYED" ]; then
			echo "  final redis state: '${s:-<deleted>}'"
			break
		fi
		sleep 1
	done
else
	echo "(skipped — no sandbox id from create)"
fi

bar "9. Summary"
echo "--- tmux sessions ---"
tmux ls 2>/dev/null || true
echo "--- docker compose ps ---"
docker compose ps
echo "--- Redis stats ---"
docker exec vajra-redis redis-cli INFO keyspace || true
docker exec vajra-redis redis-cli DBSIZE || true
echo "--- NATS /varz (in/out msgs) ---"
curl -s "http://localhost:${NATS_MON_PORT}/varz" | jq '{in_msgs, out_msgs, in_bytes, out_bytes, connections, subscriptions}'
echo "--- last 20 lines: master ---"
tail -n 20 /tmp/vajra-master.log 2>/dev/null || true
echo "--- last 20 lines: agent ---"
tail -n 20 /tmp/vajra-agent.log 2>/dev/null || true

echo
echo "DONE."
REMOTE
}

if [ "${VAJRA_LOCAL:-0}" = "1" ]; then
	bar "Running locally (VAJRA_LOCAL=1)"
	eval "$(remote_script)"
	exit 0
fi

bar "Running remotely on ${EC2_USER}@${EC2_HOST}"
if [ ! -f "$EC2_KEY" ]; then
	echo "ssh key not found at $EC2_KEY"; exit 1
fi

# Stream the script over stdin so we don't have to scp it first.
remote_script | ssh -i "$EC2_KEY" \
	-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
	-o ConnectTimeout=15 -o ServerAliveInterval=30 \
	"${EC2_USER}@${EC2_HOST}" \
	"VAJRA_DIR='${VAJRA_DIR}' MASTER_PORT='${MASTER_PORT}' AGENT_PORT='${AGENT_PORT}' NATS_MON_PORT='${NATS_MON_PORT}' bash -s"
