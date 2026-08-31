package dataplane

import (
	"context"
	"crypto/tls"
	"log"
	"sync"
	"time"
)

// HealthCheckConfig bundles the HealthChecker tuning knobs a GroupManager
// applies uniformly to every group it starts — mirrors the flags
// cmd/dataplane already exposes, just grouped for passing around as one
// value instead of four.
type HealthCheckConfig struct {
	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold int
	SuccessThreshold int

	// HTTP-mode probe settings. Mode empty/"tcp" keeps the historical
	// TCP-connect probe; "http" issues an HTTP GET and checks the status.
	Mode             HealthCheckMode
	HTTPPath         string
	HTTPExpectStatus int
	HTTPScheme       string
	HTTPHost         string
}

// GroupManager owns one BackendList (plus its Subscriber, HealthChecker,
// and HealthReporter) per backend group this data plane instance proxies
// to. Before L7 routing, a data plane instance only ever needed exactly
// one group (from -group) for its entire lifetime; a route table can now
// reference additional groups, discovered incrementally as requests
// arrive that match a rule targeting a group not seen before. Ensure is
// idempotent and safe for concurrent use, so it can be called directly
// from the request path without any separate "pre-scan the route table"
// step.
//
// Every group's background goroutines are bound to the ctx passed to
// NewGroupManager (the process/server lifetime), never to a per-request
// context — Ensure is deliberately called from request handling, but a
// group's subscription must keep running long after that one request
// completes.
type GroupManager struct {
	baseCtx              context.Context
	controlPlaneAddr     string
	instanceID           string
	tlsConfig            *tls.Config
	healthCfg            HealthCheckConfig
	healthReportInterval time.Duration

	mu     sync.RWMutex
	groups map[string]*BackendList
}

// NewGroupManager creates a manager that starts every group's Subscriber/
// HealthChecker/HealthReporter against controlPlaneAddr, using the given
// TLS config (nil for plaintext) and health-check tuning. baseCtx governs
// the lifetime of every group's background goroutines — it should be the
// data plane process's own long-lived context, cancelled only on
// shutdown.
func NewGroupManager(baseCtx context.Context, controlPlaneAddr, instanceID string, tlsConfig *tls.Config, healthCfg HealthCheckConfig, healthReportInterval time.Duration) *GroupManager {
	return &GroupManager{
		baseCtx:              baseCtx,
		controlPlaneAddr:     controlPlaneAddr,
		instanceID:           instanceID,
		tlsConfig:            tlsConfig,
		healthCfg:            healthCfg,
		healthReportInterval: healthReportInterval,
		groups:               make(map[string]*BackendList),
	}
}

// Ensure returns the BackendList for group, starting its Subscriber,
// HealthChecker, and HealthReporter (bound to the manager's baseCtx, not
// any per-request context) the first time group is requested. Subsequent
// calls for the same group return the same BackendList without starting
// anything new.
func (m *GroupManager) Ensure(group string) *BackendList {
	m.mu.RLock()
	bl, ok := m.groups[group]
	m.mu.RUnlock()
	if ok {
		return bl
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-check under the write lock: another goroutine may have created
	// this group's entry between the RUnlock above and this Lock.
	if bl, ok := m.groups[group]; ok {
		return bl
	}

	bl = NewBackendList()
	m.groups[group] = bl

	sub := NewSubscriber(m.controlPlaneAddr, group, m.instanceID, bl, m.tlsConfig)
	go sub.Run(m.baseCtx)

	checker := NewHealthChecker(bl)
	checker.Interval = m.healthCfg.Interval
	checker.Timeout = m.healthCfg.Timeout
	checker.FailureThreshold = m.healthCfg.FailureThreshold
	checker.SuccessThreshold = m.healthCfg.SuccessThreshold
	checker.Mode = m.healthCfg.Mode
	checker.HTTPPath = m.healthCfg.HTTPPath
	checker.HTTPExpectStatus = m.healthCfg.HTTPExpectStatus
	checker.HTTPScheme = m.healthCfg.HTTPScheme
	checker.HTTPHost = m.healthCfg.HTTPHost
	go checker.Run(m.baseCtx)

	reporter := NewHealthReporter(m.controlPlaneAddr, group, m.instanceID, bl, m.tlsConfig, m.healthReportInterval)
	go reporter.Run(m.baseCtx)

	log.Printf("dataplane: started subscription for newly referenced group %q", group)
	return bl
}

// Groups returns the names of every group currently being tracked
// (subscribed to), for observability/debug purposes.
func (m *GroupManager) Groups() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.groups))
	for g := range m.groups {
		out = append(out, g)
	}
	return out
}
