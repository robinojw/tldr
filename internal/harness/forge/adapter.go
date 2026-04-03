// Package forge implements the harness adapter for ForgeCode.
// ForgeCode uses .mcp.json at the project root for local scope
// and ~/forge/.mcp.json for global scope.
package forge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/robinojw/tldr/internal/backup"
	"github.com/robinojw/tldr/internal/harness"
	"github.com/robinojw/tldr/internal/logging"
	"github.com/robinojw/tldr/pkg/config"
)

var log = logging.New("harness:forge")

// Adapter implements harness.Adapter for ForgeCode.
type Adapter struct{}

var _ harness.Adapter = (*Adapter)(nil)

func (a *Adapter) Name() string { return "forge" }

func (a *Adapter) Detect(ctx context.Context) (bool, error) {
	// Check for forge binary
	if _, err := exec.LookPath("forge"); err == nil {
		log.Debug("detected forge binary")
		return true, nil
	}

	// Check for config directories
	home, _ := os.UserHomeDir()
	for _, dir := range []string{filepath.Join(home, "forge"), filepath.Join(home, ".forge")} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			log.Debug("detected forge config dir: %s", dir)
			return true, nil
		}
	}

	return false, nil
}

func (a *Adapter) ConfigPath(ctx context.Context, scope harness.Scope) (string, error) {
	if scope == harness.ScopeLocal {
		cwd, _ := os.Getwd()
		return filepath.Join(cwd, ".mcp.json"), nil
	}

	home, _ := os.UserHomeDir()
	preferred := filepath.Join(home, "forge", ".mcp.json")
	legacy := filepath.Join(home, ".forge", ".mcp.json")
	if _, err := os.Stat(preferred); err == nil {
		return preferred, nil
	}
	if _, err := os.Stat(legacy); err == nil {
		return legacy, nil
	}
	return preferred, nil
}

func (a *Adapter) LoadConfig(ctx context.Context, scope harness.Scope) (*config.HarnessMCPConfig, error) {
	path, err := a.ConfigPath(ctx, scope)
	if err != nil {
		return nil, err
	}

	cfg := &config.HarnessMCPConfig{
		MCPServers: make(map[string]*config.HarnessMCPServer),
	}

	if _, err := os.Stat(path); err == nil {
		if err := config.LoadJSON(path, cfg); err != nil {
			return nil, fmt.Errorf("failed to load forge config at %s: %w", path, err)
		}
	}

	return cfg, nil
}

func (a *Adapter) SaveConfig(ctx context.Context, scope harness.Scope, cfg *config.HarnessMCPConfig) error {
	path, err := a.ConfigPath(ctx, scope)
	if err != nil {
		return err
	}
	return config.SaveJSON(path, cfg)
}

func (a *Adapter) InstallWrapper(ctx context.Context, scope harness.Scope) error {
	path, err := a.ConfigPath(ctx, scope)
	if err != nil {
		return err
	}

	// Backup existing config
	if _, err := os.Stat(path); err == nil {
		if _, err := backup.Backup(path); err != nil {
			log.Warn("failed to backup config: %v", err)
		}
	}

	// Load existing config
	cfg, err := a.LoadConfig(ctx, scope)
	if err != nil {
		cfg = &config.HarnessMCPConfig{
			MCPServers: make(map[string]*config.HarnessMCPServer),
		}
	}

	// Add tldr entry
	cfg.MCPServers["tldr"] = harness.TldrServerEntry()

	// Save
	if err := a.SaveConfig(ctx, scope, cfg); err != nil {
		return fmt.Errorf("failed to install tldr in forge: %w", err)
	}

	log.Info("installed tldr wrapper in forge %s config: %s", scope, path)

	// Try to reload
	_ = a.Reload(ctx)

	return nil
}

func (a *Adapter) Rollback(ctx context.Context, scope harness.Scope) error {
	path, err := a.ConfigPath(ctx, scope)
	if err != nil {
		return err
	}
	return backup.Restore(path)
}

func (a *Adapter) Reload(ctx context.Context) error {
	// ForgeCode supports `forge mcp reload`
	cmd := exec.CommandContext(ctx, "forge", "mcp", "reload")
	if err := cmd.Run(); err != nil {
		log.Debug("forge mcp reload not available: %v", err)
		return nil // not critical
	}
	log.Info("triggered forge mcp reload")
	return nil
}
