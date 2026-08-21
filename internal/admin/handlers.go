package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"html/template"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Josephfell/Jbalance/internal/controlplane"
)

// StateProvider is implemented by anything that can report current
// backend group state for display, and apply manual per-backend
// weight/drain overrides — satisfied by *controlplane.Server.
type StateProvider interface {
	Snapshot(ctx context.Context) []controlplane.GroupState
	SetOverride(ctx context.Context, group, address string, weight *int32, drained bool) error
	ClearOverride(ctx context.Context, group, address string) error
	SetAlgorithm(ctx context.Context, group string, algorithm controlplane.Algorithm) error
}

// Server serves the admin web management interface: a password-protected
// dashboard showing live backend group state, with the ability to change
// the admin password from the UI. All state (password hash, session
// secret, audit log) lives in local files — no external database.
type Server struct {
	store   *Store
	state   StateProvider
	audit   *AuditLog
	limiter *loginLimiter
	// secureCookies controls the Secure flag on session cookies. Disable
	// only for plain-HTTP local development; the web UI should otherwise
	// always be served over TLS (see -admin-tls-cert/-admin-tls-key).
	secureCookies bool
	// trustForwardedFor controls whether X-Forwarded-For is used to
	// determine the client IP for rate limiting. Only enable this when the
	// admin server sits behind a trusted reverse proxy that sets this
	// header itself — otherwise a client can bypass rate limiting entirely
	// by setting it directly.
	trustForwardedFor bool

	tmpl *template.Template
}

// NewServer creates an admin web server backed by store, displaying state
// from stateProvider and recording events to auditLog. Set
// trustForwardedFor only when running behind a trusted reverse proxy that
// itself sets X-Forwarded-For.
func NewServer(store *Store, stateProvider StateProvider, auditLog *AuditLog, secureCookies, trustForwardedFor bool) (*Server, error) {
	tmpl, err := template.New("admin").Parse(templatesSource)
	if err != nil {
		return nil, err
	}

	return &Server{
		store:             store,
		state:             stateProvider,
		audit:             auditLog,
		limiter:           newLoginLimiter(5, 5*time.Minute, 15*time.Minute),
		secureCookies:     secureCookies,
		trustForwardedFor: trustForwardedFor,
		tmpl:              tmpl,
	}, nil
}

// Handler returns the http.Handler serving the admin UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("GET /", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("POST /override", s.requireAuth(s.handleOverrideSubmit))
	mux.HandleFunc("POST /algorithm", s.requireAuth(s.handleAlgorithmSubmit))
	mux.HandleFunc("GET /password", s.requireAuth(s.handlePasswordPage))
	mux.HandleFunc("POST /password", s.requireAuth(s.handlePasswordSubmit))
	mux.HandleFunc("GET /audit", s.requireAuth(s.handleAuditPage))
	return mux
}

// requireAuth wraps a handler so it redirects to /login when the request
// has no valid session cookie.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validSession(r, s.store.SessionSecret()) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if validSession(r, s.store.SessionSecret()) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", map[string]any{"CSRFToken": s.newCSRFToken(w)})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)

	if !s.limiter.Allowed(ip) {
		s.audit.Record(AuditLoginRateLimited, ip, "login attempt blocked by rate limiting")
		s.render(w, "login", map[string]any{
			"Error":     "Too many failed attempts. Try again in a few minutes.",
			"CSRFToken": s.newCSRFToken(w),
		})
		return
	}

	if !s.checkCSRF(r) {
		s.render(w, "login", map[string]any{"Error": "Session expired, please try again.", "CSRFToken": s.newCSRFToken(w)})
		return
	}

	password := r.FormValue("password")
	if !s.store.VerifyPassword(password) {
		s.limiter.RecordFailure(ip)
		s.audit.Record(AuditLoginFailure, ip, "incorrect password")
		s.render(w, "login", map[string]any{"Error": "Incorrect password.", "CSRFToken": s.newCSRFToken(w)})
		return
	}

	s.limiter.RecordSuccess(ip)
	s.audit.Record(AuditLoginSuccess, ip, "signed in")
	issueSession(w, s.store.SessionSecret(), s.secureCookies)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.audit.Record(AuditLogout, s.clientIP(r), "signed out")
	clearSession(w, s.secureCookies)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, "dashboard", map[string]any{
		"Groups":          s.state.Snapshot(r.Context()),
		"CSRFToken":       s.newCSRFToken(w),
		"ValidAlgorithms": controlplane.ValidAlgorithms,
	})
}

