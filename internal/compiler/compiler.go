// Package compiler inspects upstream MCP tool schemas and builds a
// compressed capability index for tldr's search_tools functionality.
package compiler

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/robinojw/tldr/internal/logging"
	"github.com/robinojw/tldr/pkg/config"
	"github.com/robinojw/tldr/pkg/protocol"
)

// Capability is a compressed representation of an upstream MCP tool.
type Capability struct {
	ServerName  string   `json:"serverName"`
	ToolName    string   `json:"toolName"`
	Summary     string   `json:"summary"`
	Tags        []string `json:"tags"`
	RiskLevel   string   `json:"riskLevel"` // "read", "write", "dangerous"
	InputShape  string   `json:"inputShape"`
	OutputShape string   `json:"outputShape"`
	Group       string   `json:"group,omitempty"`
}

// CapabilityIndex is the compiled set of capabilities for one or more servers.
type CapabilityIndex struct {
	Capabilities []Capability       `json:"capabilities"`
	ServerStats  map[string]*Stats  `json:"serverStats"`
	byTerm       map[string][]int   // term -> capability indices (built at load time)
}

// Stats tracks summary statistics for a server's tools.
type Stats struct {
	ServerName    string `json:"serverName"`
	ToolCount     int    `json:"toolCount"`
	ReadOnly      int    `json:"readOnly"`
	Mutating      int    `json:"mutating"`
	SchemaTokens  int    `json:"schemaTokens"`  // approximate token count of raw schemas
	WrappedTokens int    `json:"wrappedTokens"` // approximate token count via tldr
}

var log = logging.New("compiler")

// Compile takes raw MCP tools from a server and produces a capability index.
func Compile(serverName string, tools []mcp.Tool) *CapabilityIndex {
	idx := &CapabilityIndex{
		Capabilities: make([]Capability, 0, len(tools)),
		ServerStats:  make(map[string]*Stats),
	}

	stats := &Stats{
		ServerName: serverName,
		ToolCount:  len(tools),
	}

	for _, t := range tools {
		parsed := protocol.ParseToolSchema(t)
		cap := buildCapability(serverName, parsed, t)

		if cap.RiskLevel == "read" {
			stats.ReadOnly++
		} else {
			stats.Mutating++
		}

		// Estimate raw schema tokens (rough: 1 token per 4 chars of JSON)
		raw, _ := json.Marshal(t)
		stats.SchemaTokens += len(raw) / 4

		idx.Capabilities = append(idx.Capabilities, cap)
	}

	// Estimate wrapped tokens (just the capability summaries)
	wrapped, _ := json.Marshal(idx.Capabilities)
	stats.WrappedTokens = len(wrapped) / 4

	idx.ServerStats[serverName] = stats
	idx.buildTermIndex()

	log.Info("compiled %d capabilities for %s (schema: ~%d tokens -> wrapped: ~%d tokens)",
		len(tools), serverName, stats.SchemaTokens, stats.WrappedTokens)

	return idx
}

// Merge combines multiple capability indexes into one.
func Merge(indexes ...*CapabilityIndex) *CapabilityIndex {
	merged := &CapabilityIndex{
		Capabilities: make([]Capability, 0),
		ServerStats:  make(map[string]*Stats),
	}

	for _, idx := range indexes {
		merged.Capabilities = append(merged.Capabilities, idx.Capabilities...)
		for k, v := range idx.ServerStats {
			merged.ServerStats[k] = v
		}
	}

	merged.buildTermIndex()
	return merged
}

