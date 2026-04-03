package cli

import (
	"context"
	"fmt"

	"github.com/robinojw/tldr/internal/harness"
	"github.com/robinojw/tldr/internal/registry"
	"github.com/robinojw/tldr/pkg/config"
	"github.com/spf13/cobra"
)

func newInstallCmd() *cobra.Command {
	var harnessName string

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install tldr wrapper into a coding harness",
		Long: `Inject tldr as the MCP server in the specified harness's configuration.
The harness will only see tldr's 4 tools (search_tools, execute_plan,
call_raw, inspect_tool) instead of all the raw upstream MCP tools.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			adapters := AllAdapters()

			adapter, err := harness.Get(harnessName, adapters)
			if err != nil {
				return err
			}

			// Check harness is available
			ok, err := adapter.Detect(ctx)
			if err != nil || !ok {
				return fmt.Errorf("harness %q not detected on this system", harnessName)
			}

			// Install the wrapper
			if err := adapter.InstallWrapper(ctx); err != nil {
				return fmt.Errorf("failed to install: %w", err)
			}

			// Record in registry
			reg, err := registry.Open()
			if err != nil {
				fmt.Printf("Warning: failed to open registry: %v\n", err)
			} else {
				path, _ := adapter.ConfigPath(ctx)
				wrapped := reg.WrappedServers()
				serverNames := make([]string, len(wrapped))
				for i, s := range wrapped {
					serverNames[i] = s.Name
				}
				reg.AddWrapper(&config.WrapperEntry{
					Harness:    harnessName,
					Servers:    serverNames,
					ConfigPath: path,
				})
			}

			fmt.Printf("Tldr installed in %s.\n", harnessName)
			fmt.Println("The harness now sees only tldr's compressed tool surface.")
			fmt.Println("Use 'tldr rollback --harness " + harnessName + "' to undo.")
			return nil
		},
	}

	cmd.Flags().StringVar(&harnessName, "harness", "", "Target harness (forge, claude, codex)")
	cmd.MarkFlagRequired("harness")

	return cmd
}

func newRollbackCmd() *cobra.Command {
	var harnessName string

	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Restore the harness config from before tldr installation",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			adapters := AllAdapters()

			adapter, err := harness.Get(harnessName, adapters)
			if err != nil {
				return err
			}

			if err := adapter.Rollback(ctx); err != nil {
				return fmt.Errorf("rollback failed: %w", err)
			}

			fmt.Printf("Rolled back %s config to pre-tldr state.\n", harnessName)
			return nil
		},
	}

	cmd.Flags().StringVar(&harnessName, "harness", "", "Target harness (forge, claude, codex)")
	cmd.MarkFlagRequired("harness")

	return cmd
}
