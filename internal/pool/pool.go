// Package pool defines the backend pool abstraction used by the control
// plane's reconciliation loop. A Provider is anything that can report the
// current set of live backends for a named group — a fake/static provider
// for local testing today, and (eventually) a real Azure VMSS or vCenter
// provider without any change to the control plane's reconciliation logic.
package pool

import "context"

// Backend is a single upstream target the data plane can proxy traffic to.
type Backend struct {
	Address string // host:port
	Weight  int32  // relative capacity weight; 0 is treated as 1
}

// Snapshot is the full set of backends for one group at a point in time.
type Snapshot struct {
	Group    string
	Backends []Backend
}

// Provider reports the current backend set for one or more groups.
// Implementations should be safe for concurrent use.
type Provider interface {
	// Groups returns the list of group names this provider currently knows
	// about. The control plane polls this to discover new groups without
	// requiring static config.
	Groups(ctx context.Context) ([]string, error)

	// Snapshot returns the current backend set for the given group.
	// Returning a different set on each call (added/removed backends) is
	// how the control plane observes scaling events.
	Snapshot(ctx context.Context, group string) (Snapshot, error)
}
