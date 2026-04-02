// Package resultstore provides an in-memory store for intermediate tool
// results during plan execution. This keeps raw API responses inside
// gobbler rather than forwarding them to the coding harness.
package resultstore

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Store holds intermediate results from plan execution steps.
type Store struct {
	mu      sync.RWMutex
	results map[string]*StepResult
}

// StepResult is the stored result of a single execution step.
type StepResult struct {
	StepID    string          `json:"stepId"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Raw       json.RawMessage `json:"raw"`
	IsError   bool            `json:"isError"`
	Timestamp time.Time       `json:"timestamp"`
	Duration  time.Duration   `json:"duration"`
	ByteSize  int             `json:"byteSize"`
}

// New creates an empty result store.
func New() *Store {
	return &Store{
		results: make(map[string]*StepResult),
	}
}

// Put stores a step result.
func (s *Store) Put(stepID string, result *StepResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result.ByteSize = len(result.Raw)
	s.results[stepID] = result
}

// Get retrieves a step result by ID.
func (s *Store) Get(stepID string) (*StepResult, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.results[stepID]
	return r, ok
}

// ExtractField extracts a field from a stored JSON result using a simple
// path expression like "stepId.fieldName" or "stepId[0].fieldName".
func (s *Store) ExtractField(ref string) (interface{}, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Parse reference like "s1.number" or "s1[0].title"
	// For now, support simple "stepID.field" and "stepID[index].field"
	var stepID, field string
	var index int = -1

	// Simple parse
	for i, c := range ref {
		if c == '.' {
			stepID = ref[:i]
			field = ref[i+1:]
			break
		}
		if c == '[' {
			stepID = ref[:i]
			rest := ref[i:]
			fmt.Sscanf(rest, "[%d].%s", &index, &field)
			break
		}
	}

	if stepID == "" {
		return nil, fmt.Errorf("invalid reference: %s", ref)
	}

	result, ok := s.results[stepID]
	if !ok {
		return nil, fmt.Errorf("step %q not found", stepID)
	}

	var parsed interface{}
	if err := json.Unmarshal(result.Raw, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse result for %q: %w", stepID, err)
	}

	// Navigate to the value
	if index >= 0 {
		arr, ok := parsed.([]interface{})
		if !ok {
			return nil, fmt.Errorf("result for %q is not an array", stepID)
		}
		if index >= len(arr) {
			return nil, fmt.Errorf("index %d out of range for %q (len %d)", index, stepID, len(arr))
		}
		parsed = arr[index]
	}

	if field == "" {
		return parsed, nil
	}

	obj, ok := parsed.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("result for %q is not an object", stepID)
	}

	val, ok := obj[field]
	if !ok {
		return nil, fmt.Errorf("field %q not found in result for %q", field, stepID)
	}

	return val, nil
}

// Summary returns a brief summary of all stored results.
func (s *Store) Summary() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	summary := make(map[string]interface{})
	for id, r := range s.results {
		summary[id] = map[string]interface{}{
			"server":   r.Server,
			"tool":     r.Tool,
			"isError":  r.IsError,
			"byteSize": r.ByteSize,
			"duration": r.Duration.String(),
		}
	}
	return summary
}

// Clear removes all stored results.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = make(map[string]*StepResult)
}

// TotalBytes returns the total size of all stored results.
func (s *Store) TotalBytes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, r := range s.results {
		total += r.ByteSize
	}
	return total
}
