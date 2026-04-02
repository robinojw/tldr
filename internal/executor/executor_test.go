package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/robinwhite/gobbler/internal/policy"
	"github.com/robinwhite/gobbler/pkg/config"
)

// mockCaller is a test implementation of ToolCaller.
type mockCaller struct {
	responses map[string]*mcp.CallToolResult
	errors    map[string]error
	calls     []callRecord
	delay     time.Duration
	callCount atomic.Int64
}

type callRecord struct {
	Tool string
	Args map[string]interface{}
	Time time.Time
}

func (m *mockCaller) CallTool(ctx context.Context, name string, args map[string]interface{}) (*mcp.CallToolResult, error) {
	m.callCount.Add(1)
	m.calls = append(m.calls, callRecord{Tool: name, Args: args, Time: time.Now()})

	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if err, ok := m.errors[name]; ok {
		return nil, err
	}
	if resp, ok := m.responses[name]; ok {
		return resp, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{Text: "default response"},
		},
	}, nil
}

func newMockCaller(responses map[string]interface{}) *mockCaller {
	m := &mockCaller{
		responses: make(map[string]*mcp.CallToolResult),
		errors:    make(map[string]error),
	}
	for name, val := range responses {
		data, _ := json.Marshal(val)
		m.responses[name] = &mcp.CallToolResult{
			Content: []mcp.Content{
				mcp.TextContent{Text: string(data)},
			},
		}
	}
	return m
}

func defaultPolicy() *policy.Enforcer {
	cfg := config.DefaultPolicyConfig()
	cfg.AllowMutating = true // allow everything by default in tests
	cfg.StepTimeout = 5
	cfg.PlanTimeout = 30
	cfg.MaxSteps = 10
	return policy.NewEnforcer(cfg)
}

func TestExecute_SingleStep(t *testing.T) {
	mock := newMockCaller(map[string]interface{}{
		"list_prs": []map[string]interface{}{
			{"number": 1, "title": "First PR"},
			{"number": 2, "title": "Second PR"},
		},
	})

	clients := map[string]ToolCaller{"github": mock}
	exec := NewExecutor(clients, defaultPolicy())

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "github", Tool: "list_prs", Arguments: map[string]interface{}{}},
		},
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.Error)
	}
	if result.StepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", result.StepCount)
	}
	if result.PlanID == "" {
		t.Error("expected non-empty planID")
	}
}

func TestExecute_StepReferences(t *testing.T) {
	// Step 1 returns an issue number, step 2 uses it as an argument
	mock := &mockCaller{
		responses: make(map[string]*mcp.CallToolResult),
		errors:    make(map[string]error),
	}

	issueData, _ := json.Marshal(map[string]interface{}{"number": 42, "title": "Bug"})
	mock.responses["get_issue"] = &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Text: string(issueData)}},
	}
	commentData, _ := json.Marshal(map[string]interface{}{"id": 99, "body": "Fixed"})
	mock.responses["add_comment"] = &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Text: string(commentData)}},
	}

	clients := map[string]ToolCaller{"github": mock}
	exec := NewExecutor(clients, defaultPolicy())

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "github", Tool: "get_issue", Arguments: map[string]interface{}{
				"owner": "org", "repo": "repo", "number": 42,
			}},
			{ID: "s2", Server: "github", Tool: "add_comment", Arguments: map[string]interface{}{
				"issue_number": "${s1.number}",
				"body":         "Closing issue ${s1.title}",
			}, DependsOn: []string{"s1"}},
		},
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.Error)
	}

	// Verify step 2 received the resolved argument
	if len(mock.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(mock.calls))
	}
	s2Args := mock.calls[1].Args
	if s2Args["issue_number"] != float64(42) {
		t.Errorf("expected issue_number=42, got %v", s2Args["issue_number"])
	}
}

func TestExecute_UnknownServer(t *testing.T) {
	clients := map[string]ToolCaller{}
	exec := NewExecutor(clients, defaultPolicy())

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "nonexistent", Tool: "something"},
		},
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("expected failure for unknown server")
	}
	if result.Error == "" {
		t.Error("expected error message")
	}
}

func TestExecute_ToolBlocked(t *testing.T) {
	mock := newMockCaller(nil)
	clients := map[string]ToolCaller{"github": mock}

	cfg := config.DefaultPolicyConfig()
	cfg.AllowMutating = true
	cfg.BlockedTools = []string{"delete_repo"}
	enforcer := policy.NewEnforcer(cfg)

	exec := NewExecutor(clients, enforcer)

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "github", Tool: "delete_repo"},
		},
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("expected failure for blocked tool")
	}
	if mock.callCount.Load() != 0 {
		t.Error("expected no tool calls when tool is blocked")
	}
}

