// Package cli implements all gobbler CLI commands using cobra.
package cli

import (
	"github.com/robinwhite/gobbler/internal/harness"
	"github.com/robinwhite/gobbler/internal/harness/claude"
	"github.com/robinwhite/gobbler/internal/harness/codex"
	"github.com/robinwhite/gobbler/internal/harness/forge"
	"github.com/spf13/cobra"
)

// AllAdapters returns all known harness adapters.
func AllAdapters() []harness.Adapter {
	return []harness.Adapter{
		&forge.Adapter{},
		&claude.Adapter{},
		&codex.Adapter{},
	}
}

// NewRootCmd creates the root gobbler CLI command.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "gobbler",
		Short: "A local MCP gateway that compresses tool surfaces and shields models from large API responses",
		Long: `Gobbler is a local MCP gateway for coding harnesses that reduces token usage
by replacing many upstream MCP tools with a small wrapper surface and prevents
large intermediate API responses from reaching the model.

Instead of exposing dozens of MCP tools directly to your coding harness (Claude Code,
ForgeCode, Codex), gobbler sits in between and provides just 4 tools:
  - search_tools: discover capabilities across all registered servers
  - execute_plan: run multi-step plans with response shielding  
  - call_raw: direct tool calls (escape hatch)
  - inspect_tool: get parameter details for a specific tool`,
	}

	root.AddCommand(
		newHarnessCmd(),
		newMCPCmd(),
		newWrapCmd(),
		newInstallCmd(),
		newRollbackCmd(),
		newDoctorCmd(),
		newServeCmd(),
	)

	return root
}
