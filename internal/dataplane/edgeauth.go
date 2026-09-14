package dataplane

import (
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	pb "github.com/Josephfell/Jbalance/proto"
)

// routeAuth is the data plane's local form of pb.RouteAuth: edge
// authentication enforced on a matched request before it is proxied.
type routeAuth struct {
	mode             string // "", "none", "api_key", "jwt"
	apiKeys          []string
	apiKeyHeader     string
	hmacSecret       []byte
	rsaPublicKeyPEM  []byte
	expectedIssuer   string
	expectedAudience string
}

// authFromProto converts a pb.RouteAuth (possibly nil) into the local form.
func authFromProto(a *pb.RouteAuth) routeAuth {
	if a == nil {
		return routeAuth{}
	}
	return routeAuth{
		mode:             a.Mode,
		apiKeys:          a.ApiKeys,
		apiKeyHeader:     a.ApiKeyHeader,
		hmacSecret:       []byte(a.HmacSecret),
		rsaPublicKeyPEM:  []byte(a.RsaPublicKeyPem),
		expectedIssuer:   a.ExpectedIssuer,
		expectedAudience: a.ExpectedAudience,
	}
}

// enabled reports whether this route enforces any authentication.
func (a routeAuth) enabled() bool {
	return a.mode == "api_key" || a.mode == "jwt"
}

// authenticate returns nil if the request satisfies the route's auth, or
// an error describing why it was rejected (turned into a 401 by the
// caller). An open route (mode none/empty) always passes.
func (a routeAuth) authenticate(r *http.Request) error {
	switch a.mode {
	case "", "none":
		return nil
	case "api_key":
		return a.checkAPIKey(r)
	case "jwt":
		return a.checkJWT(r)
	default:
		// Unknown mode: fail closed rather than silently allowing.
		return errors.New("unknown auth mode")
	}
}

func (a routeAuth) checkAPIKey(r *http.Request) error {
	header := a.apiKeyHeader
	if header == "" {
		header = "X-API-Key"
	}
	presented := r.Header.Get(header)
	if presented == "" {
		return errors.New("missing API key")
	}
	// Constant-time compare against each configured key; the loop always
	// runs to completion so timing doesn't reveal which key matched.
	ok := false
	for _, k := range a.apiKeys {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(k)) == 1 {
			ok = true
		}
	}
	if !ok {
		return errors.New("invalid API key")
	}
	return nil
}

func (a routeAuth) checkJWT(r *http.Request) error {
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
		return errors.New("missing bearer token")
	}
	tokenStr := authz[len(prefix):]

	keyFunc := func(token *jwt.Token) (any, error) {
		switch token.Method.(type) {
		case *jwt.SigningMethodHMAC:
			if len(a.hmacSecret) == 0 {
				return nil, errors.New("HMAC token presented but no hmac_secret configured")
			}
			return a.hmacSecret, nil
		case *jwt.SigningMethodRSA:
			if len(a.rsaPublicKeyPEM) == 0 {
				return nil, errors.New("RSA token presented but no rsa_public_key configured")
			}
			return parseRSAPublicKey(a.rsaPublicKeyPEM)
		default:
			return nil, errors.New("unexpected signing method")
		}
	}

	opts := []jwt.ParserOption{}
	if a.expectedIssuer != "" {
		opts = append(opts, jwt.WithIssuer(a.expectedIssuer))
	}
	if a.expectedAudience != "" {
		opts = append(opts, jwt.WithAudience(a.expectedAudience))
	}

	token, err := jwt.Parse(tokenStr, keyFunc, opts...)
	if err != nil {
		return err
	}
	if !token.Valid {
		return errors.New("invalid token")
	}
	return nil
}

// authChallenge returns an appropriate WWW-Authenticate header value for
// the route's auth mode, so a 401 tells the client how to authenticate.
func authChallenge(a routeAuth) string {
	if a.mode == "jwt" {
		return "Bearer"
	}
	header := a.apiKeyHeader
	if header == "" {
		header = "X-API-Key"
	}
	return "ApiKey header=" + header
}

// parseRSAPublicKey parses a PEM-encoded RSA public key (PKIX or PKCS1).
func parseRSAPublicKey(pemBytes []byte) (any, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid PEM for RSA public key")
	}
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		return pub, nil
	}
	return x509.ParsePKCS1PublicKey(block.Bytes)
}
