package pool

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// FakeProvider simulates a scaling backend pool for local testing, without
// needing real Azure/vCenter credentials. It starts each group with a fixed
// number of backends and, on a timer, randomly adds or removes one to
// simulate scale-out/scale-in events — enough to exercise the control
// plane's reconciliation and streaming logic end-to-end.
type FakeProvider struct {
	mu     sync.RWMutex
	groups map[string][]Backend

	basePort int
	rng      *rand.Rand
}

// NewFakeProvider creates a fake provider with the given groups, each
// starting with initialCount backends listening on sequential ports
// starting at basePort. Backend addresses are 127.0.0.1:<port> so they can
// be pointed at real local echo/test servers if desired.
func NewFakeProvider(basePort int, groups map[string]int) *FakeProvider {
	p := &FakeProvider{
		groups:   make(map[string][]Backend),
		basePort: basePort,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	port := basePort
	for group, count := range groups {
		backends := make([]Backend, 0, count)
		for i := 0; i < count; i++ {
			backends = append(backends, Backend{
				Address: fmt.Sprintf("127.0.0.1:%d", port),
				Weight:  1,
			})
			port++
		}
		p.groups[group] = backends
	}

	return p
}

// Groups implements Provider.
func (p *FakeProvider) Groups(_ context.Context) ([]string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	groups := make([]string, 0, len(p.groups))
	for g := range p.groups {
		groups = append(groups, g)
	}
	return groups, nil
}

// Snapshot implements Provider.
func (p *FakeProvider) Snapshot(_ context.Context, group string) (Snapshot, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	backends, ok := p.groups[group]
	if !ok {
		return Snapshot{}, fmt.Errorf("pool: unknown group %q", group)
	}

	// Return a copy so callers can't mutate our internal state.
	out := make([]Backend, len(backends))
	copy(out, backends)
	return Snapshot{Group: group, Backends: out}, nil
}

// SimulateScaling starts a background goroutine that periodically adds or
// removes a backend from a randomly chosen group, simulating scale-out and
// scale-in events. It stops when ctx is cancelled.
func (p *FakeProvider) SimulateScaling(ctx context.Context, interval time.Duration, minPerGroup, maxPerGroup int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.mutateRandomGroup(minPerGroup, maxPerGroup)
		}
	}
}

func (p *FakeProvider) mutateRandomGroup(minPerGroup, maxPerGroup int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.groups) == 0 {
		return
	}

	// Pick a random group.
	i, target := 0, p.rng.Intn(len(p.groups))
	var group string
	for g := range p.groups {
		if i == target {
			group = g
			break
		}
		i++
	}

	backends := p.groups[group]
	scaleUp := p.rng.Intn(2) == 0

	switch {
	case scaleUp && len(backends) < maxPerGroup:
		nextPort := p.nextPortForGroup(group)
		p.groups[group] = append(backends, Backend{
			Address: fmt.Sprintf("127.0.0.1:%d", nextPort),
			Weight:  1,
		})
	case !scaleUp && len(backends) > minPerGroup:
		idx := p.rng.Intn(len(backends))
		p.groups[group] = append(backends[:idx], backends[idx+1:]...)
	}
}

// nextPortForGroup picks a port number not already in use across any group,
// starting from basePort. Simple linear scan — fine at fake-provider scale.
func (p *FakeProvider) nextPortForGroup(_ string) int {
	used := make(map[int]bool)
	for _, backends := range p.groups {
		for _, b := range backends {
			var port int
			if _, err := fmt.Sscanf(b.Address, "127.0.0.1:%d", &port); err == nil {
				used[port] = true
			}
		}
	}
	port := p.basePort
	for used[port] {
		port++
	}
	return port
}
