package pool

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "backends.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestFileProviderLoadsGroupsAndBackends(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, `
groups:
  web-tier:
    - address: 10.0.0.1:8080
      weight: 2
    - address: 10.0.0.2:8080
  api-tier:
    - address: 10.0.1.1:9090
`)
	p, err := NewFileProvider(path)
	if err != nil {
		t.Fatalf("NewFileProvider: %v", err)
	}

	groups, err := p.Groups(context.Background())
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	sort.Strings(groups)
	if len(groups) != 2 || groups[0] != "api-tier" || groups[1] != "web-tier" {
		t.Fatalf("groups = %v, want [api-tier web-tier]", groups)
	}

	snap, err := p.Snapshot(context.Background(), "web-tier")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Backends) != 2 {
		t.Fatalf("web-tier backends = %d, want 2", len(snap.Backends))
	}
	if snap.Backends[0].Address != "10.0.0.1:8080" || snap.Backends[0].Weight != 2 {
		t.Errorf("backend[0] = %+v, want {10.0.0.1:8080 2}", snap.Backends[0])
	}
	if snap.Backends[1].Weight != 0 {
		t.Errorf("omitted weight = %d, want 0 (control plane treats 0 as 1)", snap.Backends[1].Weight)
	}
}

func TestFileProviderUnknownGroup(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "groups:\n  web-tier:\n    - address: 1.2.3.4:80\n")
	p, err := NewFileProvider(path)
	if err != nil {
		t.Fatalf("NewFileProvider: %v", err)
	}
	if _, err := p.Snapshot(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown group")
	}
}

func TestFileProviderHotReload(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "groups:\n  web-tier:\n    - address: 1.2.3.4:80\n")
	p, err := NewFileProvider(path)
	if err != nil {
		t.Fatalf("NewFileProvider: %v", err)
	}

	snap, _ := p.Snapshot(context.Background(), "web-tier")
	if len(snap.Backends) != 1 {
		t.Fatalf("initial backends = %d, want 1", len(snap.Backends))
	}

	// Rewrite with an extra backend. Bump mtime explicitly so the change is
	// observed even if the two writes land in the same filesystem tick.
	if err := os.WriteFile(path, []byte("groups:\n  web-tier:\n    - address: 1.2.3.4:80\n    - address: 5.6.7.8:80\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	snap, err = p.Snapshot(context.Background(), "web-tier")
	if err != nil {
		t.Fatalf("Snapshot after reload: %v", err)
	}
	if len(snap.Backends) != 2 {
		t.Fatalf("after reload backends = %d, want 2", len(snap.Backends))
	}
}

func TestFileProviderMissingFileFailsFast(t *testing.T) {
	if _, err := NewFileProvider(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFileProviderEmptyPath(t *testing.T) {
	if _, err := NewFileProvider(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestFileProviderEmptyAddressRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "groups:\n  web-tier:\n    - weight: 3\n")
	if _, err := NewFileProvider(path); err == nil {
		t.Fatal("expected error for entry with empty address")
	}
}
