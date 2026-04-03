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
