package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Josephfell/Jbalance/internal/controlplane"
)

// fakeStateProvider is a minimal StateProvider for tests that don't care
// about real backend/override/algorithm state — it just needs to satisfy
// the interface so the admin server's HTTP handlers can be exercised.
type fakeStateProvider struct{}

func (fakeStateProvider) Snapshot(context.Context) []controlplane.GroupState { return nil }

func (fakeStateProvider) FleetSnapshot() []controlplane.InstanceState { return nil }

func (fakeStateProvider) Routes() []controlplane.Route { return nil }

func (fakeStateProvider) SetRoutes([]controlplane.Route) error { return nil }

func (fakeStateProvider) SetSticky(context.Context, string, controlplane.StickyConfig) error {
	return nil
}

func (fakeStateProvider) MetricsSnapshot() []controlplane.GroupMetricsSnapshot { return nil }

func (fakeStateProvider) MetricsHistory(int) []controlplane.HistoryPoint { return nil }

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
	fleet          []controlplane.InstanceState
	routes         []controlplane.Route
	routesSaved    []controlplane.Route
	stickyCalls    []stickyCall
	metrics        []controlplane.GroupMetricsSnapshot
	history        []controlplane.HistoryPoint
}

type stickyCall struct {
	group string
	cfg   controlplane.StickyConfig
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

func (s *spyStateProvider) FleetSnapshot() []controlplane.InstanceState { return s.fleet }

func (s *spyStateProvider) Routes() []controlplane.Route { return s.routes }

func (s *spyStateProvider) SetRoutes(routes []controlplane.Route) error {
	s.routesSaved = routes
	return s.err
}

func (s *spyStateProvider) SetSticky(_ context.Context, group string, cfg controlplane.StickyConfig) error {
	s.stickyCalls = append(s.stickyCalls, stickyCall{group, cfg})
	return s.err
}

func (s *spyStateProvider) MetricsSnapshot() []controlplane.GroupMetricsSnapshot { return s.metrics }

func (s *spyStateProvider) MetricsHistory(int) []controlplane.HistoryPoint { return s.history }

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

func TestHandler_FleetPage_RendersEmptyState(t *testing.T) {
	srv, password := newTestServer(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)

	req := httptest.NewRequest(http.MethodGet, "/fleet", nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No data plane instances have connected yet") {
		t.Error("expected the empty-state message when no instances are reported")
	}
}

func TestHandler_FleetPage_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/fleet", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected an unauthenticated request to be redirected, got %d", rec.Code)
	}
}

