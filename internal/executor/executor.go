// Package executor implements the plan execution engine that orchestrates
// multi-step tool calls against upstream MCP servers. It keeps intermediate
// results shielded from the harness and only returns distilled outputs.
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/robinwhite/gobbler/internal/logging"
	"github.com/robinwhite/gobbler/internal/mcpclient"
	"github.com/robinwhite/gobbler/internal/policy"
	"github.com/robinwhite/gobbler/internal/resultstore"
)

var log = logging.New("executor")

// Plan represents a structured execution plan submitted by the harness.
type Plan struct {
	Steps  []Step       `json:"steps"`
	Return *ReturnSpec  `json:"return,omitempty"`
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
	StepCount int                    `json:"stepCount"`
	Output    interface{}            `json:"output"`
	Error     string                 `json:"error,omitempty"`
	Duration  time.Duration          `json:"duration"`
	Summary   map[string]interface{} `json:"summary,omitempty"`
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

// Execute runs a plan and returns the shielded result.
func (e *Executor) Execute(ctx context.Context, plan Plan) (*Result, error) {
	start := time.Now()
	e.store.Clear()

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

	// Execute steps in order
	for i, step := range plan.Steps {
		log.Info("executing step %d/%d: %s/%s", i+1, len(plan.Steps), step.Server, step.Tool)

		// Check if tool is blocked
		fullName := step.Server + "/" + step.Tool
		if e.enforcer.IsToolBlocked(fullName) || e.enforcer.IsToolBlocked(step.Tool) {
			return &Result{
				Success:  false,
				Error:    fmt.Sprintf("tool %q is blocked by policy", fullName),
				Duration: time.Since(start),
			}, nil
		}

		// Resolve argument references from previous steps
		args, err := e.resolveArgs(step.Arguments)
		if err != nil {
			return &Result{
				Success:  false,
				Error:    fmt.Sprintf("step %s: failed to resolve arguments: %v", step.ID, err),
				Duration: time.Since(start),
			}, nil
		}

		// Get the client for this server
		client, ok := e.clients[step.Server]
		if !ok {
			return &Result{
				Success:  false,
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
				StepCount: i + 1,
				Error:     fmt.Sprintf("step %s: %v", step.ID, err),
				Duration:  time.Since(start),
			}, nil
		}

		// Store the raw result internally (shielded from harness)
		rawBytes, _ := json.Marshal(result)
		e.store.Put(step.ID, &resultstore.StepResult{
			StepID:    step.ID,
			Server:    step.Server,
			Tool:      step.Tool,
			Raw:       rawBytes,
			IsError:   result.IsError,
			Timestamp: time.Now(),
			Duration:  time.Since(stepStart),
		})

		log.Info("step %s completed in %v (%d bytes)", step.ID, time.Since(stepStart), len(rawBytes))
	}

	// Build the output based on the return spec
	output, err := e.buildOutput(plan)
	if err != nil {
		return &Result{
			Success:   true,
			StepCount: len(plan.Steps),
			Error:     fmt.Sprintf("output error: %v", err),
			Duration:  time.Since(start),
			Summary:   e.store.Summary(),
		}, nil
	}

	return &Result{
		Success:   true,
		StepCount: len(plan.Steps),
		Output:    output,
		Duration:  time.Since(start),
		Summary:   e.store.Summary(),
	}, nil
}

// CallRaw executes a single raw tool call (debugging/escape hatch).
func (e *Executor) CallRaw(ctx context.Context, server, tool string, args map[string]interface{}) (interface{}, error) {
	client, ok := e.clients[server]
	if !ok {
		return nil, fmt.Errorf("unknown server %q", server)
	}

	result, err := client.CallTool(ctx, tool, args)
	if err != nil {
		return nil, err
	}

	// Apply shielding to raw result
	rawBytes, _ := json.Marshal(result)
	shielded := e.enforcer.Shield(string(rawBytes))
	return shielded, nil
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
func (e *Executor) buildOutput(plan Plan) (interface{}, error) {
	if plan.Return == nil {
		// Default: return summary of all steps
		return e.store.Summary(), nil
	}

	// Get the target step result
	stepResult, ok := e.store.Get(plan.Return.FromStep)
	if !ok {
		return nil, fmt.Errorf("return step %q not found", plan.Return.FromStep)
	}

	switch plan.Return.Mode {
	case "full":
		if e.enforcer != nil {
			return e.enforcer.Shield(string(stepResult.Raw)), nil
		}
		return string(stepResult.Raw), nil

	case "fields":
		if len(plan.Return.Fields) > 0 {
			return e.enforcer.ShieldFields(string(stepResult.Raw), plan.Return.Fields)
		}
		return e.enforcer.Shield(string(stepResult.Raw)), nil

	case "summary":
		return e.enforcer.Shield(string(stepResult.Raw)), nil

	default:
		return e.enforcer.Shield(string(stepResult.Raw)), nil
	}
}
