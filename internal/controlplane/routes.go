package controlplane

import (
	"encoding/json"
	"fmt"
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
	// Used when Split is empty (the common single-target case).
	TargetGroup string `json:"targetGroup"`
	// Split, when non-empty, is a weighted set of destination groups for
	// canary / traffic splitting: a matching request is sent to one of
	// them chosen by weight, rather than always to TargetGroup.
	Split []RouteTarget `json:"split,omitempty"`
	// Name is a display label for the admin UI; not evaluated.
	Name string `json:"name,omitempty"`
	// Rewrite holds optional request/response transformations applied
	// when this rule matches. Nil/zero means no rewrite.
	Rewrite *RouteRewrite `json:"rewrite,omitempty"`
	// Auth, when set, requires matched requests to authenticate (API key
	// or JWT) before being proxied. Nil = open route.
	Auth *RouteAuth `json:"auth,omitempty"`
}

// RouteRewrite describes header and path transformations applied to a
// matched request. Mirrors proto.RouteRewrite; all fields optional.
type RouteRewrite struct {
	SetRequestHeaders     map[string]string `json:"setRequestHeaders,omitempty"`
	RemoveRequestHeaders  []string          `json:"removeRequestHeaders,omitempty"`
	SetResponseHeaders    map[string]string `json:"setResponseHeaders,omitempty"`
	RemoveResponseHeaders []string          `json:"removeResponseHeaders,omitempty"`
	StripPathPrefix       string            `json:"stripPathPrefix,omitempty"`
	AddPathPrefix         string            `json:"addPathPrefix,omitempty"`
}

// RouteAuth is the control-plane form of edge authentication for a route.
// Mirrors proto.RouteAuth.
type RouteAuth struct {
	Mode             string   `json:"mode"` // none|api_key|jwt
	APIKeys          []string `json:"apiKeys,omitempty"`
	APIKeyHeader     string   `json:"apiKeyHeader,omitempty"`
	HMACSecret       string   `json:"hmacSecret,omitempty"`
	RSAPublicKeyPEM  string   `json:"rsaPublicKeyPem,omitempty"`
	ExpectedIssuer   string   `json:"expectedIssuer,omitempty"`
	ExpectedAudience string   `json:"expectedAudience,omitempty"`
}

// RouteTarget is one weighted destination of a split (canary) route rule.
type RouteTarget struct {
	// Group is the backend group this share of the traffic goes to.
	Group string `json:"group"`
	// Weight is this target's relative share within the split. Defaults
	// to 1 when unset/zero.
	Weight int32 `json:"weight,omitempty"`
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
	store BlobStore
	key   string
	mu    sync.RWMutex
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
	// Backward-compatible constructor: a file path maps to a FileBlobStore
	// over its directory, keyed by the file's base name (sans .json).
	store, key := fileStoreFromPath(path)
	return NewRouteStoreWithBackend(store, key)
}

// NewRouteStoreWithBackend loads the route table from the given blob store
// under key (used for the Postgres/shared backend). A nil store yields an
// in-memory-only store that never persists (used by some tests).
func NewRouteStoreWithBackend(store BlobStore, key string) *RouteStore {
	s := &RouteStore{store: store, key: key}
	if store == nil {
		return s
	}
	data, err := store.Load(key)
	if err == nil && len(data) > 0 {
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
	if s.store == nil {
		return nil
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("controlplane: failed to marshal route table: %w", err)
	}
	return s.store.Save(s.key, encoded)
}