func TestExecute_MutatingBlocked(t *testing.T) {
	mock := newMockCaller(nil)
	clients := map[string]ToolCaller{"github": mock}

	cfg := config.DefaultPolicyConfig()
	cfg.AllowMutating = false // block mutating tools
	enforcer := policy.NewEnforcer(cfg)

	exec := NewExecutor(clients, enforcer)

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "github", Tool: "create_issue"},
		},
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("expected failure for mutating tool when allowMutating is false")
	}
	if mock.callCount.Load() != 0 {
		t.Error("expected no tool calls when mutating is blocked")
	}
}

func TestExecute_MutatingAllowed(t *testing.T) {
	mock := newMockCaller(map[string]interface{}{
		"create_issue": map[string]interface{}{"number": 1},
	})
	clients := map[string]ToolCaller{"github": mock}

	cfg := config.DefaultPolicyConfig()
	cfg.AllowMutating = true
	enforcer := policy.NewEnforcer(cfg)

	exec := NewExecutor(clients, enforcer)

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "github", Tool: "create_issue", Arguments: map[string]interface{}{
				"title": "Test",
			}},
		},
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success when mutating allowed, got: %s", result.Error)
	}
}

func TestExecute_StepTimeout(t *testing.T) {
	mock := &mockCaller{
		responses: make(map[string]*mcp.CallToolResult),
		errors:    make(map[string]error),
		delay:     10 * time.Second, // will timeout
	}

	clients := map[string]ToolCaller{"github": mock}

	cfg := config.DefaultPolicyConfig()
	cfg.AllowMutating = true
	cfg.StepTimeout = 1 // 1 second timeout
	cfg.PlanTimeout = 5
	enforcer := policy.NewEnforcer(cfg)

	exec := NewExecutor(clients, enforcer)

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "github", Tool: "slow_tool"},
		},
	}

	start := time.Now()
	result, err := exec.Execute(context.Background(), plan)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("expected failure due to timeout")
	}
	// Should timeout in ~1 second, not 10
	if elapsed > 3*time.Second {
		t.Errorf("expected timeout in ~1s, took %v", elapsed)
	}
}

func TestExecute_TooManySteps(t *testing.T) {
	mock := newMockCaller(nil)
	clients := map[string]ToolCaller{"github": mock}

	cfg := config.DefaultPolicyConfig()
	cfg.AllowMutating = true
	cfg.MaxSteps = 2
	enforcer := policy.NewEnforcer(cfg)

	exec := NewExecutor(clients, enforcer)

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "github", Tool: "a"},
			{ID: "s2", Server: "github", Tool: "b"},
			{ID: "s3", Server: "github", Tool: "c"},
		},
	}

	_, err := exec.Execute(context.Background(), plan)
	if err == nil {
		t.Error("expected error for too many steps")
	}
}

func TestExecute_EmptyPlan(t *testing.T) {
	exec := NewExecutor(nil, defaultPolicy())

	_, err := exec.Execute(context.Background(), Plan{})
	if err == nil {
		t.Error("expected error for empty plan")
	}
}

func TestExecute_ConcurrentSteps(t *testing.T) {
	// Two steps with no dependencies should run concurrently
	callTimes := make(map[string]time.Time)
	var mu sync.Mutex

	mock := &mockCaller{
		responses: make(map[string]*mcp.CallToolResult),
		errors:    make(map[string]error),
		delay:     100 * time.Millisecond,
	}

	data, _ := json.Marshal(map[string]interface{}{"ok": true})
	mock.responses["tool_a"] = &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Text: string(data)}},
	}
	mock.responses["tool_b"] = &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Text: string(data)}},
	}

	// Wrap to track start times
	wrappedMock := &timingCaller{
		inner:     mock,
		callTimes: callTimes,
		mu:        &mu,
	}

	clients := map[string]ToolCaller{"test": wrappedMock}
	exec := NewExecutor(clients, defaultPolicy())

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "test", Tool: "tool_a"},
			{ID: "s2", Server: "test", Tool: "tool_b"},
			// No DependsOn -- these should run concurrently
		},
	}

	start := time.Now()
	result, err := exec.Execute(context.Background(), plan)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}

	// With 100ms delay each, sequential would take ~200ms.
	// Concurrent should take ~100ms. Allow some slack.
	if elapsed > 180*time.Millisecond {
		t.Errorf("expected concurrent execution (~100ms), took %v -- steps may not be running in parallel", elapsed)
	}
}

