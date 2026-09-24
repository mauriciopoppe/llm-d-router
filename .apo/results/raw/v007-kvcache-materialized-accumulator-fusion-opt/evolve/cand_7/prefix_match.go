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

// EVOLVE-BLOCK-START
type podCacheEntry struct {
	name string
	ord  uint32
}

type posCacheEntry struct {
	name     string
	ord      uint32
	tierName string
	tierOrd  uint32
}

type tierCacheEntry struct {
	name string
	ord  uint32
}

func fastHash(s string) uint32 {
	n := len(s)
	if n < 2 {
		if n == 1 {
			return uint32(s[0])
		}
		return 0
	}
	return uint32(s[n-1])*31 + uint32(s[n-2])
}

func matchMaterialized(ctx context.Context, keys []kvblock.BlockHash,
	keyToPods map[kvblock.BlockHash][]kvblock.PodEntry,
	weights map[string]float64, filter sets.Set[string],
) (map[string]PodMatch, error) {
	acc := acquireAccumulator(weights, filter)
	defer releaseAccumulator(acc)

	if err := acc.foldMaterialized(ctx, keys, keyToPods); err != nil {
		return nil, err
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
	if len(buckets) == 0 {
		return 0, false
	}
	mask := uint32(len(buckets) - 1)
	i := (ordinal * 2654435761) & mask
	for {
		b := buckets[i]
		if b == 0 {
			return 0, false
		}
		if uint32(b>>32) == ordinal {
			return int32(uint32(b) - 1), true
		}
		i = (i + 1) & mask
	}
}

func (t *slotTable) insert(ordinal uint32, slot int32) {
	buckets := t.buckets
	mask := uint32(len(buckets) - 1)
	i := (ordinal * 2654435761) & mask
	for buckets[i] != 0 {
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
	hasFilter bool

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

	posCache             []uint64
	mru                  uint64
	weightCacheDirect    [64]float64
	weightCacheSet       uint64
	speculativeWeight    float64
	speculativeWeightSet bool

	podCacheFast         [1024]podCacheEntry
	posCacheFast         [256]posCacheEntry
	tierCacheFast        [4]tierCacheEntry
	mruNameFast          string
	mruOrdFast           uint32
	hasMruFast           bool
	lastTierNameFast     string
	lastTierOrdFast      uint32
	hasLastTierFast      bool
}

var accumulatorPool = sync.Pool{New: func() any {
	slots := make([]matchSlot, 256)
	for i := range slots {
		slots[i].tiers = make([]tierChain, 0, 4)
	}
	return &prefixAccumulator{
		table:       slotTable{buckets: make([]uint64, 512)},
		slots:       slots[:0],
		active:      make([]int32, 0, 256),
		weightCache: make([]tierWeight, 0, 8),
		podsMap:     make(map[string]uint32, 128),
		tiersMap:    make(map[string]uint32, 16),
		refsBuf:     make([]kvblock.EntryRef, 0, 512),
		posCache:    make([]uint64, 0, 512),
	}
}}

func acquireAccumulator(weights map[string]float64, filter sets.Set[string]) *prefixAccumulator {
	a := accumulatorPool.Get().(*prefixAccumulator)
	a.weights, a.filter = weights, filter
	a.hasFilter = filter.Len() > 0
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
	a.mru = ^uint64(0)
	a.weightCacheSet = 0
	a.speculativeWeightSet = false

	clear(a.podCacheFast[:])
	clear(a.posCacheFast[:])
	clear(a.tierCacheFast[:])
	a.mruNameFast = ""
	a.mruOrdFast = 0
	a.hasMruFast = false
	a.lastTierNameFast = ""
	a.lastTierOrdFast = 0
	a.hasLastTierFast = false
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
	if cap(a.posCache) < len(entries) {
		a.posCache = make([]uint64, len(entries))
	} else {
		a.posCache = a.posCache[:len(entries)]
	}
	for i := range a.posCache {
		a.posCache[i] = uint64(^uint32(0)) << 32
	}

	weightCacheDirect := &a.weightCacheDirect
	weightCacheSet := a.weightCacheSet

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
			if a.hasFilter && !a.filter.Has(ref.PodIdentifier) {
				continue
			}
			s = a.newSlot(ref.PodIdentifier)
			a.table.insert(ref.PodOrdinal, s)
		}

		if i < len(a.posCache) {
			a.posCache[i] = (uint64(ref.PodOrdinal) << 32) | uint64(uint32(s))
		}

		slot := &a.slots[s]

		tier, tierOrdinal := ref.DeviceTier, ref.TierOrdinal
		if ref.Speculative || ref.DeviceTier == SpeculativeTier {
			tier, tierOrdinal = SpeculativeTier, speculativeTierOrdinal
		} else {
			slot.confirmedSeen = a.keyStamp
		}

		var w float64
		if tierOrdinal < 64 {
			if (weightCacheSet & (1 << tierOrdinal)) != 0 {
				w = weightCacheDirect[tierOrdinal]
			} else {
				w = a.weightOf(tier, tierOrdinal)
				weightCacheSet = a.weightCacheSet
			}
		} else if tierOrdinal == speculativeTierOrdinal && a.speculativeWeightSet {
			w = a.speculativeWeight
		} else {
			w = a.weightOf(tier, tierOrdinal)
		}

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
	a.weightCacheSet = weightCacheSet
	return a.endKey()
}

// key folds one key's entries into the chains and reports whether any chain
// is still alive. entries is borrowed for the duration of the call.
func (a *prefixAccumulator) key(entries []kvblock.EntryRef) bool {
	a.keyStamp++
	if a.first {
		return a.keyFirst(entries)
	}

	posCache := a.posCache
	slots := a.slots
	keyStamp := a.keyStamp
	mru := a.mru
	weightCacheSet := a.weightCacheSet
	weightCacheDirect := &a.weightCacheDirect

	var prev *kvblock.EntryRef
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
		if i < len(posCache) {
			val := posCache[i]
			if uint32(val>>32) == ref.PodOrdinal {
				s = int32(val)
				ok = true
			}
		}
		if !ok {
			if uint32(mru>>32) == ref.PodOrdinal {
				s = int32(mru)
				ok = true
			} else {
				s, ok = a.table.lookup(ref.PodOrdinal)
				if ok {
					mru = (uint64(ref.PodOrdinal) << 32) | uint64(uint32(s))
					if i < len(posCache) {
						posCache[i] = mru
					}
				}
			}
		}

		if !ok {
			continue // the first key fixes the candidate set
		}
		slot := &slots[s]
		if slot.seen < keyStamp-1 {
			continue // entries for dead slots should be pruned early
		}

		tier, tierOrdinal := ref.DeviceTier, ref.TierOrdinal
		if ref.Speculative || ref.DeviceTier == SpeculativeTier {
			tier, tierOrdinal = SpeculativeTier, speculativeTierOrdinal
		} else {
			slot.confirmedSeen = keyStamp
		}

		var w float64
		if tierOrdinal < 64 {
			if (weightCacheSet & (1 << tierOrdinal)) != 0 {
				w = weightCacheDirect[tierOrdinal]
			} else {
				w = a.weightOf(tier, tierOrdinal)
				weightCacheSet = a.weightCacheSet
			}
		} else if tierOrdinal == speculativeTierOrdinal && a.speculativeWeightSet {
			w = a.speculativeWeight
		} else {
			w = a.weightOf(tier, tierOrdinal)
		}

		if slot.seen != keyStamp {
			slot.seen = keyStamp
			slot.weight = w
		} else if w > slot.weight {
			slot.weight = w
		}

		// stampTier inline
		tiers := slot.tiers
		if len(tiers) > 0 {
			if tiers[0].ordinal == tierOrdinal {
				tiers[0].seen = keyStamp
			} else if len(tiers) > 1 {
				for t := 1; t < len(tiers); t++ {
					if tiers[t].ordinal == tierOrdinal {
						tiers[t].seen = keyStamp
						break
					}
				}
			}
		}
	}
	a.mru = mru
	a.weightCacheSet = weightCacheSet
	return a.endKey()
}

