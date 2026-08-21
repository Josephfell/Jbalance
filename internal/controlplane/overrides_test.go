package controlplane

import (
	"path/filepath"
	"testing"
)

func int32Ptr(v int32) *int32 { return &v }

func TestOverrideStore_SetAndGet(t *testing.T) {
	s := NewOverrideStore("")

	if err := s.Set("g1", "a:1", int32Ptr(5), false); err != nil {
		t.Fatalf("Set() error: %v", err)
	}

	got := s.GroupOverrides("g1")
	ov, ok := got["a:1"]
	if !ok {
		t.Fatal("expected an override for a:1")
	}
	if ov.Weight == nil || *ov.Weight != 5 {
		t.Errorf("expected weight override 5, got %+v", ov)
	}
	if ov.Drained {
		t.Error("expected drained to be false")
	}
}

func TestOverrideStore_GroupOverrides_EmptyForUnknownGroup(t *testing.T) {
	s := NewOverrideStore("")
	got := s.GroupOverrides("does-not-exist")
	if len(got) != 0 {
		t.Errorf("expected no overrides for an unknown group, got %v", got)
	}
}

func TestOverrideStore_GroupOverrides_ReturnsDefensiveCopy(t *testing.T) {
	s := NewOverrideStore("")
	_ = s.Set("g1", "a:1", int32Ptr(1), false)

	got := s.GroupOverrides("g1")
	got["a:1"] = Override{Drained: true}

	got2 := s.GroupOverrides("g1")
	if got2["a:1"].Drained {
		t.Error("expected GroupOverrides to return a defensive copy, not a reference into internal state")
	}
}

func TestOverrideStore_Clear(t *testing.T) {
	s := NewOverrideStore("")
	_ = s.Set("g1", "a:1", int32Ptr(3), true)

	if err := s.Clear("g1", "a:1"); err != nil {
		t.Fatalf("Clear() error: %v", err)
	}

	got := s.GroupOverrides("g1")
	if _, ok := got["a:1"]; ok {
		t.Error("expected override to be removed after Clear")
	}
}

func TestOverrideStore_ClearUnknownIsNoop(t *testing.T) {
	s := NewOverrideStore("")
	if err := s.Clear("does-not-exist", "a:1"); err != nil {
		t.Errorf("expected Clear on an unknown group/address to be a no-op, got error: %v", err)
	}
}

func TestOverrideStore_PersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.json")

	s1 := NewOverrideStore(path)
	if err := s1.Set("g1", "a:1", int32Ptr(7), false); err != nil {
		t.Fatalf("Set() error: %v", err)
	}
	if err := s1.Set("g1", "b:1", nil, true); err != nil {
		t.Fatalf("Set() error: %v", err)
	}

	s2 := NewOverrideStore(path)
	got := s2.GroupOverrides("g1")

	ovA, ok := got["a:1"]
	if !ok || ovA.Weight == nil || *ovA.Weight != 7 {
		t.Errorf("expected reloaded override for a:1 with weight 7, got %+v", got)
	}
	ovB, ok := got["b:1"]
	if !ok || !ovB.Drained {
		t.Errorf("expected reloaded override for b:1 to be drained, got %+v", got)
	}
}

func TestOverrideStore_InMemoryWhenPathEmpty(t *testing.T) {
	s := NewOverrideStore("")
	if err := s.Set("g1", "a:1", int32Ptr(1), false); err != nil {
		t.Fatalf("Set() error: %v", err)
	}
	// No path configured — persist() should be a no-op, not an error.
}

func TestOverrideStore_MissingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s := NewOverrideStore(path)
	if got := s.GroupOverrides("g1"); len(got) != 0 {
		t.Errorf("expected an empty store when the file doesn't exist, got %v", got)
	}
}
