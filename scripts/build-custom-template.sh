#!/usr/bin/env bash
#
# build-custom-template.sh — build a Vajra microVM template by customising
# the known-good base template (ubuntu-noble) with caller-supplied setup
# commands.
#
# WHY DERIVE FROM A BASE
# ----------------------
# The agent only ever *restores* templates from a Cloud Hypervisor snapshot
# (internal/agent/sandbox.go) — there is no cold-boot path. A usable
# template is therefore a (rootfs, kernel, snapshot) triple, not just a
# rootfs. Synthesising a fresh rootfs from an arbitrary image would also
# not match the project's vmlinux/cmdline. So, like scripts/build-template.sh,
# this derives from the known-good base: copy its rootfs, run the caller's
# setup script inside it (chroot), then boot + snapshot the result.
#
# PIPELINE
# --------
#   COPYING      standalone qcow2 copy of the base rootfs + kernel
#   CUSTOMISING  qemu-nbd mount → chroot → run caller's setup script
#   BOOTING      boot the customised rootfs headless under Cloud Hypervisor
#   SNAPSHOTTING wait for guest-agent on vsock → pause → snapshot
#   HASHING      sha256 the rootfs, lay the triple out under the image cache
#   DONE         print NEW_TEMPLATE_HASH / ROOTFS_PATH / KERNEL_PATH / SNAPSHOT_PATH
#
# Usage:  build-custom-template.sh <template-name> <setup-script-path>
# Env:    SRC_HASH   base template hash (e.g. sha256:<hex>) — required
#         BUILD_ID   unique id for the build dir (default: timestamp)
#
set -euo pipefail

TEMPLATE_NAME="${1:?template name required}"
SETUP_SCRIPT="${2:?setup script path required}"
[ -f "$SETUP_SCRIPT" ] || { echo "[build] ERROR: setup script not found: $SETUP_SCRIPT" >&2; exit 1; }

CACHE_DIR="${VAJRA_AGENT_CACHE_DIR:-/var/lib/vajra/cache}"
CH_BIN="${VAJRA_AGENT_CH_BINARY:-/usr/local/bin/cloud-hypervisor}"
BUILD_ID="${BUILD_ID:-$(date +%s)}"
BUILD_DIR="/var/lib/vajra/build-custom/$BUILD_ID"
MNT="/mnt/vajra-custom-$BUILD_ID"
NBD="/dev/nbd0"
API_SOCK="$BUILD_DIR/ch-api.sock"
VSOCK_SOCK="$BUILD_DIR/vsock.sock"
GUEST_AGENT_PORT=5252            # exec port — first thing the guest-agent binds
VSOCK_WAIT_SECS=150              # generous: covers a cold cloud-image boot
QUIESCE_SECS=20                  # let the guest settle before snapshotting
MIN_FREE_MB=5120                 # GATE 3.1: refuse builds under 5 GB free

log()   { echo "[build] $*"; }
phase() { echo "PHASE:$1"; }
fail()  { echo "[build] ERROR: $*" >&2; exit 1; }

CH_PID=""
BUILD_OK=0
cleanup() {
	# Best-effort teardown — never let cleanup mask the real error.
	set +e
	if [ -n "$CH_PID" ]; then
		kill -0 "$CH_PID" 2>/dev/null && sudo kill -9 "$CH_PID" 2>/dev/null
		sudo pkill -9 -f "path=$API_SOCK" 2>/dev/null
	fi
	# Unmount chroot binds inner-first; lazy unmount as a fallback.
	for m in "$MNT/dev/pts" "$MNT/dev" "$MNT/proc" "$MNT/sys" "$MNT"; do
		sudo umount "$m" 2>/dev/null || sudo umount -l "$m" 2>/dev/null
	done
	sudo qemu-nbd --disconnect "$NBD" >/dev/null 2>&1
	sudo rmdir "$MNT" 2>/dev/null
	if [ "$BUILD_OK" -ne 1 ]; then
		sudo rm -rf "$BUILD_DIR" 2>/dev/null
		log "build failed — cleaned up $BUILD_DIR"
	fi
}
trap cleanup EXIT INT TERM

