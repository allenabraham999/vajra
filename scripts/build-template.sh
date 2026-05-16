#!/usr/bin/env bash
#
# build-template.sh — rebuild a Vajra microVM template from the current
# guest-agent source.
#
# WHY THIS EXISTS
# ---------------
# A Vajra template is a (rootfs, kernel, CH snapshot) triple. The snapshot
# captures the *running memory* of a booted VM — including the guest-agent
# process. Fixing a guest-agent bug therefore means more than swapping the
# on-disk binary: the old binary is still live in the snapshot's RAM image.
# The template must be rebuilt: boot a VM with the new binary, let the new
# guest-agent come up, then snapshot afresh.
#
# WHAT IT DOES
# ------------
# The existing ubuntu-noble template is an Ubuntu 24.04 cloud image (a
# partitioned disk, root=/dev/vda1) that already boots cleanly with the
# project's vmlinux. Rather than synthesising a fresh rootfs (which would
# not match that kernel/cmdline), this script derives the new template
# from the known-good one:
#
#   1. cross-compile scripts/guest-agent for linux/amd64
#   2. make a standalone writable copy of the template's rootfs.qcow2
#   3. mount it, replace /usr/local/bin/vajra-guest-agent with the new build
#   4. boot the copy under Cloud Hypervisor (headless)
#   5. wait for the new guest-agent to answer on its vsock port
#   6. pause + snapshot the VM
#   7. hash the rootfs, lay the triple out under the image cache as
#      <cache>/sha256:<hex>/{rootfs.qcow2,vmlinux,snapshot/}
#
# The new hash is printed as  NEW_TEMPLATE_HASH=sha256:<hex>  and also
# written to /tmp/vajra-new-template-hash.
#
# Registering the template row in PostgreSQL and pointing
# VAJRA_AGENT_POOL_TEMPLATE at the new hash are deliberately left to the
# operator — see the project deploy notes.
#
# Usage:  bash scripts/build-template.sh [template-name]
#         SRC_HASH=sha256:<hex>  may override the source template.
#
set -euo pipefail

TEMPLATE_NAME="${1:-ubuntu-noble}"
CACHE_DIR="${VAJRA_AGENT_CACHE_DIR:-/var/lib/vajra/cache}"
CH_BIN="${VAJRA_AGENT_CH_BINARY:-/usr/local/bin/cloud-hypervisor}"
BUILD_DIR="/var/lib/vajra/build-tmpl"
MNT="/mnt/vajra-tmpl-build"
NBD="/dev/nbd0"
API_SOCK="$BUILD_DIR/ch-api.sock"
VSOCK_SOCK="$BUILD_DIR/vsock.sock"
GUEST_AGENT_PORT=5252            # exec port — first thing the agent binds
VSOCK_WAIT_SECS=150              # generous: covers a cold cloud-image boot
QUIESCE_SECS=25                  # let the guest settle before snapshotting
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

export PATH="$PATH:/usr/local/go/bin"

log()  { echo "[build-template] $*"; }
fail() { echo "[build-template] ERROR: $*" >&2; exit 1; }

CH_PID=""
BUILD_OK=0
cleanup() {
	# Best-effort teardown — never let cleanup mask the real error.
	if [ -n "$CH_PID" ]; then
		# $CH_PID is the sudo wrapper; SIGKILLing it orphans cloud-hypervisor,
		# so also target the CH process directly by its api-socket argv.
		kill -0 "$CH_PID" 2>/dev/null && sudo kill -9 "$CH_PID" 2>/dev/null || true
		sudo pkill -9 -f "path=$API_SOCK" 2>/dev/null || true
	fi
	sudo umount "$MNT" 2>/dev/null || true
	sudo qemu-nbd --disconnect "$NBD" >/dev/null 2>&1 || true
	if [ "$BUILD_OK" -ne 1 ]; then
		log "build failed — leaving $BUILD_DIR for inspection"
	fi
}
trap cleanup EXIT

