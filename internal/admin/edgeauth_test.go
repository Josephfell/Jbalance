package admin

import (
	"reflect"
	"testing"
)

func TestParseAuthEmpty(t *testing.T) {
	if a := parseAuth(""); a != nil {
		t.Errorf("empty should be nil (open), got %+v", a)
	}
	if a := parseAuth("garbage"); a != nil {
		t.Errorf("unrecognised should be nil (open), got %+v", a)
	}
}

func TestParseAuthAPIKey(t *testing.T) {
	a := parseAuth("apikey:k1, k2 ,k3")
	if a == nil || a.Mode != "api_key" {
		t.Fatalf("expected api_key mode, got %+v", a)
	}
	if !reflect.DeepEqual(a.APIKeys, []string{"k1", "k2", "k3"}) {
		t.Errorf("keys = %v, want [k1 k2 k3]", a.APIKeys)
	}
}

func TestParseAuthJWT(t *testing.T) {
	a := parseAuth("jwt:hmac:sekret")
	if a == nil || a.Mode != "jwt" || a.HMACSecret != "sekret" {
		t.Fatalf("expected jwt/hmac, got %+v", a)
	}
	a2 := parseAuth("jwt:hmac:sekret:my-issuer:my-aud")
	if a2 == nil || a2.ExpectedIssuer != "my-issuer" || a2.ExpectedAudience != "my-aud" {
		t.Fatalf("expected iss/aud parsed, got %+v", a2)
	}
}

func TestAuthRoundTrip(t *testing.T) {
	for _, in := range []string{"apikey:a,b", "jwt:hmac:s", "jwt:hmac:s:iss:aud"} {
		a := parseAuth(in)
		if got := formatAuth(a); got != in {
			t.Errorf("round-trip %q -> %q", in, got)
		}
	}
}
