package dataplane

import (
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	pb "github.com/Josephfell/Jbalance/proto"
)

// route is the data plane's local form of pb.Route, with matching logic
// attached directly (mirrors controlplane.Route, kept as a separate type
// so this package doesn't need to reach into the control plane package).
type route struct {
	host        string
	pathPrefix  string
	methods     []string
	targetGroup string
	// split, when non-empty, is a weighted set of destination groups: a
	// matching request is sent to one of them chosen by weight, rather
	// than always to targetGroup. Used for canary / traffic-splitting.
	split []routeTarget
	// rewrite holds optional request/response transformations applied
	// when this rule matches. Zero value is a no-op.
	rewrite routeRewrite
	// auth, when enabled, requires the matched request to authenticate
	// (API key or JWT) before it is proxied. Zero value = open route.
	auth routeAuth
	// headerMatches and queryMatches are extra request conditions that
	// must ALL hold (AND) on top of host/path/method. Nil = no condition.
	headerMatches []kvMatch
	queryMatches  []kvMatch
}

// kvMatch is one name/value condition on a request header or query
// parameter. An empty value matches on mere presence of the name;
// otherwise the value must be present and equal.
type kvMatch struct {
	name  string
	value string
}

// routeRewrite is the data plane's local form of pb.RouteRewrite.
type routeRewrite struct {
	setRequestHeaders     map[string]string
	removeRequestHeaders  []string
	setResponseHeaders    map[string]string
	removeResponseHeaders []string
	stripPathPrefix       string
	addPathPrefix         string
}

// isZero reports whether the rewrite has no effect.
func (rw routeRewrite) isZero() bool {
	return len(rw.setRequestHeaders) == 0 &&
		len(rw.removeRequestHeaders) == 0 &&
		len(rw.setResponseHeaders) == 0 &&
		len(rw.removeResponseHeaders) == 0 &&
		rw.stripPathPrefix == "" &&
		rw.addPathPrefix == ""
}

// routeTarget is one weighted destination of a split route.
type routeTarget struct {
	group  string
	weight int32
}

