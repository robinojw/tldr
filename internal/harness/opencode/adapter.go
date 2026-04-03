// Package opencode implements the harness adapter for OpenCode.
// OpenCode uses opencode.json at the project root for local scope
// and ~/.config/opencode/opencode.json for global scope.
package opencode

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
	"github.com/tailscale/hujson"
)

var log = logging.New("harness:opencode")

// Adapter implements harness.Adapter for OpenCode.
type Adapter struct{}

var _ harness.Adapter = (*Adapter)(nil)

func (a *Adapter) Name() string { return "opencode" }

func (a *Adapter) Detect(ctx context.Context) (bool, error) {
	if _, err := exec.LookPath("opencode"); err == nil {
		log.Debug("detected opencode binary")
		return true, nil
	}

	path, err := a.ConfigPath(ctx, harness.ScopeGlobal)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err == nil {
		log.Debug("detected opencode config: %s", path)
		return true, nil
	}

	jsoncPath := stringsReplaceSuffix(path, ".json", ".jsonc")
	if _, err := os.Stat(jsoncPath); err == nil {
		log.Debug("detected opencode config: %s", jsoncPath)
		return true, nil
	}

	return false, nil
}

func (a *Adapter) ConfigPath(ctx context.Context, scope harness.Scope) (string, error) {
	if scope == harness.ScopeLocal {
		cwd, _ := os.Getwd()
		jsoncPath := filepath.Join(cwd, "opencode.jsonc")
		if _, err := os.Stat(jsoncPath); err == nil {
			return jsoncPath, nil
		}
		return filepath.Join(cwd, "opencode.json"), nil
	}

	configDir, err := opencodeConfigDir()
	if err != nil {
		return "", err
	}
	jsonPath := filepath.Join(configDir, "opencode.json")
	if _, err := os.Stat(jsonPath); err == nil {
		return jsonPath, nil
	}
	jsoncPath := filepath.Join(configDir, "opencode.jsonc")
	if _, err := os.Stat(jsoncPath); err == nil {
		return jsoncPath, nil
	}
	return jsonPath, nil
}

func (a *Adapter) LoadConfig(ctx context.Context, scope harness.Scope) (*config.HarnessMCPConfig, error) {
	path, err := a.ConfigPath(ctx, scope)
	if err != nil {
		return nil, err
	}

	cfg := &config.HarnessMCPConfig{MCPServers: make(map[string]*config.HarnessMCPServer)}
	root, err := loadOpenCodeRoot(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load opencode config at %s: %w", path, err)
	}

	cfg.MCPServers = extractOpenCodeServers(root["mcp"])
	return cfg, nil
}

func (a *Adapter) SaveConfig(ctx context.Context, scope harness.Scope, cfg *config.HarnessMCPConfig) error {
	path, err := a.ConfigPath(ctx, scope)
	if err != nil {
		return err
	}

	root, err := loadOpenCodeRoot(path)
	if err != nil {
		return fmt.Errorf("failed to load opencode config at %s: %w", path, err)
	}
	if len(root) == 0 {
		root["$schema"] = "https://opencode.ai/config.json"
	}

	existing := rawServerMap(root["mcp"])
	updated := make(map[string]any, len(cfg.MCPServers))
	for name, server := range cfg.MCPServers {
		base := cloneMap(existing[name])
		updated[name] = encodeOpenCodeServer(base, server)
	}
	root["mcp"] = updated
	return saveOpenCodeRoot(path, root)
}

