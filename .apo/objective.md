---
optimization:
  backend: optuna
  alphaevolve:
    program_language: go
    max_iterations: 20
    concurrency: 4
    timeout_sec: 1800
  metrics:
    - name: match_materialized_96pods_ns_per_op
      goal: minimize
      min_improvement_pct: 3.0
      noise_tolerance_pct: 1.5
    - name: match_walk_96pods_ns_per_op
      goal: minimize
      min_improvement_pct: 2.0
      noise_tolerance_pct: 1.5
    - name: match_materialized_96pods_bytes_per_op
      goal: minimize
      min_improvement_pct: 5.0
      noise_tolerance_pct: 0.5
  constraints:
    - metric: unit_test_failures
      max: 0.0
  allowed_file_scope: |
    pkg/kvcache/prefix_match.go
---

# Objective: Algorithmic Optimization of KV-Cache Prefix Block Matcher (`pkg/kvcache/prefix_match.go`)

Minimize CPU execution latency (`match_materialized_96pods_ns_per_op` and `match_walk_96pods_ns_per_op`) and heap memory allocations (`match_materialized_96pods_bytes_per_op`) when matching 240K-token prompt block-key chains (`3,750` block hashes at block size 64) across `96` candidate model server pods in [`pkg/kvcache/prefix_match.go`](pkg/kvcache/prefix_match.go).

## Strict Correctness & Behavioral Invariants

1. **Zero Behavioral Drift (`unit_test_failures <= 0.0`)**: All unit tests in `./pkg/kvcache/...` (`TestMatchBlockKeys*`, `TestPrefixAccumulator*`, etc.) and all benchmark assertions (`requireMatches`) must pass with 0 failures.
2. **Exported Signature Stability**: Do not alter the exported `PodMatch` struct fields (`WeightedScore`, `MatchedBlocks`, `ConfirmedBlocks`, `BlocksByTier`), the `SpeculativeTier` constant, or the `(k *Indexer) MatchBlockKeys` method signature.
3. **Matching Semantics**:
   - Candidate pods are strictly determined by the first key (`keys[0]`) filtered by `podFilter` (if non-empty).
   - A pod's contiguous prefix chain ends immediately at the first key that the pod does not hold.
   - Duplicate entries for the same pod at the same key take the highest tier weight.
   - `ConfirmedBlocks` counts contiguous keys held in any non-speculative tier (`!ref.Speculative && ref.DeviceTier != SpeculativeTier`), ending at the first speculative or missing key.
   - `BlocksByTier` tracks each tier's independent contiguous prefix chain from `keys[0]`.

## Known Algorithmic Bottlenecks in `pkg/kvcache/prefix_match.go`

1. **Dead-Chain Work in `prefixAccumulator.key()`**: Once a pod misses key $k$, `endKey()` removes it from `a.active`. However, for all subsequent keys $k+1 \dots 3750$, `a.table.lookup(ref.PodOrdinal)` still finds the dead slot and executes `weightOf()`, `slot.seen = a.keyStamp`, and `stampTier()`. Skipping dead slots (`slot.seen < a.keyStamp - 1`) or pruning dead entries avoids $O(K \times P_{\text{dead}})$ wasted work.
2. **Double Hashing & Unpooled Map Allocations in `matchMaterialized()`**: `matchMaterialized` allocates two new string maps (`pods, tiers := ordinalTable{}, ordinalTable{}`) on every call and hashes `e.PodIdentifier` and `e.DeviceTier` up to $3,750 \times 96 = 360,000$ times to construct intermediate `kvblock.EntryRef` slices before `acc.key()` hashes `PodOrdinal` again. Direct string-to-slot indexing or pooling eliminates redundant hashing and allocations.
3. **Heap Allocations in `prefixAccumulator.result()`**: `result()` allocates a separate `map[string]int` (`byTier`) per candidate pod.
