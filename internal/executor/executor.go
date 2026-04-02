// Package executor implements the plan execution engine that orchestrates
// multi-step tool calls against upstream MCP servers. It keeps intermediate
// results shielded from the harness and only returns distilled outputs.
//
// Results persist across plans. Each plan gets a unique ID, and each step
// result is addressable by ref handle (planID:stepID). The model can page
// through stored results using the get_result wrapper tool.
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/robinwhite/gobbler/internal/compiler"
	"github.com/robinwhite/gobbler/internal/logging"
	"github.com/robinwhite/gobbler/internal/policy"
	"github.com/robinwhite/gobbler/internal/resultstore"
)

var log = logging.New("executor")

var planCounter atomic.Int64

// ToolCaller is the interface for calling tools on upstream MCP servers.
// mcpclient.Client implements this, and tests can provide mocks.
type ToolCaller interface {
	CallTool(ctx context.Context, name string, args map[string]interface{}) (*mcp.CallToolResult, error)
}

// Plan represents a structured execution plan submitted by the harness.
type Plan struct {
	Steps  []Step      `json:"steps"`
	Return *ReturnSpec `json:"return,omitempty"`
}

// Step is a single step in an execution plan.
type Step struct {
	ID        string                 `json:"id"`
	Server    string                 `json:"server"`
	Tool      string                 `json:"tool"`
	Arguments map[string]interface{} `json:"arguments"`
	DependsOn []string               `json:"dependsOn,omitempty"` // step IDs that must complete first
}

// ReturnSpec describes what to return from the plan execution.
type ReturnSpec struct {
	Mode     string   `json:"mode"`     // "full", "summary", "fields"
	FromStep string   `json:"fromStep"` // which step's result to return
	Fields   []string `json:"fields,omitempty"`
}

// Result is the output of a plan execution.
type Result struct {
	Success   bool                   `json:"success"`
	PlanID    string                 `json:"planId"`
	StepCount int                    `json:"stepCount"`
	Output    interface{}            `json:"output"`
	Error     string                 `json:"error,omitempty"`
	Duration  time.Duration          `json:"duration"`
	Refs      map[string]string      `json:"refs,omitempty"` // stepID -> ref handle
	Summary   map[string]interface{} `json:"summary,omitempty"`
}

// RawResult is the output of a call_raw invocation.
type RawResult struct {
	Ref     string      `json:"ref"`
	Output  interface{} `json:"output"`
	Shielded bool       `json:"shielded"`
	Meta    *resultstore.SliceMeta `json:"meta,omitempty"`
}

// Executor runs plans against upstream MCP servers.
type Executor struct {
	clients  map[string]ToolCaller
	enforcer *policy.Enforcer
	store    *resultstore.Store
	index    *compiler.CapabilityIndex
}

// NewExecutor creates an executor with the given set of connected MCP clients.
func NewExecutor(clients map[string]ToolCaller, enforcer *policy.Enforcer) *Executor {
	return &Executor{
		clients:  clients,
		enforcer: enforcer,
		store:    resultstore.New(),
	}
}

// NewExecutorWithStore creates an executor with a shared store instance.
func NewExecutorWithStore(clients map[string]ToolCaller, enforcer *policy.Enforcer, store *resultstore.Store) *Executor {
	return &Executor{
		clients:  clients,
		enforcer: enforcer,
		store:    store,
	}
}

// SetCapabilityIndex sets the capability index used for risk-based policy enforcement.
// When set, the executor checks tool risk levels against the AllowMutating policy.
func (e *Executor) SetCapabilityIndex(idx *compiler.CapabilityIndex) {
	e.index = idx
}

// Store returns the executor's result store (for the wrapper to expose get_result).
func (e *Executor) Store() *resultstore.Store {
	return e.store
}

