package controlplane

import (
	"testing"
)

func TestFileBlobStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileBlobStore(dir)
	if err != nil {
		t.Fatalf("NewFileBlobStore: %v", err)
	}

	// Missing key -> (nil, nil), not an error.
	data, err := s.Load("routes")
	if err != nil || data != nil {
		t.Fatalf("Load(missing) = (%v, %v), want (nil, nil)", data, err)
	}

	if err := s.Save("routes", []byte(`{"version":1}`)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load("routes")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != `{"version":1}` {
		t.Errorf("Load = %q, want the saved bytes", got)
	}
}

func TestNewBlobStoreSelection(t *testing.T) {
	// Default/file backend.
	s, closeFn, err := NewBlobStore("", t.TempDir(), "")
	if err != nil || s == nil {
		t.Fatalf("file backend: %v", err)
	}
	_ = closeFn()

	// Unknown backend errors.
	if _, _, err := NewBlobStore("cassandra", "", ""); err == nil {
		t.Error("unknown backend should error")
	}

	// Postgres with no DSN errors (without needing a live DB).
	if _, _, err := NewBlobStore("postgres", "", ""); err == nil {
		t.Error("postgres backend without a DSN should error")
	}
}

func TestRouteStoreThroughBlobStore(t *testing.T) {
	dir := t.TempDir()
	blob, err := NewFileBlobStore(dir)
	if err != nil {
		t.Fatalf("NewFileBlobStore: %v", err)
	}

	rs := NewRouteStoreWithBackend(blob, "routes")
	if err := rs.Set([]Route{{Host: "acme.io", PathPrefix: "/api/", TargetGroup: "api-tier"}}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// A fresh store over the same backend loads the persisted routes.
	rs2 := NewRouteStoreWithBackend(blob, "routes")
	got := rs2.Routes()
	if len(got) != 1 || got[0].TargetGroup != "api-tier" {
		t.Fatalf("reloaded routes = %+v, want 1 rule -> api-tier", got)
	}
	if rs2.Version() != 1 {
		t.Errorf("reloaded version = %d, want 1", rs2.Version())
	}
}

func TestFileStoreFromPathEmpty(t *testing.T) {
	store, key := fileStoreFromPath("")
	if store != nil || key != "" {
		t.Errorf("empty path should yield (nil, \"\"), got (%v, %q)", store, key)
	}
}

func TestFileStoreFromPathKey(t *testing.T) {
	_, key := fileStoreFromPath("/var/lib/go-loadbalancer/routes.json")
	if key != "routes" {
		t.Errorf("key = %q, want routes", key)
	}
}
