package cli

import (
	"context"
	"fmt"

	"github.com/robinwhite/gobbler/internal/compiler"
	"github.com/robinwhite/gobbler/internal/mcpclient"
	"github.com/robinwhite/gobbler/internal/registry"
	"github.com/spf13/cobra"
)

func newWrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wrap <server...>",
		Short: "Build capability index for upstream MCP servers",
		Long: `Connect to the specified upstream MCP servers, introspect their tools,
and build gobbler's compressed capability index. This index is what powers
the search_tools command in the wrapper.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			reg, err := registry.Open()
			if err != nil {
				return fmt.Errorf("failed to open registry: %w", err)
			}

			for _, name := range args {
				entry, ok := reg.GetServer(name)
				if !ok {
					fmt.Printf("Server %q not found in registry. Skipping.\n", name)
					continue
				}

				fmt.Printf("Connecting to %s...\n", name)

				client, err := mcpclient.NewClient(entry)
				if err != nil {
					fmt.Printf("Failed to create client for %s: %v\n", name, err)
					continue
				}

				if err := client.Connect(ctx); err != nil {
					fmt.Printf("Failed to connect to %s: %v\n", name, err)
					client.Close()
					continue
				}

				tools, err := client.ListTools(ctx)
				if err != nil {
					fmt.Printf("Failed to list tools from %s: %v\n", name, err)
					client.Close()
					continue
				}

				client.Close()

				// Compile capability index
				idx := compiler.Compile(name, tools)
				if err := idx.Save(name); err != nil {
					fmt.Printf("Failed to save capabilities for %s: %v\n", name, err)
					continue
				}

				// Mark as wrapped in registry
				if err := reg.SetWrapped(name, true); err != nil {
					fmt.Printf("Warning: failed to mark %s as wrapped: %v\n", name, err)
				}

				stats := idx.ServerStats[name]
				fmt.Printf("Wrapped %s: %d tools -> ~%d schema tokens -> ~%d wrapped tokens (%.0f%% reduction)\n",
					name,
					stats.ToolCount,
					stats.SchemaTokens,
					stats.WrappedTokens,
					100*(1-float64(stats.WrappedTokens)/float64(max(stats.SchemaTokens, 1))),
				)
			}

			fmt.Println("\nRun 'gobbler install --harness <name>' to wire into your coding harness.")
			return nil
		},
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