# --- resolve the source template -------------------------------------------
SRC_HASH="${SRC_HASH:-${VAJRA_AGENT_POOL_TEMPLATE:-}}"
if [ -z "$SRC_HASH" ]; then
	# Fall back to the single template present in the cache.
	mapfile -t found < <(find "$CACHE_DIR" -maxdepth 1 -mindepth 1 -type d -name 'sha256:*' -printf '%f\n')
	[ "${#found[@]}" -eq 1 ] || fail "cannot auto-detect source template; set SRC_HASH or VAJRA_AGENT_POOL_TEMPLATE"
	SRC_HASH="${found[0]}"
fi
SRC_DIR="$CACHE_DIR/$SRC_HASH"
[ -f "$SRC_DIR/rootfs.qcow2" ] || fail "source rootfs missing: $SRC_DIR/rootfs.qcow2"
[ -f "$SRC_DIR/vmlinux" ]      || fail "source kernel missing: $SRC_DIR/vmlinux"

log "template      : $TEMPLATE_NAME"
log "source hash   : $SRC_HASH"
log "repo          : $REPO_DIR"
command -v qemu-nbd >/dev/null   || fail "qemu-nbd not installed"
command -v python3  >/dev/null   || fail "python3 not installed"
command -v go       >/dev/null   || fail "go not on PATH"

# --- 1. fresh build dir ------------------------------------------------------
sudo rm -rf "$BUILD_DIR"
sudo mkdir -p "$BUILD_DIR"
sudo chown "$(id -u):$(id -g)" "$BUILD_DIR"

