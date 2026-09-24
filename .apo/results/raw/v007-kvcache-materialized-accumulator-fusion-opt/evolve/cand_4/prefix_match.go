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

type podCacheEntry struct {
	name string
	ord  uint32
}

type posCacheEntry struct {
	name     string
	ord      uint32
	tierName string
	tierOrd  uint32
	slot     int32
}

type tierCacheEntry struct {
	name string
	ord  uint32
}

func matchMaterialized(ctx context.Context, keys []kvblock.BlockHash,
	keyToPods map[kvblock.BlockHash][]kvblock.PodEntry,
	weights map[string]float64, filter sets.Set[string],
) (map[string]PodMatch, error) {
	acc := acquireAccumulator(weights, filter)
	defer releaseAccumulator(acc)

	for pos, key := range keys {
		if pos&matchCancellationMask == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		entries := keyToPods[key]
		if len(entries) == 0 {
			break
		}
		if !acc.fold(entries) {
			break
		}
	}
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

	podCache  [1024]podCacheEntry
	posCache  [256]posCacheEntry
	tierCache [4]tierCacheEntry

	mruName      string
	mruOrd       uint32
	mruSlot      int32
	hasMru       bool
	lastTierName string
	lastTierOrd  uint32
	hasLastTier  bool

	weightCacheDirect    [64]float64
	weightCacheSet       uint64
	speculativeWeight    float64
	speculativeWeightSet bool
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
	clear(a.podCache[:])
	clear(a.posCache[:])
	clear(a.tierCache[:])
	a.mruName = ""
	a.mruOrd = 0
	a.mruSlot = 0
	a.hasMru = false
	a.lastTierName = ""
	a.lastTierOrd = 0
	a.hasLastTier = false
	a.weightCacheSet = 0
	a.speculativeWeightSet = false
	return a
}

func releaseAccumulator(a *prefixAccumulator) {
	a.weights, a.filter = nil, nil
	for i := 0; i < len(a.slots); i++ {
		a.slots[i].pod = ""
	}
	accumulatorPool.Put(a)
}

func (a *prefixAccumulator) foldFirst(entries []kvblock.PodEntry) bool {
	a.table.reset(len(entries))

	weightCacheDirect := &a.weightCacheDirect
	weightCacheSet := a.weightCacheSet

	var prevPodOrd uint32 = ^uint32(0)
	var prevTierOrd uint32 = ^uint32(0)
	var prevSpeculative bool = false
	var hasPrev bool = false

	for i := range entries {
		e := &entries[i]

		var podOrd uint32
		h := fastHash(e.PodIdentifier)
		idx := h & 1023
		for {
			if a.podCache[idx].name == "" {
				podOrd = a.podOrdinal(e.PodIdentifier)
				a.podCache[idx].name = e.PodIdentifier
				a.podCache[idx].ord = podOrd
				break
			}
			if a.podCache[idx].name == e.PodIdentifier {
				podOrd = a.podCache[idx].ord
				break
			}
			idx = (idx + 1) & 1023
		}

		var tierOrd uint32
		if a.hasLastTier && a.lastTierName == e.DeviceTier {
			tierOrd = a.lastTierOrd
		} else {
			if a.tierCache[0].name == e.DeviceTier {
				tierOrd = a.tierCache[0].ord
			} else if a.tierCache[1].name == e.DeviceTier {
				tierOrd = a.tierCache[1].ord
			} else if a.tierCache[2].name == e.DeviceTier {
				tierOrd = a.tierCache[2].ord
			} else if a.tierCache[3].name == e.DeviceTier {
				tierOrd = a.tierCache[3].ord
			} else {
				tierOrd = a.tierOrdinal(e.DeviceTier)
				if a.tierCache[0].name == "" {
					a.tierCache[0].name = e.DeviceTier
					a.tierCache[0].ord = tierOrd
				} else if a.tierCache[1].name == "" {
					a.tierCache[1].name = e.DeviceTier
					a.tierCache[1].ord = tierOrd
				} else if a.tierCache[2].name == "" {
					a.tierCache[2].name = e.DeviceTier
					a.tierCache[2].ord = tierOrd
				} else if a.tierCache[3].name == "" {
					a.tierCache[3].name = e.DeviceTier
					a.tierCache[3].ord = tierOrd
				}
			}
			a.lastTierName = e.DeviceTier
			a.lastTierOrd = tierOrd
			a.hasLastTier = true
		}

		tier, resolvedTierOrd := e.DeviceTier, tierOrd
		if e.Speculative || e.DeviceTier == SpeculativeTier {
			tier, resolvedTierOrd = SpeculativeTier, speculativeTierOrdinal
		}

		if hasPrev && podOrd == prevPodOrd && resolvedTierOrd == prevTierOrd && e.Speculative == prevSpeculative {
			continue
		}
		prevPodOrd = podOrd
		prevTierOrd = resolvedTierOrd
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

		if i < 256 {
			a.posCache[i].name = e.PodIdentifier
			a.posCache[i].ord = podOrd
			a.posCache[i].tierName = e.DeviceTier
			a.posCache[i].tierOrd = tierOrd
			a.posCache[i].slot = s
		}
		a.mruName = e.PodIdentifier
		a.mruOrd = podOrd
		a.mruSlot = s
		a.hasMru = true

		slot := &a.slots[s]

		if resolvedTierOrd != speculativeTierOrdinal {
			slot.confirmedSeen = a.keyStamp
		}

		var w float64
		if resolvedTierOrd < 64 {
			if (weightCacheSet & (1 << resolvedTierOrd)) != 0 {
				w = weightCacheDirect[resolvedTierOrd]
			} else {
				w = a.weightOf(tier, resolvedTierOrd)
				weightCacheSet = a.weightCacheSet
			}
		} else if resolvedTierOrd == speculativeTierOrdinal && a.speculativeWeightSet {
			w = a.speculativeWeight
		} else {
			w = a.weightOf(tier, resolvedTierOrd)
		}

		if slot.seen != a.keyStamp {
			slot.seen = a.keyStamp
			slot.weight = w
		} else if w > slot.weight {
			slot.weight = w
		}

		if !a.stampTier(slot, resolvedTierOrd) {
			slot.tiers = append(slot.tiers, tierChain{ordinal: resolvedTierOrd, name: tier, seen: a.keyStamp, alive: true})
		}
	}
	a.weightCacheSet = weightCacheSet
	return a.endKey()
}

