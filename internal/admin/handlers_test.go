package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Josephfell/Jbalance/internal/controlplane"
)

// fakeStateProvider is a minimal StateProvider for tests that don't care
// about real backend/override/algorithm state — it just needs to satisfy
// the interface so the admin server's HTTP handlers can be exercised.
type fakeStateProvider struct{}

func (fakeStateProvider) Snapshot(context.Context) []controlplane.GroupState { return nil }

func (fakeStateProvider) SetOverride(context.Context, string, string, *int32, bool) error {
	return nil
}

func (fakeStateProvider) ClearOverride(context.Context, string, string) error { return nil }

func (fakeStateProvider) SetAlgorithm(context.Context, string, controlplane.Algorithm) error {
	return nil
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.json")
	store, generated, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	srv, err := NewServer(store, fakeStateProvider{}, OpenAuditLog(""), false, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	return srv, generated.Password
}

// getCSRFAndCookies performs a GET to path and returns the response's
// cookies and the CSRF token embedded in the rendered form, for use in a
// subsequent POST.
func getCSRFAndCookies(t *testing.T, handler http.Handler, path string) ([]*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	cookies := rec.Result().Cookies()
	var csrfToken string
	for _, c := range cookies {
		if c.Name == csrfCookieName {
			csrfToken = c.Value
		}
	}
	if csrfToken == "" {
		t.Fatalf("expected a CSRF cookie to be set by GET %s", path)
	}
	return cookies, csrfToken
}

func TestHandler_LoginPageRendersWithoutSession(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Sign in") {
		t.Error("expected login page body to contain 'Sign in'")
	}
}

func TestHandler_DashboardRedirectsWithoutSession(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected a redirect (303), got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("expected redirect to /login, got %q", loc)
	}
}

func TestHandler_LoginWithCorrectPasswordGrantsSession(t *testing.T) {
	srv, password := newTestServer(t)
	handler := srv.Handler()

	cookies, csrfToken := getCSRFAndCookies(t, handler, "/login")

	form := url.Values{"password": {password}, "csrf_token": {csrfToken}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected a redirect after successful login, got %d: %s", rec.Code, rec.Body.String())
	}

	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected a session cookie to be issued after successful login")
	}

	// Now use that session cookie to access the dashboard.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(sessionCookie)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 for dashboard with a valid session, got %d", rec2.Code)
	}
}

func TestHandler_LoginWithWrongPasswordFails(t *testing.T) {
	srv, _ := newTestServer(t)
	handler := srv.Handler()

	cookies, csrfToken := getCSRFAndCookies(t, handler, "/login")

	form := url.Values{"password": {"totally-wrong"}, "csrf_token": {csrfToken}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (re-rendered login page with error), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Incorrect password") {
		t.Error("expected an incorrect-password error message in the response body")
	}

	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			t.Error("expected no session cookie to be issued after a failed login")
		}
	}
}

func TestHandler_LoginWithoutCSRFTokenFails(t *testing.T) {
	srv, password := newTestServer(t)
	handler := srv.Handler()

	cookies, _ := getCSRFAndCookies(t, handler, "/login")

	// Omit the csrf_token form field entirely.
	form := url.Values{"password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			t.Error("expected no session cookie to be issued when CSRF token is missing")
		}
	}
}

func TestHandler_LoginRateLimitedAfterRepeatedFailures(t *testing.T) {
	srv, _ := newTestServer(t)
	handler := srv.Handler()

	var lastBody string
	for i := 0; i < 6; i++ {
		cookies, csrfToken := getCSRFAndCookies(t, handler, "/login")
		form := url.Values{"password": {"wrong"}, "csrf_token": {csrfToken}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "9.9.9.9:1234"
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		lastBody = rec.Body.String()
	}

	if !strings.Contains(lastBody, "Too many failed attempts") {
		t.Error("expected the login endpoint to rate-limit after repeated failures from the same IP")
	}
}

func TestHandler_PasswordChangeRequiresCorrectCurrentPassword(t *testing.T) {
	srv, password := newTestServer(t)
	handler := srv.Handler()

	sessionCookie := loginAndGetSession(t, handler, password)

	cookies, csrfToken := getAuthedCSRFAndCookies(t, handler, "/password", sessionCookie)
	form := url.Values{
		"current_password": {"wrong-current-password"},
		"new_password":     {"a-new-password-123"},
		"confirm_password": {"a-new-password-123"},
		"csrf_token":       {csrfToken},
	}
	req := httptest.NewRequest(http.MethodPost, "/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "Current password is incorrect") {
		t.Error("expected an error about the current password being incorrect")
	}
}

func TestHandler_PasswordChangeSucceedsAndInvalidatesOldSession(t *testing.T) {
	srv, password := newTestServer(t)
	handler := srv.Handler()

	sessionCookie := loginAndGetSession(t, handler, password)

	cookies, csrfToken := getAuthedCSRFAndCookies(t, handler, "/password", sessionCookie)
	newPassword := "a-brand-new-password-123"
	form := url.Values{
		"current_password": {password},
		"new_password":     {newPassword},
		"confirm_password": {newPassword},
		"csrf_token":       {csrfToken},
	}
	req := httptest.NewRequest(http.MethodPost, "/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "Password updated successfully") {
		t.Fatalf("expected a success message, got body: %s", rec.Body.String())
	}

	// The old session cookie should no longer work, since SetPassword
	// rotates the session secret.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(sessionCookie)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusSeeOther {
		t.Errorf("expected the old session to be invalidated (redirect to login), got %d", rec2.Code)
	}
}

