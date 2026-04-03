package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/robinojw/tldr/internal/compiler"
	"github.com/robinojw/tldr/internal/logging"
	"github.com/robinojw/tldr/internal/mcpclient"
	"github.com/robinojw/tldr/internal/registry"
	"github.com/robinojw/tldr/internal/wrapper"
	"github.com/robinojw/tldr/pkg/config"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	var verbose bool

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the tldr MCP wrapper server (stdio)",
		Long: `Start the tldr wrapper MCP server on stdio. This is the command that
coding harnesses invoke to communicate with tldr. It exposes the
compressed tool surface (search_tools, execute_plan, call_raw, inspect_tool)
and internally connects to all registered upstream MCP servers.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if verbose {
				logging.Default().SetLevel(logging.LevelDebug)
			}

			ctx := context.Background()

			// Load registry
			reg, err := registry.Open()
			if err != nil {
				return fmt.Errorf("failed to open registry: %w", err)
			}

			// Connect to all wrapped servers
			servers := reg.WrappedServers()
			clients := make(map[string]*mcpclient.Client)
			var indexes []*compiler.CapabilityIndex

			for _, s := range servers {
				// Try to load cached capability index
				idx, err := compiler.Load(s.Name)
				if err != nil {
					// Need to introspect
					client, err := mcpclient.NewClient(s)
					if err != nil {
						logging.Default().Warn("skipping %s: %v", s.Name, err)
						continue
					}

					if err := client.Connect(ctx); err != nil {
						logging.Default().Warn("skipping %s: %v", s.Name, err)
						client.Close()
						continue
					}

					tools, err := client.ListTools(ctx)
					if err != nil {
						logging.Default().Warn("skipping %s: %v", s.Name, err)
						client.Close()
						continue
					}

					idx = compiler.Compile(s.Name, tools)
					_ = idx.Save(s.Name)
					clients[s.Name] = client
				} else {
					// Still need to connect for tool calls
					client, err := mcpclient.NewClient(s)
					if err != nil {
						logging.Default().Warn("skipping %s: %v", s.Name, err)
						continue
					}
					if err := client.Connect(ctx); err != nil {
						logging.Default().Warn("skipping %s: %v", s.Name, err)
						client.Close()
						continue
					}
					clients[s.Name] = client
				}

				indexes = append(indexes, idx)
			}

			// Merge all indexes
			merged := compiler.Merge(indexes...)

			// Create and start wrapper server
			policyCfg := reg.Policy()
			diskPath := filepath.Join(config.TldrDir(), "results")
			srv := wrapper.NewServer(merged, clients, policyCfg, diskPath)

			// Clean up clients on exit
			defer func() {
				for _, c := range clients {
					c.Close()
				}
			}()

			return srv.Serve()
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose logging")
	return cmd
}
