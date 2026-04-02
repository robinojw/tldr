// Package forge implements the harness adapter for ForgeCode.
// ForgeCode uses .mcp.json files at user (~/.forge/.mcp.json) and local (cwd/.mcp.json) scopes.
package forge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/robinwhite/gobbler/internal/backup"
	"github.com/robinwhite/gobbler/internal/harness"
	"github.com/robinwhite/gobbler/internal/logging"
	"github.com/robinwhite/gobbler/pkg/config"
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

	// Check for config directory
	home, _ := os.UserHomeDir()
	forgePath := filepath.Join(home, ".forge")
	if info, err := os.Stat(forgePath); err == nil && info.IsDir() {
		log.Debug("detected forge config dir: %s", forgePath)
		return true, nil
	}

	return false, nil
}

func (a *Adapter) ConfigPath(ctx context.Context) (string, error) {
	// Prefer local .mcp.json, then user-level
	cwd, _ := os.Getwd()
	localPath := filepath.Join(cwd, ".mcp.json")
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil
	}

	home, _ := os.UserHomeDir()
	userPath := filepath.Join(home, ".forge", ".mcp.json")
	return userPath, nil
}

func (a *Adapter) LoadConfig(ctx context.Context) (*config.HarnessMCPConfig, error) {
	path, err := a.ConfigPath(ctx)
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

func (a *Adapter) SaveConfig(ctx context.Context, cfg *config.HarnessMCPConfig) error {
	path, err := a.ConfigPath(ctx)
	if err != nil {
		return err
	}
	return config.SaveJSON(path, cfg)
}

func (a *Adapter) InstallWrapper(ctx context.Context) error {
	path, err := a.ConfigPath(ctx)
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
	cfg, err := a.LoadConfig(ctx)
	if err != nil {
		cfg = &config.HarnessMCPConfig{
			MCPServers: make(map[string]*config.HarnessMCPServer),
		}
	}

	// Add gobbler entry
	cfg.MCPServers["gobbler"] = harness.GobblerServerEntry()

	// Save
	if err := a.SaveConfig(ctx, cfg); err != nil {
		return fmt.Errorf("failed to install gobbler in forge: %w", err)
	}

	log.Info("installed gobbler wrapper in forge config: %s", path)

	// Try to reload
	_ = a.Reload(ctx)

	return nil
}

func (a *Adapter) Rollback(ctx context.Context) error {
	path, err := a.ConfigPath(ctx)
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
