package envflag

import (
	"testing"
	"time"
)

func TestString(t *testing.T) {
	t.Setenv("ENVFLAG_TEST_STRING", "hello")
	if got := String("ENVFLAG_TEST_STRING", "fallback"); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
	if got := String("ENVFLAG_TEST_STRING_UNSET", "fallback"); got != "fallback" {
		t.Errorf("got %q, want %q", got, "fallback")
	}
}

func TestBool(t *testing.T) {
	t.Setenv("ENVFLAG_TEST_BOOL", "true")
	if got := Bool("ENVFLAG_TEST_BOOL", false); got != true {
		t.Errorf("got %v, want true", got)
	}
	if got := Bool("ENVFLAG_TEST_BOOL_UNSET", true); got != true {
		t.Errorf("got %v, want true (fallback)", got)
	}

	t.Setenv("ENVFLAG_TEST_BOOL_INVALID", "not-a-bool")
	if got := Bool("ENVFLAG_TEST_BOOL_INVALID", true); got != true {
		t.Errorf("got %v, want fallback true for invalid value", got)
	}
}

func TestInt(t *testing.T) {
	t.Setenv("ENVFLAG_TEST_INT", "42")
	if got := Int("ENVFLAG_TEST_INT", 0); got != 42 {
		t.Errorf("got %d, want 42", got)
	}

	t.Setenv("ENVFLAG_TEST_INT_INVALID", "not-a-number")
	if got := Int("ENVFLAG_TEST_INT_INVALID", 7); got != 7 {
		t.Errorf("got %d, want fallback 7 for invalid value", got)
	}
}

func TestDuration(t *testing.T) {
	t.Setenv("ENVFLAG_TEST_DURATION", "5s")
	if got := Duration("ENVFLAG_TEST_DURATION", time.Second); got != 5*time.Second {
		t.Errorf("got %v, want 5s", got)
	}

	t.Setenv("ENVFLAG_TEST_DURATION_INVALID", "not-a-duration")
	if got := Duration("ENVFLAG_TEST_DURATION_INVALID", 3*time.Second); got != 3*time.Second {
		t.Errorf("got %v, want fallback 3s for invalid value", got)
	}
}
