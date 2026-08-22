package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sort"
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
	FleetSnapshot() []controlplane.InstanceState
	Routes() []controlplane.Route
	SetRoutes(routes []controlplane.Route) error
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
	// dict lets the sidebar partial be invoked with an argument (which nav
	// item is active) despite html/template's block syntax only accepting
	// a single value — {{template "sidebar" (dict "Active" "fleet")}}
	// builds that single value as a map on the fly.
	funcs := template.FuncMap{
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				key, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %v is not a string", pairs[i])
				}
				m[key] = pairs[i+1]
			}
			return m, nil
		},
	}
	tmpl, err := template.New("admin").Funcs(funcs).Parse(templatesSource)
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
	mux.HandleFunc("GET /fleet", s.requireAuth(s.handleFleetPage))
	mux.HandleFunc("GET /routes", s.requireAuth(s.handleRoutesPage))
	mux.HandleFunc("POST /routes", s.requireAuth(s.handleRoutesSubmit))
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

// fleetRow is the display-ready form of controlplane.InstanceState — the
// template package has no registered funcs for formatting durations, so
// that's done here instead of in the template itself.
type fleetRow struct {
	InstanceID       string
	Group            string
	Connected        bool
	ConnectedFor     string
	LastHealthReport string
	ReportedBackends int
	HasHealthReport  bool
}

// routeRow is the display/edit form of controlplane.Route, with Methods
// flattened to a comma-separated string for a single text input rather
// than a multi-select — L7 routing methods lists are short (GET, POST,
// etc), so a plain "GET, POST" text field is simpler than any JS-free
// multi-select control would be.
type routeRow struct {
	Order       int
	Host        string
	PathPrefix  string
	Methods     string
	TargetGroup string
	Name        string
}

func routesToRows(routes []controlplane.Route) []routeRow {
	rows := make([]routeRow, len(routes))
	for i, r := range routes {
		rows[i] = routeRow{
			Order:       i,
			Host:        r.Host,
			PathPrefix:  r.PathPrefix,
			Methods:     strings.Join(r.Methods, ", "),
			TargetGroup: r.TargetGroup,
			Name:        r.Name,
		}
	}
	return rows
}

func (s *Server) handleRoutesPage(w http.ResponseWriter, r *http.Request) {
	groups := s.state.Snapshot(r.Context())
	groupNames := make([]string, len(groups))
	for i, g := range groups {
		groupNames[i] = g.Group
	}
	routes := s.state.Routes()

	groupsJSON, err := json.Marshal(groupNames)
	if err != nil {
		groupsJSON = []byte("[]") // groupNames is always a []string of plain names — marshal cannot realistically fail
	}

	s.render(w, "routes", map[string]any{
		"Rows": routesToRows(routes),
		// Groups is used by the Go template to render each row's <select>
		// server-side; GroupsJSON feeds the same option list to the
		// client-side "+ Add rule" script, which builds new rows without
		// a round trip. template.JS marks it as already-safe JS so
		// html/template doesn't further HTML-escape valid JSON syntax
		// (e.g. escaping quotes) inside the inline <script> block.
		"Groups":     groupNames,
		"GroupsJSON": template.JS(groupsJSON), //nolint:gosec // groupNames originates from Snapshot()'s own group names, not user input
		"CSRFToken":  s.newCSRFToken(w),
		"NextOrder":  len(routes),
	})
}