func TestHandler_PasswordChangeRejectsMismatchedConfirmation(t *testing.T) {
	srv, password := newTestServer(t)
	handler := srv.Handler()

	sessionCookie := loginAndGetSession(t, handler, password)

	cookies, csrfToken := getAuthedCSRFAndCookies(t, handler, "/password", sessionCookie)
	form := url.Values{
		"current_password": {password},
		"new_password":     {"a-new-password-123"},
		"confirm_password": {"a-different-password-456"},
		"csrf_token":       {csrfToken},
	}
	req := httptest.NewRequest(http.MethodPost, "/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "do not match") {
		t.Error("expected an error about the confirmation not matching")
	}
}

func TestHandler_LogoutClearsSession(t *testing.T) {
	srv, password := newTestServer(t)
	handler := srv.Handler()

	sessionCookie := loginAndGetSession(t, handler, password)

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(sessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected a redirect after logout, got %d", rec.Code)
	}

	// Verify the dashboard is no longer accessible with the (now cleared)
	// cookie jar behaviour — simulate by checking the Set-Cookie clears it.
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("expected logout to clear the session cookie")
	}
}

// loginAndGetSession performs a full login flow and returns the resulting
// session cookie.
func loginAndGetSession(t *testing.T, handler http.Handler, password string) *http.Cookie {
	t.Helper()
	cookies, csrfToken := getCSRFAndCookies(t, handler, "/login")
	form := url.Values{"password": {password}, "csrf_token": {csrfToken}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("login did not produce a session cookie")
	return nil
}

// getAuthedCSRFAndCookies performs an authenticated GET to path (e.g.
// /password) and returns the resulting cookies plus the CSRF token,
// alongside the caller's session cookie so subsequent POSTs stay
// authenticated.
func getAuthedCSRFAndCookies(t *testing.T, handler http.Handler, path string, session *http.Cookie) ([]*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var csrfToken string
	cookies := []*http.Cookie{session}
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName {
			csrfToken = c.Value
			cookies = append(cookies, c)
		}
	}
	if csrfToken == "" {
		t.Fatalf("expected a CSRF cookie from authenticated GET %s", path)
	}
	return cookies, csrfToken
}

// spyStateProvider records calls made to SetOverride/ClearOverride/
// SetAlgorithm so handler tests can assert the right arguments were
// passed through from the submitted form.
type spyStateProvider struct {
	overrideCalls  []overrideCall
	clearCalls     []clearCall
	algorithmCalls []algorithmCall
	err            error
}

type overrideCall struct {
	group, address string
	weight         *int32
	drained        bool
}

type clearCall struct {
	group, address string
}

type algorithmCall struct {
	group     string
	algorithm controlplane.Algorithm
}

func (s *spyStateProvider) Snapshot(context.Context) []controlplane.GroupState { return nil }

func (s *spyStateProvider) SetOverride(_ context.Context, group, address string, weight *int32, drained bool) error {
	s.overrideCalls = append(s.overrideCalls, overrideCall{group, address, weight, drained})
	return s.err
}

func (s *spyStateProvider) ClearOverride(_ context.Context, group, address string) error {
	s.clearCalls = append(s.clearCalls, clearCall{group, address})
	return s.err
}

func (s *spyStateProvider) SetAlgorithm(_ context.Context, group string, algorithm controlplane.Algorithm) error {
	s.algorithmCalls = append(s.algorithmCalls, algorithmCall{group, algorithm})
	return s.err
}

func newTestServerWithSpy(t *testing.T) (*Server, string, *spyStateProvider) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.json")
	store, generated, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	spy := &spyStateProvider{}
	srv, err := NewServer(store, spy, OpenAuditLog(""), false, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	return srv, generated.Password, spy
}

