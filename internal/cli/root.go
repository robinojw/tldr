// Package cli implements all tldr CLI commands using cobra.
package cli

import (
	"github.com/robinojw/tldr/internal/harness"
	"github.com/robinojw/tldr/internal/harness/claude"
	"github.com/robinojw/tldr/internal/harness/codex"
	"github.com/robinojw/tldr/internal/harness/forge"
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

// NewRootCmd creates the root tldr CLI command.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "tldr",
		Short: "A local MCP gateway that compresses tool surfaces and shields models from large API responses",
		Long: `Tldr is a local MCP gateway for coding harnesses that reduces token usage
by replacing many upstream MCP tools with a small wrapper surface and prevents
large intermediate API responses from reaching the model.

Instead of exposing dozens of MCP tools directly to your coding harness (Claude Code,
ForgeCode, Codex), tldr sits in between and provides 5 tools:
  - search_tools: discover capabilities across all registered servers
  - execute_plan: run multi-step plans with response shielding  
  - call_raw: direct tool calls (escape hatch)
  - inspect_tool: get parameter details for a specific tool
  - get_result: page through truncated results`,
	}

	root.AddCommand(
		newHarnessCmd(),
		newMCPCmd(),
		newWrapCmd(),
		newInstallCmd(),
		newRollbackCmd(),
		newMigrateCmd(),
		newDoctorCmd(),
		newServeCmd(),
	)

	return root
}
