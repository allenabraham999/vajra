#!/usr/bin/env python3
"""Print recent boot times from the master boot-times endpoint.

Usage: bench_boot.py <api_base> <api_key> [name_prefix]
Filters to rows whose name starts with name_prefix (if given) and prints
each row plus p50/p95 of time_to_running_ms.
"""
import json
import sys
import urllib.request

api, key = sys.argv[1], sys.argv[2]
prefix = sys.argv[3] if len(sys.argv) > 3 else ""

req = urllib.request.Request(
    api + "/v1/sandboxes/boot-times",
    headers={"Authorization": "Bearer " + key},
)
rows = json.load(urllib.request.urlopen(req, timeout=15))
if prefix:
    rows = [r for r in rows if r["name"].startswith(prefix)]

for r in rows:
    print(f"  {r['name']:<22} {r['time_to_running_ms']:>7} ms  pool_hit={r['pool_hit']}")

vals = sorted(r["time_to_running_ms"] for r in rows)
if vals:
    def pct(p):
        return vals[min(len(vals) - 1, int(round(p / 100.0 * (len(vals) - 1))))]
    print(f"  n={len(vals)}  p50={pct(50)} ms  p95={pct(95)} ms  "
          f"min={vals[0]} ms  max={vals[-1]} ms")
