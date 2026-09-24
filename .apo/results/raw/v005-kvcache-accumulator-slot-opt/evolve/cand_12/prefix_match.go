/*
Copyright 2026 The llm-d Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kvcache

import (
	"context"
	"math"
	"sync"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/llm-d/llm-d-router/pkg/common/observability/semconv"
	"github.com/llm-d/llm-d-router/pkg/common/observability/tracing"
	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
	"github.com/llm-d/llm-d-router/pkg/kvcache/metrics"
)

// SpeculativeTier is the tier name under which speculative entries count in
// PodMatch.BlocksByTier. Speculative entries carry no engine-reported device
// tier; an entry whose device tier is reported under this name counts in the
// same chain.
const SpeculativeTier = "speculative"

// speculativeTierWeight scores speculative entries when the speculative tier
// has no configured weight.
const speculativeTierWeight = 1.0

// unknownTierWeight scores blocks held in a tier without a configured weight.
const unknownTierWeight = 0.0

// matchCancellationMask paces context-cancellation checks over key
// positions: positions where pos&mask == 0 poll ctx.Err().
const matchCancellationMask = 255

// PodMatch is one pod's prefix match for a key sequence. All values cover
// the contiguous chain of keys the pod holds, counted from the first key.
type PodMatch struct {
	// WeightedScore sums, per block of the chain, the highest device-tier
	// weight among the pod's entries for that block. Tiers without a
	// configured weight count unknownTierWeight; speculative entries count
	// speculativeTierWeight unless the speculative tier is configured.
	WeightedScore float64
	// MatchedBlocks is the chain length in blocks, regardless of tier.
	MatchedBlocks int
	// ConfirmedBlocks is the chain length in blocks counting only keys the
	// pod holds in an engine-reported device tier; the tier may change from
	// block to block. Speculative entries end the chain.
	ConfirmedBlocks int
	// BlocksByTier is the per-tier chain length: a tier counts a block only
	// while the pod holds every previous block in that same tier.
	// Speculative entries count under SpeculativeTier. Never nil.
	BlocksByTier map[string]int
}

// MatchBlockKeys runs the prefix matcher over keys for the pods in podFilter
// (every pod when empty) and returns one PodMatch per pod that holds the
// first key. Empty keys match nothing. The matcher walks the index when the
// backend is a kvblock.KeyWalker and otherwise materializes Lookup; both
// feed the same accumulator.
func (k *Indexer) MatchBlockKeys(ctx context.Context, keys []kvblock.BlockHash,
	podFilter sets.Set[string],
) (map[string]PodMatch, error) {
	if len(keys) == 0 {
		return map[string]PodMatch{}, nil
	}

	tracer := tracing.Tracer(TracerScope)
	ctx, span := tracer.Start(ctx, "match_block_keys",
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	defer span.End()
	span.SetAttributes(
		semconv.LLMDKVCachePrefixMatchKeyCount(len(keys)),
		semconv.LLMDKVCachePrefixMatchPodFilterCount(podFilter.Len()),
		semconv.LLMDKVCachePrefixMatchWalked(k.keyWalker != nil),
	)

	var matches map[string]PodMatch
	var err error
	if k.keyWalker != nil {
		matches, err = matchWalk(ctx, k.keyWalker, keys, k.tierWeights, podFilter)
	} else {
		matches, err = matchLookup(ctx, k.kvBlockIndex, keys, k.tierWeights, podFilter)
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	blocksFound := maxMatchedBlocks(matches)
	if k.recordHits {
		metrics.MaxPodHitCount.Add(float64(blocksFound))
		metrics.LookupHits.Add(float64(blocksFound))
	}
	span.SetAttributes(
		semconv.LLMDKVCachePrefixMatchPodsMatched(len(matches)),
		semconv.LLMDKVCachePrefixMatchLongestChain(blocksFound),
	)
	return matches, nil
}

// maxMatchedBlocks returns the longest chain among matches.
func maxMatchedBlocks(matches map[string]PodMatch) int {
	longest := 0
	for _, m := range matches {
		if m.MatchedBlocks > longest {
			longest = m.MatchedBlocks
		}
	}
	return longest
}

// matchWalk feeds the accumulator from an ordered index walk. The walk ends
// at the first key without entries or once no chain is alive.
func matchWalk(ctx context.Context, walker kvblock.KeyWalker, keys []kvblock.BlockHash,
	weights map[string]float64, filter sets.Set[string],
) (map[string]PodMatch, error) {
	acc := acquireAccumulator(weights, filter)
	defer releaseAccumulator(acc)

	err := walker.WalkKeys(ctx, keys, func(_ int, found bool, entries []kvblock.EntryRef) bool {
		return found && len(entries) > 0 && acc.key(entries)
	})
	if err != nil {
		return nil, err
	}
	return acc.result(), nil
}

// matchLookup feeds the accumulator from a materialized Lookup result, for
// backends without the walk capability.
func matchLookup(ctx context.Context, index kvblock.Index, keys []kvblock.BlockHash,
	weights map[string]float64, filter sets.Set[string],
) (map[string]PodMatch, error) {
	keyToPods, err := index.Lookup(ctx, keys, filter)
	if err != nil {
		return nil, err
	}
	return matchMaterialized(ctx, keys, keyToPods, weights, filter)
}

// matchMaterialized feeds the accumulator from a Lookup result, walking keys
// in order and stopping at the first key without entries. Pod and tier
// ordinals are assigned per call, since materialized entries carry none.
func fastHash(s string) uint32 {
	n := len(s)
	if n >= 3 {
		return uint32(s[n-1])*33 ^ uint32(s[n-2])*31 ^ uint32(s[n-3]) ^ uint32(n)
	}
	if n == 2 {
		return uint32(s[1])*31 ^ uint32(s[0]) ^ 2
	}
	if n == 1 {
		return uint32(s[0]) ^ 1
	}
	return 0
}

func matchMaterialized(ctx context.Context, keys []kvblock.BlockHash,
	keyToPods map[kvblock.BlockHash][]kvblock.PodEntry,
	weights map[string]float64, filter sets.Set[string],
) (map[string]PodMatch, error) {
	acc := acquireAccumulator(weights, filter)
	defer releaseAccumulator(acc)

	var podCacheName [1024]string
	var podCacheOrd [1024]uint32

	var posCacheName [256]string
	var posCacheOrd [256]uint32

	var mruName string
	var mruOrd uint32
	var hasMru bool

	var tierCacheName [4]string
	var tierCacheOrd [4]uint32
	var lastTierName string
	var lastTierOrd uint32
	var hasLastTier bool

	for pos, key := range keys {
		if pos&matchCancellationMask == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		entries := keyToPods[key]
		if len(entries) == 0 {
			break
		}
		if cap(acc.refsBuf) < len(entries) {
			allocCap := len(entries)
			if allocCap < 128 {
				allocCap = 128
			} else {
				allocCap = allocCap * 2
			}
			acc.refsBuf = make([]kvblock.EntryRef, allocCap)
		}
		acc.refsBuf = acc.refsBuf[:len(entries)]

		if pos == 0 {
			writeIdx := 0
			for i := range entries {
				e := &entries[i]
				podOrd := acc.podOrdinal(e.PodIdentifier)

				h := fastHash(e.PodIdentifier)
				idx := h & 1023
				for {
					if podCacheName[idx] == "" {
						podCacheName[idx] = e.PodIdentifier
						podCacheOrd[idx] = podOrd
						break
					}
					if podCacheName[idx] == e.PodIdentifier {
						break
					}
					idx = (idx + 1) & 1023
				}

				if i < 256 {
					posCacheName[i] = e.PodIdentifier
					posCacheOrd[i] = podOrd
				}
				mruName = e.PodIdentifier
				mruOrd = podOrd
				hasMru = true

				var tierOrd uint32
				if hasLastTier && lastTierName == e.DeviceTier {
					tierOrd = lastTierOrd
				} else {
					if tierCacheName[0] == e.DeviceTier {
						tierOrd = tierCacheOrd[0]
					} else if tierCacheName[1] == e.DeviceTier {
						tierOrd = tierCacheOrd[1]
					} else if tierCacheName[2] == e.DeviceTier {
						tierOrd = tierCacheOrd[2]
					} else if tierCacheName[3] == e.DeviceTier {
						tierOrd = tierCacheOrd[3]
					} else {
						tierOrd = acc.tierOrdinal(e.DeviceTier)
						if tierCacheName[0] == "" {
							tierCacheName[0] = e.DeviceTier
							tierCacheOrd[0] = tierOrd
						} else if tierCacheName[1] == "" {
							tierCacheName[1] = e.DeviceTier
							tierCacheOrd[1] = tierOrd
						} else if tierCacheName[2] == "" {
							tierCacheName[2] = e.DeviceTier
							tierCacheOrd[2] = tierOrd
						} else if tierCacheName[3] == "" {
							tierCacheName[3] = e.DeviceTier
							tierCacheOrd[3] = tierOrd
						}
					}
					lastTierName = e.DeviceTier
					lastTierOrd = tierOrd
					hasLastTier = true
				}

				ref := &acc.refsBuf[writeIdx]
				ref.PodEntry = *e
				ref.PodOrdinal = podOrd
				ref.TierOrdinal = tierOrd
				writeIdx++
			}
			acc.refsBuf = acc.refsBuf[:writeIdx]
		} else {
			writeIdx := 0
			for i := range entries {
				e := &entries[i]
				var podOrd uint32
				found := false

				if i < 256 && posCacheName[i] == e.PodIdentifier {
					podOrd = posCacheOrd[i]
					found = true
					mruName = e.PodIdentifier
					mruOrd = podOrd
					hasMru = true
				} else if hasMru && mruName == e.PodIdentifier {
					podOrd = mruOrd
					found = true
					if i < 256 {
						posCacheName[i] = e.PodIdentifier
						posCacheOrd[i] = podOrd
					}
				} else {
					h := fastHash(e.PodIdentifier)
					idx := h & 1023
					for {
						if podCacheName[idx] == "" {
							break
						}
						if podCacheName[idx] == e.PodIdentifier {
							podOrd = podCacheOrd[idx]
							found = true
							break
						}
						idx = (idx + 1) & 1023
					}
					if found {
						mruName = e.PodIdentifier
						mruOrd = podOrd
						hasMru = true
						if i < 256 {
							posCacheName[i] = e.PodIdentifier
							posCacheOrd[i] = podOrd
						}
					}
				}

				if !found {
					continue
				}

				var tierOrd uint32
				if hasLastTier && lastTierName == e.DeviceTier {
					tierOrd = lastTierOrd
				} else {
					if tierCacheName[0] == e.DeviceTier {
						tierOrd = tierCacheOrd[0]
					} else if tierCacheName[1] == e.DeviceTier {
						tierOrd = tierCacheOrd[1]
					} else if tierCacheName[2] == e.DeviceTier {
						tierOrd = tierCacheOrd[2]
					} else if tierCacheName[3] == e.DeviceTier {
						tierOrd = tierCacheOrd[3]
					} else {
						tierOrd = acc.tierOrdinal(e.DeviceTier)
						if tierCacheName[0] == "" {
							tierCacheName[0] = e.DeviceTier
							tierCacheOrd[0] = tierOrd
						} else if tierCacheName[1] == "" {
							tierCacheName[1] = e.DeviceTier
							tierCacheOrd[1] = tierOrd
						} else if tierCacheName[2] == "" {
							tierCacheName[2] = e.DeviceTier
							tierCacheOrd[2] = tierOrd
						} else if tierCacheName[3] == "" {
							tierCacheName[3] = e.DeviceTier
							tierCacheOrd[3] = tierOrd
						}
					}
					lastTierName = e.DeviceTier
					lastTierOrd = tierOrd
					hasLastTier = true
				}

				ref := &acc.refsBuf[writeIdx]
				ref.PodEntry = *e
				ref.PodOrdinal = podOrd
				ref.TierOrdinal = tierOrd
				writeIdx++
			}
			acc.refsBuf = acc.refsBuf[:writeIdx]
		}

		if !acc.key(acc.refsBuf) {
			break
		}
	}
	// Cancellation is sampled at checkpoints along the keys and once more at
	// completion, so a cancelled request never reports a match.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return acc.result(), nil
}

func (a *prefixAccumulator) podOrdinal(name string) uint32 {
	if id, ok := a.podsMap[name]; ok {
		return id
	}
	id := uint32(len(a.podsMap))
	a.podsMap[name] = id
	return id
}

func (a *prefixAccumulator) tierOrdinal(name string) uint32 {
	if id, ok := a.tiersMap[name]; ok {
		return id
	}
	id := uint32(len(a.tiersMap))
	a.tiersMap[name] = id
	return id
}

// speculativeTierOrdinal keys the speculative per-tier chain. Feeders assign
// tier ordinals from zero, so the top of the range never collides.
const speculativeTierOrdinal = math.MaxUint32

// EVOLVE-BLOCK-START
// slotTable is an open-addressed map from pod ordinal to request-local slot.
// It is sized by the first key's entry count, so request state scales with
// the live candidates rather than with every ordinal an index ever assigned.
// It packs (ordinal << 32) | (slot_index + 1) into a single uint64 bucket
// to minimize cache lines and avoid struct/alignment overhead.
type slotTable struct {
	buckets []uint64
}

func (t *slotTable) reset(numEntries int) {
	size := 2
	for size < numEntries*2 {
		size <<= 1
	}
	if cap(t.buckets) < size {
		t.buckets = make([]uint64, size)
		return
	}
	t.buckets = t.buckets[:size]
	clear(t.buckets)
}

func (t *slotTable) lookup(ordinal uint32) (int32, bool) {
	buckets := t.buckets
	n := len(buckets)
	if n == 0 {
		return 0, false
	}
	mask := uint32(n - 1)
	_ = buckets[mask]
	i := (ordinal * 2654435761) & mask
	for {
		b := buckets[i]
		slot := uint32(b)
		if slot == 0 {
			return 0, false
		}
		if uint32(b>>32) == ordinal {
			return int32(slot - 1), true
		}
		i = (i + 1) & mask
	}
}

func (t *slotTable) insert(ordinal uint32, slot int32) {
	buckets := t.buckets
	mask := uint32(len(buckets) - 1)
	_ = buckets[mask]
	i := (ordinal * 2654435761) & mask
	for uint32(buckets[i]) != 0 {
		i = (i + 1) & mask
	}
	buckets[i] = (uint64(ordinal) << 32) | uint64(slot+1)
}

// tierChain tracks one tier's contiguous prefix for a candidate pod.
type tierChain struct {
	ordinal uint32
	name    string
	count   int
	// seen is the key stamp of the last key where the pod held this tier.
	seen  uint32
	alive bool
}

// tierWeight is one tier's resolved weight, keyed by tier ordinal.
type tierWeight struct {
	ordinal uint32
	weight  float64
}

// matchSlot is one candidate pod's accumulated state.
type matchSlot struct {
	pod     string
	matched int
	score   float64
	// seen is the key stamp of the last key holding this pod; weight is the
	// highest tier weight among its entries at that key.
	seen   uint32
	weight float64
	tiers  []tierChain
	// confirmed tracks the chain of keys held in a non-speculative tier;
	// confirmedSeen is the key stamp of the last key holding one.
	confirmed      int
	confirmedSeen  uint32
	confirmedAlive bool
}

type ordinalWeight struct {
	weight float64
	valid  bool
}

// prefixAccumulator folds an ordered walk over request keys into per-pod
// prefix matches. It is the single implementation of the matching rules:
// candidates are the pods holding the first key, each chain ends at the
// first key its pod does not hold, duplicate entries for a pod at one key
// take the highest weight, and every tier tracks its own contiguous prefix.
//
// Feeders present each key's entries through key, in key order, and stop at
// the first key without entries or once key reports no live chain. Ordinals
// only need to be stable within one accumulation; they key request-local
// tables and never size state, so sparse or large values cost nothing.
type prefixAccumulator struct {
	weights map[string]float64
	filter  sets.Set[string]

	table    slotTable
	slots    []matchSlot
	active   []int32
	keyStamp uint32
	first    bool

	// weightCache holds the weight of every tier seen in this accumulation,
	// scanned linearly: requests see a handful of tiers.
	weightCache []tierWeight

	podsMap  map[string]uint32
	tiersMap map[string]uint32
	refsBuf  []kvblock.EntryRef

	posSlotOrd      []uint32
	posSlotIdx      []int32
	weightByOrdinal []ordinalWeight
}

var accumulatorPool = sync.Pool{New: func() any {
	slots := make([]matchSlot, 256)
	for i := range slots {
		slots[i].tiers = make([]tierChain, 0, 4)
	}
	return &prefixAccumulator{
		table:           slotTable{buckets: make([]uint64, 512)},
		slots:           slots[:0],
		active:          make([]int32, 0, 256),
		weightCache:     make([]tierWeight, 0, 8),
		podsMap:         make(map[string]uint32, 128),
		tiersMap:        make(map[string]uint32, 16),
		refsBuf:         make([]kvblock.EntryRef, 0, 512),
		posSlotOrd:      make([]uint32, 0, 512),
		posSlotIdx:      make([]int32, 0, 512),
		weightByOrdinal: make([]ordinalWeight, 32),
	}
}}

func acquireAccumulator(weights map[string]float64, filter sets.Set[string]) *prefixAccumulator {
	a := accumulatorPool.Get().(*prefixAccumulator)
	a.weights, a.filter = weights, filter
	a.slots = a.slots[:0]
	a.active = a.active[:0]
	a.keyStamp = 0
	a.first = true
	a.weightCache = a.weightCache[:0]
	if a.podsMap == nil {
		a.podsMap = make(map[string]uint32, 128)
	} else {
		clear(a.podsMap)
	}
	if a.tiersMap == nil {
		a.tiersMap = make(map[string]uint32, 16)
	} else {
		clear(a.tiersMap)
	}
	clear(a.refsBuf)
	a.refsBuf = a.refsBuf[:0]

	a.posSlotOrd = a.posSlotOrd[:0]
	a.posSlotIdx = a.posSlotIdx[:0]

	if a.weightByOrdinal == nil {
		a.weightByOrdinal = make([]ordinalWeight, 32)
	} else {
		for i := range a.weightByOrdinal {
			a.weightByOrdinal[i].valid = false
		}
	}
	return a
}

func releaseAccumulator(a *prefixAccumulator) {
	a.weights, a.filter = nil, nil
	for i := 0; i < len(a.slots); i++ {
		a.slots[i].pod = ""
	}
	accumulatorPool.Put(a)
}

// keyFirst is the cold path for folding the very first key's entries.
func (a *prefixAccumulator) keyFirst(entries []kvblock.EntryRef) bool {
	a.table.reset(len(entries))
	if cap(a.posSlotOrd) < len(entries) {
		a.posSlotOrd = make([]uint32, len(entries))
		a.posSlotIdx = make([]int32, len(entries))
	} else {
		a.posSlotOrd = a.posSlotOrd[:len(entries)]
		a.posSlotIdx = a.posSlotIdx[:len(entries)]
	}
	for i := range a.posSlotOrd {
		a.posSlotOrd[i] = ^uint32(0)
	}

	var prev *kvblock.EntryRef
	for i := range entries {
		ref := &entries[i]
		if prev != nil && ref.PodOrdinal == prev.PodOrdinal && ref.TierOrdinal == prev.TierOrdinal &&
			ref.Speculative == prev.Speculative {
			continue
		}
		prev = ref

		s, ok := a.table.lookup(ref.PodOrdinal)
		if !ok {
			if a.filter.Len() > 0 && !a.filter.Has(ref.PodIdentifier) {
				continue
			}
			s = a.newSlot(ref.PodIdentifier)
			a.table.insert(ref.PodOrdinal, s)
		}
		slot := &a.slots[s]

		a.posSlotOrd[i] = ref.PodOrdinal
		a.posSlotIdx[i] = s

		tier, tierOrdinal := ref.DeviceTier, ref.TierOrdinal
		if ref.Speculative || ref.DeviceTier == SpeculativeTier {
			tier, tierOrdinal = SpeculativeTier, speculativeTierOrdinal
		} else {
			slot.confirmedSeen = a.keyStamp
		}

		w := a.weightOf(tier, tierOrdinal)
		if slot.seen != a.keyStamp {
			slot.seen = a.keyStamp
			slot.weight = w
		} else if w > slot.weight {
			slot.weight = w
		}

		if !a.stampTier(slot, tierOrdinal) {
			slot.tiers = append(slot.tiers, tierChain{ordinal: tierOrdinal, name: tier, seen: a.keyStamp, alive: true})
		}
	}
	return a.endKey()
}

// key folds one key's entries into the chains and reports whether any chain
// is still alive. entries is borrowed for the duration of the call.
func (a *prefixAccumulator) key(entries []kvblock.EntryRef) bool {
	a.keyStamp++
	if a.first {
		return a.keyFirst(entries)
	}

	var prev *kvblock.EntryRef
	var lastPodOrdinal uint32 = ^uint32(0)
	var lastSlot int32 = -1
	var lastOk bool = false

	posSlotOrd := a.posSlotOrd
	posSlotIdx := a.posSlotIdx
	lenPosSlotOrd := len(posSlotOrd)

	for i := range entries {
		ref := &entries[i]
		// Another rank of an endpoint just folded at this key adds nothing
		// to its chains.
		if prev != nil && ref.PodOrdinal == prev.PodOrdinal && ref.TierOrdinal == prev.TierOrdinal &&
			ref.Speculative == prev.Speculative {
			continue
		}
		prev = ref

		var s int32
		var ok bool
		if ref.PodOrdinal == lastPodOrdinal && lastOk {
			s = lastSlot
			ok = true
		} else if i < lenPosSlotOrd && posSlotOrd[i] == ref.PodOrdinal {
			s = posSlotIdx[i]
			ok = true
			lastPodOrdinal = ref.PodOrdinal
			lastSlot = s
			lastOk = true
		} else {
			s, ok = a.table.lookup(ref.PodOrdinal)
			if ok {
				if i >= lenPosSlotOrd {
					newLen := i + 1
					if cap(a.posSlotOrd) < newLen {
						newCap := newLen * 2
						newOrd := make([]uint32, newLen, newCap)
						newIdx := make([]int32, newLen, newCap)
						copy(newOrd, a.posSlotOrd)
						copy(newIdx, a.posSlotIdx)
						for k := len(a.posSlotOrd); k < newLen; k++ {
							newOrd[k] = ^uint32(0)
						}
						a.posSlotOrd = newOrd
						a.posSlotIdx = newIdx
					} else {
						oldLen := len(a.posSlotOrd)
						a.posSlotOrd = a.posSlotOrd[:newLen]
						a.posSlotIdx = a.posSlotIdx[:newLen]
						for k := oldLen; k < newLen; k++ {
							a.posSlotOrd[k] = ^uint32(0)
						}
					}
					posSlotOrd = a.posSlotOrd
					posSlotIdx = a.posSlotIdx
					lenPosSlotOrd = len(posSlotOrd)
				}
				posSlotOrd[i] = ref.PodOrdinal
				posSlotIdx[i] = s
				lastPodOrdinal = ref.PodOrdinal
				lastSlot = s
				lastOk = true
			} else {
				lastOk = false
			}
		}

		if !ok {
			continue // the first key fixes the candidate set
		}
		slot := &a.slots[s]
		if slot.seen < a.keyStamp-1 {
			continue // entries for dead slots should be pruned early
		}

		tier, tierOrdinal := ref.DeviceTier, ref.TierOrdinal
		if ref.Speculative || ref.DeviceTier == SpeculativeTier {
			tier, tierOrdinal = SpeculativeTier, speculativeTierOrdinal
		} else {
			slot.confirmedSeen = a.keyStamp
		}

		w := a.weightOf(tier, tierOrdinal)
		if slot.seen != a.keyStamp {
			slot.seen = a.keyStamp
			slot.weight = w
		} else if w > slot.weight {
			slot.weight = w
		}

		_ = a.stampTier(slot, tierOrdinal)
	}
	return a.endKey()
}

// stampTier marks tier as held at the current key and reports whether the
// slot tracks that tier.
func (a *prefixAccumulator) stampTier(slot *matchSlot, tierOrdinal uint32) bool {
	tiers := slot.tiers
	n := len(tiers)
	if n == 1 {
		if tiers[0].ordinal == tierOrdinal {
			tiers[0].seen = a.keyStamp
			return true
		}
		return false
	}
	if n == 2 {
		if tiers[0].ordinal == tierOrdinal {
			tiers[0].seen = a.keyStamp
			return true
		}
		if tiers[1].ordinal == tierOrdinal {
			tiers[1].seen = a.keyStamp
			return true
		}
		return false
	}
	for i := 0; i < n; i++ {
		if tiers[i].ordinal == tierOrdinal {
			tiers[i].seen = a.keyStamp
			return true
		}
	}
	return false
}

// endKey closes the current key and reports whether any chain is still
// alive.
func (a *prefixAccumulator) endKey() bool {
	if a.first {
		a.first = false
		nSlots := len(a.slots)
		if cap(a.active) < nSlots {
			a.active = make([]int32, 0, nSlots)
		}
		for i := range a.slots {
			s := &a.slots[i]
			s.matched, s.score = 1, s.weight
			if s.confirmedSeen == a.keyStamp {
				s.confirmed, s.confirmedAlive = 1, true
			}
			tiers := s.tiers
			nTiers := len(tiers)
			if nTiers == 1 {
				tiers[0].count = 1
			} else if nTiers == 2 {
				tiers[0].count = 1
				tiers[1].count = 1
			} else {
				for t := 0; t < nTiers; t++ {
					tiers[t].count = 1
				}
			}
			a.active = append(a.active, int32(i))
		}
		return len(a.active) > 0
	}

	keep := a.active[:0]
	for _, i := range a.active {
		s := &a.slots[i]
		if s.seen != a.keyStamp {
			continue // the chain ends at the first key the pod does not hold
		}
		s.matched++
		s.score += s.weight
		if s.confirmedAlive {
			if s.confirmedSeen == a.keyStamp {
				s.confirmed++
			} else {
				s.confirmedAlive = false
			}
		}
		tiers := s.tiers
		nTiers := len(tiers)
		if nTiers == 1 {
			tc := &tiers[0]
			if tc.alive {
				if tc.seen == a.keyStamp {
					tc.count++
				} else {
					tc.alive = false
				}
			}
		} else if nTiers == 2 {
			tc0 := &tiers[0]
			if tc0.alive {
				if tc0.seen == a.keyStamp {
					tc0.count++
				} else {
					tc0.alive = false
				}
			}
			tc1 := &tiers[1]
			if tc1.alive {
				if tc1.seen == a.keyStamp {
					tc1.count++
				} else {
					tc1.alive = false
				}
			}
		} else {
			for t := 0; t < nTiers; t++ {
				tc := &tiers[t]
				if tc.alive {
					if tc.seen == a.keyStamp {
						tc.count++
					} else {
						tc.alive = false
					}
				}
			}
		}
		keep = append(keep, i)
	}
	a.active = keep
	return len(a.active) > 0
}

// result materializes the accumulated matches.
func (a *prefixAccumulator) result() map[string]PodMatch {
	out := make(map[string]PodMatch, len(a.slots))
	for i := range a.slots {
		s := &a.slots[i]
		var byTier map[string]int
		n := len(s.tiers)
		if n > 0 {
			byTier = make(map[string]int, n)
			for j := 0; j < n; j++ {
				tc := &s.tiers[j]
				byTier[tc.name] = tc.count
			}
		}
		out[s.pod] = PodMatch{
			WeightedScore:   s.score,
			MatchedBlocks:   s.matched,
			ConfirmedBlocks: s.confirmed,
			BlocksByTier:    byTier,
		}
	}
	return out
}

// newSlot appends a candidate, reusing a pooled slot's tier storage when one
// is available.
func (a *prefixAccumulator) newSlot(pod string) int32 {
	n := len(a.slots)
	if n < cap(a.slots) {
		a.slots = a.slots[:n+1]
		s := &a.slots[n]
		s.pod = pod
		s.matched = 0
		s.score = 0
		s.seen = 0
		s.weight = 0
		s.tiers = s.tiers[:0]
		s.confirmed = 0
		s.confirmedSeen = 0
		s.confirmedAlive = false
	} else {
		a.slots = append(a.slots, matchSlot{
			pod:   pod,
			tiers: make([]tierChain, 0, 4),
		})
	}
	return int32(n)
}


// weightOf resolves a tier's weight, caching by ordinal so the configured
// map is consulted once per tier per accumulation.
func (a *prefixAccumulator) weightOf(tier string, ordinal uint32) float64 {
	weights := a.weightByOrdinal
	if int(ordinal) < len(weights) {
		ow := weights[ordinal]
		if ow.valid {
			return ow.weight
		}
	}
	w := unknownTierWeight
	if tier == SpeculativeTier {
		w = speculativeTierWeight
	}
	if configured, ok := a.weights[tier]; ok {
		w = configured
	}
	if int(ordinal) >= len(a.weightByOrdinal) {
		newLen := int(ordinal) + 1
		if newLen < 16 {
			newLen = 16
		}
		if cap(a.weightByOrdinal) < newLen {
			newBuf := make([]ordinalWeight, newLen*2)
			copy(newBuf, a.weightByOrdinal)
			a.weightByOrdinal = newBuf
		} else {
			a.weightByOrdinal = a.weightByOrdinal[:newLen]
		}
	}
	a.weightByOrdinal[ordinal] = ordinalWeight{weight: w, valid: true}
	return w
}

// EVOLVE-BLOCK-END
