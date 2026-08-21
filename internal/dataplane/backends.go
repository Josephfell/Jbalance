package dataplane

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/Josephfell/Jbalance/proto"
)

// Algorithm identifies how BackendList.Next selects among healthy
// backends. Mirrors the values the control plane can send in
// BackendSet.algorithm; an unrecognised value falls back to round robin.
type Algorithm string

const (
	AlgorithmRoundRobin       Algorithm = "round_robin"
	AlgorithmLeastConnections Algorithm = "least_connections"
	AlgorithmRandom           Algorithm = "random"
)

// backendEntry is one upstream target along with its expanded weight slots
// for weighted round-robin selection and its current health status.
type backendEntry struct {
	address     string
	weight      int32
	healthy     bool
	activeConns atomic.Int64 // outstanding requests currently proxied to this backend
}

// BackendList holds the current set of backends for one group and
// provides thread-safe backend selection over the currently healthy
// subset, using whichever Algorithm the control plane has most recently
// specified. Updated wholesale whenever a new BackendSet arrives from the
// control plane stream; health status and active-connection counts are
// tracked independently and preserved across control-plane updates for
// addresses that continue to exist.
type BackendList struct {
	mu        sync.RWMutex
	entries   []*backendEntry
	version   int64
	algorithm Algorithm
	next      atomic.Uint64 // round-robin cursor into the expanded healthy-slot list
	slots     []int         // expanded index list into entries, healthy backends only, respecting weight
	rng       *rand.Rand
	rngMu     sync.Mutex
}