// Search finds capabilities matching the given query using TF-IDF scoring.
// Terms are expanded with synonyms (e.g., "find" -> "search", "bugs" -> "issues")
// before scoring. Each term's contribution is weighted by inverse document
// frequency: rare terms that appear in fewer capabilities score higher.
func (idx *CapabilityIndex) Search(query string, limit int) []Capability {
	if idx.byTerm == nil {
		idx.buildTermIndex()
	}

	if limit <= 0 {
		limit = 20
	}

	terms := tokenize(query)
	expanded := expandSynonyms(terms)

	// Total number of capabilities (for IDF calculation)
	N := float64(len(idx.Capabilities))
	if N == 0 {
		return nil
	}

	scores := make(map[int]float64)

	for _, term := range expanded {
		// Exact term matches
		if indices, ok := idx.byTerm[term]; ok {
			idf := math.Log(1 + N/float64(len(indices)))
			for _, i := range indices {
				scores[i] += idf
			}
		}

		// Partial matches (substring containment) at reduced weight
		for indexTerm, indices := range idx.byTerm {
			if indexTerm == term {
				continue // already scored above
			}
			if strings.Contains(indexTerm, term) || strings.Contains(term, indexTerm) {
				idf := math.Log(1+N/float64(len(indices))) * 0.5
				for _, i := range indices {
					scores[i] += idf
				}
			}
		}
	}

	// Sort by score descending
	type scored struct {
		index int
		score float64
	}
	var results []scored
	for i, s := range scores {
		results = append(results, scored{i, s})
	}
	sort.Slice(results, func(a, b int) bool {
		return results[a].score > results[b].score
	})

	if len(results) > limit {
		results = results[:limit]
	}

	caps := make([]Capability, len(results))
	for i, r := range results {
		caps[i] = idx.Capabilities[r.index]
	}
	return caps
}

// synonyms maps common query terms to their MCP-tool equivalents.
// This lets "find open bugs" match tools named "search_issues".
var synonyms = map[string][]string{
	"find":         {"search", "list", "get", "query", "lookup", "discover"},
	"search":       {"find", "list", "get", "query", "lookup", "discover"},
	"bugs":         {"issues", "errors", "defects"},
	"issues":       {"bugs", "errors", "tickets", "problems"},
	"prs":          {"pull", "requests", "merge"},
	"repos":        {"repositories", "projects"},
	"repositories": {"repos", "projects"},
	"remove":       {"delete", "destroy", "drop"},
	"delete":       {"remove", "destroy", "drop"},
	"make":         {"create", "add", "new"},
	"create":       {"make", "add", "new"},
	"change":       {"update", "modify", "edit", "patch"},
	"update":       {"change", "modify", "edit", "patch"},
	"show":         {"get", "read", "view", "display"},
	"get":          {"show", "read", "fetch", "retrieve"},
	"files":        {"file", "content", "directory", "path"},
	"open":         {"active", "pending", "unresolved"},
	"close":        {"closed", "resolved", "done"},
}

// expandSynonyms adds synonym terms to the query terms.
// Each synonym is added once; duplicates are suppressed.
func expandSynonyms(terms []string) []string {
	seen := make(map[string]bool)
	var expanded []string
	for _, t := range terms {
		if !seen[t] {
			seen[t] = true
			expanded = append(expanded, t)
		}
		if syns, ok := synonyms[t]; ok {
			for _, s := range syns {
				if !seen[s] {
					seen[s] = true
					expanded = append(expanded, s)
				}
			}
		}
	}
	return expanded
}

// ForServer returns all capabilities for a given server name.
func (idx *CapabilityIndex) ForServer(serverName string) []Capability {
	var caps []Capability
	for _, c := range idx.Capabilities {
		if c.ServerName == serverName {
			caps = append(caps, c)
		}
	}
	return caps
}

// Save writes the capability index to disk.
func (idx *CapabilityIndex) Save(serverName string) error {
	dir := config.CapabilitiesDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, serverName+".json")
	return config.SaveJSON(path, idx)
}

// Load reads a capability index from disk.
func Load(serverName string) (*CapabilityIndex, error) {
	path := filepath.Join(config.CapabilitiesDir(), serverName+".json")
	idx := &CapabilityIndex{}
	if err := config.LoadJSON(path, idx); err != nil {
		return nil, fmt.Errorf("failed to load capabilities for %s: %w", serverName, err)
	}
	idx.buildTermIndex()
	return idx, nil
}

// buildCapability creates a Capability from a parsed tool schema.
func buildCapability(serverName string, parsed protocol.ToolSchema, raw mcp.Tool) Capability {
	cap := Capability{
		ServerName: serverName,
		ToolName:   parsed.Name,
		Summary:    truncate(parsed.Description, 120),
		Tags:       inferTags(parsed.Name, parsed.Description),
		RiskLevel:  inferRisk(parsed),
		InputShape: summarizeInput(parsed.Parameters),
	}

	// Infer group from tool name
	cap.Group = inferGroup(parsed.Name)

	return cap
}

