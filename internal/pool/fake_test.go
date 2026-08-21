package pool

import (
	"context"
	"testing"
	"time"
)

func TestFakeProvider_GroupsAndSnapshot(t *testing.T) {
	p := NewFakeProvider(9000, map[string]int{"web": 3, "api": 2})
	ctx := context.Background()

	groups, err := p.Groups(ctx)
	if err != nil {
		t.Fatalf("Groups() error: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d: %v", len(groups), groups)
	}

	snap, err := p.Snapshot(ctx, "web")
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	if len(snap.Backends) != 3 {
		t.Errorf("expected 3 backends in web, got %d", len(snap.Backends))
	}
}

func TestFakeProvider_UnknownGroup(t *testing.T) {
	p := NewFakeProvider(9000, map[string]int{"web": 1})
	if _, err := p.Snapshot(context.Background(), "does-not-exist"); err == nil {
		t.Error("expected an error for an unknown group, got nil")
	}
}

func TestFakeProvider_ScalingRespectsBounds(t *testing.T) {
	p := NewFakeProvider(9000, map[string]int{"web": 3})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Run many scaling ticks quickly and confirm the group never goes
	// outside [min, max] regardless of how many mutations occur.
	go p.SimulateScaling(ctx, time.Millisecond, 2, 5)
	<-ctx.Done()

	snap, err := p.Snapshot(context.Background(), "web")
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	if len(snap.Backends) < 2 || len(snap.Backends) > 5 {
		t.Errorf("expected backend count within [2,5], got %d", len(snap.Backends))
	}
}