// NewBackendList creates an empty backend list defaulting to round robin
// until the control plane specifies otherwise.
func NewBackendList() *BackendList {
	return &BackendList{
		algorithm: AlgorithmRoundRobin,
		// Backend selection weighting only, not security-sensitive — a
		// predictable seed here has no meaningful attack surface.
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Update replaces the backend set if the incoming version is newer than the
// currently held one. Out-of-order/stale updates (lower or equal version)
// are ignored, since gRPC streams don't guarantee ordering is preserved
// across reconnects.
//
// New addresses start optimistically healthy (they'll be excluded quickly
// by the HealthChecker if a check fails). Addresses that already existed
// keep whatever health status they currently have, so an update that
// doesn't concern a given backend can't silently un-mark it unhealthy.
func (b *BackendList) Update(set *pb.BackendSet) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if set.Version <= b.version && b.version != 0 {
		return
	}

	prevHealth := make(map[string]bool, len(b.entries))
	prevEntries := make(map[string]*backendEntry, len(b.entries))
	for _, e := range b.entries {
		prevHealth[e.address] = e.healthy
		prevEntries[e.address] = e
	}

	entries := make([]*backendEntry, 0, len(set.Backends))
	for _, be := range set.Backends {
		weight := be.Weight
		if weight <= 0 {
			weight = 1
		}
		healthy, known := prevHealth[be.Address]
		if !known {
			healthy = true // optimistic default until the health checker verifies it
		}

		entry := &backendEntry{address: be.Address, weight: weight, healthy: healthy}
		// Preserve the in-flight connection counter across updates for
		// addresses that continue to exist — a backend that stays in the
		// pool across a reconcile tick shouldn't have its least-connections
		// accounting reset to zero.
		if prev, ok := prevEntries[be.Address]; ok {
			entry.activeConns.Store(prev.activeConns.Load())
		}
		entries = append(entries, entry)
	}

	b.entries = entries
	b.version = set.Version
	b.algorithm = normalizeAlgorithm(set.Algorithm)
	b.rebuildSlotsLocked()
}

// normalizeAlgorithm maps a wire-format algorithm string to a known
// Algorithm, defaulting to round robin for anything unset or unrecognised
// — a data plane running a newer/older version than the control plane
// should never end up unable to select a backend at all.
func normalizeAlgorithm(s string) Algorithm {
	switch Algorithm(s) {
	case AlgorithmLeastConnections:
		return AlgorithmLeastConnections
	case AlgorithmRandom:
		return AlgorithmRandom
	default:
		return AlgorithmRoundRobin
	}
}

// SetHealth marks the given address healthy or unhealthy. A no-op if the
// address is not currently part of the backend set (e.g. it was removed by
// a control-plane update that raced with an in-flight health check).
func (b *BackendList) SetHealth(address string, healthy bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	changed := false
	for _, e := range b.entries {
		if e.address == address && e.healthy != healthy {
			e.healthy = healthy
			changed = true
			break
		}
	}
	if changed {
		b.rebuildSlotsLocked()
	}
}

// rebuildSlotsLocked recomputes the weighted selection slot list from the
// currently healthy entries. Callers must hold b.mu.
//
// Note: for least_connections, nextLeastConnections de-duplicates these
// slots back down to one-per-backend and uses weight as a load divisor
// instead — the expanded slot list here is still built the same way for
// every algorithm so round_robin/random can share it without a separate
// code path.
func (b *BackendList) rebuildSlotsLocked() {
	slots := make([]int, 0, len(b.entries))
	for i, e := range b.entries {
		if !e.healthy {
			continue
		}
		for w := int32(0); w < e.weight; w++ {
			slots = append(slots, i)
		}
	}
	b.slots = slots
}

// Next returns the next healthy backend address to use, per the group's
// currently selected algorithm (round robin, least connections, or
// random — all weighted). Returns false if there are currently no healthy
// backends available.
//
// For least_connections, the caller must call Release with the same
// address once the request finishes, so the connection count accurately
// reflects in-flight requests. For the other algorithms, calling Release
// is harmless but unnecessary.
func (b *BackendList) Next() (string, bool) {
	b.mu.RLock()
	algorithm := b.algorithm
	slots := b.slots
	entries := b.entries
	b.mu.RUnlock()

	if len(slots) == 0 {
		return "", false
	}

	switch algorithm {
	case AlgorithmLeastConnections:
		return b.nextLeastConnections(entries, slots)
	case AlgorithmRandom:
		return b.nextRandom(entries, slots)
	default:
		return b.nextRoundRobin(entries, slots)
	}
}

func (b *BackendList) nextRoundRobin(entries []*backendEntry, slots []int) (string, bool) {
	idx := b.next.Add(1) - 1
	slot := slots[idx%uint64(len(slots))]
	entry := entries[slot]
	entry.activeConns.Add(1)
	return entry.address, true
}

func (b *BackendList) nextRandom(entries []*backendEntry, slots []int) (string, bool) {
	b.rngMu.Lock()
	slot := slots[b.rng.Intn(len(slots))]
	b.rngMu.Unlock()
	entry := entries[slot]
	entry.activeConns.Add(1)
	return entry.address, true
}

// nextLeastConnections picks the healthy backend with the fewest
// outstanding requests, weighted: a backend's effective load is its
// active connection count divided by its weight, so a backend with 2x the
// weight is expected to carry roughly 2x the connections before being
// considered equally loaded. Ties broken by iteration order (stable
// enough — this runs per-request, and "which of several tied backends"
// doesn't matter for correctness).
func (b *BackendList) nextLeastConnections(entries []*backendEntry, slots []int) (string, bool) {
	// Consider each distinct healthy backend once, not once per weight
	// slot — least-connections doesn't need slot expansion the way
	// round-robin/random do, since weight is applied directly as a load
	// divisor instead.
	seen := make(map[int]bool, len(slots))
	var best *backendEntry
	var bestLoad float64

	for _, slot := range slots {
		if seen[slot] {
			continue
		}
		seen[slot] = true

		e := entries[slot]
		weight := e.weight
		if weight <= 0 {
			weight = 1
		}
		load := float64(e.activeConns.Load()) / float64(weight)

		if best == nil || load < bestLoad {
			best = e
			bestLoad = load
		}
	}

	if best == nil {
		return "", false
	}
	best.activeConns.Add(1)
	return best.address, true
}

// Release decrements the active-connection count for address, signalling
// that a request proxied to it has completed. Only meaningful for
// least_connections selection; safe (and cheap) to call regardless of the
// currently active algorithm. A no-op if address is not currently known.
func (b *BackendList) Release(address string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, e := range b.entries {
		if e.address == address {
			// Guard against going negative if Release is ever called more
			// times than Next for the same address (shouldn't happen in
			// normal operation, but a stray double-release must not leave
			// a permanently-negative count that would make this backend
			// look artificially "less loaded" forever after).
			for {
				cur := e.activeConns.Load()
				if cur <= 0 {
					return
				}
				if e.activeConns.CompareAndSwap(cur, cur-1) {
					return
				}
			}
		}
	}
}

// Algorithm returns the currently selected load-balancing algorithm.
func (b *BackendList) Algorithm() Algorithm {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.algorithm
}

// Len returns the current number of distinct backends (healthy or not, not
// counting weight expansion) — used for health/observability reporting.
func (b *BackendList) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.entries)
}

// HealthyLen returns the current number of healthy backends.
func (b *BackendList) HealthyLen() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, e := range b.entries {
		if e.healthy {
			n++
		}
	}
	return n
}

// Version returns the currently held backend set version.
func (b *BackendList) Version() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.version
}

// Addresses returns every currently known backend address (healthy or
// not), for use by a HealthChecker deciding what to probe.
func (b *BackendList) Addresses() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	addrs := make([]string, len(b.entries))
	for i, e := range b.entries {
		addrs[i] = e.address
	}
	return addrs
}

// AddressHealth is a snapshot of one backend's address and current health
// status, returned by HealthSnapshot.
type AddressHealth struct {
	Address string
	Healthy bool
}

// HealthSnapshot returns the current health status of every known
// backend, for reporting back to the control plane.
func (b *BackendList) HealthSnapshot() []AddressHealth {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]AddressHealth, len(b.entries))
	for i, e := range b.entries {
		out[i] = AddressHealth{Address: e.address, Healthy: e.healthy}
	}
	return out
}
