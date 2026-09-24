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

// EVOLVE-BLOCK-START
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
		if !acc.keyMaterialized(entries) {
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

// speculativeTierOrdinal keys the speculative per-tier chain. Feeders assign
// tier ordinals from zero, so the top of the range never collides.
const speculativeTierOrdinal = math.MaxUint32

// slotRef maps one pod ordinal to a request-local slot.
type slotRef struct {
	ordinal uint32
	slot    uint32 // slot index plus one; zero marks an empty bucket
}

// slotTable is an open-addressed map from pod ordinal to request-local slot.
// It is sized by the first key's entry count, so request state scales with
// the live candidates rather than with every ordinal an index ever assigned.
type slotTable struct {
	buckets []slotRef
}

func (t *slotTable) reset(numEntries int) {
	size := 256
	for size < numEntries*2 {
		size <<= 1
	}
	if cap(t.buckets) < size {
		t.buckets = make([]slotRef, size)
		return
	}
	t.buckets = t.buckets[:size]
	clear(t.buckets)
}

func (t *slotTable) find(ordinal uint32) (int32, int) {
	mask := uint32(len(t.buckets) - 1)
	i := ordinal * 2654435761 & mask
	for {
		b := t.buckets[i]
		if b.slot == 0 {
			return -1, int(i)
		}
		if b.ordinal == ordinal {
			return int32(b.slot - 1), -1
		}
		i = (i + 1) & mask
	}
}

func (t *slotTable) insertAt(idx int, ordinal uint32, slot int32) {
	t.buckets[idx] = slotRef{ordinal: ordinal, slot: uint32(slot) + 1}
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
	weight float64
	set    bool
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
	tiersBuf       [4]tierChain
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

	lastPodName  string
	lastPodID    uint32
	lastTierName string
	lastTierID   uint32
}

var accumulatorPool = sync.Pool{New: func() any { return &prefixAccumulator{} }}

func acquireAccumulator(weights map[string]float64, filter sets.Set[string]) *prefixAccumulator {
	a, _ := accumulatorPool.Get().(*prefixAccumulator)
	a.weights, a.filter = weights, filter
	a.slots = a.slots[:0]
	a.active = a.active[:0]
	a.keyStamp = 0
	a.first = true
	if cap(a.weightCache) > 0 {
		cache := a.weightCache[:cap(a.weightCache)]
		for i := range cache {
			cache[i].set = false
		}
		a.weightCache = a.weightCache[:0]
	} else {
		a.weightCache = make([]tierWeight, 0, 16)
	}
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
	a.refsBuf = a.refsBuf[:0]
	a.lastPodName = ""
	a.lastTierName = ""
	return a
}

func releaseAccumulator(a *prefixAccumulator) {
	a.weights, a.filter = nil, nil
	accumulatorPool.Put(a)
}

// key folds one key's entries into the chains and reports whether any chain
// is still alive. entries is borrowed for the duration of the call.
func (a *prefixAccumulator) key(entries []kvblock.EntryRef) bool {
	a.keyStamp++
	if a.first {
		a.table.reset(len(entries))
	}

	var prev *kvblock.EntryRef
	var lastPodOrd uint32 = math.MaxUint32
	var lastSlot int32 = -1

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
		if ref.PodOrdinal == lastPodOrd {
			s = lastSlot
		} else {
			slotIdx, bucketIdx := a.table.find(ref.PodOrdinal)
			if slotIdx < 0 {
				if !a.first || (a.filter.Len() > 0 && !a.filter.Has(ref.PodIdentifier)) {
					continue // the first key fixes the candidate set
				}
				s = a.newSlot(ref.PodIdentifier)
				a.table.insertAt(bucketIdx, ref.PodOrdinal, s)
			} else {
				s = slotIdx
			}
			lastPodOrd = ref.PodOrdinal
			lastSlot = s
		}
		slot := &a.slots[s]

		tier, tierOrdinal := ref.DeviceTier, ref.TierOrdinal
		if ref.Speculative || ref.DeviceTier == SpeculativeTier {
			tier, tierOrdinal = SpeculativeTier, speculativeTierOrdinal
		} else {
			slot.confirmedSeen = a.keyStamp
		}

		w := a.weightOf(tier, tierOrdinal)
		switch {
		case slot.seen != a.keyStamp:
			slot.seen = a.keyStamp
			slot.weight = w
		case w > slot.weight:
			slot.weight = w
		}

		if !a.stampTier(slot, tierOrdinal) && a.first {
			slot.tiers = append(slot.tiers, tierChain{ordinal: tierOrdinal, name: tier, seen: a.keyStamp, alive: true})
		}
	}
	return a.endKey()
}

