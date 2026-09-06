package dataplane

import (
	"testing"
	"time"

	pb "github.com/Josephfell/Jbalance/proto"
)

func twoBackends() *BackendList {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Version:  1,
		Backends: []*pb.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}},
	})
	return bl
}

func TestOutlier_EjectsAfterConsecutiveErrors(t *testing.T) {
	bl := twoBackends()
	bl.SetOutlierConfig(OutlierConfig{
		Enabled:           true,
		ConsecutiveErrors: 3,
		BaseEjectDuration: time.Minute,
		MaxEjectPercent:   50,
	})

	// Two errors: not yet ejected.
	bl.RecordResult("a:1", false)
	bl.RecordResult("a:1", false)
	if bl.EjectedLen() != 0 {
		t.Fatalf("after 2 errors, ejected=%d, want 0", bl.EjectedLen())
	}

	// Third error trips the ejection.
	bl.RecordResult("a:1", false)
	if bl.EjectedLen() != 1 {
		t.Fatalf("after 3 errors, ejected=%d, want 1", bl.EjectedLen())
	}

	// The ejected backend must not be selected.
	for i := 0; i < 20; i++ {
		addr, ok := bl.Next()
		if !ok {
			t.Fatal("expected a healthy backend (b:1) to remain selectable")
		}
		if addr == "a:1" {
			t.Fatalf("ejected backend a:1 was selected on iteration %d", i)
		}
		bl.Release(addr)
	}
}

func TestOutlier_SuccessResetsErrorStreak(t *testing.T) {
	bl := twoBackends()
	bl.SetOutlierConfig(OutlierConfig{Enabled: true, ConsecutiveErrors: 3, BaseEjectDuration: time.Minute, MaxEjectPercent: 50})

	bl.RecordResult("a:1", false)
	bl.RecordResult("a:1", false)
	bl.RecordResult("a:1", true) // reset
	bl.RecordResult("a:1", false)
	bl.RecordResult("a:1", false)
	if bl.EjectedLen() != 0 {
		t.Fatalf("a success should reset the streak; ejected=%d, want 0", bl.EjectedLen())
	}
}

func TestOutlier_ReadmittedAfterCooldown(t *testing.T) {
	bl := twoBackends()
	bl.SetOutlierConfig(OutlierConfig{
		Enabled:           true,
		ConsecutiveErrors: 1,
		BaseEjectDuration: 30 * time.Millisecond,
		MaxEjectDuration:  30 * time.Millisecond,
		MaxEjectPercent:   50,
	})

	bl.RecordResult("a:1", false) // ejects immediately (threshold 1)
	if bl.EjectedLen() != 1 {
		t.Fatalf("ejected=%d, want 1", bl.EjectedLen())
	}

	// After the cooldown, Next should re-admit a:1 without any external
	// timer nudging it.
	time.Sleep(50 * time.Millisecond)
	seenA := false
	for i := 0; i < 50; i++ {
		addr, ok := bl.Next()
		if !ok {
			t.Fatal("expected a selectable backend after cooldown")
		}
		if addr == "a:1" {
			seenA = true
		}
		bl.Release(addr)
	}
	if !seenA {
		t.Fatal("backend a:1 was not re-admitted after its ejection cooldown lapsed")
	}
	if bl.EjectedLen() != 0 {
		t.Fatalf("after cooldown, ejected=%d, want 0", bl.EjectedLen())
	}
}

func TestOutlier_MaxEjectPercentSafetyValve(t *testing.T) {
	bl := twoBackends() // 2 backends
	// 50% cap => at most 1 of 2 may be ejected.
	bl.SetOutlierConfig(OutlierConfig{
		Enabled:           true,
		ConsecutiveErrors: 1,
		BaseEjectDuration: time.Minute,
		MaxEjectPercent:   50,
	})

	bl.RecordResult("a:1", false) // ejects a:1 (1 of 2 = 50%, allowed)
	if bl.EjectedLen() != 1 {
		t.Fatalf("ejected=%d, want 1", bl.EjectedLen())
	}

	// Ejecting b:1 too would be 2 of 2 = 100% > 50% cap — must be refused.
	bl.RecordResult("b:1", false)
	if bl.EjectedLen() != 1 {
		t.Fatalf("safety valve failed: ejected=%d, want 1 (cap should block the second)", bl.EjectedLen())
	}
	// b:1 must still be selectable.
	addr, ok := bl.Next()
	if !ok || addr != "b:1" {
		t.Fatalf("expected b:1 to remain selectable, got addr=%q ok=%v", addr, ok)
	}
	bl.Release(addr)
}

func TestOutlier_DisabledIsNoop(t *testing.T) {
	bl := twoBackends()
	// Not enabled.
	for i := 0; i < 100; i++ {
		bl.RecordResult("a:1", false)
	}
	if bl.EjectedLen() != 0 {
		t.Fatalf("with detection disabled, ejected=%d, want 0", bl.EjectedLen())
	}
}

func TestOutlier_BackoffIncreasesEjectDuration(t *testing.T) {
	bl := twoBackends()
	bl.SetOutlierConfig(OutlierConfig{
		Enabled:           true,
		ConsecutiveErrors: 1,
		BaseEjectDuration: 20 * time.Millisecond,
		MaxEjectDuration:  time.Second,
		MaxEjectPercent:   50,
	})

	// First ejection: ~20ms.
	bl.RecordResult("a:1", false)
	time.Sleep(30 * time.Millisecond)
	// Nudge re-admission.
	_, _ = bl.Next()

	// Second ejection should be ~40ms (2x). Verify it's still ejected at
	// 25ms (would already be back if it were only 20ms again).
	bl.RecordResult("a:1", false)
	time.Sleep(25 * time.Millisecond)
	// Trigger the lapsed-check path; a:1 should NOT be re-admitted yet.
	_, _ = bl.Next()
	if bl.EjectedLen() != 1 {
		t.Fatalf("second ejection should back off longer than the first; ejected=%d, want 1 at 25ms", bl.EjectedLen())
	}
}
