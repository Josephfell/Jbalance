package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/Josephfell/Jbalance/internal/pool"
	pb "github.com/Josephfell/Jbalance/proto"
)

func TestBackendSetsEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b *pb.BackendSet
		want bool
	}{
		{
			name: "both nil-ish are equal",
			a:    nil,
			b:    nil,
			want: true,
		},
		{
			name: "one nil, one not, are not equal",
			a:    nil,
			b:    &pb.BackendSet{Backends: []*pb.Backend{{Address: "a:1", Weight: 1}}},
			want: false,
		},
		{
			name: "identical sets are equal",
			a:    &pb.BackendSet{Backends: []*pb.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}}},
			b:    &pb.BackendSet{Backends: []*pb.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}}},
			want: true,
		},
		{
			name: "same backends, different order, are equal",
			a:    &pb.BackendSet{Backends: []*pb.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}}},
			b:    &pb.BackendSet{Backends: []*pb.Backend{{Address: "b:1", Weight: 1}, {Address: "a:1", Weight: 1}}},
			want: true,
		},
		{
			name: "different backend count are not equal",
			a:    &pb.BackendSet{Backends: []*pb.Backend{{Address: "a:1", Weight: 1}}},
			b:    &pb.BackendSet{Backends: []*pb.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}}},
			want: false,
		},
		{
			name: "same address different weight are not equal",
			a:    &pb.BackendSet{Backends: []*pb.Backend{{Address: "a:1", Weight: 1}}},
			b:    &pb.BackendSet{Backends: []*pb.Backend{{Address: "a:1", Weight: 2}}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := backendSetsEqual(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("backendSetsEqual() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestServer_PublishesOnlyOnChange(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil) // provider unused directly in this test — publishIfChanged doesn't touch it

	snap1 := pool.Snapshot{Group: "g1", Backends: []pool.Backend{{Address: "a:1", Weight: 1}}}
	srv.publishIfChanged("g1", snap1)
	v1 := srv.last["g1"].Version

	// Publishing the same backend set again should not bump the version.
	srv.publishIfChanged("g1", snap1)
	v2 := srv.last["g1"].Version

	if v1 != v2 {
		t.Errorf("expected version to stay at %d for an unchanged backend set, got %d", v1, v2)
	}

	snap2 := pool.Snapshot{Group: "g1", Backends: []pool.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}}}
	srv.publishIfChanged("g1", snap2)
	v3 := srv.last["g1"].Version

	if v3 != v1+1 {
		t.Errorf("expected version to increment to %d after a real change, got %d", v1+1, v3)
	}
}

func TestServer_StreamBackendsDeliversUpdates(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)

	ch := make(chan *pb.BackendSet, 4)
	srv.subscribe("g1", ch)
	defer srv.unsubscribe("g1", ch)

	snap := pool.Snapshot{Group: "g1", Backends: []pool.Backend{{Address: "a:1", Weight: 1}}}
	srv.publishIfChanged("g1", snap)

	select {
	case got := <-ch:
		if len(got.Backends) != 1 || got.Backends[0].Address != "a:1" {
			t.Errorf("unexpected backend set delivered: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscriber to receive update")
	}
}

func TestServer_SetOverride_DrainExcludesFromPublishedSetButNotSnapshot(t *testing.T) {
	provider := &stubProvider{
		groups: []string{"g1"},
		snapshots: map[string]pool.Snapshot{
			"g1": {Group: "g1", Backends: []pool.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}}},
		},
	}
	srv := NewServer(provider, nil, nil, nil, nil)
	srv.publishIfChanged("g1", provider.snapshots["g1"])

	ctx := context.Background()
	if err := srv.SetOverride(ctx, "g1", "a:1", nil, true); err != nil {
		t.Fatalf("SetOverride() error: %v", err)
	}

	// What's sent to data planes must exclude the drained backend.
	srv.mu.Lock()
	published := srv.last["g1"]
	srv.mu.Unlock()
	for _, b := range published.Backends {
		if b.Address == "a:1" {
			t.Error("expected drained backend a:1 to be excluded from the published backend set")
		}
	}

	// The admin-facing snapshot must still show it, so it can be
	// un-drained.
	states := srv.Snapshot(ctx)
	if len(states) != 1 {
		t.Fatalf("expected 1 group, got %d", len(states))
	}
	var found bool
	for _, b := range states[0].Backends {
		if b.Address == "a:1" {
			found = true
			if !b.Drained {
				t.Error("expected a:1 to be marked drained in the snapshot")
			}
		}
	}
	if !found {
		t.Error("expected drained backend a:1 to still be visible in Snapshot()")
	}
}

