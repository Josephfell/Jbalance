package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIssueAndValidateSession(t *testing.T) {
	secret := "test-secret"

	rec := httptest.NewRecorder()
	issueSession(rec, secret, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}

	if !validSession(req, secret) {
		t.Error("expected a freshly issued session to be valid")
	}
}

func TestValidSession_RejectsWrongSecret(t *testing.T) {
	rec := httptest.NewRecorder()
	issueSession(rec, "secret-a", false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}

	if validSession(req, "secret-b") {
		t.Error("expected a session signed with a different secret to be invalid")
	}
}

func TestValidSession_RejectsMissingCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if validSession(req, "any-secret") {
		t.Error("expected a request with no session cookie to be invalid")
	}
}

func TestValidSession_RejectsMalformedCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "not-a-valid-format"})
	if validSession(req, "any-secret") {
		t.Error("expected a malformed session cookie to be invalid")
	}
}

func TestValidSession_RejectsExpiredSession(t *testing.T) {
	secret := "test-secret"
	expiredExpiry := time.Now().Add(-time.Hour).Unix()
	sig := signSession(secret, expiredExpiry)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: fmt.Sprintf("%d.%s", expiredExpiry, sig),
	})

	if validSession(req, secret) {
		t.Error("expected an expired session to be invalid")
	}
}

func TestClearSession_RemovesCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	clearSession(rec, false)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected exactly 1 cookie to be set, got %d", len(cookies))
	}
	if cookies[0].MaxAge >= 0 {
		t.Errorf("expected clearSession to set a negative MaxAge, got %d", cookies[0].MaxAge)
	}
}
