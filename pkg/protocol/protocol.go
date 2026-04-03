// Package protocol provides MCP protocol types used throughout tldr.
// It re-exports key types from mcp-go and defines additional tldr-specific types.
package protocol

import (
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
)

// Re-export key mcp-go types for convenience.
type (
	Tool              = mcp.Tool
	CallToolRequest   = mcp.CallToolRequest
	CallToolResult    = mcp.CallToolResult
	TextContent       = mcp.TextContent
	Content           = mcp.Content
	Implementation    = mcp.Implementation
	InitializeRequest = mcp.InitializeRequest
	ListToolsRequest  = mcp.ListToolsRequest
	ListToolsResult   = mcp.ListToolsResult
)

// ToolSchema is a parsed, tldr-friendly representation of an MCP tool definition.
type ToolSchema struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	InputSchema json.RawMessage   `json:"inputSchema,omitempty"`
	Annotations *ToolAnnotations  `json:"annotations,omitempty"`
	Parameters  []ParameterSchema `json:"parameters,omitempty"`
}

// ToolAnnotations captures MCP tool hints.
type ToolAnnotations struct {
	ReadOnly    bool `json:"readOnlyHint,omitempty"`
	Destructive bool `json:"destructiveHint,omitempty"`
	Idempotent  bool `json:"idempotentHint,omitempty"`
	OpenWorld   bool `json:"openWorldHint,omitempty"`
}

// ParameterSchema describes a single parameter of a tool.
type ParameterSchema struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
}

// ParseToolSchema converts an mcp.Tool into a tldr ToolSchema.
func ParseToolSchema(t mcp.Tool) ToolSchema {
	ts := ToolSchema{
		Name:        t.Name,
		Description: t.Description,
	}

	// Serialize the input schema
	if t.InputSchema.Properties != nil {
		raw, err := json.Marshal(t.InputSchema)
		if err == nil {
			ts.InputSchema = raw
		}
	}

	// Parse annotations -- only capture explicitly set non-default values.
	// MCP defaults DestructiveHint=true and OpenWorldHint=true, so we only
	// record annotations when ReadOnly is explicitly true, or Destructive is
	// explicitly false (meaning the server opted to mark it safe).
	hasExplicitAnnotations := false
	ann := &ToolAnnotations{}
	if t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint {
		ann.ReadOnly = true
		hasExplicitAnnotations = true
	}
	if t.Annotations.DestructiveHint != nil && !*t.Annotations.DestructiveHint {
		// Server explicitly said "not destructive"
		hasExplicitAnnotations = true
	} else if t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint {
		// This is the default -- only mark destructive if ReadOnly is not set
		if t.Annotations.ReadOnlyHint == nil || !*t.Annotations.ReadOnlyHint {
			// Don't set ann.Destructive from default; let name-based heuristics decide
		}
	}
	if t.Annotations.IdempotentHint != nil {
		ann.Idempotent = *t.Annotations.IdempotentHint
	}
	if t.Annotations.OpenWorldHint != nil {
		ann.OpenWorld = *t.Annotations.OpenWorldHint
	}
	if hasExplicitAnnotations {
		ts.Annotations = ann
	}

	// Extract parameters from JSON Schema
	ts.Parameters = extractParameters(t.InputSchema)

	return ts
}

// extractParameters extracts parameter info from an MCP tool's input schema.
func extractParameters(schema mcp.ToolInputSchema) []ParameterSchema {
	var params []ParameterSchema

	requiredSet := make(map[string]bool)
	for _, r := range schema.Required {
		requiredSet[r] = true
	}

	for name, propRaw := range schema.Properties {
		p := ParameterSchema{
			Name:     name,
			Required: requiredSet[name],
		}

		// propRaw is map[string]interface{} from the JSON Schema
		if propMap, ok := propRaw.(map[string]interface{}); ok {
			if t, ok := propMap["type"].(string); ok {
				p.Type = t
			}
			if d, ok := propMap["description"].(string); ok {
				p.Description = d
			}
		}

		params = append(params, p)
	}

	return params
}

// LatestProtocolVersion returns the latest MCP protocol version string.
func LatestProtocolVersion() string {
	return mcp.LATEST_PROTOCOL_VERSION
}
