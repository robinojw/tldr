// Package codex implements the harness adapter for OpenAI Codex CLI.
// Codex uses .mcp.json for MCP server configuration (compatible with Claude Code format)
// and also supports its own .codex/config.toml.
package codex

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

var log = logging.New("harness:codex")

// Adapter implements harness.Adapter for Codex CLI.
type Adapter struct{}

var _ harness.Adapter = (*Adapter)(nil)

func (a *Adapter) Name() string { return "codex" }

func (a *Adapter) Detect(ctx context.Context) (bool, error) {
	// Check for codex binary
	if _, err := exec.LookPath("codex"); err == nil {
		log.Debug("detected codex binary")
		return true, nil
	}

	// Check for codex config directory
	home, _ := os.UserHomeDir()
	codexDir := filepath.Join(home, ".codex")
	if info, err := os.Stat(codexDir); err == nil && info.IsDir() {
		log.Debug("detected codex config dir: %s", codexDir)
		return true, nil
	}

	return false, nil
}

func (a *Adapter) ConfigPath(ctx context.Context) (string, error) {
	// Codex supports .mcp.json at project root
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, ".mcp.json"), nil
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
			return nil, fmt.Errorf("failed to load codex config at %s: %w", path, err)
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
		return fmt.Errorf("failed to install tldr in codex config: %w", err)
	}

	log.Info("installed tldr wrapper in codex config: %s", path)
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
	// Codex doesn't have a dedicated reload command
	log.Info("codex does not support config reload; restart the session to pick up changes")
	return nil
}
