package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/robinojw/tldr/pkg/config"
)

func TestShield_SmallOutput(t *testing.T) {
	e := NewEnforcer(config.DefaultPolicyConfig())
	result := e.Shield("hello world")
	if result != "hello world" {
		t.Errorf("small output should pass through unchanged, got %q", result)
	}
}

func TestShield_LargeOutput(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.MaxOutputBytes = 100
	cfg.MaxOutputTokens = 0 // disable token limit for this test
	e := NewEnforcer(cfg)

	large := strings.Repeat("x", 500)
	result := e.Shield(large)

	if len(result) > 200 { // some overhead from truncation message
		t.Errorf("shielded output should be limited, got %d bytes", len(result))
	}
	if !strings.Contains(result, "truncated") {
		t.Error("expected truncation notice in output")
	}
}

func TestShieldJSON_ArrayTruncation(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.MaxArrayLength = 3
	cfg.CompactArrays = false // use legacy path for this test
	e := NewEnforcer(cfg)

	input := make([]interface{}, 100)
	for i := range input {
		input[i] = map[string]interface{}{"id": i}
	}

	result := e.ShieldJSON(input)

	// Should be truncated to a map with _items, _truncated, _total
	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatal("expected map result from array truncation")
	}
	if m["_truncated"] != true {
		t.Error("expected _truncated=true")
	}
	if m["_total"] != 100 {
		t.Errorf("expected _total=100, got %v", m["_total"])
	}
	items := m["_items"].([]interface{})
	if len(items) != 3 {
		t.Errorf("expected 3 items, got %d", len(items))
	}
}

func TestShieldJSON_StringTruncation(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.MaxStringLength = 10
	e := NewEnforcer(cfg)

	result := e.ShieldJSON("this is a very long string that should be truncated")
	s, ok := result.(string)
	if !ok {
		t.Fatal("expected string result")
	}
	if len(s) > 60 { // 10 chars + "... [N chars total]"
		t.Errorf("string should be truncated, got %d chars", len(s))
	}
}

func TestShieldFields(t *testing.T) {
	e := NewEnforcer(config.DefaultPolicyConfig())

	input := `{"title": "Bug", "body": "Long description...", "labels": ["bug"], "id": 42}`
	result, err := e.ShieldFields(input, []string{"title", "id"})
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatal(err)
	}

	if parsed["title"] != "Bug" {
		t.Errorf("expected title=Bug, got %v", parsed["title"])
	}
	if parsed["body"] != nil {
		t.Error("body should be filtered out")
	}
}

func TestIsToolBlocked(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.BlockedTools = []string{"dangerous_tool", "github/delete_repo"}
	e := NewEnforcer(cfg)

	if !e.IsToolBlocked("dangerous_tool") {
		t.Error("expected dangerous_tool to be blocked")
	}
	if e.IsToolBlocked("safe_tool") {
		t.Error("expected safe_tool to not be blocked")
	}
}

// --- New tests for token-aware shielding and smart compaction ---

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"", 0},
		{"hi", 1},
		{"hello world", 3},                  // 11 chars / 4 = 2.75 -> 3
		{strings.Repeat("x", 100), 25},      // 100 / 4 = 25
		{strings.Repeat("x", 4096), 1024},   // 4096 / 4 = 1024
	}

	for _, tt := range tests {
		got := EstimateTokens(tt.input)
		if got != tt.expected {
			t.Errorf("EstimateTokens(%d chars) = %d, want %d", len(tt.input), got, tt.expected)
		}
	}
}

func TestShield_TokenLimit(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.MaxOutputBytes = 100000   // 100KB byte limit
	cfg.MaxOutputTokens = 100     // 100 tokens = ~400 chars
	e := NewEnforcer(cfg)

	// 800 chars = ~200 tokens, exceeds 100 token limit
	large := strings.Repeat("abcd", 200)
	result := e.Shield(large)

	if len(result) >= len(large) {
		t.Errorf("expected output to be truncated by token limit, got %d bytes (original %d)", len(result), len(large))
	}
	if !strings.Contains(result, "truncated") {
		t.Error("expected truncation notice")
	}
	if !strings.Contains(result, "tokens") {
		t.Error("expected token count in truncation notice")
	}
}

func TestShield_TokenLimitPassthrough(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.MaxOutputBytes = 100000
	cfg.MaxOutputTokens = 1000 // 1000 tokens = ~4000 chars
	e := NewEnforcer(cfg)

	// 100 chars = ~25 tokens, well under limit
	small := strings.Repeat("x", 100)
	result := e.Shield(small)

	if result != small {
		t.Error("small output should pass through unchanged under token limit")
	}
}