// Execute runs a plan and returns the shielded result.
// Results are stored with plan-scoped ref handles and persist across plans.
func (e *Executor) Execute(ctx context.Context, plan Plan) (*Result, error) {
	start := time.Now()
	planID := fmt.Sprintf("p%d", planCounter.Add(1))

	// Validate plan
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("plan has no steps")
	}
	if len(plan.Steps) > e.enforcer.MaxSteps() {
		return nil, fmt.Errorf("plan has %d steps, max is %d", len(plan.Steps), e.enforcer.MaxSteps())
	}

	// Set plan-level timeout
	planTimeout := time.Duration(e.enforcer.PlanTimeout()) * time.Second
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()

	refs := make(map[string]string) // stepID -> ref handle

	// Build dependency graph and execute steps concurrently where possible.
	// Steps with no unmet dependencies run in parallel; a step waits only
	// on the steps listed in its DependsOn field.
	stepErrors := make(map[string]error)
	completed := make(map[string]bool)
	completedMu := sync.Mutex{}
	completedCond := sync.NewCond(&completedMu)

	// Pre-validate: check blocked/mutating for all steps before executing any
	for _, step := range plan.Steps {
		fullName := step.Server + "/" + step.Tool
		if e.enforcer.IsToolBlocked(fullName) || e.enforcer.IsToolBlocked(step.Tool) {
			return &Result{
				Success:  false,
				PlanID:   planID,
				Error:    fmt.Sprintf("tool %q is blocked by policy", fullName),
				Duration: time.Since(start),
			}, nil
		}

		if !e.enforcer.IsMutatingAllowed() {
			risk := e.lookupToolRisk(step.Server, step.Tool)
			if risk == "write" || risk == "dangerous" {
				return &Result{
					Success:  false,
					PlanID:   planID,
					Error:    fmt.Sprintf("tool %q is %s and mutating is not allowed by policy (set allowMutating: true to permit)", fullName, risk),
					Duration: time.Since(start),
				}, nil
			}
		}
	}

	// Build step index for dependency lookup
	stepIndex := make(map[string]int)
	for i, step := range plan.Steps {
		stepIndex[step.ID] = i
	}

	// Validate dependencies exist
	for _, step := range plan.Steps {
		for _, dep := range step.DependsOn {
			if _, ok := stepIndex[dep]; !ok {
				return &Result{
					Success: false,
					PlanID:  planID,
					Error:   fmt.Sprintf("step %q depends on unknown step %q", step.ID, dep),
					Duration: time.Since(start),
				}, nil
			}
		}
	}

	// Execute steps with dependency-aware concurrency
	var wg sync.WaitGroup
	var firstErr error
	var firstErrMu sync.Mutex

	for i := range plan.Steps {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			step := plan.Steps[idx]

			// Wait for dependencies to complete
			completedMu.Lock()
			for {
				allDone := true
				for _, dep := range step.DependsOn {
					if !completed[dep] {
						allDone = false
						break
					}
					// If a dependency failed, abort
					if stepErrors[dep] != nil {
						completedMu.Unlock()
						return
					}
				}
				if allDone {
					break
				}
				completedCond.Wait()
			}
			completedMu.Unlock()

			// Check if any earlier step has failed (early exit)
			firstErrMu.Lock()
			if firstErr != nil {
				firstErrMu.Unlock()
				return
			}
			firstErrMu.Unlock()

			log.Info("executing step %s: %s/%s (plan %s)", step.ID, step.Server, step.Tool, planID)

			// Resolve argument references from previous steps
			args, err := e.resolveArgs(step.Arguments)
			if err != nil {
				firstErrMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("step %s: failed to resolve arguments: %v", step.ID, err)
				}
				firstErrMu.Unlock()
				completedMu.Lock()
				stepErrors[step.ID] = err
				completed[step.ID] = true
				completedCond.Broadcast()
				completedMu.Unlock()
				return
			}

			// Get the client for this server
			client, ok := e.clients[step.Server]
			if !ok {
				err := fmt.Errorf("step %s: unknown server %q", step.ID, step.Server)
				firstErrMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				firstErrMu.Unlock()
				completedMu.Lock()
				stepErrors[step.ID] = err
				completed[step.ID] = true
				completedCond.Broadcast()
				completedMu.Unlock()
				return
			}

			// Set step timeout
			stepTimeout := time.Duration(e.enforcer.StepTimeout()) * time.Second
			stepCtx, stepCancel := context.WithTimeout(ctx, stepTimeout)
			defer stepCancel()

			stepStart := time.Now()
			result, err := client.CallTool(stepCtx, step.Tool, args)

			if err != nil {
				firstErrMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("step %s: %v", step.ID, err)
				}
				firstErrMu.Unlock()
				completedMu.Lock()
				stepErrors[step.ID] = err
				completed[step.ID] = true
				completedCond.Broadcast()
				completedMu.Unlock()
				return
			}

			// Store the raw result with plan-scoped ref.
			// Extract the text content from the MCP response so that
			// ${stepId.field} references operate on the tool's payload,
			// not the MCP wrapper structure.
			rawBytes := extractContent(result)
			stepResult := &resultstore.StepResult{
				PlanID:    planID,
				StepID:    step.ID,
				Server:    step.Server,
				Tool:      step.Tool,
				Raw:       rawBytes,
				IsError:   result.IsError,
				Timestamp: time.Now(),
				Duration:  time.Since(stepStart),
			}
			e.store.Put(step.ID, stepResult)

			completedMu.Lock()
			refs[step.ID] = stepResult.Ref
			completed[step.ID] = true
			completedCond.Broadcast()
			completedMu.Unlock()

			log.Info("step %s stored as %s (%d bytes, %v)", step.ID, stepResult.Ref, len(rawBytes), time.Since(stepStart))
		}(i)
	}

	wg.Wait()

	if firstErr != nil {
		return &Result{
			Success:   false,
			PlanID:    planID,
			StepCount: len(completed),
			Error:     firstErr.Error(),
			Duration:  time.Since(start),
			Refs:      refs,
		}, nil
	}

	// Build the output based on the return spec
	output, err := e.buildOutput(plan, planID)
	if err != nil {
		return &Result{
			Success:   true,
			PlanID:    planID,
			StepCount: len(plan.Steps),
			Error:     fmt.Sprintf("output error: %v", err),
			Duration:  time.Since(start),
			Refs:      refs,
			Summary:   e.store.Summary(),
		}, nil
	}

	return &Result{
		Success:   true,
		PlanID:    planID,
		StepCount: len(plan.Steps),
		Output:    output,
		Duration:  time.Since(start),
		Refs:      refs,
		Summary:   e.store.Summary(),
	}, nil
}

