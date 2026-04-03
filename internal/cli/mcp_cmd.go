package cli

import (
	"fmt"
	"strings"

	"github.com/robinojw/tldr/internal/logging"
	"github.com/robinojw/tldr/internal/registry"
	"github.com/robinojw/tldr/pkg/config"
	"github.com/spf13/cobra"
)

var mcpLog = logging.New("cli:mcp")

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Manage upstream MCP servers registered with tldr",
	}

	cmd.AddCommand(
		newMCPAddCmd(),
		newMCPListCmd(),
		newMCPRemoveCmd(),
	)
	return cmd
}

func newMCPAddCmd() *cobra.Command {
	var (
		transport string
		envVars   []string
		timeout   int
	)

	cmd := &cobra.Command{
		Use:   "add <name> <command-or-url> [args...]",
		Short: "Register an upstream MCP server with tldr",
		Long: `Register an MCP server with tldr's registry. This replaces the harness-specific
command (e.g. 'claude mcp add', 'forge mcp import'). Tldr will manage the server
and make it available to harnesses through the tldr wrapper.

Examples:
  tldr mcp add --transport http figma-remote-mcp https://mcp.figma.com/mcp
  tldr mcp add --transport stdio github-mcp -- npx -y @modelcontextprotocol/server-github
  tldr mcp add --transport stdio --env GITHUB_TOKEN=ghp_xxx github npx -- -y @modelcontextprotocol/server-github`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			reg, err := registry.Open()
			if err != nil {
				return fmt.Errorf("failed to open registry: %w", err)
			}

			entry := &config.ServerEntry{
				Name:      name,
				Transport: config.TransportType(transport),
				Timeout:   timeout,
				Env:       make(map[string]string),
			}

			switch config.TransportType(transport) {
			case config.TransportHTTP, config.TransportSSE:
				entry.URL = args[1]
			case config.TransportStdio:
				entry.Command = args[1]
				if len(args) > 2 {
					entry.Args = args[2:]
				}
			default:
				return fmt.Errorf("unsupported transport: %s (use stdio, http, or sse)", transport)
			}

			// Parse environment variables
			for _, e := range envVars {
				parts := strings.SplitN(e, "=", 2)
				if len(parts) == 2 {
					entry.Env[parts[0]] = parts[1]
				}
			}

			if err := reg.AddServer(entry); err != nil {
				return fmt.Errorf("failed to add server: %w", err)
			}

			fmt.Printf("Registered MCP server: %s (%s)\n", name, transport)
			fmt.Println("Run 'tldr wrap " + name + "' to build the capability index.")
			fmt.Println("Run 'tldr install --harness <name>' to wire into your coding harness.")
			return nil
		},
	}

	cmd.Flags().StringVarP(&transport, "transport", "t", "stdio", "Transport type: stdio, http, sse")
	cmd.Flags().StringSliceVarP(&envVars, "env", "e", nil, "Environment variables (KEY=VALUE)")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "Per-call timeout in seconds (0 = default)")

	return cmd
}

func newMCPListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered MCP servers",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := registry.Open()
			if err != nil {
				return fmt.Errorf("failed to open registry: %w", err)
			}

			servers := reg.ListServers()
			if len(servers) == 0 {
				fmt.Println("No MCP servers registered. Use 'tldr mcp add' to register one.")
				return nil
			}

			fmt.Printf("Registered MCP servers (%d):\n\n", len(servers))
			for _, s := range servers {
				status := "active"
				if s.Disabled {
					status = "disabled"
				}
				wrapped := ""
				if s.Wrapped {
					wrapped = " [wrapped]"
				}

				fmt.Printf("  %-20s %s %s%s\n", s.Name, s.Transport, status, wrapped)

				switch s.Transport {
				case config.TransportStdio:
					fmt.Printf("    command: %s %s\n", s.Command, strings.Join(s.Args, " "))
				case config.TransportHTTP, config.TransportSSE:
					fmt.Printf("    url: %s\n", s.URL)
				}

				if len(s.Harnesses) > 0 {
					fmt.Printf("    harnesses: %s\n", strings.Join(s.Harnesses, ", "))
				}
				fmt.Println()
			}
			return nil
		},
	}
}

func newMCPRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an MCP server from tldr's registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := registry.Open()
			if err != nil {
				return fmt.Errorf("failed to open registry: %w", err)
			}

			if err := reg.RemoveServer(args[0]); err != nil {
				return err
			}

			fmt.Printf("Removed MCP server: %s\n", args[0])
			return nil
		},
	}
}
