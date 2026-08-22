package dataplane

import (
	"context"
	"testing"
	"time"
)

func TestGroupManager_EnsureReturnsSameBackendListForSameGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A control plane address that will simply fail to connect is fine
	// here — Ensure's contract under test is "returns a stable BackendList
	// per group name", not "successfully subscribes"; Subscriber.Run
	// handles connection failure on its own via backoff/retry.
	gm := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, HealthCheckConfig{
		Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2,
	}, time.Hour)

	a1 := gm.Ensure("web-tier")
	a2 := gm.Ensure("web-tier")
	if a1 != a2 {
		t.Error("expected repeated Ensure calls for the same group to return the same BackendList")
	}
}

func TestGroupManager_EnsureReturnsDistinctBackendListsForDifferentGroups(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gm := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, HealthCheckConfig{
		Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2,
	}, time.Hour)

	web := gm.Ensure("web-tier")
	api := gm.Ensure("api-tier")
	if web == api {
		t.Error("expected different groups to get distinct BackendLists")
	}
}

func TestGroupManager_GroupsListsEveryEnsuredGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gm := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, HealthCheckConfig{
		Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2,
	}, time.Hour)

	gm.Ensure("web-tier")
	gm.Ensure("api-tier")
	gm.Ensure("web-tier") // repeat — must not appear twice

	got := gm.Groups()
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct groups, got %d: %v", len(got), got)
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	if !seen["web-tier"] || !seen["api-tier"] {
		t.Errorf("expected both web-tier and api-tier to be tracked, got %v", got)
	}
}

func TestGroupManager_GroupsEmptyBeforeAnyEnsure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gm := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, HealthCheckConfig{Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2}, time.Hour)
	if got := gm.Groups(); len(got) != 0 {
		t.Errorf("expected no tracked groups before any Ensure call, got %v", got)
	}
}