func TestCompactArray_StripURLs(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.CompactArrays = true
	cfg.MaxFieldBytes = 100
	e := NewEnforcer(cfg)

	arr := []interface{}{
		map[string]interface{}{
			"name":         "file.go",
			"path":         "internal/file.go",
			"sha":          "abc123",
			"html_url":     "https://github.com/org/repo/blob/main/internal/file.go",
			"git_url":      "https://api.github.com/repos/org/repo/git/blobs/abc123",
			"download_url": "https://raw.githubusercontent.com/org/repo/main/internal/file.go",
			"url":          "https://api.github.com/repos/org/repo/contents/internal/file.go?ref=main",
		},
		map[string]interface{}{
			"name":         "main.go",
			"path":         "cmd/main.go",
			"sha":          "def456",
			"html_url":     "https://github.com/org/repo/blob/main/cmd/main.go",
			"git_url":      "https://api.github.com/repos/org/repo/git/blobs/def456",
			"download_url": "https://raw.githubusercontent.com/org/repo/main/cmd/main.go",
			"url":          "https://api.github.com/repos/org/repo/contents/cmd/main.go?ref=main",
		},
	}

	compacted := e.compactArray(arr)

	for i, item := range compacted {
		obj, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("element %d: expected object, got %T", i, item)
		}

		// Signal fields should be kept
		if obj["name"] == nil {
			t.Errorf("element %d: name should be kept", i)
		}
		if obj["path"] == nil {
			t.Errorf("element %d: path should be kept", i)
		}
		if obj["sha"] == nil {
			t.Errorf("element %d: sha should be kept", i)
		}

		// URL fields should be stripped
		if obj["html_url"] != nil {
			t.Errorf("element %d: html_url should be stripped", i)
		}
		if obj["git_url"] != nil {
			t.Errorf("element %d: git_url should be stripped", i)
		}
		if obj["download_url"] != nil {
			t.Errorf("element %d: download_url should be stripped", i)
		}

		// _omitted should list the stripped fields
		omitted, ok := obj["_omitted"].([]string)
		if !ok {
			t.Fatalf("element %d: expected _omitted list", i)
		}
		if len(omitted) == 0 {
			t.Errorf("element %d: expected some omitted fields", i)
		}
	}
}

func TestCompactArray_PreserveSignalFields(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.CompactArrays = true
	cfg.MaxFieldBytes = 50 // very low threshold
	e := NewEnforcer(cfg)

	arr := []interface{}{
		map[string]interface{}{
			"id":          42,
			"title":       "This is a somewhat long title that exceeds 50 bytes easily when serialized",
			"description": "A long description that also exceeds the threshold for field bytes",
			"state":       "open",
			"created_at":  "2026-01-01T00:00:00Z",
		},
	}

	compacted := e.compactArray(arr)
	obj := compacted[0].(map[string]interface{})

	// All these are signal fields and should be preserved regardless of size
	for _, field := range []string{"id", "title", "description", "state", "created_at"} {
		if obj[field] == nil {
			t.Errorf("signal field %q should be preserved even if large", field)
		}
	}
}

func TestCompactArray_StripNestedObjects(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.CompactArrays = true
	cfg.MaxFieldBytes = 100
	e := NewEnforcer(cfg)

	arr := []interface{}{
		map[string]interface{}{
			"sha":     "abc123",
			"message": "Fix bug",
			"author": map[string]interface{}{
				"login":       "user1",
				"id":          12345,
				"avatar_url":  "https://avatars.githubusercontent.com/u/12345?v=4",
				"profile_url": "https://github.com/user1",
			},
			"committer": map[string]interface{}{
				"login":       "user1",
				"id":          12345,
				"avatar_url":  "https://avatars.githubusercontent.com/u/12345?v=4",
				"profile_url": "https://github.com/user1",
			},
		},
	}

	compacted := e.compactArray(arr)
	obj := compacted[0].(map[string]interface{})

	// sha and message are signal fields
	if obj["sha"] == nil {
		t.Error("sha should be preserved")
	}
	if obj["message"] == nil {
		t.Error("message should be preserved")
	}

	// Nested objects over MaxFieldBytes/2 should be stripped
	omitted, _ := obj["_omitted"].([]string)
	hasAuthor := false
	hasCommitter := false
	for _, f := range omitted {
		if f == "author" {
			hasAuthor = true
		}
		if f == "committer" {
			hasCommitter = true
		}
	}
	if !hasAuthor {
		t.Error("author (nested object) should be omitted")
	}
	if !hasCommitter {
		t.Error("committer (nested object) should be omitted")
	}
}