// CallRaw executes a single raw tool call and stores the result.
// Returns the shielded output and a ref handle for pagination.
func (e *Executor) CallRaw(ctx context.Context, server, tool string, args map[string]interface{}) (*RawResult, error) {
	// Check if tool is blocked
	fullName := server + "/" + tool
	if e.enforcer.IsToolBlocked(fullName) || e.enforcer.IsToolBlocked(tool) {
		return nil, fmt.Errorf("tool %q is blocked by policy", fullName)
	}

	// Enforce mutating policy
	if !e.enforcer.IsMutatingAllowed() {
		risk := e.lookupToolRisk(server, tool)
		if risk == "write" || risk == "dangerous" {
			return nil, fmt.Errorf("tool %q is %s and mutating is not allowed by policy", fullName, risk)
		}
	}

	client, ok := e.clients[server]
	if !ok {
		return nil, fmt.Errorf("unknown server %q", server)
	}

	result, err := client.CallTool(ctx, tool, args)
	if err != nil {
		return nil, err
	}

	// Store the raw result so it's pageable
	rawBytes := extractContent(result)
	ref := e.store.PutRaw(server, tool, rawBytes)

	// Shield for immediate output
	shielded := e.enforcer.ShieldStructured(string(rawBytes))

	return &RawResult{
		Ref:      ref,
		Output:   shielded.Data,
		Shielded: shielded.WasTruncated,
		Meta:     shielded.Meta,
	}, nil
}