func TestServer_SetOverride_WeightOverrideAppliesToPublishedSet(t *testing.T) {
	provider := &stubProvider{
		groups: []string{"g1"},
		snapshots: map[string]pool.Snapshot{
			"g1": {Group: "g1", Backends: []pool.Backend{{Address: "a:1", Weight: 1}}},
		},
	}
	srv := NewServer(provider, nil, nil, nil, nil)
	srv.publishIfChanged("g1", provider.snapshots["g1"])

	ctx := context.Background()
	weight := int32(9)
	if err := srv.SetOverride(ctx, "g1", "a:1", &weight, false); err != nil {
		t.Fatalf("SetOverride() error: %v", err)
	}

	srv.mu.Lock()
	published := srv.last["g1"]
	srv.mu.Unlock()
	if len(published.Backends) != 1 || published.Backends[0].Weight != 9 {
		t.Errorf("expected published weight override to be 9, got %+v", published.Backends)
	}
}

func TestServer_ClearOverride_RestoresProviderWeight(t *testing.T) {
	provider := &stubProvider{
		groups: []string{"g1"},
		snapshots: map[string]pool.Snapshot{
			"g1": {Group: "g1", Backends: []pool.Backend{{Address: "a:1", Weight: 1}}},
		},
	}
	srv := NewServer(provider, nil, nil, nil, nil)
	srv.publishIfChanged("g1", provider.snapshots["g1"])

	ctx := context.Background()
	weight := int32(9)
	_ = srv.SetOverride(ctx, "g1", "a:1", &weight, false)

	if err := srv.ClearOverride(ctx, "g1", "a:1"); err != nil {
		t.Fatalf("ClearOverride() error: %v", err)
	}

	srv.mu.Lock()
	published := srv.last["g1"]
	srv.mu.Unlock()
	if len(published.Backends) != 1 || published.Backends[0].Weight != 1 {
		t.Errorf("expected weight to revert to provider's value 1 after ClearOverride, got %+v", published.Backends)
	}
}

func TestServer_SetAlgorithm_UpdatesSnapshotAndPublishedSet(t *testing.T) {
	provider := &stubProvider{
		groups: []string{"g1"},
		snapshots: map[string]pool.Snapshot{
			"g1": {Group: "g1", Backends: []pool.Backend{{Address: "a:1", Weight: 1}}},
		},
	}
	srv := NewServer(provider, nil, nil, nil, nil)
	srv.publishIfChanged("g1", provider.snapshots["g1"])

	ctx := context.Background()
	if err := srv.SetAlgorithm(ctx, "g1", AlgorithmLeastConnections); err != nil {
		t.Fatalf("SetAlgorithm() error: %v", err)
	}

	srv.mu.Lock()
	published := srv.last["g1"]
	srv.mu.Unlock()
	if published.Algorithm != string(AlgorithmLeastConnections) {
		t.Errorf("expected published algorithm least_connections, got %q", published.Algorithm)
	}

	states := srv.Snapshot(ctx)
	if len(states) != 1 || states[0].Algorithm != AlgorithmLeastConnections {
		t.Errorf("expected snapshot to reflect the new algorithm, got %+v", states)
	}
}

func TestServer_SetAlgorithm_RejectsInvalidAlgorithm(t *testing.T) {
	provider := &stubProvider{groups: []string{"g1"}, snapshots: map[string]pool.Snapshot{"g1": {Group: "g1"}}}
	srv := NewServer(provider, nil, nil, nil, nil)

	if err := srv.SetAlgorithm(context.Background(), "g1", Algorithm("bogus")); err == nil {
		t.Error("expected an error for an invalid algorithm")
	}
}

