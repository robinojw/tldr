// Package policy implements output shielding and response transformation.
// It enforces size limits, field filtering, token-aware compaction, and
// smart summarization to prevent large intermediate API responses from
// reaching the coding harness.
//
// Three layers of protection:
//  1. Smart summarization: array-of-object responses are auto-compacted by
//     stripping heavy fields (URLs, blobs, nested objects) and keeping
//     signal fields (IDs, names, short strings, dates, numbers, booleans).
//  2. Token-aware shielding: responses are capped at MaxOutputTokens
//     (estimated at ~4 chars/token) even if they're under the byte limit.
//  3. Byte-level truncation: hard cap at MaxOutputBytes as a safety net.
package policy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/robinojw/tldr/internal/resultstore"
	"github.com/robinojw/tldr/pkg/config"
)

// charsPerToken is the approximate number of characters per LLM token.
// Conservative estimate; real tokenizers vary by model.
const charsPerToken = 4

// Enforcer applies output policies to tool results before returning them
// to the harness.
type Enforcer struct {
	cfg *config.PolicyConfig
}

// NewEnforcer creates an Enforcer with the given policy configuration.
func NewEnforcer(cfg *config.PolicyConfig) *Enforcer {
	return &Enforcer{cfg: cfg}
}

// EstimateTokens returns an approximate token count for a string.
func EstimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + charsPerToken - 1) / charsPerToken
}

// exceedsTokenLimit returns true if the string exceeds the configured token limit.
func (e *Enforcer) exceedsTokenLimit(s string) bool {
	if e.cfg.MaxOutputTokens <= 0 {
		return false
	}
	return EstimateTokens(s) > e.cfg.MaxOutputTokens
}

// maxTokenBytes returns the byte limit implied by the token limit.
func (e *Enforcer) maxTokenBytes() int {
	if e.cfg.MaxOutputTokens <= 0 {
		return e.cfg.MaxOutputBytes
	}
	tokenBytes := e.cfg.MaxOutputTokens * charsPerToken
	if tokenBytes < e.cfg.MaxOutputBytes {
		return tokenBytes
	}
	return e.cfg.MaxOutputBytes
}

// Shield applies output policy to a raw result string, returning
// a truncated/summarized version that respects both byte and token limits.
func (e *Enforcer) Shield(raw string) string {
	limit := e.maxTokenBytes()

	if len(raw) <= limit {
		return raw
	}

	// Try to parse as JSON and summarize
	var parsed interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		summarized := e.smartSummarize(parsed)
		result, _ := json.MarshalIndent(summarized, "", "  ")
		s := string(result)
		if len(s) <= limit {
			return s
		}
	}

	// Fall back to truncation
	cut := limit - 100
	if cut < 0 {
		cut = 0
	}
	return raw[:cut] +
		fmt.Sprintf("\n\n... [truncated: %d bytes total, ~%d tokens, showing first %d bytes]",
			len(raw), EstimateTokens(raw), cut)
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
// about what was truncated. Uses token-aware limits and smart compaction.
func (e *Enforcer) ShieldStructured(raw string) *ShieldedResult {
	limit := e.maxTokenBytes()

	// Try JSON-aware smart summarization first
	var parsed interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		// Always apply smart summarization to arrays (compaction + token awareness)
		if arr, ok := parsed.([]interface{}); ok {
			return e.shieldArray(arr, limit)
		}

		// For objects, apply smart summarization
		summarized := e.smartSummarize(parsed)
		result, _ := json.MarshalIndent(summarized, "", "  ")
		s := string(result)
		if len(s) <= limit {
			return &ShieldedResult{Data: s, WasTruncated: s != raw}
		}
	}

	// Under both limits -- pass through
	if len(raw) <= limit {
		return &ShieldedResult{Data: raw, WasTruncated: false}
	}

	// Fall back to byte truncation with structured metadata
	cut := limit - 100
	if cut < 0 {
		cut = 0
	}
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

// shieldArray handles array responses with smart compaction and token-aware trimming.
func (e *Enforcer) shieldArray(arr []interface{}, limit int) *ShieldedResult {
	total := len(arr)

	// Step 1: Compact array elements (strip heavy fields)
	compacted := e.compactArray(arr)

	// Step 2: Check if compacted version fits within limits
	data, _ := json.MarshalIndent(compacted, "", "  ")
	if len(data) <= limit {
		wasCompacted := false
		if total > 0 {
			// Check if any element was actually modified by compaction
			origBytes, _ := json.Marshal(arr)
			compBytes, _ := json.Marshal(compacted)
			wasCompacted = len(origBytes) != len(compBytes)
		}
		result := &ShieldedResult{
			Data:         string(data),
			WasTruncated: wasCompacted,
		}
		if wasCompacted {
			result.Meta = &resultstore.SliceMeta{
				Total:   total,
				Offset:  0,
				Count:   total,
				HasMore: false,
			}
		}
		return result
	}

	// Step 3: Compacted version still too large -- trim array length
	showing := total
	for showing > 1 {
		showing = showing * 3 / 4 // reduce by 25% each iteration
		if showing < 1 {
			showing = 1
		}
		trimmed := compacted[:showing]
		data, _ = json.MarshalIndent(trimmed, "", "  ")
		if len(data) <= limit {
			break
		}
	}

	return &ShieldedResult{
		Data:         string(data),
		WasTruncated: true,
		Meta: &resultstore.SliceMeta{
			Total:   total,
			Offset:  0,
			Count:   showing,
			HasMore: showing < total,
		},
	}
}

