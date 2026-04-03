// Package config defines configuration types for the tldr registry,
// harness adapters, upstream MCP servers, and output policies.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Version is the current tldr version. Set at build time via:
//
//	go build -ldflags "-X github.com/robinojw/tldr/pkg/config.Version=v1.0.0"
//
// Defaults to "dev" for local builds.
var Version = "dev"

// TldrConfig is the top-level tldr configuration.
type TldrConfig struct {
	Servers    map[string]*ServerEntry `json:"servers"`
	Wrappers   map[string]*WrapperEntry `json:"wrappers,omitempty"`
	Policies   *PolicyConfig            `json:"policies,omitempty"`
	LastUpdate time.Time                `json:"lastUpdate,omitempty"`
}

// NewTldrConfig returns an initialized empty config.
func NewTldrConfig() *TldrConfig {
	return &TldrConfig{
		Servers:  make(map[string]*ServerEntry),
		Wrappers: make(map[string]*WrapperEntry),
		Policies: DefaultPolicyConfig(),
	}
}

// ServerEntry represents a registered upstream MCP server in tldr's registry.
type ServerEntry struct {
	Name      string            `json:"name"`
	Transport TransportType     `json:"transport"`
	Command   string            `json:"command,omitempty"`   // stdio
	Args      []string          `json:"args,omitempty"`      // stdio
	URL       string            `json:"url,omitempty"`       // http/sse
	Headers   map[string]string `json:"headers,omitempty"`   // http/sse
	Env       map[string]string `json:"env,omitempty"`       // environment variables
	Timeout   int               `json:"timeout,omitempty"`   // seconds
	Disabled  bool              `json:"disabled,omitempty"`
	Wrapped   bool              `json:"wrapped,omitempty"`   // whether tldr wraps this
	Harnesses []string          `json:"harnesses,omitempty"` // which harnesses see this
	AddedAt   time.Time         `json:"addedAt,omitempty"`
}

// TransportType identifies the MCP transport protocol.
type TransportType string

const (
	TransportStdio TransportType = "stdio"
	TransportHTTP  TransportType = "http"
	TransportSSE   TransportType = "sse"
)

// WrapperEntry tracks a tldr wrapper installation for a harness.
type WrapperEntry struct {
	Harness    string    `json:"harness"`
	Servers    []string  `json:"servers"`   // upstream server names routed through this wrapper
	ConfigPath string    `json:"configPath"`
	Installed  bool      `json:"installed"`
	InstalledAt time.Time `json:"installedAt,omitempty"`
}

// PolicyConfig defines output shielding and safety policies.
type PolicyConfig struct {
	MaxOutputBytes   int      `json:"maxOutputBytes"`
	MaxOutputTokens  int      `json:"maxOutputTokens"`  // approximate token limit (1 token ~ 4 chars)
	MaxArrayLength   int      `json:"maxArrayLength"`
	MaxStringLength  int      `json:"maxStringLength"`
	MaxFieldBytes    int      `json:"maxFieldBytes"`     // auto-compact fields larger than this in array elements
	CompactArrays    bool     `json:"compactArrays"`     // auto-strip heavy fields from array-of-object responses
	StepTimeout      int      `json:"stepTimeout"`       // seconds per step
	PlanTimeout      int      `json:"planTimeout"`       // seconds per plan
	MaxSteps         int      `json:"maxSteps"`
	AllowMutating    bool     `json:"allowMutating"`
	BlockedTools     []string `json:"blockedTools,omitempty"`
	RawModeEnabled   bool     `json:"rawModeEnabled"`
}

// DefaultPolicyConfig returns sensible default policies.
func DefaultPolicyConfig() *PolicyConfig {
	return &PolicyConfig{
		MaxOutputBytes:  64 * 1024, // 64KB
		MaxOutputTokens: 4096,      // ~16KB of text
		MaxArrayLength:  50,
		MaxStringLength: 8192,
		MaxFieldBytes:   256,       // auto-compact fields larger than this
		CompactArrays:   true,      // auto-strip heavy fields from arrays
		StepTimeout:     30,
		PlanTimeout:     120,
		MaxSteps:        10,
		AllowMutating:   false,
		RawModeEnabled:  false,
	}
}

// HarnessMCPConfig represents the standard .mcp.json format used by
// Claude Code and ForgeCode.
type HarnessMCPConfig struct {
	MCPServers map[string]*HarnessMCPServer `json:"mcpServers"`
}

// HarnessMCPServer is a single MCP server entry in a harness config.
type HarnessMCPServer struct {
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	ServerURL string            `json:"serverUrl,omitempty"` // ForgeCode alias
	Headers   map[string]string `json:"headers,omitempty"`
	Transport string            `json:"transport,omitempty"`
	Type      string            `json:"type,omitempty"` // Claude Code uses "type": "http"
	Timeout   int               `json:"timeout,omitempty"`
	Disable   bool              `json:"disable,omitempty"`
}

// TldrDir returns the path to the tldr config directory.
func TldrDir() string {
	if dir := os.Getenv("TLDR_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "tldr")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "tldr")
		}
		return filepath.Join(home, "tldr")
	default: // linux, etc.
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "tldr")
		}
		return filepath.Join(home, ".config", "tldr")
	}
}

// ServersPath returns the path to the servers registry file.
func ServersPath() string {
	return filepath.Join(TldrDir(), "servers.json")
}

// CapabilitiesDir returns the path to the capabilities index directory.
func CapabilitiesDir() string {
	return filepath.Join(TldrDir(), "capabilities")
}

// BackupDir returns the path to the backup directory.
func BackupDir() string {
	return filepath.Join(TldrDir(), "backups")
}

// LogDir returns the path to the log directory.
func LogDir() string {
	return filepath.Join(TldrDir(), "logs")
}

// LoadJSON loads a JSON file into the given target.
func LoadJSON(path string, target interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// SaveJSON writes the given value as JSON to the file path.
func SaveJSON(path string, value interface{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