func TestServer_ForceRepublish_BumpsVersionEvenIfSetLooksUnchanged(t *testing.T) {
	provider := &stubProvider{
		groups: []string{"g1"},
		snapshots: map[string]pool.Snapshot{
			"g1": {Group: "g1", Backends: []pool.Backend{{Address: "a:1", Weight: 1}}},
		},
	}
	srv := NewServer(provider, nil, nil, nil, nil)
	srv.publishIfChanged("g1", provider.snapshots["g1"])

	v1 := srv.last["g1"].Version

	// Re-applying the same weight override produces a BackendSet that
	// looks identical to what's already published, but forceRepublish
	// must still bump the version and notify subscribers.
	weight := int32(1)
	ctx := context.Background()
	_ = srv.SetOverride(ctx, "g1", "a:1", &weight, false)

	v2 := srv.last["g1"].Version
	if v2 != v1+1 {
		t.Errorf("expected forceRepublish to bump version from %d to %d, got %d", v1, v1+1, v2)
	}
}

func TestServer_FleetSnapshot_EmptyBeforeAnyConnection(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)
	if got := srv.FleetSnapshot(); len(got) != 0 {
		t.Errorf("expected an empty fleet before any instance connects, got %v", got)
	}
}

func TestServer_FleetSnapshot_ReflectsConnectAndDisconnect(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)

	srv.markInstanceConnected("dp-1", "web-tier")
	fleet := srv.FleetSnapshot()
	if len(fleet) != 1 {
		t.Fatalf("expected 1 fleet entry after connect, got %d", len(fleet))
	}
	if fleet[0].InstanceID != "dp-1" || fleet[0].Group != "web-tier" || !fleet[0].Connected {
		t.Errorf("unexpected fleet entry after connect: %+v", fleet[0])
	}

	srv.markInstanceDisconnected("dp-1")
	fleet = srv.FleetSnapshot()
	if len(fleet) != 1 {
		t.Fatalf("expected the entry to persist (not be removed) after disconnect, got %d entries", len(fleet))
	}
	if fleet[0].Connected {
		t.Error("expected Connected to be false after markInstanceDisconnected")
	}
}

func TestServer_FleetSnapshot_IgnoresEmptyInstanceID(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)
	srv.markInstanceConnected("", "web-tier")
	srv.markInstanceDisconnected("")
	srv.recordInstanceHealthReport("", 3, time.Now())

	if got := srv.FleetSnapshot(); len(got) != 0 {
		t.Errorf("expected an empty instance ID to be skipped entirely, got %v", got)
	}
}

func TestServer_FleetSnapshot_ReconnectOverwritesPriorEntry(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)
	srv.markInstanceConnected("dp-1", "web-tier")
	srv.markInstanceDisconnected("dp-1")

	srv.markInstanceConnected("dp-1", "api-tier") // reconnected, now serving a different group
	fleet := srv.FleetSnapshot()
	if len(fleet) != 1 {
		t.Fatalf("expected reconnecting to overwrite the prior entry rather than add a second, got %d entries", len(fleet))
	}
	if fleet[0].Group != "api-tier" || !fleet[0].Connected {
		t.Errorf("expected the reconnected entry to reflect the new group and Connected=true, got %+v", fleet[0])
	}
}

func TestServer_FleetSnapshot_HealthReportUpdatesExistingEntry(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)
	srv.markInstanceConnected("dp-1", "web-tier")

	now := time.Now()
	srv.recordInstanceHealthReport("dp-1", 4, now)

	fleet := srv.FleetSnapshot()
	if len(fleet) != 1 {
		t.Fatalf("expected 1 fleet entry, got %d", len(fleet))
	}
	if fleet[0].ReportedBackends != 4 {
		t.Errorf("expected ReportedBackends 4, got %d", fleet[0].ReportedBackends)
	}
	if !fleet[0].LastHealthReport.Equal(now) {
		t.Errorf("expected LastHealthReport %v, got %v", now, fleet[0].LastHealthReport)
	}
}

func TestServer_FleetSnapshot_HealthReportWithoutStreamIsNoop(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)
	// No markInstanceConnected call — a health report for an instance
	// with no fleet entry yet should not fabricate one.
	srv.recordInstanceHealthReport("dp-ghost", 2, time.Now())

	if got := srv.FleetSnapshot(); len(got) != 0 {
		t.Errorf("expected no fleet entry to be created from a health report alone, got %v", got)
	}
}

