package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Route is one L7 routing rule: a request matching Host/PathPrefix/Methods
// is proxied to TargetGroup. Mirrors proto.Route field-for-field; kept as
// a separate type (rather than reusing the generated pb.Route directly)
// so the rest of the control plane package doesn't need to import proto
// just to hold routing config, matching the pattern already used for
// Override and Algorithm.
type Route struct {
	// Host to match, exact string. "*" or "" matches any host.
	Host string `json:"host"`
	// PathPrefix to match as a literal prefix, e.g. "/api/". "/" (or "")
	// matches every path.
	PathPrefix string `json:"pathPrefix"`
	// Methods this rule applies to, e.g. ["GET", "POST"]. Empty means any
	// method.
	Methods []string `json:"methods,omitempty"`
	// TargetGroup is the backend group a matching request is sent to.
	TargetGroup string `json:"targetGroup"`
	// Name is a display label for the admin UI; not evaluated.
	Name string `json:"name,omitempty"`
}

// Matches reports whether this route applies to a request with the given
// host, path, and method. Host comparison is case-insensitive (matching
// HTTP's own treatment of host names); path prefix comparison is exact
// (paths are case-sensitive per the HTTP spec).
func (r Route) Matches(host, path, method string) bool {
	if r.Host != "" && r.Host != "*" && !strings.EqualFold(r.Host, host) {
		return false
	}
	if r.PathPrefix != "" && r.PathPrefix != "/" && !strings.HasPrefix(path, r.PathPrefix) {
		return false
	}
	if len(r.Methods) > 0 {
		matched := false
		for _, m := range r.Methods {
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

// RouteStore holds the global L7 route table, persisted to a single local
// JSON file — no external database, consistent with OverrideStore and
// AlgorithmStore. Unlike those two, routing is not scoped per group: one
// table applies to every data plane instance that subscribes to it,
// which is what lets a single data plane route different requests to
// different backend groups.
type RouteStore struct {
	path string
	mu   sync.RWMutex
	// routes is stored in evaluation order — first match wins, so order
	// is significant and must survive persistence/reload.
	routes  []Route
	version int64
}

// NewRouteStore loads a route table from path if it exists, or starts
// empty (no routes configured) if it doesn't. An empty table is a valid,
// common state: every data plane instance simply falls back to its own
// -group flag, exactly as it did before L7 routing existed.
func NewRouteStore(path string) *RouteStore {
	s := &RouteStore{path: path}

	if path == "" {
		return s
	}

	data, err := os.ReadFile(path)
	if err == nil {
		var loaded struct {
			Routes  []Route `json:"routes"`
			Version int64   `json:"version"`
		}
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			s.routes = loaded.Routes
			s.version = loaded.Version
		}
	}

	return s
}

// Routes returns a defensive copy of the current route table, in
// evaluation order.
func (s *RouteStore) Routes() []Route {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Route, len(s.routes))
	copy(out, s.routes)
	return out
}

// Version returns the route table's current version.
func (s *RouteStore) Version() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// Set replaces the entire route table with routes (in the given order)
// and persists it, bumping the version. Routing rules are managed as a
// whole ordered list rather than individual add/remove operations —
// order is part of the rules' meaning (first match wins), so a UI/API
// that let callers add or delete a single rule without seeing the whole
// list would risk silently reordering evaluation.
func (s *RouteStore) Set(routes []Route) error {
	s.mu.Lock()
	s.routes = make([]Route, len(routes))
	copy(s.routes, routes)
	s.version++
	snapshot := struct {
		Routes  []Route `json:"routes"`
		Version int64   `json:"version"`
	}{Routes: s.routes, Version: s.version}
	s.mu.Unlock()

	return s.persist(snapshot)
}

func (s *RouteStore) persist(data any) error {
	if s.path == "" {
		return nil
	}

	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("controlplane: failed to marshal route table: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("controlplane: failed to create route table directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".routes-*.tmp")
	if err != nil {
		return fmt.Errorf("controlplane: failed to create route table temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("controlplane: failed to write route table temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("controlplane: failed to set route table file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("controlplane: failed to close route table temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("controlplane: failed to persist route table: %w", err)
	}
	return nil
}
