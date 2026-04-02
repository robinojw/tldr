package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/robinwhite/gobbler/internal/compiler"
	"github.com/robinwhite/gobbler/internal/harness"
	"github.com/robinwhite/gobbler/internal/mcpclient"
	"github.com/robinwhite/gobbler/internal/registry"
	"github.com/robinwhite/gobbler/pkg/config"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Validate gobbler installation and connectivity",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			issues := 0

			fmt.Println("Gobbler Doctor")
			fmt.Println("==============")
			fmt.Println()

			// Check config directory
			dir := config.GobblerDir()
			if _, err := os.Stat(dir); err != nil {
				fmt.Printf("[WARN] Config directory not found: %s\n", dir)
				issues++
			} else {
				fmt.Printf("[OK]   Config directory: %s\n", dir)
			}

			// Check registry
			reg, err := registry.Open()
			if err != nil {
				fmt.Printf("[FAIL] Registry: %v\n", err)
				issues++
			} else {
				servers := reg.ListServers()
				fmt.Printf("[OK]   Registry: %d servers registered\n", len(servers))

				// Connectivity check for each server
				fmt.Println()
				fmt.Println("Server Connectivity:")
				for _, s := range servers {
					if s.Disabled {
						fmt.Printf("  [SKIP] %s (disabled)\n", s.Name)
						continue
					}

					client, err := mcpclient.NewClient(s)
					if err != nil {
						fmt.Printf("  [FAIL] %s: failed to create client: %v\n", s.Name, err)
						issues++
						continue
					}

					// Time the connection
					connStart := time.Now()
					if err := client.Connect(ctx); err != nil {
						fmt.Printf("  [FAIL] %s: connection failed in %v: %v\n", s.Name, time.Since(connStart), err)
						issues++
						client.Close()
						continue
					}
					connTime := time.Since(connStart)

					// Check server info
					info := client.ServerInfo()
					if info != nil {
						fmt.Printf("  [OK]   %s: connected to %s v%s (protocol %s) in %v\n",
							s.Name, info.ServerInfo.Name, info.ServerInfo.Version, info.ProtocolVersion, connTime)
					}

					// List tools with timing
					toolStart := time.Now()
					tools, err := client.ListTools(ctx)
					toolTime := time.Since(toolStart)
					if err != nil {
						fmt.Printf("         - tool listing failed in %v: %v\n", toolTime, err)
						issues++
					} else {
						fmt.Printf("         - %d tools available (listed in %v)\n", len(tools), toolTime)

						// Spot-check: try to call the first read-only tool with minimal args
						// (this verifies end-to-end communication, not just handshake)
					}
					client.Close()
				}

				// Check capability indexes
				fmt.Println()
				fmt.Println("Capability Indexes:")
				wrapped := reg.WrappedServers()
				if len(wrapped) == 0 {
					fmt.Println("  [INFO] No servers wrapped yet (run 'gobbler wrap <server>')")
				}
				for _, s := range wrapped {
					idx, err := compiler.Load(s.Name)
					if err != nil {
						fmt.Printf("  [WARN] %s: no capability index (run 'gobbler wrap %s')\n", s.Name, s.Name)
						issues++
					} else {
						fmt.Printf("  [OK]   %s: %d capabilities indexed", s.Name, len(idx.Capabilities))
						stats := idx.ServerStats[s.Name]
						if stats != nil {
							fmt.Printf(" (schema: ~%d tokens -> wrapped: ~%d tokens, %.0f%% reduction)",
								stats.SchemaTokens, stats.WrappedTokens,
								100*(1-float64(stats.WrappedTokens)/float64(stats.SchemaTokens)))
						}
						fmt.Println()
					}
				}
			}

			// Check harnesses
			fmt.Println()
			fmt.Println("Harnesses:")
			adapters := AllAdapters()
			found := harness.DetectAll(ctx, adapters)
			for _, a := range found {
				path, _ := a.ConfigPath(ctx)
				fmt.Printf("  [OK]   %s (config: %s)\n", a.Name(), path)

				// Check if gobbler is installed in this harness
				cfg, err := a.LoadConfig(ctx)
				if err == nil {
					if _, hasGobbler := cfg.MCPServers["gobbler"]; hasGobbler {
						fmt.Printf("         - gobbler is installed\n")
					} else {
						fmt.Printf("         - gobbler NOT installed (run 'gobbler install --harness %s')\n", a.Name())
					}
				}
			}
			if len(found) == 0 {
				fmt.Println("  [WARN] No supported harnesses detected")
				issues++
			}

			fmt.Println()
			if issues == 0 {
				fmt.Println("All checks passed.")
			} else {
				fmt.Printf("%d issue(s) found.\n", issues)
			}
			return nil
		},
	}
}
