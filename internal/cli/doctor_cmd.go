package cli

import (
	"context"
	"fmt"
	"os"

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

				// Check each server
				for _, s := range servers {
					if s.Disabled {
						fmt.Printf("       - %s (disabled)\n", s.Name)
						continue
					}

					client, err := mcpclient.NewClient(s)
					if err != nil {
						fmt.Printf("[WARN] %s: failed to create client: %v\n", s.Name, err)
						issues++
						continue
					}

					if err := client.Connect(ctx); err != nil {
						fmt.Printf("[WARN] %s: connection failed: %v\n", s.Name, err)
						issues++
						client.Close()
						continue
					}

					tools, err := client.ListTools(ctx)
					if err != nil {
						fmt.Printf("[WARN] %s: tool listing failed: %v\n", s.Name, err)
						issues++
					} else {
						fmt.Printf("[OK]   %s: %d tools available\n", s.Name, len(tools))
					}
					client.Close()
				}

				// Check capability indexes
				wrapped := reg.WrappedServers()
				for _, s := range wrapped {
					if _, err := compiler.Load(s.Name); err != nil {
						fmt.Printf("[WARN] %s: no capability index (run 'gobbler wrap %s')\n", s.Name, s.Name)
						issues++
					} else {
						fmt.Printf("[OK]   %s: capability index present\n", s.Name)
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
				fmt.Printf("[OK]   %s (config: %s)\n", a.Name(), path)
			}
			if len(found) == 0 {
				fmt.Println("[WARN] No supported harnesses detected")
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
