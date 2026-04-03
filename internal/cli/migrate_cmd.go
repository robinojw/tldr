package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/robinojw/tldr/internal/backup"
	"github.com/robinojw/tldr/internal/harness"
	"github.com/robinojw/tldr/internal/logging"
	"github.com/robinojw/tldr/internal/registry"
	"github.com/robinojw/tldr/pkg/config"
	"github.com/spf13/cobra"
)

var migrateLog = logging.New("cli:migrate")

func newMigrateCmd() *cobra.Command {
	var (
		harnessName string
		dryRun      bool
		scopeRaw    string
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate existing MCP servers from a harness into tldr (one-shot)",
		Long: `Read all MCP servers from a harness config, import each one into tldr's
registry, build capability indexes, then rewrite the harness config so it
points only at tldr. The original config is backed up before any changes.

If no --harness flag is given, tldr detects all installed harnesses and
migrates from every one it finds.

This is the fastest way to get started: one command replaces your entire
MCP setup with tldr's compressed surface.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			adapters := AllAdapters()

			scope, err := harness.ParseScope(scopeRaw)
			if err != nil {
				return err
			}

			var targets []harness.Adapter
			if harnessName != "" {
				a, err := harness.Get(harnessName, adapters)
				if err != nil {
					return err
				}
				targets = []harness.Adapter{a}
			} else {
				targets = harness.DetectAll(ctx, adapters)
				if len(targets) == 0 {
					fmt.Println("No supported harnesses detected.")
					return nil
				}
			}

			reg, err := registry.Open()
			if err != nil {
				return fmt.Errorf("failed to open registry: %w", err)
			}

			totalImported := 0

			for _, adapter := range targets {
				ok, err := adapter.Detect(ctx)
				if err != nil || !ok {
					continue
				}

				path, _ := adapter.ConfigPath(ctx, scope)
				fmt.Printf("\n--- %s (%s, %s scope) ---\n", adapter.Name(), path, scope)

				cfg, err := adapter.LoadConfig(ctx, scope)
				if err != nil {
					fmt.Printf("  Failed to load config: %v\n", err)
					continue
				}

				if len(cfg.MCPServers) == 0 {
					fmt.Println("  No MCP servers found in config.")
					continue
				}

				// Skip if the only entry is already tldr
				if len(cfg.MCPServers) == 1 {
					if _, exists := cfg.MCPServers["tldr"]; exists {
						fmt.Println("  Already migrated (only tldr entry present).")
						continue
					}
				}

				imported := 0
				for name, server := range cfg.MCPServers {
					if name == "tldr" {
						continue
					}

					entry := convertHarnessEntry(name, server)
					entry.Harnesses = []string{adapter.Name()}

					if dryRun {
						fmt.Printf("  [dry-run] Would import: %s (%s)\n", name, entry.Transport)
					} else {
						if err := reg.AddServer(entry); err != nil {
							fmt.Printf("  Failed to import %s: %v\n", name, err)
							continue
						}
						if err := reg.SetWrapped(name, true); err != nil {
							migrateLog.Warn("failed to mark %s as wrapped: %v", name, err)
						}
						fmt.Printf("  Imported: %s (%s)\n", name, entry.Transport)
					}
					imported++
				}

				if dryRun {
					fmt.Printf("  Would import %d servers, then replace harness config with tldr entry.\n", imported)
					continue
				}

				if imported == 0 {
					continue
				}

				// Backup the original config
				if _, backupErr := backup.Backup(path); backupErr != nil {
					migrateLog.Warn("failed to backup %s: %v", path, backupErr)
				}

				// Rewrite harness config: only tldr
				newCfg := &config.HarnessMCPConfig{
					MCPServers: map[string]*config.HarnessMCPServer{
						"tldr": harness.TldrServerEntry(),
					},
				}
				if err := adapter.SaveConfig(ctx, scope, newCfg); err != nil {
					fmt.Printf("  Failed to rewrite config: %v\n", err)
					fmt.Println("  Your original servers are imported into tldr but the harness config was not updated.")
					fmt.Println("  Run 'tldr install --harness " + adapter.Name() + " --scope " + string(scope) + "' to complete the migration.")
					continue
				}

				_ = adapter.Reload(ctx)

				totalImported += imported
				fmt.Printf("  Migrated %d servers. Harness config now points only at tldr.\n", imported)

				// Record wrapper
				serverNames := make([]string, 0)
				for name := range cfg.MCPServers {
					if name != "tldr" {
						serverNames = append(serverNames, name)
					}
				}
				_ = reg.AddWrapper(&config.WrapperEntry{
					Harness:    adapter.Name(),
					Servers:    serverNames,
					ConfigPath: path,
				})
			}

			if dryRun {
				fmt.Println("\nDry run complete. No changes were made.")
				return nil
			}

			if totalImported > 0 {
				fmt.Printf("\nDone. Imported %d servers total.\n", totalImported)
				fmt.Println("Run 'tldr mcp list' to see them.")
				fmt.Println("Run 'tldr rollback --harness <name> --scope " + string(scope) + "' to undo any harness.")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&harnessName, "harness", "", "Migrate from a specific harness only (forge, claude, codex, opencode)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be migrated without making changes")
	cmd.Flags().StringVar(&scopeRaw, "scope", "global", "Migration scope (global or local)")

	return cmd
}

// convertHarnessEntry turns a harness MCP server config into a tldr ServerEntry.
func convertHarnessEntry(name string, h *config.HarnessMCPServer) *config.ServerEntry {
	entry := &config.ServerEntry{
		Name:    name,
		Env:     h.Env,
		Headers: h.Headers,
		Timeout: h.Timeout,
	}

	// Determine transport
	url := h.URL
	if url == "" {
		url = h.ServerURL
	}

	if url != "" {
		entry.Transport = config.TransportHTTP
		entry.URL = url
		if h.Transport == "sse" || h.Type == "sse" {
			entry.Transport = config.TransportSSE
		}
	} else if h.Command != "" {
		entry.Transport = config.TransportStdio
		entry.Command = h.Command
		entry.Args = h.Args
	} else {
		// Fallback: treat the name as a guess
		entry.Transport = config.TransportStdio
		entry.Command = name
	}

	if entry.Env == nil {
		entry.Env = make(map[string]string)
	}
	if entry.Headers == nil {
		entry.Headers = make(map[string]string)
	}

	// Preserve disabled state
	if h.Disable {
		entry.Disabled = true
	}

	return entry
}

// formatEnv formats env vars for display.
func formatEnv(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	var parts []string
	for k := range env {
		parts = append(parts, k+"=***")
	}
	return " env:[" + strings.Join(parts, ",") + "]"
}
