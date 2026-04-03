// Package harness defines the adapter interface for coding harnesses
// and provides detection across all supported harnesses.
package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/robinojw/tldr/pkg/config"
)

// Scope controls whether tldr writes harness config globally or locally.
type Scope string

const (
	ScopeGlobal Scope = "global"
	ScopeLocal  Scope = "local"
)

// ParseScope normalizes CLI scope values into the two supported scopes.
func ParseScope(raw string) (Scope, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(ScopeGlobal), "user":
		return ScopeGlobal, nil
	case string(ScopeLocal), "project":
		return ScopeLocal, nil
	default:
		return "", fmt.Errorf("unsupported scope: %s", raw)
	}
}

// Adapter is the interface each coding harness must implement.
type Adapter interface {
	// Name returns the harness identifier (e.g. "forge", "claude", "codex").
	Name() string

	// Detect checks if this harness is installed and configured.
	Detect(ctx context.Context) (bool, error)

	// ConfigPath returns the path to the harness's MCP config file.
	ConfigPath(ctx context.Context, scope Scope) (string, error)

	// LoadConfig reads the harness's MCP configuration.
	LoadConfig(ctx context.Context, scope Scope) (*config.HarnessMCPConfig, error)

	// SaveConfig writes the harness's MCP configuration.
	SaveConfig(ctx context.Context, scope Scope, cfg *config.HarnessMCPConfig) error

	// InstallWrapper injects tldr as the single MCP server in the harness.
	InstallWrapper(ctx context.Context, scope Scope) error

	// Rollback restores the harness config from the latest backup.
	Rollback(ctx context.Context, scope Scope) error

	// Reload triggers the harness to reload its MCP configuration if supported.
	Reload(ctx context.Context) error
}

// WrapperSpec describes how tldr should be registered in a harness.
type WrapperSpec struct {
	Name      string
	Command   string
	Args      []string
	Transport string
}

// DefaultWrapperSpec returns the default tldr wrapper spec.
func DefaultWrapperSpec() WrapperSpec {
	return WrapperSpec{
		Name:      "tldr",
		Command:   "tldr",
		Args:      []string{"serve"},
		Transport: "stdio",
	}
}

// TldrServerEntry returns the harness MCP config entry for tldr.
func TldrServerEntry() *config.HarnessMCPServer {
	return &config.HarnessMCPServer{
		Command: "tldr",
		Args:    []string{"serve"},
		Type:    "stdio",
	}
}

// DetectAll probes all known harnesses and returns which are available.
func DetectAll(ctx context.Context, adapters []Adapter) []Adapter {
	var found []Adapter
	for _, a := range adapters {
		ok, err := a.Detect(ctx)
		if err == nil && ok {
			found = append(found, a)
		}
	}
	return found
}

// Get returns the adapter with the given name.
func Get(name string, adapters []Adapter) (Adapter, error) {
	for _, a := range adapters {
		if a.Name() == name {
			return a, nil
		}
	}
	return nil, fmt.Errorf("unsupported harness: %s", name)
}