func TestServer_FleetSnapshot_SortedByInstanceID(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)
	srv.markInstanceConnected("dp-3", "web-tier")
	srv.markInstanceConnected("dp-1", "web-tier")
	srv.markInstanceConnected("dp-2", "web-tier")

	fleet := srv.FleetSnapshot()
	if len(fleet) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(fleet))
	}
	want := []string{"dp-1", "dp-2", "dp-3"}
	for i, id := range want {
		if fleet[i].InstanceID != id {
			t.Errorf("expected sorted order %v, got %v", want, fleet)
			break
		}
	}
}

func TestServer_SetRoutes_PublishesToSubscribers(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)

	ch := make(chan *pb.RouteTable, 4)
	srv.routeSubsMu.Lock()
	srv.routeSubs[ch] = struct{}{}
	srv.routeSubsMu.Unlock()

	routes := []Route{{Host: "acme.io", PathPrefix: "/api/", TargetGroup: "api-tier"}}
	if err := srv.SetRoutes(routes); err != nil {
		t.Fatalf("SetRoutes() error: %v", err)
	}

	select {
	case got := <-ch:
		if len(got.Routes) != 1 || got.Routes[0].TargetGroup != "api-tier" {
			t.Errorf("unexpected route table delivered: %+v", got)
		}
		if got.Version != 1 {
			t.Errorf("expected version 1, got %d", got.Version)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscriber to receive the route table")
	}
}

func TestServer_Routes_ReflectsSetRoutes(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)
	routes := []Route{
		{Host: "acme.io", PathPrefix: "/static/", TargetGroup: "static-edge"},
		{PathPrefix: "/", TargetGroup: "web-tier"},
	}
	if err := srv.SetRoutes(routes); err != nil {
		t.Fatalf("SetRoutes() error: %v", err)
	}

	got := srv.Routes()
	if len(got) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(got))
	}
	if got[0].TargetGroup != "static-edge" || got[1].TargetGroup != "web-tier" {
		t.Errorf("expected evaluation order preserved, got %+v", got)
	}
}

func TestServer_StreamRoutes_DeliversCurrentTableImmediately(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)
	_ = srv.SetRoutes([]Route{{PathPrefix: "/", TargetGroup: "web-tier"}})

	srv.routeSubsMu.Lock()
	current := srv.lastRoutes
	srv.routeSubsMu.Unlock()

	if current == nil {
		t.Fatal("expected lastRoutes to be populated after SetRoutes")
	}
	if len(current.Routes) != 1 || current.Routes[0].TargetGroup != "web-tier" {
		t.Errorf("unexpected lastRoutes content: %+v", current)
	}
}

func TestServer_SetSticky_UpdatesPublishedSetAndSnapshot(t *testing.T) {
	provider := &stubProvider{
		groups: []string{"g1"},
		snapshots: map[string]pool.Snapshot{
			"g1": {Group: "g1", Backends: []pool.Backend{{Address: "a:1", Weight: 1}}},
		},
	}
	srv := NewServer(provider, nil, nil, nil, nil)
	srv.publishIfChanged("g1", provider.snapshots["g1"])

	ctx := context.Background()
	cfg := StickyConfig{Enabled: true, CookieName: "jb_test", TTL: 20 * time.Minute}
	if err := srv.SetSticky(ctx, "g1", cfg); err != nil {
		t.Fatalf("SetSticky() error: %v", err)
	}

	srv.mu.Lock()
	published := srv.last["g1"]
	srv.mu.Unlock()
	if !published.Sticky || published.StickyCookieName != "jb_test" || published.StickyTtlSeconds != 1200 {
		t.Errorf("expected published sticky fields to reflect SetSticky, got sticky=%v cookie=%q ttl=%d",
			published.Sticky, published.StickyCookieName, published.StickyTtlSeconds)
	}

	states := srv.Snapshot(ctx)
	if len(states) != 1 || !states[0].Sticky.Enabled || states[0].Sticky.CookieName != "jb_test" {
		t.Errorf("expected snapshot to reflect the new sticky config, got %+v", states)
	}
}

func TestServer_Sticky_DefaultsToDisabled(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil)
	if srv.Sticky("g1").Enabled {
		t.Error("expected an unconfigured group's sticky config to default to disabled")
	}
}
