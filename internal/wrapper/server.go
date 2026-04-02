// Package wrapper implements the gobbler MCP wrapper server that exposes
// a small tool surface (search_tools, execute_plan, call_raw) to coding
// harnesses while keeping large intermediate responses shielded internally.
package wrapper

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/robinwhite/gobbler/internal/compiler"
	"github.com/robinwhite/gobbler/internal/executor"
	"github.com/robinwhite/gobbler/internal/logging"
	"github.com/robinwhite/gobbler/internal/mcpclient"
	"github.com/robinwhite/gobbler/internal/policy"
	"github.com/robinwhite/gobbler/pkg/config"
)

var log = logging.New("wrapper")

// Server is the gobbler MCP wrapper server.
type Server struct {
	mcpServer *server.MCPServer
	index     *compiler.CapabilityIndex
	clients   map[string]*mcpclient.Client
	exec      *executor.Executor
	enforcer  *policy.Enforcer
}

// NewServer creates a new gobbler wrapper MCP server.
func NewServer(
	index *compiler.CapabilityIndex,
	clients map[string]*mcpclient.Client,
	policyCfg *config.PolicyConfig,
) *Server {
	if policyCfg == nil {
		policyCfg = config.DefaultPolicyConfig()
	}

	enforcer := policy.NewEnforcer(policyCfg)
	exec := executor.NewExecutor(clients, enforcer)

	s := &Server{
		index:    index,
		clients:  clients,
		exec:     exec,
		enforcer: enforcer,
	}

	s.mcpServer = server.NewMCPServer(
		"gobbler",
		"0.1.0",
		server.WithToolCapabilities(false),
		server.WithInstructions(gobblerInstructions),
	)

	s.registerTools()
	return s
}

// Serve starts the wrapper server on stdio.
func (s *Server) Serve() error {
	log.Info("starting gobbler wrapper server on stdio")
	return server.ServeStdio(s.mcpServer)
}

// MCPServer returns the underlying mcp-go server (for testing).
func (s *Server) MCPServer() *server.MCPServer {
	return s.mcpServer
}

func (s *Server) registerTools() {
	// Tool 1: search_tools
	searchTool := mcp.NewTool("search_tools",
		mcp.WithDescription(
			"Search available capabilities across all registered MCP servers. "+
				"Returns compressed summaries of matching tools without loading full schemas. "+
				"Use this to discover what gobbler can do before calling execute_plan."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search query describing the capability you need, e.g. 'list pull requests', 'search web', 'read file'"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results to return (default: 10)"),
		),
	)
	s.mcpServer.AddTool(searchTool, s.handleSearchTools)

	// Tool 2: execute_plan
	executeTool := mcp.NewTool("execute_plan",
		mcp.WithDescription(
			"Execute a structured plan of tool calls against upstream MCP servers. "+
				"Steps are executed in order. Intermediate results are kept internally -- "+
				"only the final output is returned. Steps can reference previous step results "+
				"using ${stepId.field} syntax in arguments. "+
				"This keeps large API responses from consuming your context."),
		mcp.WithString("plan",
			mcp.Required(),
			mcp.Description(
				"JSON plan object with 'steps' array and optional 'return' spec. "+
					"Each step: {id, server, tool, arguments}. "+
					"Return spec: {mode: 'full'|'summary'|'fields', fromStep, fields?}. "+
					"Example: {\"steps\":[{\"id\":\"s1\",\"server\":\"github-mcp\",\"tool\":\"list_pull_requests\",\"arguments\":{\"owner\":\"org\",\"repo\":\"repo\"}}],\"return\":{\"mode\":\"summary\",\"fromStep\":\"s1\"}}"),
		),
	)
	s.mcpServer.AddTool(executeTool, s.handleExecutePlan)

	// Tool 3: call_raw (escape hatch / debug)
	rawTool := mcp.NewTool("call_raw",
		mcp.WithDescription(
			"Call a single upstream MCP tool directly and return the shielded result. "+
				"Use this as an escape hatch when execute_plan is not needed. "+
				"Output is still subject to response shielding policies."),
		mcp.WithString("server",
			mcp.Required(),
			mcp.Description("Name of the upstream MCP server"),
		),
		mcp.WithString("tool",
			mcp.Required(),
			mcp.Description("Name of the tool to call"),
		),
		mcp.WithString("arguments",
			mcp.Description("JSON object of tool arguments"),
		),
	)
	s.mcpServer.AddTool(rawTool, s.handleCallRaw)

	// Tool 4: inspect_tool (detailed tool info)
	inspectTool := mcp.NewTool("inspect_tool",
		mcp.WithDescription(
			"Get detailed parameter information for a specific upstream tool. "+
				"Use this when search_tools returns a match but you need more detail "+
				"about the parameters before building a plan."),
		mcp.WithString("server",
			mcp.Required(),
			mcp.Description("Name of the upstream MCP server"),
		),
		mcp.WithString("tool",
			mcp.Required(),
			mcp.Description("Name of the tool to inspect"),
		),
	)
	s.mcpServer.AddTool(inspectTool, s.handleInspectTool)

	log.Info("registered 4 wrapper tools")
}