func TestHandler_FleetPage_RendersInstanceRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.json")
	store, generated, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	spy := &spyStateProvider{
		fleet: []controlplane.InstanceState{
			{InstanceID: "dp-1", Group: "web-tier", Connected: true, ConnectedSince: time.Now().Add(-90 * time.Second)},
			{InstanceID: "dp-2", Group: "api-tier", Connected: false, ConnectedSince: time.Now().Add(-3 * time.Hour)},
		},
	}
	srv, err := NewServer(store, spy, OpenAuditLog(""), false, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, generated.Password)

	req := httptest.NewRequest(http.MethodGet, "/fleet", nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "dp-1") || !strings.Contains(body, "web-tier") {
		t.Errorf("expected the connected instance to be rendered, got: %s", body)
	}
	if !strings.Contains(body, "dp-2") || !strings.Contains(body, "api-tier") {
		t.Errorf("expected the disconnected instance to be rendered, got: %s", body)
	}
	if !strings.Contains(body, "Streaming") {
		t.Error("expected the connected instance to show as Streaming")
	}
	if !strings.Contains(body, "Disconnected") {
		t.Error("expected the disconnected instance to show as Disconnected")
	}
	if !strings.Contains(body, "no report yet") {
		t.Error("expected instances with no health report to show 'no report yet'")
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m"},
		{45 * time.Minute, "45m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	}
	for _, tc := range cases {
		if got := formatDuration(tc.d); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestHandler_RoutesPage_RendersExistingRules(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	spy.routes = []controlplane.Route{
		{Host: "acme.io", PathPrefix: "/api/", Methods: []string{"GET", "POST"}, TargetGroup: "api-tier", Name: "api"},
	}
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)

	req := httptest.NewRequest(http.MethodGet, "/routes", nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="acme.io"`) {
		t.Error("expected the existing route's host to be rendered")
	}
	if !strings.Contains(body, `value="/api/"`) {
		t.Error("expected the existing route's path prefix to be rendered")
	}
	if !strings.Contains(body, "GET, POST") {
		t.Error("expected the existing route's methods to be rendered as a comma-separated list")
	}
}

func TestHandler_RoutesPage_RequiresAuth(t *testing.T) {
	srv, _, _ := newTestServerWithSpy(t)
	req := httptest.NewRequest(http.MethodGet, "/routes", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected an unauthenticated request to be redirected, got %d", rec.Code)
	}
}

func TestHandler_RoutesSubmit_SavesOrderedRoutes(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrf := getAuthedCSRFAndCookies(t, handler, "/routes", session)

	form := url.Values{
		"csrf_token":   {csrf},
		"order":        {"0", "1"},
		"name":         {"static", "default"},
		"host":         {"acme.io", ""},
		"path_prefix":  {"/static/", "/"},
		"methods":      {"", "GET, post"},
		"target_group": {"static-edge", "web-tier"},
		"action":       {"keep", "keep"},
	}
	req := httptest.NewRequest(http.MethodPost, "/routes", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected a redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(spy.routesSaved) != 2 {
		t.Fatalf("expected 2 saved routes, got %d: %+v", len(spy.routesSaved), spy.routesSaved)
	}
	if spy.routesSaved[0].TargetGroup != "static-edge" || spy.routesSaved[0].Host != "acme.io" {
		t.Errorf("unexpected first saved route: %+v", spy.routesSaved[0])
	}
	if spy.routesSaved[1].TargetGroup != "web-tier" {
		t.Errorf("unexpected second saved route: %+v", spy.routesSaved[1])
	}
	if len(spy.routesSaved[1].Methods) != 2 || spy.routesSaved[1].Methods[0] != "GET" || spy.routesSaved[1].Methods[1] != "POST" {
		t.Errorf("expected methods to be parsed and normalised to uppercase, got %+v", spy.routesSaved[1].Methods)
	}
}

func TestHandler_RoutesSubmit_DeleteActionRemovesRow(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrf := getAuthedCSRFAndCookies(t, handler, "/routes", session)

	form := url.Values{
		"csrf_token":   {csrf},
		"order":        {"0", "1"},
		"name":         {"keep-me", "delete-me"},
		"host":         {"", ""},
		"path_prefix":  {"/", "/gone/"},
		"methods":      {"", ""},
		"target_group": {"web-tier", "api-tier"},
		"action":       {"keep", "delete"},
	}
	req := httptest.NewRequest(http.MethodPost, "/routes", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(spy.routesSaved) != 1 || spy.routesSaved[0].Name != "keep-me" {
		t.Errorf("expected the deleted row to be excluded, got %+v", spy.routesSaved)
	}
}

func TestHandler_RoutesSubmit_RowWithoutTargetGroupIsDropped(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrf := getAuthedCSRFAndCookies(t, handler, "/routes", session)

	form := url.Values{
		"csrf_token":   {csrf},
		"order":        {"0"},
		"name":         {"incomplete"},
		"host":         {""},
		"path_prefix":  {"/"},
		"methods":      {""},
		"target_group": {""}, // no target selected
		"action":       {"keep"},
	}
	req := httptest.NewRequest(http.MethodPost, "/routes", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(spy.routesSaved) != 0 {
		t.Errorf("expected a row with no target group to be dropped, got %+v", spy.routesSaved)
	}
}

func TestHandler_RoutesSubmit_RequiresCSRF(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)

	form := url.Values{
		"order":        {"0"},
		"target_group": {"web-tier"},
		"action":       {"keep"},
	}
	req := httptest.NewRequest(http.MethodPost, "/routes", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(spy.routesSaved) != 0 {
		t.Errorf("expected no SetRoutes call without a valid CSRF token, got %+v", spy.routesSaved)
	}
}

func TestSplitMethods(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"GET", []string{"GET"}},
		{"get, post", []string{"GET", "POST"}},
		{" GET ,  , POST ", []string{"GET", "POST"}},
		{",,,", nil},
	}
	for _, tc := range cases {
		got := splitMethods(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitMethods(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitMethods(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestValueAt(t *testing.T) {
	s := []string{"a", " b "}
	if valueAt(s, 0) != "a" {
		t.Errorf("expected valueAt(s, 0) = %q, got %q", "a", valueAt(s, 0))
	}
	if valueAt(s, 1) != "b" {
		t.Errorf("expected valueAt to trim whitespace, got %q", valueAt(s, 1))
	}
	if valueAt(s, 5) != "" {
		t.Errorf("expected an out-of-range index to return empty string, got %q", valueAt(s, 5))
	}
	if valueAt(s, -1) != "" {
		t.Errorf("expected a negative index to return empty string, got %q", valueAt(s, -1))
	}
}

// TestHandler_LoginPage_ReusesExistingCSRFCookieAcrossConcurrentRequests
// is a regression test for a real bug: handleLoginPage used to mint a
// brand new CSRF cookie on every single GET /login, unconditionally. In
// a browser, any other request sharing the same cookie jar while the
// login page is open — a favicon fetch, a second tab, a redirect from
// hitting a protected route while logged out — would silently overwrite
// the cookie the visible page's <form> still references, so submitting
// that (now-stale) form failed with "Session expired" even though the
// user never actually left or reloaded the page.
func TestHandler_LoginPage_ReusesExistingCSRFCookieAcrossConcurrentRequests(t *testing.T) {
	srv, password := newTestServer(t)
	handler := srv.Handler()

	// First "tab": load the login page and capture its form token.
	cookies, formToken := getCSRFAndCookies(t, handler, "/login")

	// Simulate an unrelated background request sharing the same cookie
	// jar — e.g. the browser redirecting a stray request to a
	// protected route back to GET /login.
	req2 := httptest.NewRequest(http.MethodGet, "/login", nil)
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	// The cookie must not have been rotated: submitting the ORIGINAL
	// form's token, with the ORIGINAL cookie, must still succeed.
	form := url.Values{"password": {password}, "csrf_token": {formToken}}
	req3 := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req3.AddCookie(c)
	}
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusSeeOther {
		t.Fatalf("expected the original form token to still be valid after a concurrent GET /login, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

func TestCSRFToken_ReusesExistingCookieValue(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "existing-token-value"})
	rec := httptest.NewRecorder()

	got := srv.csrfToken(rec, req)
	if got != "existing-token-value" {
		t.Errorf("expected csrfToken to reuse the existing cookie value, got %q", got)
	}
	// Must not have set a new cookie, since a valid one was already present.
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName {
			t.Error("expected csrfToken not to re-set the cookie when a valid one already exists on the request")
		}
	}
}

func TestCSRFToken_MintsNewTokenWhenNoneExists(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()

	got := srv.csrfToken(rec, req)
	if got == "" {
		t.Fatal("expected a freshly minted, non-empty token")
	}

	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName && c.Value == got {
			found = true
		}
	}
	if !found {
		t.Error("expected csrfToken to set a cookie matching the returned token when none existed")
	}
}

func TestHandler_StickySubmit_EnablesWithCustomCookieAndTTL(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrf := getAuthedCSRFAndCookies(t, handler, "/", session)

	form := url.Values{
		"csrf_token":  {csrf},
		"group":       {"g1"},
		"enabled":     {"on"},
		"cookie_name": {"my_cookie"},
		"ttl_minutes": {"45"},
	}
	req := httptest.NewRequest(http.MethodPost, "/sticky", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected a redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(spy.stickyCalls) != 1 {
		t.Fatalf("expected 1 SetSticky call, got %d", len(spy.stickyCalls))
	}
	call := spy.stickyCalls[0]
	if call.group != "g1" || !call.cfg.Enabled || call.cfg.CookieName != "my_cookie" || call.cfg.TTL != 45*time.Minute {
		t.Errorf("unexpected SetSticky call: %+v", call)
	}
}

func TestHandler_StickySubmit_UncheckedCheckboxDisables(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)
	cookies, csrf := getAuthedCSRFAndCookies(t, handler, "/", session)

	// "enabled" omitted entirely — mirrors an unchecked HTML checkbox.
	form := url.Values{
		"csrf_token":  {csrf},
		"group":       {"g1"},
		"cookie_name": {""},
		"ttl_minutes": {""},
	}
	req := httptest.NewRequest(http.MethodPost, "/sticky", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(spy.stickyCalls) != 1 || spy.stickyCalls[0].cfg.Enabled {
		t.Errorf("expected sticky sessions to be disabled when 'enabled' is absent from the form, got %+v", spy.stickyCalls)
	}
}

func TestHandler_StickySubmit_RequiresCSRF(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)

	form := url.Values{"group": {"g1"}, "enabled": {"on"}}
	req := httptest.NewRequest(http.MethodPost, "/sticky", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(spy.stickyCalls) != 0 {
		t.Errorf("expected no SetSticky call without a valid CSRF token, got %+v", spy.stickyCalls)
	}
}

func TestHandler_StickySubmit_RequiresAuth(t *testing.T) {
	srv, _, spy := newTestServerWithSpy(t)
	handler := srv.Handler()

	form := url.Values{"group": {"g1"}, "enabled": {"on"}}
	req := httptest.NewRequest(http.MethodPost, "/sticky", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected an unauthenticated request to be redirected, got %d", rec.Code)
	}
	if len(spy.stickyCalls) != 0 {
		t.Errorf("expected no SetSticky call without auth, got %+v", spy.stickyCalls)
	}
}

func TestHandler_MetricsJSON_RendersHistory(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	now := time.Now()
	spy.history = []controlplane.HistoryPoint{
		{Time: now, RequestsDelta: 12, Errors5xxDelta: 1, ActiveConns: 4, AvgDurationMs: 8.5},
	}
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)

	req := httptest.NewRequest(http.MethodGet, "/metrics.json", nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("expected JSON content type, got %q", rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"req":12`) || !strings.Contains(body, `"err":1`) || !strings.Contains(body, `"active":4`) {
		t.Errorf("expected the rendered JSON to reflect the history point, got: %s", body)
	}
}

func TestHandler_MetricsJSON_RequiresAuth(t *testing.T) {
	srv, _, _ := newTestServerWithSpy(t)
	req := httptest.NewRequest(http.MethodGet, "/metrics.json", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected an unauthenticated request to be redirected, got %d", rec.Code)
	}
}

func TestHandler_Dashboard_RendersMetricsTable(t *testing.T) {
	srv, password, spy := newTestServerWithSpy(t)
	spy.metrics = []controlplane.GroupMetricsSnapshot{
		{Group: "web-tier", RequestsTotal: 500, Errors5xxTotal: 3, ActiveConnections: 7, AvgDurationMs: 12.3, ReportingInstances: 2},
	}
	handler := srv.Handler()
	session := loginAndGetSession(t, handler, password)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "web-tier") || !strings.Contains(body, "500") {
		t.Errorf("expected the metrics table to render the group's traffic summary, got: %s", body)
	}
}