func TestExecute_DependencyChain(t *testing.T) {
	// s1 -> s2 -> s3: must execute sequentially
	mock := newMockCaller(map[string]interface{}{
		"step1": map[string]interface{}{"id": 1},
		"step2": map[string]interface{}{"id": 2},
		"step3": map[string]interface{}{"id": 3},
	})

	clients := map[string]ToolCaller{"test": mock}
	exec := NewExecutor(clients, defaultPolicy())

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "test", Tool: "step1"},
			{ID: "s2", Server: "test", Tool: "step2", DependsOn: []string{"s1"}},
			{ID: "s3", Server: "test", Tool: "step3", DependsOn: []string{"s2"}},
		},
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}
	if result.StepCount != 3 {
		t.Errorf("expected 3 steps, got %d", result.StepCount)
	}
}

func TestExecute_InvalidDependency(t *testing.T) {
	mock := newMockCaller(nil)
	clients := map[string]ToolCaller{"test": mock}
	exec := NewExecutor(clients, defaultPolicy())

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "test", Tool: "a", DependsOn: []string{"nonexistent"}},
		},
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("expected failure for invalid dependency")
	}
}

func TestExecute_ResultRefs(t *testing.T) {
	mock := newMockCaller(map[string]interface{}{
		"get_data": map[string]interface{}{"items": []int{1, 2, 3}},
	})

	clients := map[string]ToolCaller{"test": mock}
	exec := NewExecutor(clients, defaultPolicy())

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "test", Tool: "get_data"},
		},
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}

	// Result should have a ref handle for s1
	if result.Refs == nil || result.Refs["s1"] == "" {
		t.Error("expected ref handle for s1")
	}
}

func TestCallRaw_BlockedTool(t *testing.T) {
	mock := newMockCaller(nil)
	clients := map[string]ToolCaller{"github": mock}

	cfg := config.DefaultPolicyConfig()
	cfg.BlockedTools = []string{"github/delete_repo"}
	enforcer := policy.NewEnforcer(cfg)

	exec := NewExecutor(clients, enforcer)

	_, err := exec.CallRaw(context.Background(), "github", "delete_repo", nil)
	if err == nil {
		t.Error("expected error for blocked tool in CallRaw")
	}
}

func TestCallRaw_MutatingBlocked(t *testing.T) {
	mock := newMockCaller(nil)
	clients := map[string]ToolCaller{"github": mock}

	cfg := config.DefaultPolicyConfig()
	cfg.AllowMutating = false
	enforcer := policy.NewEnforcer(cfg)

	exec := NewExecutor(clients, enforcer)

	_, err := exec.CallRaw(context.Background(), "github", "create_issue", nil)
	if err == nil {
		t.Error("expected error for mutating tool in CallRaw when AllowMutating is false")
	}
}

func TestCallRaw_StoresResult(t *testing.T) {
	mock := newMockCaller(map[string]interface{}{
		"list_items": []int{1, 2, 3},
	})

	clients := map[string]ToolCaller{"test": mock}
	exec := NewExecutor(clients, defaultPolicy())

	rawResult, err := exec.CallRaw(context.Background(), "test", "list_items", nil)
	if err != nil {
		t.Fatal(err)
	}

	if rawResult.Ref == "" {
		t.Error("expected non-empty ref handle")
	}

	// Verify the result is stored
	stored, ok := exec.Store().Get(rawResult.Ref)
	if !ok {
		t.Fatal("expected result to be in store")
	}
	if stored.Server != "test" || stored.Tool != "list_items" {
		t.Errorf("unexpected stored result: server=%s tool=%s", stored.Server, stored.Tool)
	}
}

func TestExecute_PartialFailure(t *testing.T) {
	mock := &mockCaller{
		responses: make(map[string]*mcp.CallToolResult),
		errors:    make(map[string]error),
	}

	data, _ := json.Marshal(map[string]interface{}{"ok": true})
	mock.responses["good_tool"] = &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Text: string(data)}},
	}
	mock.errors["bad_tool"] = fmt.Errorf("upstream error: connection refused")

	clients := map[string]ToolCaller{"test": mock}
	exec := NewExecutor(clients, defaultPolicy())

	plan := Plan{
		Steps: []Step{
			{ID: "s1", Server: "test", Tool: "good_tool"},
			{ID: "s2", Server: "test", Tool: "bad_tool", DependsOn: []string{"s1"}},
		},
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("expected failure due to step s2 error")
	}
	if result.Error == "" {
		t.Error("expected error message")
	}
}

// timingCaller wraps a ToolCaller and records call start times.
type timingCaller struct {
	inner     ToolCaller
	callTimes map[string]time.Time
	mu        *sync.Mutex
}

func (tc *timingCaller) CallTool(ctx context.Context, name string, args map[string]interface{}) (*mcp.CallToolResult, error) {
	tc.mu.Lock()
	tc.callTimes[name] = time.Now()
	tc.mu.Unlock()
	return tc.inner.CallTool(ctx, name, args)
}