# --- GATE 3.1: disk check ---------------------------------------------------
free_mb=$(df -Pm /var/lib/vajra 2>/dev/null | awk 'NR==2{print $4}')
[ "${free_mb:-0}" -ge "$MIN_FREE_MB" ] \
	|| fail "insufficient disk: ${free_mb:-0}MB free under /var/lib/vajra, need ${MIN_FREE_MB}MB"

# --- resolve the base template ----------------------------------------------
SRC_HASH="${SRC_HASH:-${VAJRA_AGENT_POOL_TEMPLATE:-}}"
[ -n "$SRC_HASH" ] || fail "SRC_HASH (base template hash) not set"
SRC_DIR="$CACHE_DIR/$SRC_HASH"
[ -f "$SRC_DIR/rootfs.qcow2" ] || fail "base rootfs missing: $SRC_DIR/rootfs.qcow2"
[ -f "$SRC_DIR/vmlinux" ]      || fail "base kernel missing: $SRC_DIR/vmlinux"

command -v qemu-nbd >/dev/null || fail "qemu-nbd not installed"
command -v qemu-img >/dev/null || fail "qemu-img not installed"
command -v python3  >/dev/null || fail "python3 not installed"
[ -x "$CH_BIN" ]               || fail "cloud-hypervisor not found at $CH_BIN"

log "template=$TEMPLATE_NAME base=$SRC_HASH build=$BUILD_ID free=${free_mb}MB"

# --- 1. standalone copy of the base rootfs ----------------------------------
phase COPYING
sudo rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"
log "copying base rootfs (qemu-img convert, standalone qcow2)..."
qemu-img convert -f qcow2 -O qcow2 "$SRC_DIR/rootfs.qcow2" "$BUILD_DIR/rootfs.qcow2"
cp "$SRC_DIR/vmlinux" "$BUILD_DIR/vmlinux"

# --- 2. mount the rootfs and run the caller's setup script (chroot) ---------
phase CUSTOMISING
sudo modprobe nbd max_part=8
sudo qemu-nbd --disconnect "$NBD" >/dev/null 2>&1 || true
sudo qemu-nbd --connect="$NBD" "$BUILD_DIR/rootfs.qcow2"
for _ in $(seq 1 10); do [ -b "${NBD}p1" ] && break; sleep 1; done
[ -b "${NBD}p1" ] || fail "partition ${NBD}p1 never appeared"

# The base qcow2 came from a paused (not cleanly shut down) VM, so its ext4
# journal may be dirty — recover it before a read-write mount. e2fsck exit
# codes 0/1 are success ("clean" / "errors corrected"); >=4 is fatal.
fsck_rc=0
sudo e2fsck -fy "${NBD}p1" || fsck_rc=$?
[ "$fsck_rc" -lt 4 ] || fail "e2fsck failed on rootfs (rc=$fsck_rc)"

sudo mkdir -p "$MNT"
sudo mount "${NBD}p1" "$MNT"
[ -f "$MNT/etc/systemd/system/vajra-guest-agent.service" ] \
	|| fail "base rootfs has no vajra-guest-agent.service unit — wrong base template?"

# Bind /dev, /proc, /sys and supply working DNS so the setup script can run
# apt-get (chroot shares the host network namespace; 169.254.169.253 is the
# Amazon-provided resolver, reachable from any EC2 instance). The base
# rootfs ships /etc/resolv.conf as a symlink into /run (empty at build
# time) — replace it outright with a static resolver.
sudo mount --bind /dev     "$MNT/dev"
sudo mount --bind /dev/pts "$MNT/dev/pts"
sudo mount -t proc  proc   "$MNT/proc"
sudo mount -t sysfs sysfs  "$MNT/sys"
sudo rm -f "$MNT/etc/resolv.conf"
printf 'nameserver 169.254.169.253\nnameserver 8.8.8.8\n' | sudo tee "$MNT/etc/resolv.conf" >/dev/null

sudo install -m 0755 "$SETUP_SCRIPT" "$MNT/tmp/vajra-setup.sh"
log "running caller setup script in chroot..."
setup_rc=0
sudo chroot "$MNT" /usr/bin/env DEBIAN_FRONTEND=noninteractive \
	/bin/bash -e /tmp/vajra-setup.sh || setup_rc=$?
