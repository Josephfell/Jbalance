package dataplane

import (
	"math/rand"
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
}

// routeTarget is one weighted destination of a split route.
type routeTarget struct {
	group  string
	weight int32
}

func (r route) matches(host, path, method string) bool {
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
	return true
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
			host:        r.Host,
			pathPrefix:  r.PathPrefix,
			methods:     r.Methods,
			targetGroup: r.TargetGroup,
			split:       split,
		})
	}
	t.routes = routes
	t.version = table.Version
}

// Resolve returns the backend group that a request with the given host,
// path, and method should be proxied to: for the first matching rule,
// either its single target group or — if the rule configures a weighted
// split — one of the split targets chosen by weight. Falls back to the
// data plane's default group if no rule matches.
func (t *RouteTable) Resolve(host, path, method string) string {
	t.mu.RLock()
	var matched *route
	for i := range t.routes {
		if t.routes[i].matches(host, path, method) {
			matched = &t.routes[i]
			break
		}
	}
	t.mu.RUnlock()

	if matched == nil {
		return t.defaultGroup
	}
	if len(matched.split) == 0 {
		return matched.targetGroup
	}
	return t.pickSplit(matched.split)
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
