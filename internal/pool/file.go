package pool

import (
	"context"
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// FileProvider reports backends from a local YAML file, re-reading it when
// the file changes on disk (checked by modification time on each call).
// This lets an operator run jbalance against a static or externally-managed
// backend list — edited by hand, templated by config management, or written
// by another process — without needing a cloud provider (Azure/Kubernetes)
// or a rebuild. Editing the file and saving it is picked up on the next
// reconcile tick, the same way a real provider observes scaling events.
//
// File format (YAML):
//
//	groups:
//	  web-tier:
//	    - address: 10.0.0.1:8080
//	      weight: 2
//	    - address: 10.0.0.2:8080   # weight omitted -> defaults to 1
//	  api-tier:
//	    - address: 10.0.1.1:9090
type FileProvider struct {
	path string

	mu      sync.RWMutex
	modTime int64
	groups  map[string][]Backend
}

// fileConfig is the on-disk YAML shape.
type fileConfig struct {
	Groups map[string][]fileBackend `yaml:"groups"`
}

type fileBackend struct {
	Address string `yaml:"address"`
	Weight  int32  `yaml:"weight"`
}

// NewFileProvider creates a provider backed by the YAML file at path. The
// file is read once up front so a missing or malformed file fails fast at
// startup rather than surfacing later on the first reconcile.
func NewFileProvider(path string) (*FileProvider, error) {
	if path == "" {
		return nil, fmt.Errorf("pool: file provider requires a path (set LB_FILE_PROVIDER_PATH)")
	}
	p := &FileProvider{path: path, groups: make(map[string][]Backend)}
	if err := p.reload(); err != nil {
		return nil, err
	}
	return p, nil
}

// reloadIfChanged re-reads the file when its mtime has advanced since the
// last successful load. A read/parse error on reload is returned to the
// caller but the previously-loaded state is left intact, so a half-written
// file mid-edit does not blank out the backend list.
func (p *FileProvider) reloadIfChanged() error {
	info, err := os.Stat(p.path)
	if err != nil {
		return fmt.Errorf("pool: stat backend file %q: %w", p.path, err)
	}
	mt := info.ModTime().UnixNano()

	p.mu.RLock()
	unchanged := mt == p.modTime
	p.mu.RUnlock()
	if unchanged {
		return nil
	}
	return p.reload()
}

// reload reads and parses the file, replacing the cached state atomically.
func (p *FileProvider) reload() error {
	info, err := os.Stat(p.path)
	if err != nil {
		return fmt.Errorf("pool: stat backend file %q: %w", p.path, err)
	}
	data, err := os.ReadFile(p.path)
	if err != nil {
		return fmt.Errorf("pool: read backend file %q: %w", p.path, err)
	}

	var cfg fileConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("pool: parse backend file %q: %w", p.path, err)
	}

	groups := make(map[string][]Backend, len(cfg.Groups))
	for group, entries := range cfg.Groups {
		backends := make([]Backend, 0, len(entries))
		for i, e := range entries {
			if e.Address == "" {
				return fmt.Errorf("pool: backend file %q: group %q entry %d has an empty address", p.path, group, i)
			}
			backends = append(backends, Backend{Address: e.Address, Weight: e.Weight}) //nolint:staticcheck // S1016: explicit mapping kept — fileBackend is the on-disk shape, intentionally decoupled from Backend
		}
		groups[group] = backends
	}

	p.mu.Lock()
	p.groups = groups
	p.modTime = info.ModTime().UnixNano()
	p.mu.Unlock()
	return nil
}

// Groups implements Provider.
func (p *FileProvider) Groups(_ context.Context) ([]string, error) {
	if err := p.reloadIfChanged(); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	groups := make([]string, 0, len(p.groups))
	for g := range p.groups {
		groups = append(groups, g)
	}
	return groups, nil
}

// Snapshot implements Provider.
func (p *FileProvider) Snapshot(_ context.Context, group string) (Snapshot, error) {
	if err := p.reloadIfChanged(); err != nil {
		return Snapshot{}, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	backends, ok := p.groups[group]
	if !ok {
		return Snapshot{}, fmt.Errorf("pool: unknown group %q", group)
	}
	// Return a copy so callers can't mutate the cached slice.
	out := make([]Backend, len(backends))
	copy(out, backends)
	return Snapshot{Group: group, Backends: out}, nil
}