// compactArray applies smart field compaction to an array of objects.
// For each element, it classifies fields as "light" (keep) or "heavy" (strip)
// based on the field's serialized size. Heavy fields are replaced with a
// short placeholder so the model knows they exist.
func (e *Enforcer) compactArray(arr []interface{}) []interface{} {
	if !e.cfg.CompactArrays || len(arr) == 0 {
		// Fall back to legacy summarization
		result := make([]interface{}, len(arr))
		for i, item := range arr {
			result[i] = e.summarizeJSON(item)
		}
		return result
	}

	// Analyze field weights across all elements to find consistently heavy fields
	heavyFields := e.detectHeavyFields(arr)

	result := make([]interface{}, len(arr))
	for i, item := range arr {
		result[i] = e.compactObject(item, heavyFields)
	}
	return result
}

// fieldWeight tracks the total serialized size of a field across array elements.
type fieldWeight struct {
	name       string
	totalBytes int
	count      int
	isURL      bool // heuristic: field value looks like a URL
	isNested   bool // field value is an object or array
}

// detectHeavyFields samples array elements and identifies fields that are
// consistently large or low-signal (URLs, nested objects, long strings).
func (e *Enforcer) detectHeavyFields(arr []interface{}) map[string]bool {
	weights := make(map[string]*fieldWeight)

	// Sample up to 10 elements for field analysis
	sampleSize := len(arr)
	if sampleSize > 10 {
		sampleSize = 10
	}

	for i := 0; i < sampleSize; i++ {
		obj, ok := arr[i].(map[string]interface{})
		if !ok {
			continue
		}
		for k, v := range obj {
			w, exists := weights[k]
			if !exists {
				w = &fieldWeight{name: k}
				weights[k] = w
			}
			w.count++

			b, _ := json.Marshal(v)
			w.totalBytes += len(b)

			// Detect URL-like values
			if s, ok := v.(string); ok {
				if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
					w.isURL = true
				}
			}

			// Detect nested structures
			switch v.(type) {
			case map[string]interface{}, []interface{}:
				w.isNested = true
			}
		}
	}

	maxFieldBytes := e.cfg.MaxFieldBytes
	if maxFieldBytes <= 0 {
		maxFieldBytes = 256
	}

	heavy := make(map[string]bool)
	for name, w := range weights {
		avgBytes := w.totalBytes / max(w.count, 1)

		// A field is "heavy" if:
		// 1. Its average serialized size exceeds MaxFieldBytes, OR
		// 2. It's a URL field (low signal for the model), OR
		// 3. It contains nested objects/arrays that are large
		if avgBytes > maxFieldBytes {
			heavy[name] = true
		} else if w.isURL && avgBytes > 40 {
			heavy[name] = true
		} else if w.isNested && avgBytes > maxFieldBytes/2 {
			heavy[name] = true
		}

		// Never strip common identity/signal fields regardless of size
		if isSignalField(name) {
			delete(heavy, name)
		}
	}

	return heavy
}

// signalFields are field names that should never be stripped, even if large.
var signalFields = map[string]bool{
	"id": true, "name": true, "title": true, "message": true,
	"description": true, "state": true, "status": true, "type": true,
	"login": true, "email": true, "full_name": true, "number": true,
	"created_at": true, "updated_at": true, "date": true,
	"language": true, "default_branch": true, "private": true,
	"fork": true, "archived": true, "disabled": true,
	"stargazers_count": true, "forks_count": true, "open_issues_count": true,
	"path": true, "ref": true, "sha": true, "tag": true, "label": true,
	"body": true, "size": true, "score": true, "total_count": true,
}

func isSignalField(name string) bool {
	return signalFields[strings.ToLower(name)]
}

// compactObject strips heavy fields from an object, replacing them with
// a short placeholder. Non-object values pass through unchanged.
func (e *Enforcer) compactObject(v interface{}, heavyFields map[string]bool) interface{} {
	obj, ok := v.(map[string]interface{})
	if !ok {
		return e.summarizeJSON(v)
	}

	result := make(map[string]interface{})
	var omitted []string

	// Sort keys for deterministic output
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		child := obj[k]
		if heavyFields[k] {
			omitted = append(omitted, k)
			continue
		}
		// Apply recursive summarization to kept fields
		result[k] = e.summarizeJSON(child)
	}

	if len(omitted) > 0 {
		result["_omitted"] = omitted
	}

	return result
}

// smartSummarize applies the best summarization strategy based on the data shape.
// For arrays of objects, it uses field compaction. For other types, it uses
// the legacy recursive summarization.
func (e *Enforcer) smartSummarize(v interface{}) interface{} {
	switch val := v.(type) {
	case []interface{}:
		if len(val) > 0 {
			if _, ok := val[0].(map[string]interface{}); ok && e.cfg.CompactArrays {
				return e.compactArray(val)
			}
		}
		return e.summarizeJSON(v)
	default:
		return e.summarizeJSON(v)
	}
}

// ShieldJSON applies output policy to a structured value.
func (e *Enforcer) ShieldJSON(v interface{}) interface{} {
	return e.smartSummarize(v)
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
// This is the legacy summarization path used for non-array data and as a
// fallback when CompactArrays is disabled.
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
