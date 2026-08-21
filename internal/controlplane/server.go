// Package controlplane implements the gRPC ControlPlane service. It polls a
// pool.Provider on an interval, and whenever a group's backend set changes,
// pushes the new set to every data plane instance currently streaming for
// that group. This is the xDS pattern: data planes connect once and receive
// updates pushed to them, rather than polling the control plane themselves.
package controlplane

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/Josephfell/Jbalance/internal/pool"
	pb "github.com/Josephfell/Jbalance/proto"
)

// backendHealthKey identifies one backend address as reported by one
// specific data plane instance — health is tracked per-reporter because
// different data plane instances could theoretically disagree (e.g. one
// has a network path to a backend that another doesn't), and losing a
// report from one instance shouldn't silently erase another's.
type backendHealthKey struct {
	group      string
	instanceID string
	address    string
}

// healthEntry is one reported health status with the time it was received,
// so stale reports from a disconnected/dead data plane can be aged out.
type healthEntry struct {
	healthy    bool
	reportedAt time.Time
}

// instanceKey identifies one currently-or-recently-connected data plane
// instance. Keyed by instanceID alone (not also group) since an
// instance ID is expected to be unique per data plane process — if two
// concurrently-connected streams ever reported the same instanceID for
// different groups, the most recent StreamBackends call wins the fleet
// entry, which is an acceptable edge case for a display-only feature.
type instanceKey string

// instanceState tracks one data plane instance for the admin UI's Fleet
// view, independent of the per-backend health map: it answers "which data
// planes are out there and are they currently connected", not "what do
// they think about any given backend".
type instanceState struct {
	group       string
	connectedAt time.Time
	connected   bool // false once the StreamBackends call for this instance returns
	// lastHealthReportAt and reportedBackends come from ReportHealth calls
	// naming this instanceID — zero until the first report arrives, which
	// can lag connectedAt by up to one health-report interval.
	lastHealthReportAt time.Time
	reportedBackends   int
}

// Server implements pb.ControlPlaneServer.
type Server struct {
	pb.UnimplementedControlPlaneServer

	provider   pool.Provider
	overrides  *OverrideStore
	algorithms *AlgorithmStore

	mu   sync.Mutex
	subs map[string]map[chan *pb.BackendSet]struct{} // group -> set of subscriber channels
	last map[string]*pb.BackendSet                   // group -> last known backend set (for version tracking + late joiners)

	healthMu sync.RWMutex
	health   map[backendHealthKey]healthEntry

	instancesMu sync.RWMutex
	instances   map[instanceKey]*instanceState
}

// NewServer creates a control plane server backed by the given provider.
// overrides and algorithms may be nil, in which case manual weight/drain
// overrides are disabled and every group uses AlgorithmRoundRobin (empty
// in-memory stores are used instead so callers don't need nil checks
// everywhere).
func NewServer(provider pool.Provider, overrides *OverrideStore, algorithms *AlgorithmStore) *Server {
	if overrides == nil {
		overrides = NewOverrideStore("")
	}
	if algorithms == nil {
		algorithms = NewAlgorithmStore("")
	}
	return &Server{
		provider:   provider,
		overrides:  overrides,
		algorithms: algorithms,
		subs:       make(map[string]map[chan *pb.BackendSet]struct{}),
		last:       make(map[string]*pb.BackendSet),
		health:     make(map[backendHealthKey]healthEntry),
		instances:  make(map[instanceKey]*instanceState),
	}
}

// Run starts the reconciliation loop. It polls the provider every interval
// and pushes updates to subscribers whenever a group's backend set differs
// from the last one it sent. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Reconcile once immediately so subscribers connecting right at startup
	// don't have to wait a full interval for their first backend set.
	s.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

func (s *Server) reconcile(ctx context.Context) {
	groups, err := s.provider.Groups(ctx)
	if err != nil {
		log.Printf("controlplane: failed to list groups: %v", err)
		return
	}

	for _, group := range groups {
		s.reconcileGroup(ctx, group)
	}
}

func (s *Server) reconcileGroup(ctx context.Context, group string) {
	snap, err := s.provider.Snapshot(ctx, group)
	if err != nil {
		log.Printf("controlplane: failed to snapshot group %q: %v", group, err)
		return
	}
	s.publishIfChanged(group, snap)
}

// SetOverride sets (or updates) a manual weight/drain override for one
// backend address in group and immediately re-publishes that group so the
// change takes effect right away, rather than waiting for the next
// scheduled reconcile tick or a provider-side change to trigger it.
func (s *Server) SetOverride(ctx context.Context, group, address string, weight *int32, drained bool) error {
	if err := s.overrides.Set(group, address, weight, drained); err != nil {
		return err
	}
	s.forceRepublish(ctx, group)
	return nil
}

