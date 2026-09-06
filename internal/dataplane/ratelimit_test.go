package dataplane

import (
	"testing"

	pb "github.com/Josephfell/Jbalance/proto"
)

func TestGroupRateLimiter_AllowsWhenDisabled(t *testing.T) {
	g := newGroupRateLimiter()
	// No config set => disabled => always allow.
	for i := 0; i < 1000; i++ {
		if !g.allow("1.2.3.4") {
			t.Fatalf("disabled limiter denied request %d", i)
		}
	}
}

func TestGroupRateLimiter_EnforcesBurstThenLimits(t *testing.T) {
	g := newGroupRateLimiter()
	g.setConfig(RateLimitConfig{Enabled: true, RPS: 1, Burst: 3})

	// The first `burst` requests from one client are allowed immediately.
	allowed := 0
	for i := 0; i < 10; i++ {
		if g.allow("client-a") {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("expected exactly burst=3 immediate allows, got %d", allowed)
	}
}

func TestGroupRateLimiter_PerClientIsolation(t *testing.T) {
	g := newGroupRateLimiter()
	g.setConfig(RateLimitConfig{Enabled: true, RPS: 1, Burst: 2})

	// Exhaust client-a's bucket.
	g.allow("client-a")
	g.allow("client-a")
	if g.allow("client-a") {
		t.Fatal("client-a should be limited after exhausting its burst")
	}
	// client-b has its own independent bucket.
	if !g.allow("client-b") {
		t.Fatal("client-b should not be affected by client-a's limit")
	}
}

func TestGroupRateLimiter_ConfigChangeResetsBuckets(t *testing.T) {
	g := newGroupRateLimiter()
	g.setConfig(RateLimitConfig{Enabled: true, RPS: 1, Burst: 1})
	g.allow("client-a") // consume the single token
	if g.allow("client-a") {
		t.Fatal("expected client-a to be limited")
	}
	// Reconfiguring drops existing buckets, so client-a starts fresh.
	g.setConfig(RateLimitConfig{Enabled: true, RPS: 5, Burst: 5})
	if !g.allow("client-a") {
		t.Fatal("after a config change, client-a's bucket should reset and allow")
	}
}

func TestBackendList_RateLimitFromBackendSet(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Version:        1,
		Backends:       []*pb.Backend{{Address: "a:1", Weight: 1}},
		RateLimitRps:   2,
		RateLimitBurst: 2,
	})

	allowed := 0
	for i := 0; i < 10; i++ {
		if bl.RateLimitAllow("client-x") {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("expected burst=2 allows via BackendList, got %d", allowed)
	}

	// A subsequent update disabling the limit (rps 0) should allow freely.
	bl.Update(&pb.BackendSet{
		Version:      2,
		Backends:     []*pb.Backend{{Address: "a:1", Weight: 1}},
		RateLimitRps: 0,
	})
	for i := 0; i < 100; i++ {
		if !bl.RateLimitAllow("client-y") {
			t.Fatalf("limit disabled but request %d denied", i)
		}
	}
}
