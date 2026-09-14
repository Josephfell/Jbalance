package dataplane

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	pb "github.com/Josephfell/Jbalance/proto"
)

func TestEdgeAuthOpenRoute(t *testing.T) {
	a := authFromProto(nil)
	if a.enabled() {
		t.Fatal("nil auth should be an open route")
	}
	r := httptest.NewRequest("GET", "/", nil)
	if err := a.authenticate(r); err != nil {
		t.Errorf("open route should always pass, got %v", err)
	}
}

func TestEdgeAuthAPIKey(t *testing.T) {
	a := authFromProto(&pb.RouteAuth{Mode: "api_key", ApiKeys: []string{"good1", "good2"}})
	if !a.enabled() {
		t.Fatal("api_key mode should be enabled")
	}

	// Missing key.
	r := httptest.NewRequest("GET", "/", nil)
	if err := a.authenticate(r); err == nil {
		t.Error("missing API key should fail")
	}
	// Wrong key.
	r.Header.Set("X-API-Key", "nope")
	if err := a.authenticate(r); err == nil {
		t.Error("wrong API key should fail")
	}
	// Correct key.
	r.Header.Set("X-API-Key", "good2")
	if err := a.authenticate(r); err != nil {
		t.Errorf("correct API key should pass, got %v", err)
	}
}

func TestEdgeAuthAPIKeyCustomHeader(t *testing.T) {
	a := authFromProto(&pb.RouteAuth{Mode: "api_key", ApiKeys: []string{"k"}, ApiKeyHeader: "X-Token"})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Token", "k")
	if err := a.authenticate(r); err != nil {
		t.Errorf("custom header key should pass, got %v", err)
	}
}

func signHS256(t *testing.T, secret string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func TestEdgeAuthJWT(t *testing.T) {
	secret := "topsecret"
	a := authFromProto(&pb.RouteAuth{Mode: "jwt", HmacSecret: secret})

	// Valid token.
	good := signHS256(t, secret, jwt.MapClaims{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+good)
	if err := a.authenticate(r); err != nil {
		t.Errorf("valid JWT should pass, got %v", err)
	}

	// Wrong secret.
	bad := signHS256(t, "wrong", jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()})
	r.Header.Set("Authorization", "Bearer "+bad)
	if err := a.authenticate(r); err == nil {
		t.Error("JWT signed with wrong secret should fail")
	}

	// Expired token.
	expired := signHS256(t, secret, jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()})
	r.Header.Set("Authorization", "Bearer "+expired)
	if err := a.authenticate(r); err == nil {
		t.Error("expired JWT should fail")
	}

	// Missing bearer.
	r.Header.Del("Authorization")
	if err := a.authenticate(r); err == nil {
		t.Error("missing bearer token should fail")
	}
}

func TestEdgeAuthJWTIssuerAudience(t *testing.T) {
	secret := "s"
	a := authFromProto(&pb.RouteAuth{Mode: "jwt", HmacSecret: secret, ExpectedIssuer: "iss1", ExpectedAudience: "aud1"})

	// Correct issuer + audience.
	good := signHS256(t, secret, jwt.MapClaims{"iss": "iss1", "aud": "aud1", "exp": time.Now().Add(time.Hour).Unix()})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+good)
	if err := a.authenticate(r); err != nil {
		t.Errorf("matching iss/aud should pass, got %v", err)
	}

	// Wrong issuer.
	wrong := signHS256(t, secret, jwt.MapClaims{"iss": "other", "aud": "aud1", "exp": time.Now().Add(time.Hour).Unix()})
	r.Header.Set("Authorization", "Bearer "+wrong)
	if err := a.authenticate(r); err == nil {
		t.Error("wrong issuer should fail")
	}
}

func TestEdgeAuthUnknownModeFailsClosed(t *testing.T) {
	a := routeAuth{mode: "wat"}
	r := httptest.NewRequest("GET", "/", nil)
	if err := a.authenticate(r); err == nil {
		t.Error("unknown auth mode must fail closed")
	}
}