// ClearOverride removes a manual override for one backend address in
// group and immediately re-publishes that group.
func (s *Server) ClearOverride(ctx context.Context, group, address string) error {
	if err := s.overrides.Clear(group, address); err != nil {
		return err
	}
	s.forceRepublish(ctx, group)
	return nil
}

// SetAlgorithm sets the load-balancing algorithm for group and immediately
// re-publishes so connected data planes switch over right away.
func (s *Server) SetAlgorithm(ctx context.Context, group string, algorithm Algorithm) error {
	if err := s.algorithms.Set(group, algorithm); err != nil {
		return err
	}
	s.forceRepublish(ctx, group)
	return nil
}

// forceRepublish re-fetches the provider's current snapshot for group and
// publishes it (with the now-current overrides applied) even if the
// provider's own view hasn't changed — used after an override is
// set/cleared, since that's a change in *our* view, not the provider's.
func (s *Server) forceRepublish(ctx context.Context, group string) {
	snap, err := s.provider.Snapshot(ctx, group)
	if err != nil {
		log.Printf("controlplane: failed to snapshot group %q while applying override: %v", group, err)
		return
	}

	// Bypass publishIfChanged's equality check: an override change must
	// always produce a visible update even if the resulting BackendSet
	// happens to look identical to what a stale s.last already holds in
	// an edge case (e.g. re-applying the same override value).
	next := snapshotToBackendSet(group, snap, s.overrides.GroupOverrides(group), s.algorithms.Get(group))

	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.last[group]
	version := int64(1)
	if prev != nil {
		version = prev.Version + 1
	}
	next.Version = version
	s.last[group] = next

	for ch := range s.subs[group] {
		select {
		case ch <- next:
		default:
			log.Printf("controlplane: subscriber for group %q is slow, dropping override update", group)
		}
	}

	log.Printf("controlplane: group %q updated to version %d (%d backends) after override change", group, next.Version, len(next.Backends))
}

// publishIfChanged compares the new snapshot against the last one sent for
// this group and, if different, bumps the version and pushes it to every
// currently-connected subscriber for that group.
func (s *Server) publishIfChanged(group string, snap pool.Snapshot) {
	next := snapshotToBackendSet(group, snap, s.overrides.GroupOverrides(group), s.algorithms.Get(group))

	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.last[group]
	if backendSetsEqual(prev, next) {
		return
	}

	version := int64(1)
	if prev != nil {
		version = prev.Version + 1
	}
	next.Version = version
	s.last[group] = next

	for ch := range s.subs[group] {
		// Non-blocking send: a slow/stuck subscriber shouldn't block the
		// reconciliation loop or other subscribers. It will simply miss
		// this update; the next change will still be delivered, or it can
		// reconnect to get the latest snapshot.
		select {
		case ch <- next:
		default:
			log.Printf("controlplane: subscriber for group %q is slow, dropping update", group)
		}
	}

	log.Printf("controlplane: group %q updated to version %d (%d backends)", group, next.Version, len(next.Backends))
}

// StreamBackends implements pb.ControlPlaneServer. It registers the caller
// as a subscriber for the requested group, immediately sends the latest
// known backend set (if any), and then streams further updates until the
// client disconnects or the context is cancelled.
func (s *Server) StreamBackends(req *pb.StreamBackendsRequest, stream pb.ControlPlane_StreamBackendsServer) error {
	group := req.GetGroup()
	instanceID := req.GetInstanceId()

	ch := make(chan *pb.BackendSet, 4)
	s.subscribe(group, ch)
	defer s.unsubscribe(group, ch)

	s.markInstanceConnected(instanceID, group)
	defer s.markInstanceDisconnected(instanceID)

	log.Printf("controlplane: data plane %q subscribed to group %q", instanceID, group)

	// Send the current snapshot immediately so a newly-connected data plane
	// doesn't sit with an empty backend list until the next reconcile tick.
	s.mu.Lock()
	current := s.last[group]
	s.mu.Unlock()
	if current != nil {
		if err := stream.Send(current); err != nil {
			return err
		}
	}

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			log.Printf("controlplane: data plane %q disconnected from group %q", instanceID, group)
			return ctx.Err()
		case update := <-ch:
			if err := stream.Send(update); err != nil {
				return err
			}
		}
	}
}

// healthReportMaxAge bounds how long a reported health status is trusted
// before being treated as unknown — protects against a dashboard showing
// stale "healthy" state for a data plane instance that has silently died.
const healthReportMaxAge = 30 * time.Second

