package admin

import (
	"testing"
	"time"
)

func TestLoginLimiter_AllowsUntilThreshold(t *testing.T) {
	l := newLoginLimiter(3, time.Minute, time.Minute)

	for i := 0; i < 2; i++ {
		if !l.Allowed("1.2.3.4") {
			t.Fatalf("expected attempt %d to be allowed", i)
		}
		l.RecordFailure("1.2.3.4")
	}

	if !l.Allowed("1.2.3.4") {
		t.Error("expected attempt 3 to still be allowed (threshold is 3 failures)")
	}
}

func TestLoginLimiter_BlocksAfterThreshold(t *testing.T) {
	l := newLoginLimiter(3, time.Minute, time.Minute)

	for i := 0; i < 3; i++ {
		l.RecordFailure("1.2.3.4")
	}

	if l.Allowed("1.2.3.4") {
		t.Error("expected the IP to be blocked after reaching the failure threshold")
	}
}

func TestLoginLimiter_DoesNotAffectOtherIPs(t *testing.T) {
	l := newLoginLimiter(3, time.Minute, time.Minute)

	for i := 0; i < 3; i++ {
		l.RecordFailure("1.2.3.4")
	}

	if !l.Allowed("5.6.7.8") {
		t.Error("expected a different IP to remain unaffected by another IP's lockout")
	}
}

func TestLoginLimiter_SuccessClearsFailures(t *testing.T) {
	l := newLoginLimiter(3, time.Minute, time.Minute)

	l.RecordFailure("1.2.3.4")
	l.RecordFailure("1.2.3.4")
	l.RecordSuccess("1.2.3.4")
	l.RecordFailure("1.2.3.4")

	if !l.Allowed("1.2.3.4") {
		t.Error("expected failure count to reset after a recorded success")
	}
}

func TestLoginLimiter_UnlocksAfterLockoutExpires(t *testing.T) {
	l := newLoginLimiter(2, time.Minute, 10*time.Millisecond)

	l.RecordFailure("1.2.3.4")
	l.RecordFailure("1.2.3.4")
	if l.Allowed("1.2.3.4") {
		t.Fatal("expected the IP to be locked out immediately after reaching the threshold")
	}

	time.Sleep(20 * time.Millisecond)

	if !l.Allowed("1.2.3.4") {
		t.Error("expected the IP to be unlocked after the lockout period elapsed")
	}
}
