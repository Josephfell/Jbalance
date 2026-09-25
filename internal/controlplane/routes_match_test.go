package controlplane

import (
	"net/http"
	"net/url"
	"testing"
)

// hdr builds an http.Header from name/value pairs for terse test setup.
func hdr(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Add(pairs[i], pairs[i+1])
	}
	return h
}

func TestRoute_MatchesRequest_HeaderEquals(t *testing.T) {
	r := Route{
		PathPrefix:    "/",
		TargetGroup:   "api-v2",
		HeaderMatches: []HeaderMatch{{Name: "X-Api-Version", Value: "2"}},
	}
	if !r.MatchesRequest("", "/orders", "GET", hdr("X-Api-Version", "2"), nil) {
		t.Error("expected a request with the matching header value to match")
	}
	if r.MatchesRequest("", "/orders", "GET", hdr("X-Api-Version", "1"), nil) {
		t.Error("expected a request with a different header value not to match")
	}
	if r.MatchesRequest("", "/orders", "GET", nil, nil) {
		t.Error("expected a request missing the header not to match")
	}
}

func TestRoute_MatchesRequest_HeaderPresenceOnly(t *testing.T) {
	r := Route{
		PathPrefix:    "/",
		TargetGroup:   "debug",
		HeaderMatches: []HeaderMatch{{Name: "X-Debug"}}, // empty value = presence
	}
	if !r.MatchesRequest("", "/", "GET", hdr("X-Debug", "anything"), nil) {
		t.Error("expected presence-only header condition to match on any value")
	}
	if !r.MatchesRequest("", "/", "GET", hdr("X-Debug", ""), nil) {
		t.Error("expected presence-only header condition to match on an empty value")
	}
	if r.MatchesRequest("", "/", "GET", hdr("X-Other", "y"), nil) {
		t.Error("expected presence-only condition not to match when the header is absent")
	}
}

func TestRoute_MatchesRequest_HeaderNameCaseInsensitive(t *testing.T) {
	r := Route{
		PathPrefix:    "/",
		TargetGroup:   "g",
		HeaderMatches: []HeaderMatch{{Name: "x-api-version", Value: "2"}},
	}
	// http.Header canonicalises names, so a differently-cased name still matches.
	if !r.MatchesRequest("", "/", "GET", hdr("X-Api-Version", "2"), nil) {
		t.Error("expected header-name matching to be case-insensitive")
	}
}

func TestRoute_MatchesRequest_QueryEquals(t *testing.T) {
	r := Route{
		PathPrefix:   "/",
		TargetGroup:  "canary",
		QueryMatches: []QueryMatch{{Name: "canary", Value: "true"}},
	}
	if !r.MatchesRequest("", "/", "GET", nil, url.Values{"canary": {"true"}}) {
		t.Error("expected the matching query value to match")
	}
	if r.MatchesRequest("", "/", "GET", nil, url.Values{"canary": {"false"}}) {
		t.Error("expected a different query value not to match")
	}
	if r.MatchesRequest("", "/", "GET", nil, url.Values{"other": {"true"}}) {
		t.Error("expected an absent query param not to match")
	}
}

func TestRoute_MatchesRequest_QueryPresenceOnly(t *testing.T) {
	r := Route{
		PathPrefix:   "/",
		TargetGroup:  "g",
		QueryMatches: []QueryMatch{{Name: "debug"}},
	}
	if !r.MatchesRequest("", "/", "GET", nil, url.Values{"debug": {""}}) {
		t.Error("expected presence-only query condition to match an empty value")
	}
	if r.MatchesRequest("", "/", "GET", nil, url.Values{}) {
		t.Error("expected presence-only query condition not to match when absent")
	}
}

func TestRoute_MatchesRequest_AllConditionsAnded(t *testing.T) {
	r := Route{
		PathPrefix:    "/api/",
		Methods:       []string{"POST"},
		TargetGroup:   "g",
		HeaderMatches: []HeaderMatch{{Name: "X-Api-Version", Value: "2"}},
		QueryMatches:  []QueryMatch{{Name: "canary", Value: "true"}},
	}
	h := hdr("X-Api-Version", "2")
	q := url.Values{"canary": {"true"}}

	if !r.MatchesRequest("", "/api/x", "POST", h, q) {
		t.Error("expected a request satisfying every condition to match")
	}
	// Fail on each condition in turn — any one miss should drop the match.
	if r.MatchesRequest("", "/api/x", "GET", h, q) {
		t.Error("wrong method should not match")
	}
	if r.MatchesRequest("", "/other", "POST", h, q) {
		t.Error("wrong path should not match")
	}
	if r.MatchesRequest("", "/api/x", "POST", hdr("X-Api-Version", "1"), q) {
		t.Error("wrong header value should not match")
	}
	if r.MatchesRequest("", "/api/x", "POST", h, url.Values{"canary": {"false"}}) {
		t.Error("wrong query value should not match")
	}
}

func TestRoute_MatchesRequest_NoConditionsBehavesLikeMatches(t *testing.T) {
	// A route with no header/query conditions must match exactly when the
	// host/path/method-only Matches would, even with nil header/query.
	r := Route{PathPrefix: "/api/", TargetGroup: "api"}
	if !r.MatchesRequest("", "/api/v1", "GET", nil, nil) {
		t.Error("expected a route with no header/query conditions to match on host/path/method alone")
	}
	if r.MatchesRequest("", "/static", "GET", nil, nil) {
		t.Error("expected path mismatch to still fail with no header/query conditions")
	}
}
