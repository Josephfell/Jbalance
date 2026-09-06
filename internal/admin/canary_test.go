package admin

import (
	"testing"

	"github.com/Josephfell/Jbalance/internal/controlplane"
)

func TestParseSplit(t *testing.T) {
	got := parseSplit("stable:90, canary:10")
	want := []controlplane.RouteTarget{
		{Group: "stable", Weight: 90},
		{Group: "canary", Weight: 10},
	}
	if len(got) != len(want) {
		t.Fatalf("parseSplit len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Bare group defaults to weight 1.
	g := parseSplit("solo")
	if len(g) != 1 || g[0].Group != "solo" || g[0].Weight != 1 {
		t.Errorf("bare group parse = %+v, want [{solo 1}]", g)
	}

	// Malformed weight drops the entry; empty input -> nil.
	if len(parseSplit("bad:notanumber")) != 0 {
		t.Error("expected malformed-weight entry to be dropped")
	}
	if parseSplit("  ") != nil {
		t.Error("expected empty input to parse to nil")
	}
}

func TestFormatSplitRoundTrips(t *testing.T) {
	targets := []controlplane.RouteTarget{
		{Group: "stable", Weight: 90},
		{Group: "canary", Weight: 10},
	}
	s := formatSplit(targets)
	if s != "stable:90, canary:10" {
		t.Fatalf("formatSplit = %q", s)
	}
	// Re-parsing the formatted string yields the original targets.
	reparsed := parseSplit(s)
	if len(reparsed) != 2 || reparsed[0] != targets[0] || reparsed[1] != targets[1] {
		t.Errorf("round-trip mismatch: %+v", reparsed)
	}

	if formatSplit(nil) != "" {
		t.Error("formatSplit(nil) should be empty")
	}
}
