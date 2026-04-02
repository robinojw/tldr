// Package wrapper implements the gobbler MCP wrapper server that exposes
// a small tool surface to coding harnesses while keeping large intermediate
// responses shielded internally.
//
// The 5 tools are:
//   - search_tools: discover capabilities across registered servers
//   - execute_plan: run multi-step plans with response shielding
//   - call_raw: direct tool call (escape hatch, still shielded)
//   - inspect_tool: get parameter details for a specific tool
//   - get_result: paginate through stored results that were truncated
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
	"github.com/robinwhite/gobbler/internal/resultstore"
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
	store     *resultstore.Store
}

// NewServer creates a new gobbler wrapper MCP server.
// If diskPath is non-empty, results are persisted to disk and survive restarts.
func NewServer(
	index *compiler.CapabilityIndex,
	clients map[string]*mcpclient.Client,
	policyCfg *config.PolicyConfig,
	diskPath string,
) *Server {
	if policyCfg == nil {
		policyCfg = config.DefaultPolicyConfig()
	}

	enforcer := policy.NewEnforcer(policyCfg)
	var store *resultstore.Store
	if diskPath != "" {
		store = resultstore.NewDiskBacked(diskPath)
	} else {
		store = resultstore.New()
	}

	// Convert concrete clients to ToolCaller interface
	callers := make(map[string]executor.ToolCaller, len(clients))
	for name, c := range clients {
		callers[name] = c
	}

	exec := executor.NewExecutorWithStore(callers, enforcer, store)
	exec.SetCapabilityIndex(index)

	s := &Server{
		index:    index,
		clients:  clients,
		exec:     exec,
		enforcer: enforcer,
		store:    store,
	}

	s.mcpServer = server.NewMCPServer(
		"gobbler",
		"0.2.0",
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
				"Steps are executed in dependency order (concurrently when possible). "+
				"Intermediate results are stored internally and only the final output (after shielding) is returned. "+
				"Steps can reference previous step results using ${stepId.field} syntax. "+
				"If a result is truncated, the response includes a 'ref' handle and truncation metadata. "+
				"Use get_result with that ref to page through the full data."),
		mcp.WithObject("plan",
			mcp.Required(),
			mcp.Description(
				"Plan object with 'steps' array and optional 'return' spec. "+
					"Each step: {id, server, tool, arguments, dependsOn?: [stepIds]}. "+
					"Return spec: {mode: 'full'|'summary'|'fields', fromStep, fields?}. "+
					"Example: {\"steps\":[{\"id\":\"s1\",\"server\":\"github\",\"tool\":\"list_pull_requests\",\"arguments\":{\"owner\":\"org\",\"repo\":\"repo\"}}],\"return\":{\"mode\":\"summary\",\"fromStep\":\"s1\"}}"),
		),
	)
	s.mcpServer.AddTool(executeTool, s.handleExecutePlan)

	// Tool 3: call_raw (escape hatch)
	rawTool := mcp.NewTool("call_raw",
		mcp.WithDescription(
			"Call a single upstream MCP tool directly. The result is shielded and stored. "+
				"If the response is truncated, a 'ref' handle is returned. "+
				"Use get_result with that ref to page through the full data."),
		mcp.WithString("server",
			mcp.Required(),
			mcp.Description("Name of the upstream MCP server"),
		),
		mcp.WithString("tool",
			mcp.Required(),
			mcp.Description("Name of the tool to call"),
		),
		mcp.WithObject("arguments",
			mcp.Description("Tool arguments as a JSON object"),
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

	// Tool 5: get_result (pagination for stored results)
	getResultTool := mcp.NewTool("get_result",
		mcp.WithDescription(
			"Page through a stored result that was truncated. "+
				"When execute_plan or call_raw returns a truncated response, the output includes "+
				"a 'ref' handle and metadata (total items, showing count, hasMore). "+
				"Use this tool with that ref to retrieve the next page, specific array slices, "+
				"or project specific fields from array elements. "+
				"Results are held in memory for 10 minutes after creation."),
		mcp.WithString("ref",
			mcp.Required(),
			mcp.Description("Result ref handle from a previous execute_plan or call_raw response (e.g. 'p1:s1', 'raw:3')"),
		),
		mcp.WithNumber("offset",
			mcp.Description("Start index for array pagination (default: 0)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Number of elements to return (default: 50)"),
		),
		mcp.WithString("fields",
			mcp.Description("Comma-separated list of fields to project from each array element (e.g. 'id,title,state'). Omit to return full elements."),
		),
		mcp.WithString("path",
			mcp.Description("Optional path expression to navigate into the result before slicing (e.g. 'items', 'data.results'). Supports dot notation."),
		),
	)
	s.mcpServer.AddTool(getResultTool, s.handleGetResult)

	log.Info("registered 5 wrapper tools")
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
	// plan comes as a structured JSON object (not a string) from the MCP layer.
	// The model sends {"plan": {"steps": [...], "return": {...}}} directly.
	planRaw, ok := req.GetArguments()["plan"]
	if !ok {
		return mcp.NewToolResultError("plan is required"), nil
	}

	// Re-marshal and unmarshal to get the typed Plan struct.
	// This handles both the object case (MCP sends a map) and the legacy
	// string case (in case a client sends a JSON string).
	var plan executor.Plan
	switch v := planRaw.(type) {
	case string:
		// Legacy: some clients may still send a JSON string
		if err := json.Unmarshal([]byte(v), &plan); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid plan JSON string: %v", err)), nil
		}
	default:
		// Standard: plan is a structured object from the MCP transport
		planBytes, err := json.Marshal(v)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to serialize plan: %v", err)), nil
		}
		if err := json.Unmarshal(planBytes, &plan); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid plan structure: %v", err)), nil
		}
	}

	result, err := s.exec.Execute(ctx, plan)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("execution error: %v", err)), nil
	}

	if !result.Success {
		return mcp.NewToolResultError(fmt.Sprintf("plan failed at step %d: %s", result.StepCount, result.Error)), nil
	}

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

	// arguments comes as a structured object or a legacy JSON string
	args := make(map[string]interface{})
	if argsRaw, ok := req.GetArguments()["arguments"]; ok {
		switch v := argsRaw.(type) {
		case string:
			// Legacy: client sends a JSON string
			if err := json.Unmarshal([]byte(v), &args); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid arguments JSON: %v", err)), nil
			}
		case map[string]interface{}:
			args = v
		default:
			// Marshal/unmarshal roundtrip for other types
			b, _ := json.Marshal(v)
			if err := json.Unmarshal(b, &args); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid arguments: %v", err)), nil
			}
		}
	}

	result, err := s.exec.CallRaw(ctx, serverName, toolName, args)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("call failed: %v", err)), nil
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
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