// keyMaterialized folds one key's materialized entries into the chains.
func (a *prefixAccumulator) keyMaterialized(entries []kvblock.PodEntry) bool {
	a.keyStamp++
	if a.first {
		a.table.reset(len(entries))
	}

	var prevPodOrdinal uint32 = math.MaxUint32
	var prevTierOrdinal uint32 = math.MaxUint32
	var prevSpeculative bool = false
	var hasPrev bool = false

	var lastPodOrd uint32 = math.MaxUint32
	var lastSlot int32 = -1

	for i := range entries {
		ref := &entries[i]
		var podOrd uint32
		if ref.PodIdentifier == a.lastPodName {
			podOrd = a.lastPodID
		} else {
			podOrd = a.podOrdinal(ref.PodIdentifier)
		}

		var tierOrd uint32
		if ref.DeviceTier == a.lastTierName {
			tierOrd = a.lastTierID
		} else {
			tierOrd = a.tierOrdinal(ref.DeviceTier)
		}

		speculative := ref.Speculative

		if hasPrev && podOrd == prevPodOrdinal && tierOrd == prevTierOrdinal && speculative == prevSpeculative {
			continue
		}
		prevPodOrdinal = podOrd
		prevTierOrdinal = tierOrd
		prevSpeculative = speculative
		hasPrev = true

		var s int32
		if podOrd == lastPodOrd {
			s = lastSlot
		} else {
			slotIdx, bucketIdx := a.table.find(podOrd)
			if slotIdx < 0 {
				if !a.first || (a.filter.Len() > 0 && !a.filter.Has(ref.PodIdentifier)) {
					continue // the first key fixes the candidate set
				}
				s = a.newSlot(ref.PodIdentifier)
				a.table.insertAt(bucketIdx, podOrd, s)
			} else {
				s = slotIdx
			}
			lastPodOrd = podOrd
			lastSlot = s
		}
		slot := &a.slots[s]

		tier, tierOrdinal := ref.DeviceTier, tierOrd
		if speculative || ref.DeviceTier == SpeculativeTier {
			tier, tierOrdinal = SpeculativeTier, speculativeTierOrdinal
		} else {
			slot.confirmedSeen = a.keyStamp
		}

		w := a.weightOf(tier, tierOrdinal)
		switch {
		case slot.seen != a.keyStamp:
			slot.seen = a.keyStamp
			slot.weight = w
		case w > slot.weight:
			slot.weight = w
		}

		if !a.stampTier(slot, tierOrdinal) && a.first {
			slot.tiers = append(slot.tiers, tierChain{ordinal: tierOrdinal, name: tier, seen: a.keyStamp, alive: true})
		}
	}
	return a.endKey()
}

// stampTier marks tier as held at the current key and reports whether the
// slot tracks that tier.
func (a *prefixAccumulator) stampTier(slot *matchSlot, tierOrdinal uint32) bool {
	tiers := slot.tiers
	for i := range tiers {
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
		for i := range a.slots {
			s := &a.slots[i]
			s.matched, s.score = 1, s.weight
			if s.confirmedSeen == a.keyStamp {
				s.confirmed, s.confirmedAlive = 1, true
			}
			for t := range s.tiers {
				s.tiers[t].count = 1
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
		for t := range tiers {
			tc := &tiers[t]
			if tc.alive {
				if tc.seen == a.keyStamp {
					tc.count++
				} else {
					tc.alive = false
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
		byTier := make(map[string]int, len(s.tiers))
		for _, tc := range s.tiers {
			byTier[tc.name] = tc.count
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
		tiers := s.tiers[:0]
		*s = matchSlot{pod: pod}
		if cap(tiers) > 0 {
			s.tiers = tiers
		} else {
			s.tiers = s.tiersBuf[:0]
		}
	} else {
		a.slots = append(a.slots, matchSlot{pod: pod})
		s := &a.slots[n]
		s.tiers = s.tiersBuf[:0]
	}
	return int32(n)
}

func (a *prefixAccumulator) podOrdinal(name string) uint32 {
	if name == a.lastPodName {
		return a.lastPodID
	}
	if id, ok := a.podsMap[name]; ok {
		a.lastPodName = name
		a.lastPodID = id
		return id
	}
	id := uint32(len(a.podsMap))
	a.podsMap[name] = id
	a.lastPodName = name
	a.lastPodID = id
	return id
}

func (a *prefixAccumulator) tierOrdinal(name string) uint32 {
	if name == a.lastTierName {
		return a.lastTierID
	}
	if id, ok := a.tiersMap[name]; ok {
		a.lastTierName = name
		a.lastTierID = id
		return id
	}
	id := uint32(len(a.tiersMap))
	a.tiersMap[name] = id
	a.lastTierName = name
	a.lastTierID = id
	return id
}

// weightOf resolves a tier's weight, caching by ordinal so the configured
// map is consulted once per tier per accumulation.
func (a *prefixAccumulator) weightOf(tier string, ordinal uint32) float64 {
	if ordinal == speculativeTierOrdinal {
		return speculativeTierWeight
	}
	if int(ordinal) < len(a.weightCache) {
		if tw := a.weightCache[ordinal]; tw.set {
			return tw.weight
		}
	} else {
		// Grow weightCache
		if int(ordinal) < cap(a.weightCache) {
			a.weightCache = a.weightCache[:ordinal+1]
		} else {
			newCap := (ordinal + 1) * 2
			temp := make([]tierWeight, newCap)
			copy(temp, a.weightCache)
			a.weightCache = temp[:ordinal+1]
		}
	}
	w := unknownTierWeight
	if configured, ok := a.weights[tier]; ok {
		w = configured
	}
	a.weightCache[ordinal] = tierWeight{weight: w, set: true}
	return w
}
// EVOLVE-BLOCK-END

