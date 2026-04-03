// Package claude implements the harness adapter for Claude Code / Claude Desktop.
// Claude Code uses .mcp.json at the project root for project scope,
// and ~/.claude.json for user/local scopes.
package claude

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

var log = logging.New("harness:claude")

// Adapter implements harness.Adapter for Claude Code.
type Adapter struct{}

var _ harness.Adapter = (*Adapter)(nil)

func (a *Adapter) Name() string { return "claude" }

func (a *Adapter) Detect(ctx context.Context) (bool, error) {
	// Check for claude binary
	if _, err := exec.LookPath("claude"); err == nil {
		log.Debug("detected claude binary")
		return true, nil
	}

	// Check for Claude Desktop config
	home, _ := os.UserHomeDir()
	claudeJSON := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(claudeJSON); err == nil {
		log.Debug("detected claude config: %s", claudeJSON)
		return true, nil
	}

	return false, nil
}

func (a *Adapter) ConfigPath(ctx context.Context) (string, error) {
	// Claude Code uses .mcp.json at project root (project scope)
	cwd, _ := os.Getwd()
	projectPath := filepath.Join(cwd, ".mcp.json")
	return projectPath, nil
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
			return nil, fmt.Errorf("failed to load claude config at %s: %w", path, err)
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

	// Add tldr entry
	cfg.MCPServers["tldr"] = harness.TldrServerEntry()

	// Save
	if err := a.SaveConfig(ctx, cfg); err != nil {
		return fmt.Errorf("failed to install tldr in claude config: %w", err)
	}

	log.Info("installed tldr wrapper in claude config: %s", path)
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
	// Claude Code doesn't have a reload command -- user must restart
	log.Info("claude code does not support config reload; restart the session to pick up changes")
	return nil
}