func TestShieldStructured_TokenAwareArrayTrimming(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.MaxOutputTokens = 200 // ~800 chars
	cfg.MaxOutputBytes = 100000
	cfg.CompactArrays = true
	cfg.MaxFieldBytes = 100
	e := NewEnforcer(cfg)

	// Build a large array that exceeds token limit even after compaction
	arr := make([]interface{}, 50)
	for i := range arr {
		arr[i] = map[string]interface{}{
			"id":   i,
			"name": strings.Repeat("item", 5),
		}
	}

	raw, _ := json.Marshal(arr)
	result := e.ShieldStructured(string(raw))

	if !result.WasTruncated {
		t.Error("expected truncation for large array exceeding token limit")
	}
	if result.Meta == nil {
		t.Fatal("expected meta with pagination info")
	}
	if result.Meta.Total != 50 {
		t.Errorf("expected total=50, got %d", result.Meta.Total)
	}
	if result.Meta.Count >= 50 {
		t.Errorf("expected fewer items shown, got %d", result.Meta.Count)
	}
	if !result.Meta.HasMore {
		t.Error("expected hasMore=true")
	}
}

func TestShieldStructured_SmallArrayPassthrough(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.CompactArrays = true
	e := NewEnforcer(cfg)

	arr := []interface{}{
		map[string]interface{}{"id": 1, "name": "foo"},
		map[string]interface{}{"id": 2, "name": "bar"},
	}

	raw, _ := json.Marshal(arr)
	result := e.ShieldStructured(string(raw))

	if result.WasTruncated {
		t.Error("small array should not be truncated")
	}
}

func TestCompactArray_DisabledFallsBackToLegacy(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.CompactArrays = false
	cfg.MaxArrayLength = 2
	e := NewEnforcer(cfg)

	arr := []interface{}{
		map[string]interface{}{"id": 1, "url": "https://example.com/very/long/url"},
		map[string]interface{}{"id": 2, "url": "https://example.com/very/long/url"},
		map[string]interface{}{"id": 3, "url": "https://example.com/very/long/url"},
	}

	compacted := e.compactArray(arr)

	// With CompactArrays disabled, should use legacy summarization
	// which truncates to MaxArrayLength
	m, ok := compacted[0].(map[string]interface{})
	if !ok {
		// Legacy path returns the array items directly (no compaction)
		t.Log("legacy path returned non-object, checking array length")
	} else {
		// URL should NOT be stripped in legacy mode
		if m["url"] == nil {
			t.Error("legacy mode should not strip URL fields")
		}
	}
}

func TestMaxTokenBytes(t *testing.T) {
	// Token limit is tighter than byte limit
	cfg := config.DefaultPolicyConfig()
	cfg.MaxOutputBytes = 100000
	cfg.MaxOutputTokens = 100 // 400 bytes
	e := NewEnforcer(cfg)
	if got := e.maxTokenBytes(); got != 400 {
		t.Errorf("expected 400, got %d", got)
	}

	// Byte limit is tighter than token limit
	cfg2 := config.DefaultPolicyConfig()
	cfg2.MaxOutputBytes = 200
	cfg2.MaxOutputTokens = 1000 // 4000 bytes
	e2 := NewEnforcer(cfg2)
	if got := e2.maxTokenBytes(); got != 200 {
		t.Errorf("expected 200, got %d", got)
	}

	// Token limit disabled
	cfg3 := config.DefaultPolicyConfig()
	cfg3.MaxOutputBytes = 500
	cfg3.MaxOutputTokens = 0
	e3 := NewEnforcer(cfg3)
	if got := e3.maxTokenBytes(); got != 500 {
		t.Errorf("expected 500, got %d", got)
	}
}

func TestDetectHeavyFields_GitHubCommitShape(t *testing.T) {
	cfg := config.DefaultPolicyConfig()
	cfg.CompactArrays = true
	cfg.MaxFieldBytes = 256
	e := NewEnforcer(cfg)

	// Simulate a GitHub list_commits response shape
	arr := []interface{}{
		map[string]interface{}{
			"sha":      "abc123def456",
			"html_url": "https://github.com/org/repo/commit/abc123def456",
			"commit": map[string]interface{}{
				"message": "Fix bug in parser",
				"author": map[string]interface{}{
					"name":  "Alice",
					"email": "alice@example.com",
					"date":  "2026-04-01T10:00:00Z",
				},
			},
			"author": map[string]interface{}{
				"login":       "alice",
				"id":          1234,
				"avatar_url":  "https://avatars.githubusercontent.com/u/1234?v=4",
				"profile_url": "https://github.com/alice",
			},
			"committer": map[string]interface{}{
				"login":       "alice",
				"id":          1234,
				"avatar_url":  "https://avatars.githubusercontent.com/u/1234?v=4",
				"profile_url": "https://github.com/alice",
			},
		},
	}

	heavy := e.detectHeavyFields(arr)

	// html_url should be heavy (URL)
	if !heavy["html_url"] {
		t.Error("html_url should be detected as heavy")
	}

	// sha should NOT be heavy (signal field)
	if heavy["sha"] {
		t.Error("sha should not be heavy (signal field)")
	}
}
