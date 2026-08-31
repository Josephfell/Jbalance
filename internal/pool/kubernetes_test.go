package pool

import (
	"context"
	"sort"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
)

// fakeEndpoints is an in-memory endpointsGetter for testing Snapshot
// without a real API server.
type fakeEndpoints struct {
	// keyed by "namespace/service"
	slices map[string][]discoveryv1.EndpointSlice
	err    error
}

func (f *fakeEndpoints) ListEndpointSlices(_ context.Context, namespace, service string) ([]discoveryv1.EndpointSlice, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.slices[namespace+"/"+service], nil
}

func endpoint(ready, terminating *bool, addrs ...string) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{
		Addresses: addrs,
		Conditions: discoveryv1.EndpointConditions{
			Ready:       ready,
			Terminating: terminating,
		},
	}
}

func TestKubernetesProvider_SnapshotReportsReadyEndpoints(t *testing.T) {
	fake := &fakeEndpoints{slices: map[string][]discoveryv1.EndpointSlice{
		"default/web": {
			{
				Endpoints: []discoveryv1.Endpoint{
					endpoint(boolPtr(true), nil, "10.0.0.1"),
					endpoint(boolPtr(false), nil, "10.0.0.2"),          // not ready -> excluded
					endpoint(boolPtr(true), boolPtr(true), "10.0.0.3"), // terminating -> excluded
					endpoint(nil, nil, "10.0.0.4"),                     // nil Ready -> treated as ready
				},
			},
		},
	}}

	p := newKubernetesProviderWithClient(fake, []KubernetesGroup{
		{Group: "web-tier", Namespace: "default", Service: "web", Port: 8080},
	})

	snap, err := p.Snapshot(context.Background(), "web-tier")
	if err != nil {
		t.Fatalf("Snapshot returned error: %v", err)
	}

	got := addrs(snap)
	want := []string{"10.0.0.1:8080", "10.0.0.4:8080"}
	if !equalStringSets(got, want) {
		t.Errorf("expected only ready, non-terminating endpoints %v, got %v", want, got)
	}
	for _, b := range snap.Backends {
		if b.Weight != 1 {
			t.Errorf("expected default weight 1, got %d for %s", b.Weight, b.Address)
		}
	}
}

func TestKubernetesProvider_SnapshotDeduplicatesAcrossSlices(t *testing.T) {
	fake := &fakeEndpoints{slices: map[string][]discoveryv1.EndpointSlice{
		"default/web": {
			{Endpoints: []discoveryv1.Endpoint{endpoint(boolPtr(true), nil, "10.0.0.1")}},
			{Endpoints: []discoveryv1.Endpoint{endpoint(boolPtr(true), nil, "10.0.0.1", "10.0.0.5")}},
		},
	}}

	p := newKubernetesProviderWithClient(fake, []KubernetesGroup{
		{Group: "web-tier", Namespace: "default", Service: "web", Port: 9000, Weight: 3},
	})

	snap, err := p.Snapshot(context.Background(), "web-tier")
	if err != nil {
		t.Fatalf("Snapshot returned error: %v", err)
	}

	got := addrs(snap)
	want := []string{"10.0.0.1:9000", "10.0.0.5:9000"}
	if !equalStringSets(got, want) {
		t.Errorf("expected de-duplicated addresses %v, got %v", want, got)
	}
	for _, b := range snap.Backends {
		if b.Weight != 3 {
			t.Errorf("expected configured weight 3, got %d", b.Weight)
		}
	}
}

func TestKubernetesProvider_UnknownGroup(t *testing.T) {
	p := newKubernetesProviderWithClient(&fakeEndpoints{}, []KubernetesGroup{
		{Group: "web-tier", Namespace: "default", Service: "web", Port: 8080},
	})
	if _, err := p.Snapshot(context.Background(), "does-not-exist"); err == nil {
		t.Error("expected an error for an unknown group")
	}
}

func TestKubernetesProvider_Groups(t *testing.T) {
	p := newKubernetesProviderWithClient(&fakeEndpoints{}, []KubernetesGroup{
		{Group: "web-tier", Namespace: "default", Service: "web", Port: 8080},
		{Group: "api-tier", Namespace: "default", Service: "api", Port: 8081},
	})
	groups, err := p.Groups(context.Background())
	if err != nil {
		t.Fatalf("Groups returned error: %v", err)
	}
	if !equalStringSets(groups, []string{"web-tier", "api-tier"}) {
		t.Errorf("unexpected groups: %v", groups)
	}
}

func TestParseKubernetesGroups(t *testing.T) {
	groups, err := ParseKubernetesGroups("web-tier:default:web:8080,api-tier:prod:api:8081:5")
	if err != nil {
		t.Fatalf("ParseKubernetesGroups returned error: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0] != (KubernetesGroup{Group: "web-tier", Namespace: "default", Service: "web", Port: 8080, Weight: 1}) {
		t.Errorf("group 0 mismatch: %+v", groups[0])
	}
	if groups[1] != (KubernetesGroup{Group: "api-tier", Namespace: "prod", Service: "api", Port: 8081, Weight: 5}) {
		t.Errorf("group 1 mismatch: %+v", groups[1])
	}
}

func TestParseKubernetesGroups_Invalid(t *testing.T) {
	for _, spec := range []string{
		"too:few",
		"g:ns:svc",          // missing port
		"g:ns:svc:notaport", // non-numeric port
		"g:ns:svc:80:x",     // non-numeric weight
		"a:b:c:d:e:f",       // too many fields
	} {
		if _, err := ParseKubernetesGroups(spec); err == nil {
			t.Errorf("expected error for invalid spec %q", spec)
		}
	}

	// Empty spec is valid and yields no groups.
	groups, err := ParseKubernetesGroups("  ")
	if err != nil || len(groups) != 0 {
		t.Errorf("expected empty spec to yield no groups and no error, got %v / %v", groups, err)
	}
}

func addrs(s Snapshot) []string {
	out := make([]string, 0, len(s.Backends))
	for _, b := range s.Backends {
		out = append(out, b.Address)
	}
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}