func TestHandler_OverrideSubmit_Drain(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrfToken := getAuthedCSRFAndCookies(t, handler, "/", session)

	form := url.Values{
		"csrf_token": {csrfToken},
		"group":      {"g1"},
		"address":    {"a:1"},
		"action":     {"drain"},
	}
	req := httptest.NewRequest(http.MethodPost, "/override", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected a redirect, got %d", rec.Code)
	}
	if len(spy.overrideCalls) != 1 {
		t.Fatalf("expected 1 SetOverride call, got %d", len(spy.overrideCalls))
	}
	call := spy.overrideCalls[0]
	if call.group != "g1" || call.address != "a:1" || !call.drained || call.weight != nil {
		t.Errorf("unexpected SetOverride call: %+v", call)
	}
}

func TestHandler_OverrideSubmit_SetWeight(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrfToken := getAuthedCSRFAndCookies(t, handler, "/", session)

	form := url.Values{
		"csrf_token": {csrfToken},
		"group":      {"g1"},
		"address":    {"a:1"},
		"action":     {"set_weight"},
		"weight":     {"7"},
	}
	req := httptest.NewRequest(http.MethodPost, "/override", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(spy.overrideCalls) != 1 {
		t.Fatalf("expected 1 SetOverride call, got %d", len(spy.overrideCalls))
	}
	call := spy.overrideCalls[0]
	if call.weight == nil || *call.weight != 7 || call.drained {
		t.Errorf("unexpected SetOverride call: %+v", call)
	}
}

func TestHandler_OverrideSubmit_InvalidWeightIsRejected(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrfToken := getAuthedCSRFAndCookies(t, handler, "/", session)

	form := url.Values{
		"csrf_token": {csrfToken},
		"group":      {"g1"},
		"address":    {"a:1"},
		"action":     {"set_weight"},
		"weight":     {"not-a-number"},
	}
	req := httptest.NewRequest(http.MethodPost, "/override", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(spy.overrideCalls) != 0 {
		t.Errorf("expected no SetOverride call for an invalid weight, got %d", len(spy.overrideCalls))
	}
}

func TestHandler_OverrideSubmit_Clear(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrfToken := getAuthedCSRFAndCookies(t, handler, "/", session)

	form := url.Values{
		"csrf_token": {csrfToken},
		"group":      {"g1"},
		"address":    {"a:1"},
		"action":     {"clear"},
	}
	req := httptest.NewRequest(http.MethodPost, "/override", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(spy.clearCalls) != 1 || spy.clearCalls[0].group != "g1" || spy.clearCalls[0].address != "a:1" {
		t.Errorf("expected 1 ClearOverride call for g1/a:1, got %+v", spy.clearCalls)
	}
}

func TestHandler_OverrideSubmit_RequiresCSRF(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)

	// No csrf_token at all.
	form := url.Values{"group": {"g1"}, "address": {"a:1"}, "action": {"drain"}}
	req := httptest.NewRequest(http.MethodPost, "/override", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(spy.overrideCalls) != 0 {
		t.Errorf("expected no SetOverride call without a valid CSRF token, got %d", len(spy.overrideCalls))
	}
}

func TestHandler_AlgorithmSubmit_ValidAlgorithm(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrfToken := getAuthedCSRFAndCookies(t, handler, "/", session)

	form := url.Values{
		"csrf_token": {csrfToken},
		"group":      {"g1"},
		"algorithm":  {"least_connections"},
	}
	req := httptest.NewRequest(http.MethodPost, "/algorithm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected a redirect, got %d", rec.Code)
	}
	if len(spy.algorithmCalls) != 1 {
		t.Fatalf("expected 1 SetAlgorithm call, got %d", len(spy.algorithmCalls))
	}
	call := spy.algorithmCalls[0]
	if call.group != "g1" || call.algorithm != controlplane.AlgorithmLeastConnections {
		t.Errorf("unexpected SetAlgorithm call: %+v", call)
	}
}

func TestHandler_AlgorithmSubmit_RejectsInvalidAlgorithm(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrfToken := getAuthedCSRFAndCookies(t, handler, "/", session)

	form := url.Values{
		"csrf_token": {csrfToken},
		"group":      {"g1"},
		"algorithm":  {"not-a-real-algorithm"},
	}
	req := httptest.NewRequest(http.MethodPost, "/algorithm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(spy.algorithmCalls) != 0 {
		t.Errorf("expected no SetAlgorithm call for an invalid algorithm, got %d", len(spy.algorithmCalls))
	}
}

func TestHandler_AlgorithmSubmit_RequiresAuth(t *testing.T) {
	srv, _, spy := newTestServerWithSpy(t)
	handler := srv.Handler()

	form := url.Values{"group": {"g1"}, "algorithm": {"random"}}
	req := httptest.NewRequest(http.MethodPost, "/algorithm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected an unauthenticated request to be redirected, got %d", rec.Code)
	}
	if len(spy.algorithmCalls) != 0 {
		t.Errorf("expected no SetAlgorithm call without auth, got %d", len(spy.algorithmCalls))
	}
}