// ReportHealth implements pb.ControlPlaneServer. Data plane instances call
// this periodically (independent of the StreamBackends stream) to report
// what their own local health checker currently believes about each
// backend they know of.
func (s *Server) ReportHealth(_ context.Context, report *pb.HealthReport) (*pb.HealthReportAck, error) {
	now := time.Now()

	s.healthMu.Lock()
	for _, bh := range report.GetBackends() {
		key := backendHealthKey{
			group:      report.GetGroup(),
			instanceID: report.GetInstanceId(),
			address:    bh.GetAddress(),
		}
		s.health[key] = healthEntry{healthy: bh.GetHealthy(), reportedAt: now}
	}
	s.healthMu.Unlock()

	s.recordInstanceHealthReport(report.GetInstanceId(), len(report.GetBackends()), now)

	return &pb.HealthReportAck{}, nil
}

// markInstanceConnected records that instanceID has an active
// StreamBackends call for group, for display on the admin UI's Fleet
// view. Overwrites any prior entry for this instanceID — an instance
// reconnecting under the same ID (e.g. after a control-plane restart)
// should show its new connection time, not a stale one.
func (s *Server) markInstanceConnected(instanceID, group string) {
	if instanceID == "" {
		return // nothing to key the fleet entry on; skip rather than collide entries
	}
	s.instancesMu.Lock()
	defer s.instancesMu.Unlock()
	s.instances[instanceKey(instanceID)] = &instanceState{
		group:       group,
		connectedAt: time.Now(),
		connected:   true,
	}
}

// markInstanceDisconnected marks instanceID as no longer streaming, once
// its StreamBackends call returns (client disconnect, context
// cancellation, or a send error). The entry is kept (not deleted) so the
// Fleet view can show "last seen" for an instance that disconnected
// recently, rather than having it vanish instantly.
func (s *Server) markInstanceDisconnected(instanceID string) {
	if instanceID == "" {
		return
	}
	s.instancesMu.Lock()
	defer s.instancesMu.Unlock()
	if st, ok := s.instances[instanceKey(instanceID)]; ok {
		st.connected = false
	}
}

// recordInstanceHealthReport updates the fleet entry for instanceID with
// the time and size of its most recent ReportHealth call. A no-op if the
// instance has no StreamBackends-derived fleet entry yet — a health
// report received before (or without) an active stream is rare (only a
// brief startup race) and not worth fabricating a synthetic connection
// entry for.
func (s *Server) recordInstanceHealthReport(instanceID string, backendCount int, at time.Time) {
	if instanceID == "" {
		return
	}
	s.instancesMu.Lock()
	defer s.instancesMu.Unlock()
	if st, ok := s.instances[instanceKey(instanceID)]; ok {
		st.lastHealthReportAt = at
		st.reportedBackends = backendCount
	}
}

// healthForAddress aggregates all non-stale reports for a given
// group/address across every reporting data plane instance. Returns
// (healthy, hasData) — hasData is false if no data plane has reported on
// this address recently, which the dashboard should render distinctly
// from a known-unhealthy backend (e.g. "no data plane connected yet"
// rather than implying it's down).
//
// A backend is considered healthy only if every instance that has
// reported on it recently agrees it's healthy — one data plane reporting
// a backend as down is enough to surface a warning, since that's exactly
// the kind of partial-network-path failure this feature exists to catch.
func (s *Server) healthForAddress(group, address string) (healthy bool, hasData bool) {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()

	now := time.Now()
	healthy = true
	for key, entry := range s.health {
		if key.group != group || key.address != address {
			continue
		}
		if now.Sub(entry.reportedAt) > healthReportMaxAge {
			continue // stale — treat as if this instance hasn't reported
		}
		hasData = true
		if !entry.healthy {
			healthy = false
		}
	}
	return healthy, hasData
}

func (s *Server) subscribe(group string, ch chan *pb.BackendSet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs[group] == nil {
		s.subs[group] = make(map[chan *pb.BackendSet]struct{})
	}
	s.subs[group][ch] = struct{}{}
}

func (s *Server) unsubscribe(group string, ch chan *pb.BackendSet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs[group], ch)
}

// BackendState is a read-only view of one backend's configuration and
// currently known health, for display purposes.
type BackendState struct {
	Address string
	Weight  int32
	// Healthy and HealthKnown together describe three states: healthy
	// (Healthy=true, HealthKnown=true), unhealthy (Healthy=false,
	// HealthKnown=true), and unknown/no data plane has reported yet
	// (HealthKnown=false).
	Healthy     bool
	HealthKnown bool
	// Drained is true if a manual override currently excludes this
	// backend from what's sent to data planes (set via the admin UI).
	Drained bool
	// WeightOverridden is true if Weight reflects a manual override
	// rather than the pool provider's own reported weight.
	WeightOverridden bool
}

// GroupState is a read-only view of one group's current backend set, for
// display purposes (e.g. the admin web UI).
type GroupState struct {
	Group           string
	Version         int64
	Backends        []BackendState
	SubscriberCount int
	Algorithm       Algorithm
}