func (s *Server) handleSearchTools(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}

	limit := req.GetInt("limit", 10)

	results := s.index.Search(query, limit)

	// Format as compact text
	var output []string
	for _, cap := range results {
		risk := ""
		if cap.RiskLevel != "read" {
			risk = fmt.Sprintf(" [%s]", cap.RiskLevel)
		}
		output = append(output, fmt.Sprintf("- %s/%s%s: %s\n  input: %s\n  tags: %s",
			cap.ServerName, cap.ToolName, risk,
			cap.Summary,
			cap.InputShape,
			joinTags(cap.Tags),
		))
	}

	if len(output) == 0 {
		return mcp.NewToolResultText("No matching capabilities found. Try broader terms like 'github', 'search', 'file', 'docs'."), nil
	}

	header := fmt.Sprintf("Found %d capabilities:\n\n", len(results))
	return mcp.NewToolResultText(header + joinLines(output)), nil
}

func (s *Server) handleExecutePlan(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	planJSON, err := req.RequireString("plan")
	if err != nil {
		return mcp.NewToolResultError("plan is required"), nil
	}

	var plan executor.Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid plan JSON: %v", err)), nil
	}

	result, err := s.exec.Execute(ctx, plan)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("execution error: %v", err)), nil
	}

	if !result.Success {
		return mcp.NewToolResultError(fmt.Sprintf("plan failed at step %d: %s", result.StepCount, result.Error)), nil
	}

	// Serialize the output
	outBytes, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(outBytes)), nil
}

func (s *Server) handleCallRaw(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serverName, err := req.RequireString("server")
	if err != nil {
		return mcp.NewToolResultError("server is required"), nil
	}

	toolName, err := req.RequireString("tool")
	if err != nil {
		return mcp.NewToolResultError("tool is required"), nil
	}

	argsJSON := req.GetString("arguments", "{}")
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid arguments JSON: %v", err)), nil
	}

	result, err := s.exec.CallRaw(ctx, serverName, toolName, args)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("call failed: %v", err)), nil
	}

	switch v := result.(type) {
	case string:
		return mcp.NewToolResultText(v), nil
	default:
		out, _ := json.MarshalIndent(v, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}
}

func (s *Server) handleInspectTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serverName, err := req.RequireString("server")
	if err != nil {
		return mcp.NewToolResultError("server is required"), nil
	}

	toolName, err := req.RequireString("tool")
	if err != nil {
		return mcp.NewToolResultError("tool is required"), nil
	}

	// Search the capability index for this specific tool
	caps := s.index.ForServer(serverName)
	for _, cap := range caps {
		if cap.ToolName == toolName {
			out, _ := json.MarshalIndent(cap, "", "  ")
			return mcp.NewToolResultText(string(out)), nil
		}
	}

	return mcp.NewToolResultError(fmt.Sprintf("tool %s/%s not found in capability index", serverName, toolName)), nil
}

func joinTags(tags []string) string {
	if len(tags) == 0 {
		return "(none)"
	}
	return fmt.Sprintf("[%s]", join(tags, ", "))
}

func join(s []string, sep string) string {
	result := ""
	for i, v := range s {
		if i > 0 {
			result += sep
		}
		result += v
	}
	return result
}

func joinLines(lines []string) string {
	result := ""
	for i, l := range lines {
		if i > 0 {
			result += "\n"
		}
		result += l
	}
	return result
}

const gobblerInstructions = `Gobbler is a local MCP gateway that compresses tool surfaces and shields models from large intermediate API responses.

Instead of calling many individual MCP tools directly, use gobbler's small surface:

1. search_tools: Discover what capabilities are available across all registered servers.
2. execute_plan: Run multi-step plans against upstream tools. Intermediate results stay inside gobbler -- you only see the final output.
3. call_raw: Call a single tool directly (escape hatch, still shielded).
4. inspect_tool: Get detailed parameter info for a specific tool.

Workflow:
1. Use search_tools to find relevant capabilities.
2. Use inspect_tool if you need parameter details.
3. Build a plan with execute_plan for multi-step operations.
4. Use call_raw for simple one-off calls.

Plans use step references: arguments can reference previous step results with ${stepId.field} syntax.`
