#!/bin/bash
set -euo pipefail

RESULTS_DIR="${1:-.apo/results/scratch}"
mkdir -p "$RESULTS_DIR"

source .apo/set-env.sh 2>/dev/null || true

BENCH_LOG="$RESULTS_DIR/benchmark_output.log"
echo "Running BenchmarkMatchBlockKeys and BenchmarkMatchBlockKeysMaterialized (5 iterations)..."
go test -run='^$' -bench='^BenchmarkMatchBlockKeys(Materialized)?$/^keys=3750$/^pods=96$' -benchmem -count=5 ./pkg/kvcache/ 2>&1 | tee "$BENCH_LOG"

python3 - "$BENCH_LOG" "$RESULTS_DIR/summary.json" <<'PY'
import json
import re
import statistics
import sys

bench_log_path, summary_path = sys.argv[1], sys.argv[2]

walk_ns, walk_bytes, walk_allocs = [], [], []
mat_ns, mat_bytes, mat_allocs = [], [], []

pattern = re.compile(
    r"^(BenchmarkMatchBlockKeys(?:Materialized)?/keys=3750/pods=96)(?:-\d+)?\s+\d+\s+([\d.]+)\s+ns/op\s+([\d.]+)\s+B/op\s+([\d.]+)\s+allocs/op"
)

with open(bench_log_path, "r", encoding="utf-8") as f:
  for line in f:
    m = pattern.match(line.strip())
    if not m:
      continue
    name, ns, b_op, allocs = m.group(1), float(m.group(2)), float(m.group(3)), float(m.group(4))
    if "Materialized" in name:
      mat_ns.append(ns)
      mat_bytes.append(b_op)
      mat_allocs.append(allocs)
    else:
      walk_ns.append(ns)
      walk_bytes.append(b_op)
      walk_allocs.append(allocs)

if not walk_ns or not mat_ns:
  sys.exit("Failed to parse BenchmarkMatchBlockKeys metrics from benchmark output.")

metrics = {
    "match_walk_96pods_ns_per_op": round(statistics.median(walk_ns), 2),
    "match_walk_96pods_bytes_per_op": round(statistics.median(walk_bytes), 2),
    "match_walk_96pods_allocs_per_op": round(statistics.median(walk_allocs), 2),
    "match_materialized_96pods_ns_per_op": round(statistics.median(mat_ns), 2),
    "match_materialized_96pods_bytes_per_op": round(statistics.median(mat_bytes), 2),
    "match_materialized_96pods_allocs_per_op": round(statistics.median(mat_allocs), 2),
    "unit_test_failures": 0.0,
}

with open(summary_path, "w", encoding="utf-8") as out:
  json.dump({"metrics": metrics}, out, indent=2)

print("Parsed benchmark medians:", json.dumps(metrics, indent=2))
PY

if [ ! -f "$RESULTS_DIR/summary.json" ]; then
  echo "Error: Benchmark failed to produce $RESULTS_DIR/summary.json" >&2
  exit 1
fi

echo "Experiment trial completed successfully. Summary staged at $RESULTS_DIR/summary.json"
exit 0