// inferRisk classifies a tool as read-only, write, or dangerous.
func inferRisk(t protocol.ToolSchema) string {
	if t.Annotations != nil {
		if t.Annotations.ReadOnly {
			return "read"
		}
		if t.Annotations.Destructive {
			return "dangerous"
		}
	}

	name := strings.ToLower(t.Name)
	desc := strings.ToLower(t.Description)

	// Tokenize into words for matching (avoids false substring matches)
	words := tokenize(name + " " + desc)
	wordSet := make(map[string]bool)
	for _, w := range words {
		wordSet[w] = true
	}

	dangerousWords := []string{"delete", "remove", "destroy", "drop", "purge"}
	for _, w := range dangerousWords {
		if wordSet[w] {
			return "dangerous"
		}
	}

	writeWords := []string{"create", "update", "set", "add", "edit", "modify", "write", "push", "merge", "post", "put", "patch"}
	for _, w := range writeWords {
		if wordSet[w] {
			return "write"
		}
	}

	return "read"
}

// inferTags extracts keyword tags from a tool name and description.
func inferTags(name, description string) []string {
	combined := strings.ToLower(name + " " + description)
	tagSet := make(map[string]bool)

	// Common domain tags
	domains := map[string][]string{
		"github":   {"github", "git", "repo", "repository", "pr", "pull request", "issue", "commit", "branch"},
		"search":   {"search", "find", "query", "lookup", "discover"},
		"file":     {"file", "read", "write", "directory", "path", "content"},
		"web":      {"web", "url", "fetch", "http", "browse", "crawl"},
		"docs":     {"docs", "documentation", "library", "package", "api"},
		"code":     {"code", "function", "class", "module", "source"},
		"database": {"database", "db", "sql", "table", "query"},
	}

	for tag, keywords := range domains {
		for _, kw := range keywords {
			if strings.Contains(combined, kw) {
				tagSet[tag] = true
				break
			}
		}
	}

	// Extract name parts as tags
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	for _, p := range parts {
		if len(p) > 2 {
			tagSet[strings.ToLower(p)] = true
		}
	}

	tags := make([]string, 0, len(tagSet))
	for t := range tagSet {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	return tags
}

// inferGroup groups tools by prefix or domain.
func inferGroup(name string) string {
	parts := strings.SplitN(name, "_", 2)
	if len(parts) > 1 && len(parts[0]) > 2 {
		return parts[0]
	}
	parts = strings.SplitN(name, "-", 2)
	if len(parts) > 1 && len(parts[0]) > 2 {
		return parts[0]
	}
	return "general"
}

// summarizeInput produces a short description of a tool's parameters.
func summarizeInput(params []protocol.ParameterSchema) string {
	if len(params) == 0 {
		return "(none)"
	}
	var parts []string
	for _, p := range params {
		marker := ""
		if p.Required {
			marker = "*"
		}
		parts = append(parts, fmt.Sprintf("%s%s:%s", p.Name, marker, p.Type))
	}
	return strings.Join(parts, ", ")
}

// tokenize splits a string into lowercase search terms.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '_' || r == '-' || r == '.' || r == ',' || r == '/'
	})
	seen := make(map[string]bool)
	var result []string
	for _, f := range fields {
		if !seen[f] && len(f) > 1 {
			seen[f] = true
			result = append(result, f)
		}
	}
	return result
}

// buildTermIndex builds the in-memory term-to-capability index.
func (idx *CapabilityIndex) buildTermIndex() {
	idx.byTerm = make(map[string][]int)
	for i, cap := range idx.Capabilities {
		terms := tokenize(cap.ToolName + " " + cap.Summary + " " + cap.ServerName)
		for _, tag := range cap.Tags {
			terms = append(terms, tokenize(tag)...)
		}
		for _, term := range terms {
			idx.byTerm[term] = append(idx.byTerm[term], i)
		}
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
