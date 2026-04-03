package cli

import (
	"context"
	"fmt"

	"github.com/robinojw/tldr/internal/harness"
	"github.com/spf13/cobra"
)

func newHarnessCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "harness",
		Short: "Detect and manage coding harness integrations",
	}

	cmd.AddCommand(newHarnessDetectCmd())
	return cmd
}

func newHarnessDetectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "detect",
		Short: "Detect installed coding harnesses",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			adapters := AllAdapters()
			found := harness.DetectAll(ctx, adapters)

			if len(found) == 0 {
				fmt.Println("No supported coding harnesses detected.")
				fmt.Println("Supported: forge, claude, codex")
				return nil
			}

			fmt.Printf("Detected %d harness(es):\n", len(found))
			for _, a := range found {
				path, _ := a.ConfigPath(ctx)
				fmt.Printf("  - %s (config: %s)\n", a.Name(), path)
			}
			return nil
		},
	}
}