// Snapshot returns a read-only view of every group's current backend set,
// health, override state, and subscriber count, for display purposes
// (e.g. the admin web UI).
//
// Deliberately queries the provider directly for the full backend list
// (annotating it with override/health state) rather than reading the
// already-filtered s.last — a drained backend is excluded from what's
// actually sent to data planes, but still needs to be visible here so it
// can be un-drained from the UI.
func (s *Server) Snapshot(ctx context.Context) []GroupState {
	groups, err := s.provider.Groups(ctx)
	if err != nil {
		log.Printf("controlplane: failed to list groups for snapshot: %v", err)
		return nil
	}

	states := make([]GroupState, 0, len(groups))
	for _, group := range groups {
		snap, err := s.provider.Snapshot(ctx, group)
		if err != nil {
			log.Printf("controlplane: failed to snapshot group %q for display: %v", group, err)
			continue
		}

		overrides := s.overrides.GroupOverrides(group)
		backends := make([]BackendState, 0, len(snap.Backends))
		for _, b := range snap.Backends {
			ov, hasOverride := overrides[b.Address]
			weight := b.Weight
			weightOverridden := false
			if hasOverride && ov.Weight != nil {
				weight = *ov.Weight
				weightOverridden = true
			}

			healthy, known := s.healthForAddress(group, b.Address)
			backends = append(backends, BackendState{
				Address:          b.Address,
				Weight:           weight,
				Healthy:          healthy,
				HealthKnown:      known,
				Drained:          hasOverride && ov.Drained,
				WeightOverridden: weightOverridden,
			})
		}

		s.mu.Lock()
		var version int64
		var subCount int
		if last := s.last[group]; last != nil {
			version = last.Version
		}
		subCount = len(s.subs[group])
		s.mu.Unlock()

		states = append(states, GroupState{
			Group:           group,
			Version:         version,
			Backends:        backends,
			SubscriberCount: subCount,
			Algorithm:       s.algorithms.Get(group),
		})
	}
	return states
}

// InstanceState is a read-only view of one data plane instance's
// connection state, for display on the admin UI's Fleet view.
type InstanceState struct {
	InstanceID string
	Group      string
	// Connected is true if this instance currently has an active
	// StreamBackends call open.
	Connected bool
	// ConnectedSince is when the current (or most recent, if Connected is
	// false) StreamBackends call began.
	ConnectedSince time.Time
	// LastHealthReport is the zero time if this instance has never called
	// ReportHealth.
	LastHealthReport time.Time
	ReportedBackends int
}

// FleetSnapshot returns every known data plane instance (currently
// connected or seen recently), sorted by instance ID for stable display.
// An instance is remembered until the control plane process restarts —
// there is deliberately no expiry here, since "this instance disconnected
// 20 minutes ago and hasn't come back" is exactly the kind of thing an
// operator wants the Fleet view to still show, not silently drop.
func (s *Server) FleetSnapshot() []InstanceState {
	s.instancesMu.RLock()
	defer s.instancesMu.RUnlock()

	out := make([]InstanceState, 0, len(s.instances))
	for id, st := range s.instances {
		out = append(out, InstanceState{
			InstanceID:       string(id),
			Group:            st.group,
			Connected:        st.connected,
			ConnectedSince:   st.connectedAt,
			LastHealthReport: st.lastHealthReportAt,
			ReportedBackends: st.reportedBackends,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out
}

// snapshotToBackendSet converts a provider snapshot into the wire format,
// applying any manual overrides on top: a drained backend is omitted
// entirely (never sent to data planes, regardless of what the provider
// reports), and an overridden weight replaces the provider's weight. The
// group's currently selected load-balancing algorithm is included so data
// planes apply it without a separate round-trip.
func snapshotToBackendSet(group string, snap pool.Snapshot, overrides map[string]Override, algorithm Algorithm) *pb.BackendSet {
	backends := make([]*pb.Backend, 0, len(snap.Backends))
	for _, b := range snap.Backends {
		ov, hasOverride := overrides[b.Address]
		if hasOverride && ov.Drained {
			continue
		}

		weight := b.Weight
		if hasOverride && ov.Weight != nil {
			weight = *ov.Weight
		}
		backends = append(backends, &pb.Backend{Address: b.Address, Weight: weight})
	}
	return &pb.BackendSet{Group: group, Backends: backends, Algorithm: string(algorithm)}
}

// backendSetsEqual compares two backend sets by address+weight, ignoring
// order and version, so a provider that returns backends in a different
// order each call doesn't trigger spurious updates.
func backendSetsEqual(a, b *pb.BackendSet) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Backends) != len(b.Backends) {
		return false
	}

	count := make(map[string]int32)
	for _, be := range a.Backends {
		count[be.Address] = be.Weight
	}
	for _, be := range b.Backends {
		w, ok := count[be.Address]
		if !ok || w != be.Weight {
			return false
		}
		delete(count, be.Address)
	}
	return len(count) == 0
}