// handleRoutesSubmit rebuilds the entire route table from the submitted
// form and saves it in one call — routing order is part of a rule's
// meaning (first match wins), so the table is edited as a whole ordered
// list rather than through individual add/remove endpoints that could
// leave order ambiguous between concurrent edits.
//
// Every row's fields are submitted as same-named, same-length arrays
// (r.Form["host"][i] belongs to the same row as r.Form["order"][i], etc)
// — relying on Go's http.Request preserving multi-value form fields in
// the order the browser sent them, which for a plain sequentially
// rendered form matches row order. A "action" value of "delete" (rather
// than a checkbox, which would omit unchecked boxes and break positional
// alignment between the arrays) removes that row instead of keeping it.
func (s *Server) handleRoutesSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		http.Redirect(w, r, "/routes", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/routes", http.StatusSeeOther)
		return
	}

	orders := r.Form["order"]
	hosts := r.Form["host"]
	pathPrefixes := r.Form["path_prefix"]
	methodsList := r.Form["methods"]
	targetGroups := r.Form["target_group"]
	names := r.Form["name"]
	actions := r.Form["action"]

	n := len(hosts)
	type indexed struct {
		order int
		route controlplane.Route
	}
	kept := make([]indexed, 0, n)
	for i := 0; i < n; i++ {
		if i < len(actions) && actions[i] == "delete" {
			continue
		}
		targetGroup := valueAt(targetGroups, i)
		if targetGroup == "" {
			continue // a row with no target group is meaningless — silently dropped rather than saved as broken config
		}
		order, _ := strconv.Atoi(valueAt(orders, i))
		kept = append(kept, indexed{
			order: order,
			route: controlplane.Route{
				Host:        valueAt(hosts, i),
				PathPrefix:  valueAt(pathPrefixes, i),
				Methods:     splitMethods(valueAt(methodsList, i)),
				TargetGroup: targetGroup,
				Name:        valueAt(names, i),
			},
		})
	}
	sort.SliceStable(kept, func(a, b int) bool { return kept[a].order < kept[b].order })

	routes := make([]controlplane.Route, len(kept))
	for i, k := range kept {
		routes[i] = k.route
	}

	if err := s.state.SetRoutes(routes); err != nil {
		s.audit.Record(AuditRoutesChanged, s.clientIP(r), "route table update failed: "+err.Error())
	} else {
		s.audit.Record(AuditRoutesChanged, s.clientIP(r), fmt.Sprintf("route table updated (%d rule(s))", len(routes)))
	}

	http.Redirect(w, r, "/routes", http.StatusSeeOther)
}

// valueAt returns s[i] if i is in range, or "" otherwise — form field
// arrays can end up shorter than expected if a browser ever omits a
// disabled/malformed field, and a missing value should be treated as
// empty rather than panicking the request.
func valueAt(s []string, i int) string {
	if i < 0 || i >= len(s) {
		return ""
	}
	return strings.TrimSpace(s[i])
}

// splitMethods parses a comma-separated methods field into a clean list,
// dropping empty entries from stray commas/whitespace. Returns nil (not
// an empty non-nil slice) for an empty input, matching Route.Methods'
// "empty means any method" contract.
func splitMethods(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToUpper(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) handleFleetPage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	instances := s.state.FleetSnapshot()
	rows := make([]fleetRow, 0, len(instances))
	for _, inst := range instances {
		row := fleetRow{
			InstanceID:       inst.InstanceID,
			Group:            inst.Group,
			Connected:        inst.Connected,
			ReportedBackends: inst.ReportedBackends,
		}
		if inst.Connected {
			row.ConnectedFor = formatDuration(now.Sub(inst.ConnectedSince)) + " ago"
		} else {
			row.ConnectedFor = "disconnected " + formatDuration(now.Sub(inst.ConnectedSince)) + " ago"
		}
		if !inst.LastHealthReport.IsZero() {
			row.HasHealthReport = true
			row.LastHealthReport = formatDuration(now.Sub(inst.LastHealthReport)) + " ago"
		}
		rows = append(rows, row)
	}
	s.render(w, "fleet", map[string]any{"Instances": rows})
}

// formatDuration renders d as a short, human-scale duration ("4s", "12m",
// "3h", "2d") — coarser than time.Duration.String() on purpose, since the
// Fleet view cares about "roughly how long", not precise sub-second
// values.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
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
