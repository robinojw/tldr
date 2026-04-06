package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

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
				logging.SetGlobalLevel(logging.LevelDebug)
			}

			ctx := context.Background()

			// Load registry
			reg, err := registry.Open()
			if err != nil {
				return fmt.Errorf("failed to open registry: %w", err)
			}

			// Connect to all wrapped servers in parallel.
			// Each server connects in its own goroutine so a slow or
			// unreachable server doesn't block the others.
			servers := reg.WrappedServers()

			type connResult struct {
				name   string
				client *mcpclient.Client
				index  *compiler.CapabilityIndex
			}

			var (
				mu      sync.Mutex
				wg      sync.WaitGroup
				results []connResult
			)

			for _, s := range servers {
				wg.Add(1)
				go func(s *config.ServerEntry) {
					defer wg.Done()

					// Try to load cached capability index
					idx, err := compiler.Load(s.Name)
					if err != nil {
						// No cache -- need to introspect
						client, err := mcpclient.NewClient(s)
						if err != nil {
							logging.Default().Warn("skipping %s: %v", s.Name, err)
							return
						}

						if err := client.Connect(ctx); err != nil {
							logging.Default().Warn("skipping %s: %v", s.Name, err)
							client.Close()
							return
						}

						tools, err := client.ListTools(ctx)
						if err != nil {
							logging.Default().Warn("skipping %s: %v", s.Name, err)
							client.Close()
							return
						}

						idx = compiler.Compile(s.Name, tools)
						_ = idx.Save(s.Name)

						mu.Lock()
						results = append(results, connResult{s.Name, client, idx})
						mu.Unlock()
						return
					}

					// Cache hit -- still need to connect for tool calls
					client, err := mcpclient.NewClient(s)
					if err != nil {
						logging.Default().Warn("skipping %s: %v", s.Name, err)
						return
					}
					if err := client.Connect(ctx); err != nil {
						logging.Default().Warn("skipping %s: %v", s.Name, err)
						client.Close()
						return
					}

					mu.Lock()
					results = append(results, connResult{s.Name, client, idx})
					mu.Unlock()
				}(s)
			}

			wg.Wait()

			// Collect clients and indexes from parallel results
			clients := make(map[string]*mcpclient.Client, len(results))
			indexes := make([]*compiler.CapabilityIndex, 0, len(results))
			for _, r := range results {
				clients[r.name] = r.client
				indexes = append(indexes, r.index)
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
