// Package codex implements the harness adapter for OpenAI Codex CLI.
// Codex uses .mcp.json at the project root for local scope
// and ~/.codex/config.toml for global scope.
package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
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

func (a *Adapter) ConfigPath(ctx context.Context, scope harness.Scope) (string, error) {
	if scope == harness.ScopeGlobal {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".codex", "config.toml"), nil
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
		root, err := loadCodexRoot(path)
		if err != nil {
			return nil, fmt.Errorf("failed to load codex config at %s: %w", path, err)
		}
		cfg.MCPServers = extractCodexServers(root["mcp_servers"])
		return cfg, nil
	}

	if err := config.LoadJSON(path, cfg); err != nil {
		return nil, fmt.Errorf("failed to load codex config at %s: %w", path, err)
	}

	return cfg, nil
}

func (a *Adapter) SaveConfig(ctx context.Context, scope harness.Scope, cfg *config.HarnessMCPConfig) error {
	path, err := a.ConfigPath(ctx, scope)
	if err != nil {
		return err
	}

	if scope == harness.ScopeGlobal {
		root, err := loadCodexRoot(path)
		if err != nil {
			return fmt.Errorf("failed to load codex config at %s: %w", path, err)
		}
		root["mcp_servers"] = encodeCodexServers(cfg.MCPServers)
		return saveCodexRoot(path, root)
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

	// Replace all existing servers with just tldr
	cfg.MCPServers = map[string]*config.HarnessMCPServer{
		"tldr": harness.TldrServerEntry(),
	}

	// Save
	if err := a.SaveConfig(ctx, scope, cfg); err != nil {
		return fmt.Errorf("failed to install tldr in codex config: %w", err)
	}

	log.Info("installed tldr wrapper in codex %s config: %s", scope, path)
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
	// Codex doesn't have a dedicated reload command
	log.Info("codex does not support config reload; restart the session to pick up changes")
	return nil
}

func loadCodexRoot(path string) (map[string]any, error) {
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
	if err := toml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	return root, nil
}

func saveCodexRoot(path string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := toml.Marshal(root)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func extractCodexServers(raw any) map[string]*config.HarnessMCPServer {
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
		server := &config.HarnessMCPServer{}
		if v, ok := serverMap["command"].(string); ok {
			server.Command = v
		}
		if args, ok := serverMap["args"].([]any); ok {
			server.Args = toStringSlice(args)
		}
		if url, ok := serverMap["url"].(string); ok {
			server.URL = url
		}
		if timeout, ok := serverMap["timeout"].(int64); ok {
			server.Timeout = int(timeout)
		}
		if env, ok := serverMap["env"].(map[string]any); ok {
			server.Env = toStringMap(env)
		}
		servers[name] = server
	}

	return servers
}

func encodeCodexServers(servers map[string]*config.HarnessMCPServer) map[string]any {
	encoded := make(map[string]any, len(servers))
	for name, server := range servers {
		entry := map[string]any{}
		if server.Command != "" {
			entry["command"] = server.Command
		}
		if len(server.Args) > 0 {
			entry["args"] = server.Args
		}
		if len(server.Env) > 0 {
			entry["env"] = server.Env
		}
		if url := firstNonEmpty(server.URL, server.ServerURL); url != "" {
			entry["url"] = url
		}
		if server.Timeout > 0 {
			entry["timeout"] = server.Timeout
		}
		encoded[name] = entry
	}
	return encoded
}

func toStringSlice(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if s, ok := value.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toStringMap(values map[string]any) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		if s, ok := value.(string); ok {
			out[key] = s
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