// stampTier marks tier as held at the current key and reports whether the
// slot tracks that tier.
func (a *prefixAccumulator) stampTier(slot *matchSlot, tierOrdinal uint32) bool {
	tiers := slot.tiers
	if len(tiers) > 0 {
		if tiers[0].ordinal == tierOrdinal {
			tiers[0].seen = a.keyStamp
			return true
		}
		for i := 1; i < len(tiers); i++ {
			if tiers[i].ordinal == tierOrdinal {
				tiers[i].seen = a.keyStamp
				return true
			}
		}
	}
	return false
}

// endKey closes the current key and reports whether any chain is still
// alive.
func (a *prefixAccumulator) endKey() bool {
	keyStamp := a.keyStamp
	slots := a.slots
	if a.first {
		a.first = false
		for i := range slots {
			s := &slots[i]
			s.matched, s.score = 1, s.weight
			if s.confirmedSeen == keyStamp {
				s.confirmed, s.confirmedAlive = 1, true
			}
			tiers := s.tiers
			if len(tiers) == 1 {
				tiers[0].count = 1
			} else {
				for t := range tiers {
					tiers[t].count = 1
				}
			}
			a.active = append(a.active, int32(i))
		}
		return len(a.active) > 0
	}

	keep := a.active[:0]
	for _, i := range a.active {
		s := &slots[i]
		if s.seen != keyStamp {
			continue // the chain ends at the first key the pod does not hold
		}
		s.matched++
		s.score += s.weight
		if s.confirmedAlive {
			if s.confirmedSeen == keyStamp {
				s.confirmed++
			} else {
				s.confirmedAlive = false
			}
		}
		tiers := s.tiers
		if len(tiers) == 1 {
			tc := &tiers[0]
			if tc.alive {
				if tc.seen == keyStamp {
					tc.count++
				} else {
					tc.alive = false
				}
			}
		} else {
			for t := range tiers {
				tc := &tiers[t]
				if tc.alive {
					if tc.seen == keyStamp {
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
		if n := len(s.tiers); n > 0 {
			if n == 1 {
				byTier = map[string]int{s.tiers[0].name: s.tiers[0].count}
			} else {
				byTier = make(map[string]int, n)
				for j := 0; j < n; j++ {
					tc := &s.tiers[j]
					byTier[tc.name] = tc.count
				}
			}
		}
		out[s.pod] = PodMatch{WeightedScore: s.score, MatchedBlocks: s.matched, ConfirmedBlocks: s.confirmed, BlocksByTier: byTier}
	}
	return out
}

func (a *prefixAccumulator) foldMaterialized(ctx context.Context, keys []kvblock.BlockHash, keyToPods map[kvblock.BlockHash][]kvblock.PodEntry) error {
	for pos, key := range keys {
		if pos&matchCancellationMask == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		entries := keyToPods[key]
		if len(entries) == 0 {
			break
		}
		a.keyStamp++

		if pos == 0 {
			a.table.reset(len(entries))
			var prevPodOrd uint32 = ^uint32(0)
			var prevTierOrd uint32 = ^uint32(0)
			var prevSpeculative bool
			var hasPrev bool

			for i := range entries {
				e := &entries[i]
				var podOrd uint32

				h := fastHash(e.PodIdentifier)
				idx := h & 1023
				for {
					if a.podCacheFast[idx].name == "" {
						podOrd = a.podOrdinal(e.PodIdentifier)
						a.podCacheFast[idx].name = e.PodIdentifier
						a.podCacheFast[idx].ord = podOrd
						break
					}
					if a.podCacheFast[idx].name == e.PodIdentifier {
						podOrd = a.podCacheFast[idx].ord
						break
					}
					idx = (idx + 1) & 1023
				}

				if i < 256 {
					a.posCacheFast[i].name = e.PodIdentifier
					a.posCacheFast[i].ord = podOrd
				}
				a.mruNameFast = e.PodIdentifier
				a.mruOrdFast = podOrd
				a.hasMruFast = true

				var tierOrd uint32
				if a.hasLastTierFast && a.lastTierNameFast == e.DeviceTier {
					tierOrd = a.lastTierOrdFast
				} else {
					if a.tierCacheFast[0].name == e.DeviceTier {
						tierOrd = a.tierCacheFast[0].ord
					} else if a.tierCacheFast[1].name == e.DeviceTier {
						tierOrd = a.tierCacheFast[1].ord
					} else if a.tierCacheFast[2].name == e.DeviceTier {
						tierOrd = a.tierCacheFast[2].ord
					} else if a.tierCacheFast[3].name == e.DeviceTier {
						tierOrd = a.tierCacheFast[3].ord
					} else {
						tierOrd = a.tierOrdinal(e.DeviceTier)
						if a.tierCacheFast[0].name == "" {
							a.tierCacheFast[0].name = e.DeviceTier
							a.tierCacheFast[0].ord = tierOrd
						} else if a.tierCacheFast[1].name == "" {
							a.tierCacheFast[1].name = e.DeviceTier
							a.tierCacheFast[1].ord = tierOrd
						} else if a.tierCacheFast[2].name == "" {
							a.tierCacheFast[2].name = e.DeviceTier
							a.tierCacheFast[2].ord = tierOrd
						} else if a.tierCacheFast[3].name == "" {
							a.tierCacheFast[3].name = e.DeviceTier
							a.tierCacheFast[3].ord = tierOrd
						}
					}
					a.lastTierNameFast = e.DeviceTier
					a.lastTierOrdFast = tierOrd
					a.hasLastTierFast = true
				}

				if i < 256 {
					a.posCacheFast[i].tierName = e.DeviceTier
					a.posCacheFast[i].tierOrd = tierOrd
				}

				if hasPrev && podOrd == prevPodOrd && tierOrd == prevTierOrd && e.Speculative == prevSpeculative {
					continue
				}
				prevPodOrd = podOrd
				prevTierOrd = tierOrd
				prevSpeculative = e.Speculative
				hasPrev = true

				s, ok := a.table.lookup(podOrd)
				if !ok {
					if a.hasFilter && !a.filter.Has(e.PodIdentifier) {
						continue
					}
					s = a.newSlot(e.PodIdentifier)
					a.table.insert(podOrd, s)
				}

				slot := &a.slots[s]

				tier, finalTierOrd := e.DeviceTier, tierOrd
				if e.Speculative || e.DeviceTier == SpeculativeTier {
					tier, finalTierOrd = SpeculativeTier, speculativeTierOrdinal
				} else {
					slot.confirmedSeen = a.keyStamp
				}

				var w float64
				if finalTierOrd < 64 {
					if (a.weightCacheSet & (1 << finalTierOrd)) != 0 {
						w = a.weightCacheDirect[finalTierOrd]
					} else {
						w = a.weightOf(tier, finalTierOrd)
					}
				} else if finalTierOrd == speculativeTierOrdinal && a.speculativeWeightSet {
					w = a.speculativeWeight
				} else {
					w = a.weightOf(tier, finalTierOrd)
				}

				if slot.seen != a.keyStamp {
					slot.seen = a.keyStamp
					slot.weight = w
				} else if w > slot.weight {
					slot.weight = w
				}

				if !a.stampTier(slot, finalTierOrd) {
					slot.tiers = append(slot.tiers, tierChain{ordinal: finalTierOrd, name: tier, seen: a.keyStamp, alive: true})
				}
			}

			if !a.endKey() {
				break
			}
		} else {
			var prevPodOrd uint32 = ^uint32(0)
			var prevTierOrd uint32 = ^uint32(0)
			var prevSpeculative bool
			var hasPrev bool

			for i := range entries {
				e := &entries[i]

				var podOrd uint32
				var tierOrd uint32
				var found = false
				var tierFound = false

				if i < 256 && a.posCacheFast[i].name == e.PodIdentifier {
					podOrd = a.posCacheFast[i].ord
					found = true
					a.mruNameFast = e.PodIdentifier
					a.mruOrdFast = podOrd
					a.hasMruFast = true
					if a.posCacheFast[i].tierName == e.DeviceTier {
						tierOrd = a.posCacheFast[i].tierOrd
						tierFound = true
					}
				} else if a.hasMruFast && a.mruNameFast == e.PodIdentifier {
					podOrd = a.mruOrdFast
					found = true
					if i < 256 {
						a.posCacheFast[i].name = e.PodIdentifier
						a.posCacheFast[i].ord = podOrd
						if a.posCacheFast[i].tierName == e.DeviceTier {
							tierOrd = a.posCacheFast[i].tierOrd
							tierFound = true
						}
					}
				} else {
					h := fastHash(e.PodIdentifier)
					idx := h & 1023
					for {
						if a.podCacheFast[idx].name == "" {
							break
						}
						if a.podCacheFast[idx].name == e.PodIdentifier {
							podOrd = a.podCacheFast[idx].ord
							found = true
							break
						}
						idx = (idx + 1) & 1023
					}
					if found {
						a.mruNameFast = e.PodIdentifier
						a.mruOrdFast = podOrd
						a.hasMruFast = true
						if i < 256 {
							a.posCacheFast[i].name = e.PodIdentifier
							a.posCacheFast[i].ord = podOrd
						}
					}
				}

				if !found {
					continue
				}

				s, foundSlot := a.table.lookup(podOrd)
				if !foundSlot {
					continue
				}

				slot := &a.slots[s]
				if slot.seen < a.keyStamp-1 {
					continue
				}

				if !tierFound {
					if a.hasLastTierFast && a.lastTierNameFast == e.DeviceTier {
						tierOrd = a.lastTierOrdFast
					} else {
						if a.tierCacheFast[0].name == e.DeviceTier {
							tierOrd = a.tierCacheFast[0].ord
						} else if a.tierCacheFast[1].name == e.DeviceTier {
							tierOrd = a.tierCacheFast[1].ord
						} else if a.tierCacheFast[2].name == e.DeviceTier {
							tierOrd = a.tierCacheFast[2].ord
						} else if a.tierCacheFast[3].name == e.DeviceTier {
							tierOrd = a.tierCacheFast[3].ord
						} else {
							tierOrd = a.tierOrdinal(e.DeviceTier)
							if a.tierCacheFast[0].name == "" {
								a.tierCacheFast[0].name = e.DeviceTier
								a.tierCacheFast[0].ord = tierOrd
							} else if a.tierCacheFast[1].name == "" {
								a.tierCacheFast[1].name = e.DeviceTier
								a.tierCacheFast[1].ord = tierOrd
							} else if a.tierCacheFast[2].name == "" {
								a.tierCacheFast[2].name = e.DeviceTier
								a.tierCacheFast[2].ord = tierOrd
							} else if a.tierCacheFast[3].name == "" {
								a.tierCacheFast[3].name = e.DeviceTier
								a.tierCacheFast[3].ord = tierOrd
							}
						}
						a.lastTierNameFast = e.DeviceTier
						a.lastTierOrdFast = tierOrd
						a.hasLastTierFast = true
					}
					if i < 256 {
						a.posCacheFast[i].tierName = e.DeviceTier
						a.posCacheFast[i].tierOrd = tierOrd
					}
				}

				if hasPrev && podOrd == prevPodOrd && tierOrd == prevTierOrd && e.Speculative == prevSpeculative {
					continue
				}
				prevPodOrd = podOrd
				prevTierOrd = tierOrd
				prevSpeculative = e.Speculative
				hasPrev = true

				tier, finalTierOrd := e.DeviceTier, tierOrd
				if e.Speculative || e.DeviceTier == SpeculativeTier {
					tier, finalTierOrd = SpeculativeTier, speculativeTierOrdinal
				} else {
					slot.confirmedSeen = a.keyStamp
				}

				var w float64
				if finalTierOrd < 64 {
					if (a.weightCacheSet & (1 << finalTierOrd)) != 0 {
						w = a.weightCacheDirect[finalTierOrd]
					} else {
						w = a.weightOf(tier, finalTierOrd)
					}
				} else if finalTierOrd == speculativeTierOrdinal && a.speculativeWeightSet {
					w = a.speculativeWeight
				} else {
					w = a.weightOf(tier, finalTierOrd)
				}

				if slot.seen != a.keyStamp {
					slot.seen = a.keyStamp
					slot.weight = w
				} else if w > slot.weight {
					slot.weight = w
				}

				// stampTier inline
				tiers := slot.tiers
				if len(tiers) > 0 {
					if tiers[0].ordinal == finalTierOrd {
						tiers[0].seen = a.keyStamp
					} else if len(tiers) > 1 {
						for t := 1; t < len(tiers); t++ {
							if tiers[t].ordinal == finalTierOrd {
								tiers[t].seen = a.keyStamp
								break
							}
						}
					}
				}
			}

			if !a.endKey() {
				break
			}
		}
	}
	return nil
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
	if ordinal < 64 {
		if (a.weightCacheSet & (1 << ordinal)) != 0 {
			return a.weightCacheDirect[ordinal]
		}
		w := unknownTierWeight
		if tier == SpeculativeTier {
			w = speculativeTierWeight
		} else if configured, ok := a.weights[tier]; ok {
			w = configured
		}
		a.weightCacheDirect[ordinal] = w
		a.weightCacheSet |= (1 << ordinal)
		return w
	}
	if ordinal == speculativeTierOrdinal {
		if a.speculativeWeightSet {
			return a.speculativeWeight
		}
		w := speculativeTierWeight
		if configured, ok := a.weights[tier]; ok {
			w = configured
		}
		a.speculativeWeight = w
		a.speculativeWeightSet = true
		return w
	}
	for i := range a.weightCache {
		if a.weightCache[i].ordinal == ordinal {
			return a.weightCache[i].weight
		}
	}
	w := unknownTierWeight
	if tier == SpeculativeTier {
		w = speculativeTierWeight
	}
	if configured, ok := a.weights[tier]; ok {
		w = configured
	}
	a.weightCache = append(a.weightCache, tierWeight{ordinal: ordinal, weight: w})
	return w
}

// EVOLVE-BLOCK-END