// resolveArgs replaces argument references like "${s1.field}" with actual values
// from the result store.
func (e *Executor) resolveArgs(args map[string]interface{}) (map[string]interface{}, error) {
	resolved := make(map[string]interface{})
	for k, v := range args {
		switch val := v.(type) {
		case string:
			if strings.HasPrefix(val, "${") && strings.HasSuffix(val, "}") {
				ref := val[2 : len(val)-1]
				extracted, err := e.store.ExtractField(ref)
				if err != nil {
					return nil, fmt.Errorf("arg %q: %w", k, err)
				}
				resolved[k] = extracted
			} else {
				resolved[k] = val
			}
		default:
			resolved[k] = val
		}
	}
	return resolved, nil
}

// buildOutput constructs the final output based on the return spec.
func (e *Executor) buildOutput(plan Plan, planID string) (interface{}, error) {
	if plan.Return == nil {
		// Default: return summary with refs
		return e.store.Summary(), nil
	}

	// Try plan-scoped ref first, then bare stepID
	ref := planID + ":" + plan.Return.FromStep
	stepResult, ok := e.store.Get(ref)
	if !ok {
		stepResult, ok = e.store.Get(plan.Return.FromStep)
		if !ok {
			return nil, fmt.Errorf("return step %q not found", plan.Return.FromStep)
		}
	}

	switch plan.Return.Mode {
	case "full":
		shielded := e.enforcer.ShieldStructured(string(stepResult.Raw))
		output := map[string]interface{}{
			"data":    shielded.Data,
			"ref":     stepResult.Ref,
			"shielded": shielded.WasTruncated,
		}
		if shielded.Meta != nil {
			output["meta"] = shielded.Meta
		}
		return output, nil

	case "fields":
		if len(plan.Return.Fields) > 0 {
			result, err := e.enforcer.ShieldFields(string(stepResult.Raw), plan.Return.Fields)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"data": result,
				"ref":  stepResult.Ref,
			}, nil
		}
		return e.enforcer.Shield(string(stepResult.Raw)), nil

	case "summary":
		shielded := e.enforcer.ShieldStructured(string(stepResult.Raw))
		return map[string]interface{}{
			"data":    shielded.Data,
			"ref":     stepResult.Ref,
			"shielded": shielded.WasTruncated,
		}, nil

	default:
		return e.enforcer.Shield(string(stepResult.Raw)), nil
	}
}

// lookupToolRisk returns the risk level ("read", "write", "dangerous") for a tool.
// Falls back to name-based heuristic if no capability index is available.
func (e *Executor) lookupToolRisk(server, tool string) string {
	if e.index != nil {
		caps := e.index.ForServer(server)
		for _, cap := range caps {
			if cap.ToolName == tool {
				return cap.RiskLevel
			}
		}
	}

	// Heuristic fallback: use tool name keywords
	lower := strings.ToLower(tool)
	for _, w := range []string{"delete", "remove", "destroy", "drop", "purge"} {
		if strings.Contains(lower, w) {
			return "dangerous"
		}
	}
	for _, w := range []string{"create", "update", "set", "add", "edit", "modify", "write", "push", "merge", "post", "put", "patch"} {
		if strings.Contains(lower, w) {
			return "write"
		}
	}
	return "read"
}

// extractContent pulls the text payload from an MCP CallToolResult.
// If the first content piece is text and parses as JSON, store that JSON directly.
// Otherwise, fall back to marshaling the entire MCP result.
func extractContent(result *mcp.CallToolResult) json.RawMessage {
	if result == nil {
		return []byte("{}")
	}

	// Try to extract text content from the first piece
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text := tc.Text
			// If it's valid JSON, store it as-is (most MCP tools return JSON text)
			if json.Valid([]byte(text)) {
				return json.RawMessage(text)
			}
			// If it's plain text, wrap it as a JSON string
			quoted, _ := json.Marshal(text)
			return json.RawMessage(quoted)
		}
	}

	// Fallback: marshal the whole result
	b, _ := json.Marshal(result)
	return json.RawMessage(b)
}