# --- 2. cross-compile the guest agent ---------------------------------------
log "compiling guest-agent (linux/amd64)..."
( cd "$REPO_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -trimpath -ldflags='-s -w' -o "$BUILD_DIR/vajra-guest-agent" ./scripts/guest-agent )
file "$BUILD_DIR/vajra-guest-agent" | grep -q 'ELF 64-bit' || fail "guest-agent did not build as an ELF binary"

# --- 3. standalone writable copy of the rootfs ------------------------------
log "copying rootfs (qemu-img convert, standalone qcow2)..."
qemu-img convert -f qcow2 -O qcow2 "$SRC_DIR/rootfs.qcow2" "$BUILD_DIR/rootfs.qcow2"
cp "$SRC_DIR/vmlinux" "$BUILD_DIR/vmlinux"

# --- 4. swap the binary inside the rootfs -----------------------------------
log "mounting rootfs to install the new guest-agent..."
sudo modprobe nbd max_part=8
sudo qemu-nbd --disconnect "$NBD" >/dev/null 2>&1 || true
sudo qemu-nbd --connect="$NBD" "$BUILD_DIR/rootfs.qcow2"
for _ in $(seq 1 10); do [ -b "${NBD}p1" ] && break; sleep 1; done
[ -b "${NBD}p1" ] || fail "partition ${NBD}p1 never appeared"

# The qcow2 came from a paused (not cleanly shut down) VM, so its ext4
# journal may be dirty — recover it before a read-write mount. e2fsck
# exit codes 0/1 are success ("clean" / "errors corrected"); >=4 is fatal.
fsck_rc=0
sudo e2fsck -fy "${NBD}p1" || fsck_rc=$?
[ "$fsck_rc" -lt 4 ] || fail "e2fsck failed on rootfs (rc=$fsck_rc)"

sudo mkdir -p "$MNT"
sudo mount "${NBD}p1" "$MNT"
# The unit file is a regular file; its enable marker is a symlink whose
# target is absolute, so test it with -L (a -f test would follow the link
# against the host's filesystem root, not the mounted rootfs).
[ -f "$MNT/etc/systemd/system/vajra-guest-agent.service" ] \
	|| fail "rootfs has no vajra-guest-agent.service unit — wrong source template?"
[ -L "$MNT/etc/systemd/system/multi-user.target.wants/vajra-guest-agent.service" ] \
	|| fail "vajra-guest-agent.service is not enabled in the rootfs"

sudo install -m 0755 -o 0 -g 0 "$BUILD_DIR/vajra-guest-agent" "$MNT/usr/local/bin/vajra-guest-agent"
# Disable cloud-init: this rootfs already completed first-boot, and Vajra
# sandboxes are vsock-only with no NIC, so cloud-init has nothing to do.
# Disabling it makes the build boot fast and deterministic (no datasource
# search). Restored sandboxes never boot, so this is invisible to them.
sudo touch "$MNT/etc/cloud/cloud-init.disabled"
sync
sudo umount "$MNT"
sudo qemu-nbd --disconnect "$NBD"
log "new guest-agent installed in rootfs"

# --- 5. boot the VM under Cloud Hypervisor (headless) -----------------------
# This cloud-hypervisor build requires the VM payload on the command line
# (an API-only vm.create is rejected with "required arguments not
# provided"), so the full config is passed as argv. CH creates and boots
# the VM at start-up; the API socket is still used for pause/snapshot.
rm -f "$API_SOCK" "$VSOCK_SOCK"
log "booting build VM..."
sudo "$CH_BIN" \
	--api-socket "path=$API_SOCK" \
	--kernel     "$BUILD_DIR/vmlinux" \
	--cmdline    "console=hvc0 root=/dev/vda1 rw" \
	--disk       "path=$BUILD_DIR/rootfs.qcow2" \
	--cpus       "boot=2" \
	--memory     "size=512M" \
	--vsock      "cid=3,socket=$VSOCK_SOCK" \
	--rng        "src=/dev/urandom" \
	--console off --serial off \
	>"$BUILD_DIR/ch.log" 2>&1 &
CH_PID=$!

ch_api() { # ch_api METHOD endpoint [curl-args...]
	local method="$1" ep="$2"; shift 2
	local out code
	# Capture body + status in one shot — no temp file: a sudo-owned file
	# under /tmp's sticky bit could not be removed by this non-root script.
	out=$(sudo curl -s -w '\n%{http_code}' --unix-socket "$API_SOCK" \
		-X "$method" "http://localhost/api/v1/$ep" "$@" 2>/dev/null) || true
	code="${out##*$'\n'}"
	if [ "${code:0:1}" != "2" ]; then
		echo "[build-template] CH $ep -> HTTP ${code:-?}: ${out%$'\n'*}" >&2
		return 1
	fi
	return 0
}
ch_get() { sudo curl -s --unix-socket "$API_SOCK" "http://localhost/api/v1/$1"; }

# Wait for the API server to accept connections.
ready=0
for _ in $(seq 1 150); do
	if ! kill -0 "$CH_PID" 2>/dev/null; then
		cat "$BUILD_DIR/ch.log" >&2; fail "cloud-hypervisor exited before its API was ready"
	fi
	if sudo curl -s -o /dev/null --unix-socket "$API_SOCK" \
		http://localhost/api/v1/vmm.ping 2>/dev/null; then ready=1; break; fi
	sleep 0.2
done
[ "$ready" -eq 1 ] || fail "CH API socket never became ready"

# CH boots the CLI-configured VM itself; nudge it only if still Created.
state="$(ch_get vm.info | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("state",""))
except Exception: print("")' 2>/dev/null || true)"
if [ "$state" != "Running" ]; then
	ch_api PUT vm.boot || fail "vm.boot failed (state was '${state:-unknown}')"
fi
log "VM booted; waiting for guest-agent on vsock port $GUEST_AGENT_PORT..."

# --- 6. wait for the guest agent to answer ----------------------------------
# CH's hybrid-vsock: connect to the host socket, write "CONNECT <port>\n",
# expect "OK ...". A reply means the guest-agent's listener is up.
sudo python3 - "$VSOCK_SOCK" "$GUEST_AGENT_PORT" "$VSOCK_WAIT_SECS" <<'PY' || fail "guest-agent never came up"
import socket, sys, time
path, port, timeout = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
deadline = time.time() + timeout
while time.time() < deadline:
    try:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(5)
        s.connect(path)
        s.sendall(b"CONNECT %d\n" % port)
        data = s.recv(64)
        s.close()
        if data.startswith(b"OK"):
            print("[build-template] guest-agent responded: %s"
                  % data.decode("ascii", "replace").strip())
            sys.exit(0)
    except Exception:
        pass
    time.sleep(1)
sys.exit(1)
PY

log "guest-agent is up; quiescing ${QUIESCE_SECS}s before snapshot..."
sleep "$QUIESCE_SECS"

# --- 7. pause + snapshot -----------------------------------------------------
mkdir -p "$BUILD_DIR/snapshot"
ch_api PUT vm.pause || fail "vm.pause failed"
ch_api PUT vm.snapshot -H 'Content-Type: application/json' \
	-d "{\"destination_url\":\"file://$BUILD_DIR/snapshot\"}" || fail "vm.snapshot failed"
ch_api PUT vmm.shutdown || true
for _ in $(seq 1 30); do kill -0 "$CH_PID" 2>/dev/null || break; sleep 0.5; done
kill -0 "$CH_PID" 2>/dev/null && sudo kill -9 "$CH_PID" 2>/dev/null || true
CH_PID=""
[ -f "$BUILD_DIR/snapshot/state.json" ] || fail "snapshot did not produce state.json"
log "snapshot captured"

# CH ran as root — reclaim ownership so we can finalise as the build user.
sudo chown -R "$(id -u):$(id -g)" "$BUILD_DIR"

# --- 8. hash + lay out the template -----------------------------------------
HEX="$(sha256sum "$BUILD_DIR/rootfs.qcow2" | cut -d' ' -f1)"
NEW_HASH="sha256:$HEX"
FINAL_DIR="$CACHE_DIR/$NEW_HASH"

# The snapshot's config.json carries absolute paths into $BUILD_DIR. The
# disk path is rewritten per-sandbox at restore time, but the kernel path
# is only rewritten when relative — so it must point at the final dir.
python3 - "$BUILD_DIR/snapshot/config.json" \
	"$FINAL_DIR/vmlinux" "$FINAL_DIR/rootfs.qcow2" <<'PY'
import json, sys
cfg_path, kernel, disk = sys.argv[1], sys.argv[2], sys.argv[3]
with open(cfg_path) as f:
    cfg = json.load(f)
cfg.setdefault("payload", {})["kernel"] = kernel
for d in cfg.get("disks") or []:
    d["path"] = disk
with open(cfg_path, "w") as f:
    json.dump(cfg, f)
PY

# Keep only the template triple in the final directory.
rm -f "$BUILD_DIR/vajra-guest-agent" "$BUILD_DIR/vmconfig.json" \
	"$BUILD_DIR/ch.log" "$API_SOCK" "$VSOCK_SOCK"

if [ -e "$FINAL_DIR" ]; then
	log "target $FINAL_DIR already exists — replacing"
	sudo rm -rf "$FINAL_DIR"
fi
mv "$BUILD_DIR" "$FINAL_DIR"
BUILD_OK=1

echo "$NEW_HASH" > /tmp/vajra-new-template-hash
log "template laid out at $FINAL_DIR"
ls -la "$FINAL_DIR"
echo
echo "NEW_TEMPLATE_HASH=$NEW_HASH"
echo "TEMPLATE_NAME=$TEMPLATE_NAME"
echo "ROOTFS_PATH=$FINAL_DIR/rootfs.qcow2"
echo "KERNEL_PATH=$FINAL_DIR/vmlinux"
echo "SNAPSHOT_PATH=$FINAL_DIR/snapshot"
log "DONE"
