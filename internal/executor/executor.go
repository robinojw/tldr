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
	"sync/atomic"
	"time"

	"github.com/robinwhite/gobbler/internal/logging"
	"github.com/robinwhite/gobbler/internal/mcpclient"
	"github.com/robinwhite/gobbler/internal/policy"
	"github.com/robinwhite/gobbler/internal/resultstore"
)

var log = logging.New("executor")

var planCounter atomic.Int64

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
	DependsOn string                 `json:"dependsOn,omitempty"` // step ID to wait for
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
	clients  map[string]*mcpclient.Client
	enforcer *policy.Enforcer
	store    *resultstore.Store
}

// NewExecutor creates an executor with the given set of connected MCP clients.
func NewExecutor(clients map[string]*mcpclient.Client, enforcer *policy.Enforcer) *Executor {
	return &Executor{
		clients:  clients,
		enforcer: enforcer,
		store:    resultstore.New(),
	}
}

// NewExecutorWithStore creates an executor with a shared store instance.
func NewExecutorWithStore(clients map[string]*mcpclient.Client, enforcer *policy.Enforcer, store *resultstore.Store) *Executor {
	return &Executor{
		clients:  clients,
		enforcer: enforcer,
		store:    store,
	}
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

	// Execute steps in order
	for i, step := range plan.Steps {
		log.Info("executing step %d/%d: %s/%s (plan %s)", i+1, len(plan.Steps), step.Server, step.Tool, planID)

		// Check if tool is blocked
		fullName := step.Server + "/" + step.Tool
		if e.enforcer.IsToolBlocked(fullName) || e.enforcer.IsToolBlocked(step.Tool) {
			return &Result{
				Success:  false,
				PlanID:   planID,
				Error:    fmt.Sprintf("tool %q is blocked by policy", fullName),
				Duration: time.Since(start),
			}, nil
		}

		// Resolve argument references from previous steps
		args, err := e.resolveArgs(step.Arguments)
		if err != nil {
			return &Result{
				Success:  false,
				PlanID:   planID,
				Error:    fmt.Sprintf("step %s: failed to resolve arguments: %v", step.ID, err),
				Duration: time.Since(start),
			}, nil
		}

		// Get the client for this server
		client, ok := e.clients[step.Server]
		if !ok {
			return &Result{
				Success:  false,
				PlanID:   planID,
				Error:    fmt.Sprintf("step %s: unknown server %q", step.ID, step.Server),
				Duration: time.Since(start),
			}, nil
		}

		// Set step timeout
		stepTimeout := time.Duration(e.enforcer.StepTimeout()) * time.Second
		stepCtx, stepCancel := context.WithTimeout(ctx, stepTimeout)

		stepStart := time.Now()
		result, err := client.CallTool(stepCtx, step.Tool, args)
		stepCancel()

		if err != nil {
			return &Result{
				Success:   false,
				PlanID:    planID,
				StepCount: i + 1,
				Error:     fmt.Sprintf("step %s: %v", step.ID, err),
				Duration:  time.Since(start),
			}, nil
		}

		// Store the raw result with plan-scoped ref
		rawBytes, _ := json.Marshal(result)
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
		refs[step.ID] = stepResult.Ref

		log.Info("step %s stored as %s (%d bytes, %v)", step.ID, stepResult.Ref, len(rawBytes), time.Since(stepStart))
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
	client, ok := e.clients[server]
	if !ok {
		return nil, fmt.Errorf("unknown server %q", server)
	}

	result, err := client.CallTool(ctx, tool, args)
	if err != nil {
		return nil, err
	}

	// Store the raw result so it's pageable
	rawBytes, _ := json.Marshal(result)
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