func (r route) matches(host, path, method string, header http.Header, query url.Values) bool {
	if r.host != "" && r.host != "*" && !strings.EqualFold(r.host, host) {
		return false
	}
	if r.pathPrefix != "" && r.pathPrefix != "/" && !strings.HasPrefix(path, r.pathPrefix) {
		return false
	}
	if len(r.methods) > 0 {
		matched := false
		for _, m := range r.methods {
			if strings.EqualFold(m, method) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, hm := range r.headerMatches {
		if !headerConditionMet(header, hm) {
			return false
		}
	}
	for _, qm := range r.queryMatches {
		if !queryConditionMet(query, qm) {
			return false
		}
	}
	return true
}

// headerConditionMet reports whether header satisfies hm: the header must
// be present, and when hm.value is non-empty the value must equal it.
// Header-name lookup is case-insensitive (http.Header canonicalises);
// the value comparison is case-sensitive.
func headerConditionMet(header http.Header, hm kvMatch) bool {
	if header == nil {
		return false
	}
	if hm.value == "" {
		return len(header.Values(hm.name)) > 0
	}
	for _, v := range header.Values(hm.name) {
		if v == hm.value {
			return true
		}
	}
	return false
}

// queryConditionMet reports whether query satisfies qm: the parameter
// must be present, and when qm.value is non-empty a value must equal it.
// Both name and value are compared case-sensitively.
func queryConditionMet(query url.Values, qm kvMatch) bool {
	if query == nil {
		return false
	}
	vals, ok := query[qm.name]
	if !ok {
		return false
	}
	if qm.value == "" {
		return true
	}
	for _, v := range vals {
		if v == qm.value {
			return true
		}
	}
	return false
}

// RouteTable holds the data plane's current L7 route table and resolves
// an incoming request to a target backend group. Thread-safe for
// concurrent use: Update is called by a RouteSubscriber as new tables
// arrive, Resolve is called once per incoming request.
//
// A request matching no rule (including the common case of an empty
// table — no L7 routing configured at all) resolves to defaultGroup, so
// a data plane instance behaves exactly as it did before L7 routing
// existed unless routes are explicitly configured for it.
type RouteTable struct {
	mu           sync.RWMutex
	routes       []route // in evaluation order; first match wins
	version      int64
	defaultGroup string
	rng          *rand.Rand
	rngMu        sync.Mutex
}

// NewRouteTable creates a route table that resolves every request to
// defaultGroup until (and unless) Update is called with a non-empty
// table.
func NewRouteTable(defaultGroup string) *RouteTable {
	return &RouteTable{
		defaultGroup: defaultGroup,
		// Traffic-split selection weighting only; not security-sensitive,
		// so a time-seeded PRNG is fine (same rationale as BackendList).
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Update replaces the route table if the incoming version is newer than
// the currently held one. Stale/out-of-order updates are ignored, for the
// same reason BackendList.Update ignores them: gRPC streams don't
// guarantee ordering is preserved across reconnects.
func (t *RouteTable) Update(table *pb.RouteTable) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if table.Version <= t.version && t.version != 0 {
		return
	}

	routes := make([]route, 0, len(table.Routes))
	for _, r := range table.Routes {
		var split []routeTarget
		for _, tgt := range r.Split {
			if tgt.Group == "" {
				continue
			}
			w := tgt.Weight
			if w <= 0 {
				w = 1
			}
			split = append(split, routeTarget{group: tgt.Group, weight: w})
		}
		routes = append(routes, route{
			host:          r.Host,
			pathPrefix:    r.PathPrefix,
			methods:       r.Methods,
			targetGroup:   r.TargetGroup,
			split:         split,
			rewrite:       rewriteFromProto(r.Rewrite),
			auth:          authFromProto(r.Auth),
			headerMatches: kvMatchesFromProto(r.HeaderMatches),
			queryMatches:  queryMatchesFromProto(r.QueryMatches),
		})
	}
	t.routes = routes
	t.version = table.Version
}

// Resolve returns the backend group that a request with the given host,
// path, and method should be proxied to: for the first matching rule,
// either its single target group or — if the rule configures a weighted
// split — one of the split targets chosen by weight. Falls back to the
// data plane's default group if no rule matches. header/query may be nil
// (rules with no header/query conditions still match).
func (t *RouteTable) Resolve(host, path, method string, header http.Header, query url.Values) string {
	group, _, _ := t.resolveMatch(host, path, method, header, query)
	return group
}

// ResolveRoute is Resolve plus the matched rule's rewrite. The returned
// rewrite is the zero value (a no-op) when no rule matched or the matched
// rule configured no rewrites.
func (t *RouteTable) ResolveRoute(host, path, method string, header http.Header, query url.Values) (string, routeRewrite) {
	group, rw, _ := t.resolveMatch(host, path, method, header, query)
	return group, rw
}

// ResolveWithAuth is Resolve plus the matched rule's edge-auth policy. The
// returned routeAuth is the zero value (open) when no rule matched or the
// matched rule configured no auth.
func (t *RouteTable) ResolveWithAuth(host, path, method string, header http.Header, query url.Values) (string, routeAuth) {
	group, _, auth := t.resolveMatch(host, path, method, header, query)
	return group, auth
}

// resolveMatch resolves host/path/method (plus optional header/query
// conditions) to a target group plus the matched rule's rewrite and auth
// policies (both zero when no rule matched). Single matching path shared
// by all three resolve methods.
func (t *RouteTable) resolveMatch(host, path, method string, header http.Header, query url.Values) (string, routeRewrite, routeAuth) {
	t.mu.RLock()
	var matched *route
	for i := range t.routes {
		if t.routes[i].matches(host, path, method, header, query) {
			matched = &t.routes[i]
			break
		}
	}
	group := t.defaultGroup
	var rw routeRewrite
	var auth routeAuth
	if matched != nil {
		rw = matched.rewrite
		auth = matched.auth
		if len(matched.split) == 0 {
			group = matched.targetGroup
		} else {
			group = t.pickSplit(matched.split)
		}
	}
	t.mu.RUnlock()
	return group, rw, auth
}

// kvMatchesFromProto converts pb.HeaderMatch entries into local kvMatch
// conditions, dropping any with an empty name.
func kvMatchesFromProto(in []*pb.HeaderMatch) []kvMatch {
	if len(in) == 0 {
		return nil
	}
	out := make([]kvMatch, 0, len(in))
	for _, m := range in {
		if m == nil || m.Name == "" {
			continue
		}
		out = append(out, kvMatch{name: m.Name, value: m.Value})
	}
	return out
}

// queryMatchesFromProto converts pb.QueryMatch entries into local kvMatch
// conditions, dropping any with an empty name.
func queryMatchesFromProto(in []*pb.QueryMatch) []kvMatch {
	if len(in) == 0 {
		return nil
	}
	out := make([]kvMatch, 0, len(in))
	for _, m := range in {
		if m == nil || m.Name == "" {
			continue
		}
		out = append(out, kvMatch{name: m.Name, value: m.Value})
	}
	return out
}

// rewriteFromProto converts a pb.RouteRewrite (possibly nil) into the
// local routeRewrite form.
func rewriteFromProto(rw *pb.RouteRewrite) routeRewrite {
	if rw == nil {
		return routeRewrite{}
	}
	return routeRewrite{
		setRequestHeaders:     rw.SetRequestHeaders,
		removeRequestHeaders:  rw.RemoveRequestHeaders,
		setResponseHeaders:    rw.SetResponseHeaders,
		removeResponseHeaders: rw.RemoveResponseHeaders,
		stripPathPrefix:       rw.StripPathPrefix,
		addPathPrefix:         rw.AddPathPrefix,
	}
}

// applyRequest mutates the outbound request per the rewrite: strips/adds a
// path prefix and sets/removes request headers. Called before the request
// is proxied to the backend.
func (rw routeRewrite) applyRequest(r *http.Request) {
	if rw.isZero() {
		return
	}
	if rw.stripPathPrefix != "" && strings.HasPrefix(r.URL.Path, rw.stripPathPrefix) {
		r.URL.Path = r.URL.Path[len(rw.stripPathPrefix):]
		if r.URL.Path == "" || r.URL.Path[0] != '/' {
			r.URL.Path = "/" + r.URL.Path
		}
	}
	if rw.addPathPrefix != "" {
		r.URL.Path = rw.addPathPrefix + r.URL.Path
	}
	for name, val := range rw.setRequestHeaders {
		r.Header.Set(name, val)
	}
	for _, name := range rw.removeRequestHeaders {
		r.Header.Del(name)
	}
}

// applyResponse mutates the backend response's headers per the rewrite,
// before it is written back to the client.
func (rw routeRewrite) applyResponse(h http.Header) {
	for name, val := range rw.setResponseHeaders {
		h.Set(name, val)
	}
	for _, name := range rw.removeResponseHeaders {
		h.Del(name)
	}
}

// pickSplit chooses one target from a weighted split. A single-entry
// split returns that entry directly (no RNG needed).
func (t *RouteTable) pickSplit(split []routeTarget) string {
	if len(split) == 1 {
		return split[0].group
	}
	total := 0
	for _, s := range split {
		total += int(s.weight)
	}
	if total <= 0 {
		return split[0].group
	}
	t.rngMu.Lock()
	n := t.rng.Intn(total)
	t.rngMu.Unlock()
	for _, s := range split {
		if n < int(s.weight) {
			return s.group
		}
		n -= int(s.weight)
	}
	return split[len(split)-1].group // unreachable given total>0, but safe
}

// Version returns the currently held route table version.
func (t *RouteTable) Version() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.version
}

// Len returns the number of currently configured routing rules (not
// counting the implicit default-group fallback).
func (t *RouteTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.routes)
}

// TargetGroups returns every distinct backend group referenced by the
// current route table, plus the default group — the full set of groups
// this data plane instance needs an active BackendList/subscription for.
func (t *RouteTable) TargetGroups() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	seen := map[string]bool{t.defaultGroup: true}
	out := []string{t.defaultGroup}
	add := func(g string) {
		if g == "" || seen[g] {
			return
		}
		seen[g] = true
		out = append(out, g)
	}
	for _, r := range t.routes {
		add(r.targetGroup)
		for _, s := range r.split {
			add(s.group)
		}
	}
	return out
}
