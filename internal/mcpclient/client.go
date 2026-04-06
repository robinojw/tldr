// Package mcpclient provides MCP client connectivity to upstream servers.
// It supports stdio and HTTP transports, wrapping the mcp-go library with
// tldr-specific error handling and logging.
package mcpclient

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/robinojw/tldr/internal/logging"
	"github.com/robinojw/tldr/pkg/config"
)

// Client wraps mcp-go's Client for upstream MCP server communication.
type Client struct {
	Name      string
	Entry     *config.ServerEntry
	mcpClient *client.Client
	log       *logging.Logger
	initResult *mcp.InitializeResult
}

// NewClient creates a new MCP client for the given server entry.
func NewClient(entry *config.ServerEntry) (*Client, error) {
	c := &Client{
		Name:  entry.Name,
		Entry: entry,
		log:   logging.New("mcpclient:" + entry.Name),
	}
	return c, nil
}

// Connect establishes a connection to the upstream MCP server.
func (c *Client) Connect(ctx context.Context) error {
	var t transport.Interface

	switch c.Entry.Transport {
	case config.TransportStdio:
		env := buildEnv(c.Entry.Env)
		args := c.Entry.Args
		if len(args) == 0 {
			args = []string{}
		}
		t = transport.NewStdio(c.Entry.Command, env, args...)
		c.log.Debug("using stdio transport: %s %v", c.Entry.Command, args)

	case config.TransportHTTP, config.TransportSSE:
		url := c.Entry.URL
		if url == "" {
			return fmt.Errorf("HTTP/SSE transport requires a URL")
		}
		opts := []transport.StreamableHTTPCOption{}
		if len(c.Entry.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(c.Entry.Headers))
		}
		var httpErr error
		t, httpErr = transport.NewStreamableHTTP(url, opts...)
		if httpErr != nil {
			return fmt.Errorf("failed to create HTTP transport for %s: %w", c.Name, httpErr)
		}
		c.log.Debug("using HTTP transport: %s", url)

	default:
		return fmt.Errorf("unsupported transport: %s", c.Entry.Transport)
	}

	c.mcpClient = client.NewClient(t)

	// Start the transport
	if err := c.mcpClient.Start(ctx); err != nil {
		return fmt.Errorf("failed to start transport for %s: %w", c.Name, err)
	}

	// Initialize MCP handshake
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "tldr",
		Version: "0.1.0",
	}
	initReq.Params.Capabilities = mcp.ClientCapabilities{}

	// Use a shorter default timeout for HTTP transports (which should
	// respond quickly) and a longer one for stdio (which may need to
	// spawn a child process).
	timeout := 15 * time.Second
	if c.Entry.Transport == config.TransportStdio {
		timeout = 30 * time.Second
	}
	if c.Entry.Timeout > 0 {
		timeout = time.Duration(c.Entry.Timeout) * time.Second
	}

	initCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c.log.Info("connecting (%s, timeout %s)...", c.Entry.Transport, timeout)
	result, err := c.mcpClient.Initialize(initCtx, initReq)
	if err != nil {
		return fmt.Errorf("failed to initialize %s: %w", c.Name, err)
	}

	c.initResult = result
	c.log.Info("connected to %s v%s (protocol %s)",
		result.ServerInfo.Name,
		result.ServerInfo.Version,
		result.ProtocolVersion)

	return nil
}

// ListTools fetches the full tool list from the upstream server.
func (c *Client) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	if c.mcpClient == nil {
		return nil, fmt.Errorf("client not connected")
	}

	result, err := c.mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list tools from %s: %w", c.Name, err)
	}

	c.log.Info("discovered %d tools from %s", len(result.Tools), c.Name)
	return result.Tools, nil
}

// CallTool invokes a tool on the upstream server.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]interface{}) (*mcp.CallToolResult, error) {
	if c.mcpClient == nil {
		return nil, fmt.Errorf("client not connected")
	}

	timeout := 30 * time.Second
	if c.Entry.Timeout > 0 {
		timeout = time.Duration(c.Entry.Timeout) * time.Second
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args

	result, err := c.mcpClient.CallTool(callCtx, req)
	if err != nil {
		return nil, fmt.Errorf("tool call %s/%s failed: %w", c.Name, name, err)
	}

	return result, nil
}

// ServerInfo returns the server's initialization info, or nil if not connected.
func (c *Client) ServerInfo() *mcp.InitializeResult {
	return c.initResult
}

// Close shuts down the client connection.
func (c *Client) Close() error {
	if c.mcpClient != nil {
		return c.mcpClient.Close()
	}
	return nil
}

// buildEnv merges the given env vars with the current environment.
func buildEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}

	// Start with current env
	current := os.Environ()
	merged := make(map[string]string)
	for _, e := range current {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			merged[parts[0]] = parts[1]
		}
	}

	// Override with server-specific env
	for k, v := range env {
		merged[k] = v
	}

	result := make([]string, 0, len(merged))
	for k, v := range merged {
		result = append(result, k+"="+v)
	}
	return result
}
