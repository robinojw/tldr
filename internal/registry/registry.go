// Package registry manages gobbler's server registry -- the source of truth
// for which upstream MCP servers are known, their configuration, and state.
package registry

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/robinwhite/gobbler/internal/logging"
	"github.com/robinwhite/gobbler/pkg/config"
)

var log = logging.New("registry")

// Registry manages the set of known upstream MCP servers.
type Registry struct {
	mu   sync.RWMutex
	cfg  *config.GobblerConfig
	path string
}

// Open loads or creates the gobbler registry.
func Open() (*Registry, error) {
	path := config.ServersPath()
	r := &Registry{path: path}

	cfg := config.NewGobblerConfig()
	if _, err := os.Stat(path); err == nil {
		if err := config.LoadJSON(path, cfg); err != nil {
			return nil, fmt.Errorf("failed to load registry: %w", err)
		}
		log.Debug("loaded registry from %s (%d servers)", path, len(cfg.Servers))
	} else {
		log.Info("creating new registry at %s", path)
	}

	r.cfg = cfg
	return r, nil
}

// AddServer registers a new upstream MCP server.
func (r *Registry) AddServer(entry *config.ServerEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry.AddedAt = time.Now()
	r.cfg.Servers[entry.Name] = entry
	r.cfg.LastUpdate = time.Now()

	log.Info("registered server: %s (%s)", entry.Name, entry.Transport)
	return r.save()
}

// RemoveServer removes a server from the registry.
func (r *Registry) RemoveServer(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.cfg.Servers[name]; !ok {
		return fmt.Errorf("server %q not found", name)
	}

	delete(r.cfg.Servers, name)
	r.cfg.LastUpdate = time.Now()

	log.Info("removed server: %s", name)
	return r.save()
}

// GetServer returns a server entry by name.
func (r *Registry) GetServer(name string) (*config.ServerEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.cfg.Servers[name]
	return s, ok
}

// ListServers returns all registered servers.
func (r *Registry) ListServers() []*config.ServerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]*config.ServerEntry, 0, len(r.cfg.Servers))
	for _, s := range r.cfg.Servers {
		entries = append(entries, s)
	}
	return entries
}

// WrappedServers returns servers that are configured for wrapping.
func (r *Registry) WrappedServers() []*config.ServerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var entries []*config.ServerEntry
	for _, s := range r.cfg.Servers {
		if s.Wrapped && !s.Disabled {
			entries = append(entries, s)
		}
	}
	return entries
}

// SetWrapped marks a server as wrapped (or not).
func (r *Registry) SetWrapped(name string, wrapped bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.cfg.Servers[name]
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}
	s.Wrapped = wrapped
	r.cfg.LastUpdate = time.Now()
	return r.save()
}

// SetHarness associates a server with a harness.
func (r *Registry) SetHarness(name, harness string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.cfg.Servers[name]
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}

	// Avoid duplicates
	for _, h := range s.Harnesses {
		if h == harness {
			return r.save()
		}
	}
	s.Harnesses = append(s.Harnesses, harness)
	r.cfg.LastUpdate = time.Now()
	return r.save()
}

// AddWrapper records a wrapper installation for a harness.
func (r *Registry) AddWrapper(entry *config.WrapperEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry.InstalledAt = time.Now()
	entry.Installed = true
	r.cfg.Wrappers[entry.Harness] = entry
	r.cfg.LastUpdate = time.Now()
	return r.save()
}

// GetWrapper returns the wrapper entry for a harness.
func (r *Registry) GetWrapper(harness string) (*config.WrapperEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.cfg.Wrappers[harness]
	return w, ok
}

// Policy returns the current policy config.
func (r *Registry) Policy() *config.PolicyConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cfg.Policies == nil {
		r.cfg.Policies = config.DefaultPolicyConfig()
	}
	return r.cfg.Policies
}

// Config returns the full config (read-only snapshot).
func (r *Registry) Config() *config.GobblerConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

func (r *Registry) save() error {
	return config.SaveJSON(r.path, r.cfg)
}
