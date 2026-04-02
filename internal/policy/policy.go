// Package policy implements output shielding and response transformation.
// It enforces size limits, field filtering, and summarization to prevent
// large intermediate API responses from reaching the coding harness.
package policy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/robinwhite/gobbler/internal/resultstore"
	"github.com/robinwhite/gobbler/pkg/config"
)

// Enforcer applies output policies to tool results before returning them
// to the harness.
type Enforcer struct {
	cfg *config.PolicyConfig
}

// NewEnforcer creates an Enforcer with the given policy configuration.
func NewEnforcer(cfg *config.PolicyConfig) *Enforcer {
	return &Enforcer{cfg: cfg}
}

// Shield applies output policy to a raw result string, returning
// a truncated/summarized version that respects size limits.
func (e *Enforcer) Shield(raw string) string {
	if len(raw) <= e.cfg.MaxOutputBytes {
		return raw
	}

	// Try to parse as JSON and summarize
	var parsed interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		summarized := e.summarizeJSON(parsed)
		result, _ := json.MarshalIndent(summarized, "", "  ")
		s := string(result)
		if len(s) <= e.cfg.MaxOutputBytes {
			return s
		}
	}

	// Fall back to truncation
	return raw[:e.cfg.MaxOutputBytes-100] +
		fmt.Sprintf("\n\n... [truncated: %d bytes total, showing first %d]",
			len(raw), e.cfg.MaxOutputBytes-100)
}

// ShieldedResult is the structured output of ShieldStructured.
// It tells the model exactly what was truncated and provides a ref handle
// for pagination via get_result.
type ShieldedResult struct {
	Data         interface{}            `json:"data"`
	WasTruncated bool                   `json:"wasTruncated"`
	Meta         *resultstore.SliceMeta `json:"meta,omitempty"`
}

// ShieldStructured applies output policy and returns structured metadata
// about what was truncated. This replaces the string-suffix approach
// so the model can programmatically decide whether to paginate.
func (e *Enforcer) ShieldStructured(raw string) *ShieldedResult {
	if len(raw) <= e.cfg.MaxOutputBytes {
		return &ShieldedResult{Data: raw, WasTruncated: false}
	}

	// Try JSON-aware summarization
	var parsed interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		// Check if top-level is an array
		if arr, ok := parsed.([]interface{}); ok && len(arr) > e.cfg.MaxArrayLength {
			trimmed := make([]interface{}, e.cfg.MaxArrayLength)
			for i := 0; i < e.cfg.MaxArrayLength; i++ {
				trimmed[i] = e.summarizeJSON(arr[i])
			}
			data, _ := json.MarshalIndent(trimmed, "", "  ")
			return &ShieldedResult{
				Data:         string(data),
				WasTruncated: true,
				Meta: &resultstore.SliceMeta{
					Total:   len(arr),
					Offset:  0,
					Count:   e.cfg.MaxArrayLength,
					HasMore: true,
				},
			}
		}

		// Object or small array -- summarize recursively
		summarized := e.summarizeJSON(parsed)
		result, _ := json.MarshalIndent(summarized, "", "  ")
		s := string(result)
		if len(s) <= e.cfg.MaxOutputBytes {
			return &ShieldedResult{Data: s, WasTruncated: false}
		}
	}

	// Fall back to byte truncation with structured metadata
	cut := e.cfg.MaxOutputBytes - 100
	return &ShieldedResult{
		Data:         raw[:cut],
		WasTruncated: true,
		Meta: &resultstore.SliceMeta{
			Total:   len(raw),
			Offset:  0,
			Count:   cut,
			HasMore: true,
		},
	}
}

// ShieldJSON applies output policy to a structured value.
func (e *Enforcer) ShieldJSON(v interface{}) interface{} {
	return e.summarizeJSON(v)
}

// ShieldFields extracts only the specified fields from a JSON object.
func (e *Enforcer) ShieldFields(raw string, fields []string) (string, error) {
	if len(fields) == 0 {
		return e.Shield(raw), nil
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// Not an object, just shield the whole thing
		return e.Shield(raw), nil
	}

	filtered := make(map[string]interface{})
	for _, f := range fields {
		if v, ok := parsed[f]; ok {
			filtered[f] = v
		}
	}

	result, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		return e.Shield(raw), nil
	}
	return e.Shield(string(result)), nil
}

// IsToolBlocked returns true if the given tool is on the blocklist.
func (e *Enforcer) IsToolBlocked(toolName string) bool {
	for _, blocked := range e.cfg.BlockedTools {
		if blocked == toolName {
			return true
		}
	}
	return false
}

// IsMutatingAllowed returns true if mutating tools are permitted.
func (e *Enforcer) IsMutatingAllowed() bool {
	return e.cfg.AllowMutating
}

// StepTimeout returns the per-step timeout in seconds.
func (e *Enforcer) StepTimeout() int {
	return e.cfg.StepTimeout
}

// PlanTimeout returns the total plan timeout in seconds.
func (e *Enforcer) PlanTimeout() int {
	return e.cfg.PlanTimeout
}

// MaxSteps returns the maximum number of steps allowed in a plan.
func (e *Enforcer) MaxSteps() int {
	return e.cfg.MaxSteps
}

// summarizeJSON recursively trims a JSON value to fit policy constraints.
func (e *Enforcer) summarizeJSON(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for k, child := range val {
			result[k] = e.summarizeJSON(child)
		}
		return result

	case []interface{}:
		if len(val) > e.cfg.MaxArrayLength {
			trimmed := make([]interface{}, e.cfg.MaxArrayLength)
			for i := 0; i < e.cfg.MaxArrayLength; i++ {
				trimmed[i] = e.summarizeJSON(val[i])
			}
			return map[string]interface{}{
				"_items":     trimmed,
				"_truncated": true,
				"_total":     len(val),
				"_showing":   e.cfg.MaxArrayLength,
			}
		}
		result := make([]interface{}, len(val))
		for i, child := range val {
			result[i] = e.summarizeJSON(child)
		}
		return result

	case string:
		if len(val) > e.cfg.MaxStringLength {
			return val[:e.cfg.MaxStringLength] +
				fmt.Sprintf("... [%d chars total]", len(val))
		}
		return val

	default:
		return val
	}
}

// SummarizeToolResult takes raw content pieces from an MCP tool result
// and produces a shielded text summary.
func (e *Enforcer) SummarizeToolResult(contents []interface{}) string {
	var parts []string
	for _, c := range contents {
		if m, ok := c.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok {
				parts = append(parts, e.Shield(text))
			}
		}
	}
	return strings.Join(parts, "\n")
}
