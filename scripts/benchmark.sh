#!/usr/bin/env bash
# benchmark.sh — build and run the Go-based snapshot restore benchmark.
#
# Usage:
#   scripts/benchmark.sh [snapshot-path]            # default /tmp/ch-snapshot
#   N=20 scripts/benchmark.sh /var/lib/vajra/snap   # 20 iterations
#
# Requires KVM (Linux) and a cloud-hypervisor binary. On a host without KVM
# the benchmark binary still builds; it just refuses to run.
set -euo pipefail

cd "$(dirname "$0")/.."

SNAPSHOT="${1:-/tmp/ch-snapshot}"
N="${N:-10}"
BIN="${BIN:-/usr/local/bin/cloud-hypervisor}"
OUT="${OUT:-bin/vajra-benchmark}"

mkdir -p "$(dirname "$OUT")"

echo ">>> building $OUT"
go build -o "$OUT" ./scripts

echo ">>> running benchmark (snapshot=$SNAPSHOT n=$N)"
exec "$OUT" -snapshot "$SNAPSHOT" -n "$N" -bin "$BIN"