func (a *Adapter) InstallWrapper(ctx context.Context, scope harness.Scope) error {
	path, err := a.ConfigPath(ctx, scope)
	if err != nil {
		return err
	}

	if _, err := os.Stat(path); err == nil {
		if _, err := backup.Backup(path); err != nil {
			log.Warn("failed to backup config: %v", err)
		}
	}

	cfg, err := a.LoadConfig(ctx, scope)
	if err != nil {
		cfg = &config.HarnessMCPConfig{MCPServers: make(map[string]*config.HarnessMCPServer)}
	}
	// Replace all existing servers with just tldr
	cfg.MCPServers = map[string]*config.HarnessMCPServer{
		"tldr": harness.TldrServerEntry(),
	}

	if err := a.SaveConfig(ctx, scope, cfg); err != nil {
		return fmt.Errorf("failed to install tldr in opencode config: %w", err)
	}

	log.Info("installed tldr wrapper in opencode %s config: %s", scope, path)
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
	log.Info("opencode does not support config reload; restart the session to pick up changes")
	return nil
}

func opencodeConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

func loadOpenCodeRoot(path string) (map[string]any, error) {
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
	standard, err := hujson.Standardize(data)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(standard, &root); err != nil {
		return nil, err
	}
	return root, nil
}

func saveOpenCodeRoot(path string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

func extractOpenCodeServers(raw any) map[string]*config.HarnessMCPServer {
	servers := make(map[string]*config.HarnessMCPServer)
	for name, entry := range rawServerMap(raw) {
		server := &config.HarnessMCPServer{}
		typ, _ := entry["type"].(string)
		server.Type = typ

		if enabled, ok := entry["enabled"].(bool); ok && !enabled {
			server.Disable = true
		}
		if timeout, ok := intFromAny(entry["timeout"]); ok {
			server.Timeout = timeout
		}

		switch typ {
		case "remote":
			if url, ok := entry["url"].(string); ok {
				server.URL = url
			}
			if headers, ok := entry["headers"].(map[string]any); ok {
				server.Headers = toStringMap(headers)
			}
		default:
			command, args := decodeCommand(entry["command"])
			server.Command = command
			server.Args = args
			if env, ok := entry["environment"].(map[string]any); ok {
				server.Env = toStringMap(env)
			}
		}

		servers[name] = server
	}
	return servers
}

func encodeOpenCodeServer(base map[string]any, server *config.HarnessMCPServer) map[string]any {
	entry := cloneMap(base)
	if entry == nil {
		entry = map[string]any{}
	}

	if server.URL != "" || server.ServerURL != "" {
		entry["type"] = "remote"
		entry["url"] = firstNonEmpty(server.URL, server.ServerURL)
		if len(server.Headers) > 0 {
			entry["headers"] = server.Headers
		} else {
			delete(entry, "headers")
		}
		delete(entry, "command")
		delete(entry, "environment")
	} else {
		entry["type"] = "local"
		command := append([]string{}, server.Args...)
		if server.Command != "" {
			command = append([]string{server.Command}, command...)
		}
		entry["command"] = command
		if len(server.Env) > 0 {
			entry["environment"] = server.Env
		} else {
			delete(entry, "environment")
		}
		delete(entry, "url")
		delete(entry, "headers")
	}

	entry["enabled"] = !server.Disable
	if server.Timeout > 0 {
		entry["timeout"] = server.Timeout
	} else {
		delete(entry, "timeout")
	}

	return entry
}

func rawServerMap(raw any) map[string]map[string]any {
	out := map[string]map[string]any{}
	entries, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	for name, value := range entries {
		if entry, ok := value.(map[string]any); ok {
			out[name] = entry
		}
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func decodeCommand(raw any) (string, []string) {
	switch value := raw.(type) {
	case string:
		return value, nil
	case []any:
		parts := toStringSlice(value)
		if len(parts) == 0 {
			return "", nil
		}
		return parts[0], parts[1:]
	case []string:
		if len(value) == 0 {
			return "", nil
		}
		return value[0], append([]string{}, value[1:]...)
	default:
		return "", nil
	}
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

func intFromAny(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func stringsReplaceSuffix(value, old, new string) string {
	if filepath.Ext(value) != old {
		return value
	}
	return value[:len(value)-len(old)] + new
}
