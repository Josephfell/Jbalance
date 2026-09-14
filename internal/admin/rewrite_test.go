package admin

import (
	"reflect"
	"testing"
)

func TestParseRewriteEmpty(t *testing.T) {
	if rw := parseRewrite("", ""); rw != nil {
		t.Errorf("empty inputs should yield nil rewrite, got %+v", rw)
	}
	if rw := parseRewrite("  ", "\n  \n"); rw != nil {
		t.Errorf("blank inputs should yield nil rewrite, got %+v", rw)
	}
}

func TestParseRewriteSetAndRemove(t *testing.T) {
	rw := parseRewrite("/api", "X-From: edge\n-X-Debug\nX-Trace: on")
	if rw == nil {
		t.Fatal("expected non-nil rewrite")
	}
	if rw.StripPathPrefix != "/api" {
		t.Errorf("StripPathPrefix = %q, want /api", rw.StripPathPrefix)
	}
	want := map[string]string{"X-From": "edge", "X-Trace": "on"}
	if !reflect.DeepEqual(rw.SetRequestHeaders, want) {
		t.Errorf("SetRequestHeaders = %v, want %v", rw.SetRequestHeaders, want)
	}
	if !reflect.DeepEqual(rw.RemoveRequestHeaders, []string{"X-Debug"}) {
		t.Errorf("RemoveRequestHeaders = %v, want [X-Debug]", rw.RemoveRequestHeaders)
	}
}

func TestRewriteRoundTrip(t *testing.T) {
	strip, hdrs := "/api", "X-A: 1\nX-B: 2\n-X-Drop"
	rw := parseRewrite(strip, hdrs)
	gotStrip, gotHdrs := formatRewrite(rw)
	if gotStrip != strip {
		t.Errorf("strip round-trip = %q, want %q", gotStrip, strip)
	}
	// Re-parsing the formatted output must yield an equal rewrite (order of
	// set-headers is normalized by formatRewrite's sort).
	rw2 := parseRewrite(gotStrip, gotHdrs)
	if !reflect.DeepEqual(rw, rw2) {
		t.Errorf("round-trip mismatch:\n first=%+v\nsecond=%+v", rw, rw2)
	}
}
