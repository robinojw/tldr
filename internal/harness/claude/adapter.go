// Package claude implements the harness adapter for Claude Code / Claude Desktop.
// Claude Code uses .mcp.json at the project root for local scope,
// and ~/.claude.json for global scope.
package claude

import (
	"context"
	"encoding/json"
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

func (a *Adapter) ConfigPath(ctx context.Context, scope harness.Scope) (string, error) {
	home, _ := os.UserHomeDir()
	if scope == harness.ScopeGlobal {
		return filepath.Join(home, ".claude.json"), nil
	}

	cwd, _ := os.Getwd()
	return filepath.Join(cwd, ".mcp.json"), nil
}

func (a *Adapter) LoadConfig(ctx context.Context, scope harness.Scope) (*config.HarnessMCPConfig, error) {
	path, err := a.ConfigPath(ctx, scope)
	if err != nil {
		return nil, err
	}

	cfg := &config.HarnessMCPConfig{
		MCPServers: make(map[string]*config.HarnessMCPServer),
	}

	if _, err := os.Stat(path); err != nil {
		return cfg, nil
	}

	if scope == harness.ScopeGlobal {
		root, err := loadClaudeRoot(path)
		if err != nil {
			return nil, fmt.Errorf("failed to load claude config at %s: %w", path, err)
		}
		cfg.MCPServers = extractMCPServers(root["mcpServers"])
		return cfg, nil
	}

	if err := config.LoadJSON(path, cfg); err != nil {
		return nil, fmt.Errorf("failed to load claude config at %s: %w", path, err)
	}

	return cfg, nil
}

func (a *Adapter) SaveConfig(ctx context.Context, scope harness.Scope, cfg *config.HarnessMCPConfig) error {
	path, err := a.ConfigPath(ctx, scope)
	if err != nil {
		return err
	}

	if scope == harness.ScopeGlobal {
		root, err := loadClaudeRoot(path)
		if err != nil {
			return fmt.Errorf("failed to load claude config at %s: %w", path, err)
		}
		root["mcpServers"] = cfg.MCPServers
		return saveClaudeRoot(path, root)
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
		return fmt.Errorf("failed to install tldr in claude config: %w", err)
	}

	log.Info("installed tldr wrapper in claude %s config: %s", scope, path)
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
	// Claude Code doesn't have a reload command -- user must restart
	log.Info("claude code does not support config reload; restart the session to pick up changes")
	return nil
}

func loadClaudeRoot(path string) (map[string]any, error) {
	root := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return root, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return root, nil
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	return root, nil
}

func saveClaudeRoot(path string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func extractMCPServers(raw any) map[string]*config.HarnessMCPServer {
	servers := make(map[string]*config.HarnessMCPServer)
	entries, ok := raw.(map[string]any)
	if !ok {
		return servers
	}

	for name, value := range entries {
		serverMap, ok := value.(map[string]any)
		if !ok {
			continue
		}
		data, err := json.Marshal(serverMap)
		if err != nil {
			continue
		}
		var server config.HarnessMCPServer
		if err := json.Unmarshal(data, &server); err != nil {
			continue
		}
		servers[name] = &server
	}

	return servers
}