sudo rm -f "$MNT/tmp/vajra-setup.sh"
[ "$setup_rc" -eq 0 ] || fail "setup script failed (exit $setup_rc)"

# Trim the apt cache so the rootfs stays lean; keep cloud-init disabled so
# the build VM boots fast and deterministically (no datasource search).
sudo chroot "$MNT" /bin/bash -c 'command -v apt-get >/dev/null && apt-get clean' 2>/dev/null || true
sudo touch "$MNT/etc/cloud/cloud-init.disabled"
sync

for m in "$MNT/dev/pts" "$MNT/dev" "$MNT/proc" "$MNT/sys" "$MNT"; do
	sudo umount "$m" || sudo umount -l "$m"
done
sudo qemu-nbd --disconnect "$NBD"
log "customisation applied"

# --- 3. boot the customised VM headless -------------------------------------
phase BOOTING
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
	out=$(sudo curl -s -w '\n%{http_code}' --unix-socket "$API_SOCK" \
		-X "$method" "http://localhost/api/v1/$ep" "$@" 2>/dev/null) || true
	code="${out##*$'\n'}"
	if [ "${code:0:1}" != "2" ]; then
		echo "[build] CH $ep -> HTTP ${code:-?}: ${out%$'\n'*}" >&2
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
	ch_api PUT vm.boot || { cat "$BUILD_DIR/ch.log" >&2; fail "vm.boot failed (state was '${state:-unknown}')"; }
fi
log "VM booted; waiting for guest-agent on vsock port $GUEST_AGENT_PORT..."

# --- 4. wait for the guest-agent to answer ----------------------------------
phase SNAPSHOTTING
# CH hybrid-vsock: connect to the host socket, write "CONNECT <port>\n",
# expect "OK ...". A reply means the guest-agent's listener is up.
sudo python3 - "$VSOCK_SOCK" "$GUEST_AGENT_PORT" "$VSOCK_WAIT_SECS" <<'PY' || { cat "$BUILD_DIR/ch.log" >&2; fail "guest-agent never came up"; }
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
            print("[build] guest-agent responded: %s"
                  % data.decode("ascii", "replace").strip())
            sys.exit(0)
    except Exception:
        pass
    time.sleep(1)
sys.exit(1)
PY

log "guest-agent up; quiescing ${QUIESCE_SECS}s before snapshot..."
sleep "$QUIESCE_SECS"

# --- 5. pause + snapshot ----------------------------------------------------
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

# CH ran as root — reclaim ownership so the agent (same user as this script)
# can read the snapshot's 0600 files.
sudo chown -R "$(id -u):$(id -g)" "$BUILD_DIR"

# --- 6. hash + lay the template out under the image cache -------------------
phase HASHING
HEX="$(sha256sum "$BUILD_DIR/rootfs.qcow2" | cut -d' ' -f1)"
NEW_HASH="sha256:$HEX"
FINAL_DIR="$CACHE_DIR/$NEW_HASH"

# The snapshot's config.json carries absolute paths into $BUILD_DIR. The disk
# path is rewritten per-sandbox at restore time, but the kernel path is only
# rewritten when relative — so point both at the final cache directory.
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

rm -f "$API_SOCK" "$VSOCK_SOCK" "$BUILD_DIR/ch.log"

if [ -e "$FINAL_DIR" ]; then
	log "target $FINAL_DIR already exists — replacing"
	sudo rm -rf "$FINAL_DIR"
fi
mv "$BUILD_DIR" "$FINAL_DIR"
BUILD_OK=1

phase DONE
log "template laid out at $FINAL_DIR"
echo "NEW_TEMPLATE_HASH=$NEW_HASH"
echo "TEMPLATE_NAME=$TEMPLATE_NAME"
echo "ROOTFS_PATH=$FINAL_DIR/rootfs.qcow2"
echo "KERNEL_PATH=$FINAL_DIR/vmlinux"
echo "SNAPSHOT_PATH=$FINAL_DIR/snapshot"
log "DONE"