func (a *prefixAccumulator) fold(entries []kvblock.PodEntry) bool {
	a.keyStamp++
	if a.first {
		return a.foldFirst(entries)
	}

	slots := a.slots
	keyStamp := a.keyStamp
	weightCacheSet := a.weightCacheSet
	weightCacheDirect := &a.weightCacheDirect

	var prevPodOrd uint32 = ^uint32(0)
	var prevTierOrd uint32 = ^uint32(0)
	var prevSpeculative bool = false
	var hasPrev bool = false

	for i := range entries {
		e := &entries[i]

		var podOrd uint32
		var tierOrd uint32
		var s int32
		var ok bool

		if i < 256 && a.posCache[i].name == e.PodIdentifier && a.posCache[i].tierName == e.DeviceTier {
			podOrd = a.posCache[i].ord
			tierOrd = a.posCache[i].tierOrd
			s = a.posCache[i].slot
			ok = true
			a.mruName = e.PodIdentifier
			a.mruOrd = podOrd
			a.mruSlot = s
			a.hasMru = true
		} else {
			found := false
			tierFound := false

			if i < 256 && a.posCache[i].name == e.PodIdentifier {
				podOrd = a.posCache[i].ord
				s = a.posCache[i].slot
				found = true
				ok = true
				a.mruName = e.PodIdentifier
				a.mruOrd = podOrd
				a.mruSlot = s
				a.hasMru = true
				if a.posCache[i].tierName == e.DeviceTier {
					tierOrd = a.posCache[i].tierOrd
					tierFound = true
				}
			} else if a.hasMru && a.mruName == e.PodIdentifier {
				podOrd = a.mruOrd
				s = a.mruSlot
				found = true
				ok = true
				if i < 256 {
					a.posCache[i].name = e.PodIdentifier
					a.posCache[i].ord = podOrd
					a.posCache[i].slot = s
					if a.posCache[i].tierName == e.DeviceTier {
						tierOrd = a.posCache[i].tierOrd
						tierFound = true
					}
				}
			} else {
				h := fastHash(e.PodIdentifier)
				idx := h & 1023
				for {
					if a.podCache[idx].name == "" {
						break
					}
					if a.podCache[idx].name == e.PodIdentifier {
						podOrd = a.podCache[idx].ord
						found = true
						break
					}
					idx = (idx + 1) & 1023
				}
				if found {
					s, ok = a.table.lookup(podOrd)
					if ok {
						a.mruName = e.PodIdentifier
						a.mruOrd = podOrd
						a.mruSlot = s
						a.hasMru = true
						if i < 256 {
							a.posCache[i].name = e.PodIdentifier
							a.posCache[i].ord = podOrd
							a.posCache[i].slot = s
						}
					} else {
						found = false
					}
				}
			}

			if !found {
				continue
			}

			if !tierFound {
				if a.hasLastTier && a.lastTierName == e.DeviceTier {
					tierOrd = a.lastTierOrd
				} else {
					if a.tierCache[0].name == e.DeviceTier {
						tierOrd = a.tierCache[0].ord
					} else if a.tierCache[1].name == e.DeviceTier {
						tierOrd = a.tierCache[1].ord
					} else if a.tierCache[2].name == e.DeviceTier {
						tierOrd = a.tierCache[2].ord
					} else if a.tierCache[3].name == e.DeviceTier {
						tierOrd = a.tierCache[3].ord
					} else {
						tierOrd = a.tierOrdinal(e.DeviceTier)
						if a.tierCache[0].name == "" {
							a.tierCache[0].name = e.DeviceTier
							a.tierCache[0].ord = tierOrd
						} else if a.tierCache[1].name == "" {
							a.tierCache[1].name = e.DeviceTier
							a.tierCache[1].ord = tierOrd
						} else if a.tierCache[2].name == "" {
							a.tierCache[2].name = e.DeviceTier
							a.tierCache[2].ord = tierOrd
						} else if a.tierCache[3].name == "" {
							a.tierCache[3].name = e.DeviceTier
							a.tierCache[3].ord = tierOrd
						}
					}
					a.lastTierName = e.DeviceTier
					a.lastTierOrd = tierOrd
					a.hasLastTier = true
				}
				if i < 256 {
					a.posCache[i].tierName = e.DeviceTier
					a.posCache[i].tierOrd = tierOrd
				}
			}
		}

		if !ok {
			continue // first key fixes the candidate set
		}

		tier, resolvedTierOrd := e.DeviceTier, tierOrd
		if e.Speculative || e.DeviceTier == SpeculativeTier {
			tier, resolvedTierOrd = SpeculativeTier, speculativeTierOrdinal
		}

		if hasPrev && podOrd == prevPodOrd && resolvedTierOrd == prevTierOrd && e.Speculative == prevSpeculative {
			continue
		}
		prevPodOrd = podOrd
		prevTierOrd = resolvedTierOrd
		prevSpeculative = e.Speculative
		hasPrev = true

		slot := &slots[s]
		if slot.seen < keyStamp-1 {
			continue // dead slots pruned early
		}

		if resolvedTierOrd != speculativeTierOrdinal {
			slot.confirmedSeen = keyStamp
		}

		var w float64
		if resolvedTierOrd < 64 {
			if (weightCacheSet & (1 << resolvedTierOrd)) != 0 {
				w = weightCacheDirect[resolvedTierOrd]
			} else {
				w = a.weightOf(tier, resolvedTierOrd)
				weightCacheSet = a.weightCacheSet
			}
		} else if resolvedTierOrd == speculativeTierOrdinal && a.speculativeWeightSet {
			w = a.speculativeWeight
		} else {
			w = a.weightOf(tier, resolvedTierOrd)
		}

		if slot.seen != keyStamp {
			slot.seen = keyStamp
			slot.weight = w
		} else if w > slot.weight {
			slot.weight = w
		}

		tiers := slot.tiers
		if len(tiers) > 0 {
			if tiers[0].ordinal == resolvedTierOrd {
				tiers[0].seen = keyStamp
			} else if len(tiers) > 1 {
				for t := 1; t < len(tiers); t++ {
					if tiers[t].ordinal == resolvedTierOrd {
						tiers[t].seen = keyStamp
						break
					}
				}
			}
		}
	}
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
			activeTiers := 0
			var singleTier *tierChain
			for j := 0; j < n; j++ {
				tc := &s.tiers[j]
				if tc.name != "" && tc.count > 0 {
					activeTiers++
					singleTier = tc
				}
			}
			if activeTiers > 0 {
				byTier = make(map[string]int, activeTiers)
				if activeTiers == 1 {
					byTier[singleTier.name] = singleTier.count
				} else {
					for j := 0; j < n; j++ {
						tc := &s.tiers[j]
						if tc.name != "" && tc.count > 0 {
							byTier[tc.name] = tc.count
						}
					}
				}
			}
		}
		out[s.pod] = PodMatch{WeightedScore: s.score, MatchedBlocks: s.matched, ConfirmedBlocks: s.confirmed, BlocksByTier: byTier}
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

