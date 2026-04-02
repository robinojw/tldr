package compiler

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestCompile(t *testing.T) {
	tools := []mcp.Tool{
		mcp.NewTool("list_pull_requests",
			mcp.WithDescription("List pull requests in a GitHub repository"),
			mcp.WithString("owner", mcp.Required(), mcp.Description("Repository owner")),
			mcp.WithString("repo", mcp.Required(), mcp.Description("Repository name")),
			mcp.WithString("state", mcp.Description("Filter by state")),
		),
		mcp.NewTool("get_file_contents",
			mcp.WithDescription("Get the contents of a file from a GitHub repository"),
			mcp.WithString("owner", mcp.Required()),
			mcp.WithString("repo", mcp.Required()),
			mcp.WithString("path", mcp.Required()),
		),
		mcp.NewTool("create_issue",
			mcp.WithDescription("Create a new issue in a GitHub repository"),
			mcp.WithString("owner", mcp.Required()),
			mcp.WithString("repo", mcp.Required()),
			mcp.WithString("title", mcp.Required()),
			mcp.WithString("body"),
		),
		mcp.NewTool("delete_branch",
			mcp.WithDescription("Delete a branch from a GitHub repository"),
			mcp.WithString("owner", mcp.Required()),
			mcp.WithString("repo", mcp.Required()),
			mcp.WithString("branch", mcp.Required()),
		),
	}

	idx := Compile("github-mcp", tools)

	// Check basic compilation
	if len(idx.Capabilities) != 4 {
		t.Fatalf("expected 4 capabilities, got %d", len(idx.Capabilities))
	}

	stats := idx.ServerStats["github-mcp"]
	if stats == nil {
		t.Fatal("expected stats for github-mcp")
	}
	if stats.ToolCount != 4 {
		t.Errorf("expected 4 tools, got %d", stats.ToolCount)
	}

	// Token reduction should be positive
	if stats.WrappedTokens >= stats.SchemaTokens {
		t.Errorf("expected wrapped tokens (%d) < schema tokens (%d)", stats.WrappedTokens, stats.SchemaTokens)
	}

	// Check risk inference
	for _, cap := range idx.Capabilities {
		switch cap.ToolName {
		case "list_pull_requests":
			if cap.RiskLevel != "read" {
				t.Errorf("list_pull_requests should be 'read', got %q", cap.RiskLevel)
			}
		case "create_issue":
			if cap.RiskLevel != "write" {
				t.Errorf("create_issue should be 'write', got %q", cap.RiskLevel)
			}
		case "delete_branch":
			if cap.RiskLevel != "dangerous" {
				t.Errorf("delete_branch should be 'dangerous', got %q", cap.RiskLevel)
			}
		}
	}
}

func TestSearch(t *testing.T) {
	tools := []mcp.Tool{
		mcp.NewTool("list_pull_requests",
			mcp.WithDescription("List pull requests in a GitHub repository"),
		),
		mcp.NewTool("search_code",
			mcp.WithDescription("Search for code in GitHub repositories"),
		),
		mcp.NewTool("tavily_search",
			mcp.WithDescription("Search the web using Tavily"),
		),
	}

	idx := Compile("test-server", tools)

	// Search for "pull requests"
	results := idx.Search("pull requests", 10)
	if len(results) == 0 {
		t.Fatal("expected results for 'pull requests'")
	}
	if results[0].ToolName != "list_pull_requests" {
		t.Errorf("expected first result to be list_pull_requests, got %s", results[0].ToolName)
	}

	// Search for "search"
	results = idx.Search("search", 10)
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results for 'search', got %d", len(results))
	}

	// Search with limit
	results = idx.Search("search", 1)
	if len(results) != 1 {
		t.Errorf("expected 1 result with limit=1, got %d", len(results))
	}
}

func TestMerge(t *testing.T) {
	idx1 := Compile("github", []mcp.Tool{
		mcp.NewTool("list_prs", mcp.WithDescription("List PRs")),
	})
	idx2 := Compile("tavily", []mcp.Tool{
		mcp.NewTool("web_search", mcp.WithDescription("Search the web")),
	})

	merged := Merge(idx1, idx2)

	if len(merged.Capabilities) != 2 {
		t.Fatalf("expected 2 capabilities after merge, got %d", len(merged.Capabilities))
	}
	if len(merged.ServerStats) != 2 {
		t.Errorf("expected 2 server stats entries, got %d", len(merged.ServerStats))
	}

	// Search should work across merged index
	results := merged.Search("search", 10)
	if len(results) == 0 {
		t.Error("expected search results from merged index")
	}
}

func TestInferTags(t *testing.T) {
	tags := inferTags("github_list_pull_requests", "List pull requests in a repository")

	tagSet := make(map[string]bool)
	for _, tag := range tags {
		tagSet[tag] = true
	}

	if !tagSet["github"] {
		t.Error("expected 'github' tag")
	}
}