func (s *Server) handleGetResult(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ref, err := req.RequireString("ref")
	if err != nil {
		return mcp.NewToolResultError("ref is required"), nil
	}

	offset := req.GetInt("offset", 0)
	limit := req.GetInt("limit", 50)
	fieldsStr := req.GetString("fields", "")
	pathExpr := req.GetString("path", "")

	// Parse fields
	var fields []string
	if fieldsStr != "" {
		for _, f := range splitFields(fieldsStr) {
			if f != "" {
				fields = append(fields, f)
			}
		}
	}

	// If a path is specified, extract that sub-value first, then slice
	if pathExpr != "" {
		fullExpr := ref + "." + pathExpr
		val, err := s.store.ExtractField(fullExpr)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("path extraction failed: %v", err)), nil
		}

		// If the extracted value is an array, apply offset/limit/fields
		if arr, ok := val.([]interface{}); ok {
			return s.sliceAndReturn(arr, ref, offset, limit, fields)
		}

		// Otherwise return the value directly
		out, _ := json.MarshalIndent(val, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}

	// Standard slice on the stored result
	data, meta, err := s.store.Slice(ref, offset, limit, fields)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get_result failed: %v", err)), nil
	}

	response := map[string]interface{}{
		"data": data,
		"meta": meta,
	}

	out, _ := json.MarshalIndent(response, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
}

// sliceAndReturn applies offset/limit/fields to an already-extracted array.
func (s *Server) sliceAndReturn(arr []interface{}, ref string, offset, limit int, fields []string) (*mcp.CallToolResult, error) {
	total := len(arr)
	if offset >= total {
		response := map[string]interface{}{
			"data": []interface{}{},
			"meta": &resultstore.SliceMeta{
				Ref:     ref,
				Total:   total,
				Offset:  offset,
				Count:   0,
				HasMore: false,
			},
		}
		out, _ := json.MarshalIndent(response, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}

	end := offset + limit
	if end > total {
		end = total
	}
	slice := arr[offset:end]

	// Project fields
	if len(fields) > 0 {
		projected := make([]interface{}, len(slice))
		for i, item := range slice {
			if obj, ok := item.(map[string]interface{}); ok {
				p := make(map[string]interface{})
				for _, f := range fields {
					if v, exists := obj[f]; exists {
						p[f] = v
					}
				}
				projected[i] = p
			} else {
				projected[i] = item
			}
		}
		slice = projected
	}

	response := map[string]interface{}{
		"data": slice,
		"meta": &resultstore.SliceMeta{
			Ref:     ref,
			Total:   total,
			Offset:  offset,
			Count:   len(slice),
			HasMore: end < total,
		},
	}

	out, _ := json.MarshalIndent(response, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
}

func splitFields(s string) []string {
	var fields []string
	current := ""
	for _, c := range s {
		if c == ',' {
			fields = append(fields, current)
			current = ""
		} else if c != ' ' {
			current += string(c)
		}
	}
	if current != "" {
		fields = append(fields, current)
	}
	return fields
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
5. get_result: Page through stored results when responses are truncated.

Workflow:
1. Use search_tools to find relevant capabilities.
2. Use inspect_tool if you need parameter details.
3. Build a plan with execute_plan for multi-step operations.
4. Use call_raw for simple one-off calls.
5. If any response shows "shielded: true" or "hasMore: true", use get_result with the "ref" handle to get the next page.

Plans use step references: arguments can reference previous step results with ${stepId.field} syntax.
Pagination: get_result supports offset/limit for arrays, field projection via comma-separated fields, and path expressions to navigate into nested objects.`