// handleOverrideSubmit handles setting or clearing a manual weight/drain
// override for one backend, then redirects back to the dashboard. Uses a
// single endpoint with an "action" field (set_weight / drain / undrain /
// clear) rather than separate routes per action, since they all operate
// on the same group+address target and share the same auth/CSRF handling.
func (s *Server) handleOverrideSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	group := r.FormValue("group")
	address := r.FormValue("address")
	action := r.FormValue("action")

	if group == "" || address == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	var err error
	switch action {
	case "drain":
		err = s.state.SetOverride(r.Context(), group, address, nil, true)
		if err == nil {
			s.audit.Record(AuditOverrideChanged, s.clientIP(r), "drained backend "+address+" in group "+group)
		}
	case "set_weight":
		weightStr := r.FormValue("weight")
		w64, parseErr := strconv.ParseInt(weightStr, 10, 32)
		if parseErr != nil || w64 < 0 {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		weight := int32(w64)
		err = s.state.SetOverride(r.Context(), group, address, &weight, false)
		if err == nil {
			s.audit.Record(AuditOverrideChanged, s.clientIP(r), "set weight override for "+address+" in group "+group+" to "+weightStr)
		}
	case "clear":
		err = s.state.ClearOverride(r.Context(), group, address)
		if err == nil {
			s.audit.Record(AuditOverrideChanged, s.clientIP(r), "cleared override for "+address+" in group "+group)
		}
	}

	if err != nil {
		// Best-effort UI: log server-side, still redirect back rather than
		// showing a raw error page for what's a minor operational action.
		s.audit.Record(AuditOverrideChanged, s.clientIP(r), "override change failed: "+err.Error())
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleAlgorithmSubmit handles switching a group's load-balancing
// algorithm, then redirects back to the dashboard.
func (s *Server) handleAlgorithmSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	group := r.FormValue("group")
	algorithm := controlplane.Algorithm(r.FormValue("algorithm"))

	if group == "" || !controlplane.IsValidAlgorithm(algorithm) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if err := s.state.SetAlgorithm(r.Context(), group, algorithm); err != nil {
		s.audit.Record(AuditAlgorithmChanged, s.clientIP(r), "algorithm change failed for group "+group+": "+err.Error())
	} else {
		s.audit.Record(AuditAlgorithmChanged, s.clientIP(r), "changed algorithm for group "+group+" to "+string(algorithm))
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleAuditPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "audit", map[string]any{"Entries": s.audit.Recent(200)})
}

func (s *Server) handlePasswordPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "password", map[string]any{"CSRFToken": s.newCSRFToken(w)})
}

func (s *Server) handlePasswordSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.render(w, "password", map[string]any{"Error": "Session expired, please try again.", "CSRFToken": s.newCSRFToken(w)})
		return
	}

	current := r.FormValue("current_password")
	newPassword := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	if !s.store.VerifyPassword(current) {
		s.render(w, "password", map[string]any{"Error": "Current password is incorrect.", "CSRFToken": s.newCSRFToken(w)})
		return
	}
	if len(newPassword) < 12 {
		s.render(w, "password", map[string]any{"Error": "New password must be at least 12 characters.", "CSRFToken": s.newCSRFToken(w)})
		return
	}
	if newPassword != confirm {
		s.render(w, "password", map[string]any{"Error": "New password and confirmation do not match.", "CSRFToken": s.newCSRFToken(w)})
		return
	}

	if err := s.store.SetPassword(newPassword); err != nil {
		s.render(w, "password", map[string]any{"Error": "Failed to save new password. Check container logs.", "CSRFToken": s.newCSRFToken(w)})
		return
	}

	s.audit.Record(AuditPasswordChanged, s.clientIP(r), "password changed via web UI")

	// Changing the password rotates the session secret (see Store.SetPassword),
	// which invalidates every existing session including this one — issue a
	// fresh session so the user isn't immediately logged out.
	clearSession(w, s.secureCookies)
	issueSession(w, s.store.SessionSecret(), s.secureCookies)
	s.render(w, "password", map[string]any{"Success": true, "CSRFToken": s.newCSRFToken(w)})
}

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "internal error rendering page", http.StatusInternalServerError)
	}
}

// ── CSRF ────────────────────────────────────────────────────────────────
//
// A simple double-submit-cookie CSRF scheme: a random token is set as a
// cookie and also embedded in the form; on submit we check they match.
// No server-side token store needed (consistent with "no separate
// database, all local").

const csrfCookieName = "lb_admin_csrf"

func (s *Server) newCSRFToken(w http.ResponseWriter) string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf) // crypto/rand.Read only errors if the OS RNG is broken; nothing sensible to do but proceed
	token := base64.RawURLEncoding.EncodeToString(buf)

	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int((10 * time.Minute).Seconds()),
	})
	return token
}

func (s *Server) checkCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	formToken := r.FormValue("csrf_token")
	return formToken != "" && formToken == cookie.Value
}

// clientIP extracts the request's originating IP for rate-limiting
// purposes. Only trusts X-Forwarded-For when s.trustForwardedFor is set
// (i.e. a trusted reverse proxy sits in front of this server) — otherwise
// a client could bypass rate limiting by setting the header itself.
func (s *Server) clientIP(r *http.Request) string {
	if s.trustForwardedFor {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if ip := strings.TrimSpace(strings.Split(fwd, ",")[0]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
