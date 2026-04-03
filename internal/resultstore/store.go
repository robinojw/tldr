// Package resultstore provides an in-memory store for intermediate tool
// results during plan execution. This keeps raw API responses inside
// tldr rather than forwarding them to the coding harness.
//
// Results persist across plans with TTL-based eviction. Each result is
// addressable by a ref handle (planID:stepID) so the model can page
// through truncated data after a plan completes.
//
// When a disk path is configured, results are also written to disk and
// reloaded on startup, surviving process restarts. This matters for
// stdio-based MCP transports where the harness spawns tldr per session.
package resultstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Default TTL for stored results.
const DefaultTTL = 10 * time.Minute

// DefaultMaxStorageBytes caps total memory used by the store.
const DefaultMaxStorageBytes = 128 * 1024 * 1024 // 128MB

// Store holds results from plan execution and raw calls.
type Store struct {
	mu         sync.RWMutex
	results    map[string]*StepResult // key: "planID:stepID" or "raw:N"
	order      []string               // insertion order for LRU eviction
	ttl        time.Duration
	maxBytes   int
	totalBytes int
	rawCounter atomic.Int64
	diskPath   string // if set, results are persisted to/loaded from this directory
}

// StepResult is the stored result of a single execution step.
type StepResult struct {
	Ref       string          `json:"ref"`       // addressable handle: "p1:s1" or "raw:3"
	PlanID    string          `json:"planId"`
	StepID    string          `json:"stepId"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Raw       json.RawMessage `json:"raw"`
	IsError   bool            `json:"isError"`
	Timestamp time.Time       `json:"timestamp"`
	Duration  time.Duration   `json:"duration"`
	ByteSize  int             `json:"byteSize"`
	ExpiresAt time.Time       `json:"expiresAt"`

	// Precomputed metadata about the raw result
	ArrayLen  int  `json:"arrayLen,omitempty"`  // -1 if not an array
	IsArray   bool `json:"isArray,omitempty"`
	StringLen int  `json:"stringLen,omitempty"` // -1 if not a string
	IsString  bool `json:"isString,omitempty"`
}

// New creates a result store with default settings.
func New() *Store {
	return &Store{
		results:  make(map[string]*StepResult),
		order:    make([]string, 0),
		ttl:      DefaultTTL,
		maxBytes: DefaultMaxStorageBytes,
	}
}

// NewWithConfig creates a store with custom TTL and max bytes.
func NewWithConfig(ttl time.Duration, maxBytes int) *Store {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxStorageBytes
	}
	return &Store{
		results:  make(map[string]*StepResult),
		order:    make([]string, 0),
		ttl:      ttl,
		maxBytes: maxBytes,
	}
}

// NewDiskBacked creates a store that persists results to disk.
// On creation, it loads any non-expired results from the disk path.
// Each Put/PutRaw also writes the result to disk.
func NewDiskBacked(diskPath string) *Store {
	s := &Store{
		results:  make(map[string]*StepResult),
		order:    make([]string, 0),
		ttl:      DefaultTTL,
		maxBytes: DefaultMaxStorageBytes,
		diskPath: diskPath,
	}
	s.loadFromDisk()
	return s
}

// Put stores a step result with a plan-scoped ref handle.
func (s *Store) Put(stepID string, result *StepResult) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result.ByteSize = len(result.Raw)
	result.ExpiresAt = time.Now().Add(s.ttl)
	result.analyzeShape()

	// Build the ref handle
	if result.PlanID != "" {
		result.Ref = result.PlanID + ":" + stepID
	} else {
		result.Ref = stepID
	}

	s.evictExpired()
	s.evictIfNeeded(result.ByteSize)

	s.results[result.Ref] = result
	s.order = append(s.order, result.Ref)
	s.totalBytes += result.ByteSize

	// Persist to disk if configured
	s.writeToDisk(result)
}

// PutRaw stores a result from a call_raw invocation and returns the ref handle.
func (s *Store) PutRaw(server, tool string, raw json.RawMessage) string {
	id := s.rawCounter.Add(1)
	ref := fmt.Sprintf("raw:%d", id)

	result := &StepResult{
		Ref:       ref,
		StepID:    ref,
		Server:    server,
		Tool:      tool,
		Raw:       raw,
		Timestamp: time.Now(),
	}

	s.Put(ref, result)
	return ref
}

// Get retrieves a step result by ref handle.
func (s *Store) Get(ref string) (*StepResult, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Try exact match first
	if r, ok := s.results[ref]; ok {
		if time.Now().Before(r.ExpiresAt) {
			return r, true
		}
	}

	// Try legacy stepID-only lookup (backward compat within a plan)
	for _, r := range s.results {
		if r.StepID == ref && time.Now().Before(r.ExpiresAt) {
			return r, true
		}
	}

	return nil, false
}

// ExtractField extracts a value from a stored JSON result using path expressions.
//
// Supported syntax:
//
//	"ref.field"           - extract a field from an object
//	"ref[0].field"        - extract a field from an array element
//	"ref[10:20]"          - slice an array (returns elements 10-19)
//	"ref[10:20].field"    - slice then project a single field
//	"ref[*].field"        - project a field from every array element
//	"ref"                 - return the whole parsed result
func (s *Store) ExtractField(expr string) (interface{}, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ref, path, err := parseRef(expr)
	if err != nil {
		return nil, err
	}

	// Find the result
	result, ok := s.findResult(ref)
	if !ok {
		return nil, fmt.Errorf("result %q not found or expired", ref)
	}

	var parsed interface{}
	if err := json.Unmarshal(result.Raw, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse result for %q: %w", ref, err)
	}

	return navigatePath(parsed, path)
}

// Slice returns a sub-array from a stored result.
// offset and limit operate on the top-level array. If fields is non-empty,
// only those fields are projected from each element.
func (s *Store) Slice(ref string, offset, limit int, fields []string) (interface{}, *SliceMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result, ok := s.findResult(ref)
	if !ok {
		return nil, nil, fmt.Errorf("result %q not found or expired", ref)
	}

	var parsed interface{}
	if err := json.Unmarshal(result.Raw, &parsed); err != nil {
		return nil, nil, fmt.Errorf("failed to parse result: %w", err)
	}

	arr, ok := parsed.([]interface{})
	if !ok {
		// If it's an object with a content array, try common patterns
		if obj, ok := parsed.(map[string]interface{}); ok {
			for _, key := range []string{"content", "items", "data", "results", "entries"} {
				if v, exists := obj[key]; exists {
					if a, ok := v.([]interface{}); ok {
						arr = a
						break
					}
				}
			}
		}
		if arr == nil {
			// Not an array -- try as a parsed string value
			if str, ok := parsed.(string); ok {
				return sliceString(str, offset, limit)
			}
			// Fall back to raw JSON bytes
			raw := string(result.Raw)
			return sliceString(raw, offset, limit)
		}
	}

	total := len(arr)
	if offset >= total {
		return []interface{}{}, &SliceMeta{
			Ref:     ref,
			Total:   total,
			Offset:  offset,
			Count:   0,
			HasMore: false,
		}, nil
	}

	end := offset + limit
	if end > total {
		end = total
	}

	slice := arr[offset:end]

	// Project fields if requested
	if len(fields) > 0 {
		projected := make([]interface{}, len(slice))
		for i, item := range slice {
			if obj, ok := item.(map[string]interface{}); ok {
				p := make(map[string]interface{})
				for _, f := range fields {
					if v, exists := obj[f]; exists {
						p[f] = v
					}
				}
				projected[i] = p
			} else {
				projected[i] = item
			}
		}
		slice = projected
	}

	meta := &SliceMeta{
		Ref:     ref,
		Total:   total,
		Offset:  offset,
		Count:   len(slice),
		HasMore: end < total,
	}

	return slice, meta, nil
}

// SliceMeta contains pagination metadata returned with sliced results.
type SliceMeta struct {
	Ref     string `json:"ref"`
	Total   int    `json:"total"`
	Offset  int    `json:"offset"`
	Count   int    `json:"count"`
	HasMore bool   `json:"hasMore"`
}

// Summary returns metadata about all non-expired stored results.
func (s *Store) Summary() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	summary := make(map[string]interface{})
	for ref, r := range s.results {
		if now.Before(r.ExpiresAt) {
			entry := map[string]interface{}{
				"ref":      r.Ref,
				"server":   r.Server,
				"tool":     r.Tool,
				"isError":  r.IsError,
				"byteSize": r.ByteSize,
				"duration": r.Duration.String(),
			}
			if r.IsArray {
				entry["arrayLen"] = r.ArrayLen
			}
			if r.IsString {
				entry["stringLen"] = r.StringLen
			}
			summary[ref] = entry
		}
	}
	return summary
}

// ListRefs returns all non-expired result ref handles.
func (s *Store) ListRefs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	refs := make([]string, 0)
	for _, ref := range s.order {
		if r, ok := s.results[ref]; ok && now.Before(r.ExpiresAt) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// Clear removes all stored results.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = make(map[string]*StepResult)
	s.order = make([]string, 0)
	s.totalBytes = 0
}

// TotalBytes returns the total size of all stored results.
func (s *Store) TotalBytes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalBytes
}

// --- Internal helpers ---

// analyzeShape pre-computes metadata about the raw JSON.
func (r *StepResult) analyzeShape() {
	r.ArrayLen = -1
	r.StringLen = -1

	var parsed interface{}
	if err := json.Unmarshal(r.Raw, &parsed); err != nil {
		return
	}

	switch v := parsed.(type) {
	case []interface{}:
		r.IsArray = true
		r.ArrayLen = len(v)
	case string:
		r.IsString = true
		r.StringLen = len(v)
	}
}

func (s *Store) findResult(ref string) (*StepResult, bool) {
	now := time.Now()

	// Exact ref match
	if r, ok := s.results[ref]; ok && now.Before(r.ExpiresAt) {
		return r, true
	}

	// Legacy stepID-only match
	for _, r := range s.results {
		if r.StepID == ref && now.Before(r.ExpiresAt) {
			return r, true
		}
	}

	return nil, false
}

func (s *Store) evictExpired() {
	now := time.Now()
	newOrder := make([]string, 0, len(s.order))
	for _, ref := range s.order {
		if r, ok := s.results[ref]; ok {
			if now.After(r.ExpiresAt) {
				s.totalBytes -= r.ByteSize
				delete(s.results, ref)
				continue
			}
		}
		newOrder = append(newOrder, ref)
	}
	s.order = newOrder
}

func (s *Store) evictIfNeeded(incoming int) {
	for s.totalBytes+incoming > s.maxBytes && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		if r, ok := s.results[oldest]; ok {
			s.totalBytes -= r.ByteSize
			delete(s.results, oldest)
			s.deleteFromDisk(oldest)
		}
	}
}

// --- Disk persistence ---

// refToFilename converts a ref handle to a safe filename.
// "p1:s1" -> "p1_s1.json", "raw:3" -> "raw_3.json"
func refToFilename(ref string) string {
	safe := strings.ReplaceAll(ref, ":", "_")
	safe = strings.ReplaceAll(safe, "/", "_")
	return safe + ".json"
}

// writeToDisk persists a single result to the disk path. Must be called
// while holding the write lock (or after the result is fully built).
func (s *Store) writeToDisk(result *StepResult) {
	if s.diskPath == "" {
		return
	}

	if err := os.MkdirAll(s.diskPath, 0755); err != nil {
		return // best-effort; don't fail the in-memory store
	}

	data, err := json.Marshal(result)
	if err != nil {
		return
	}

	path := filepath.Join(s.diskPath, refToFilename(result.Ref))
	_ = os.WriteFile(path, data, 0644)
}

// deleteFromDisk removes a result file. Best-effort.
func (s *Store) deleteFromDisk(ref string) {
	if s.diskPath == "" {
		return
	}
	path := filepath.Join(s.diskPath, refToFilename(ref))
	_ = os.Remove(path)
}

// loadFromDisk reads all non-expired result files from the disk path.
func (s *Store) loadFromDisk() {
	if s.diskPath == "" {
		return
	}

	entries, err := os.ReadDir(s.diskPath)
	if err != nil {
		return // directory may not exist yet
	}

	now := time.Now()
	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(s.diskPath, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var result StepResult
		if err := json.Unmarshal(data, &result); err != nil {
			_ = os.Remove(path) // corrupted file
			continue
		}

		// Skip expired results
		if now.After(result.ExpiresAt) {
			_ = os.Remove(path)
			continue
		}

		result.ByteSize = len(result.Raw)
		result.analyzeShape()

		s.results[result.Ref] = &result
		s.order = append(s.order, result.Ref)
		s.totalBytes += result.ByteSize
		loaded++

		// Track the highest raw counter for PutRaw continuity
		if strings.HasPrefix(result.Ref, "raw:") {
			if numStr := strings.TrimPrefix(result.Ref, "raw:"); numStr != "" {
				if num, err := strconv.ParseInt(numStr, 10, 64); err == nil {
					if num >= s.rawCounter.Load() {
						s.rawCounter.Store(num)
					}
				}
			}
		}
	}
}

// --- Path parsing ---

// pathOp represents a navigation operation in a path expression.
type pathOp struct {
	kind  string // "field", "index", "slice", "wildcard"
	field string
	index int
	start int // for slices
	end   int // for slices
}

// parseRef splits "ref.path" into the ref handle and path operations.
func parseRef(expr string) (string, []pathOp, error) {
	if expr == "" {
		return "", nil, fmt.Errorf("empty expression")
	}

	// Find where the ref ends and the path begins.
	// Refs can contain ":" (for planID:stepID) and digits (for raw:N).
	// Path starts at first ".", "[", or end of string.
	refEnd := len(expr)
	for i, c := range expr {
		if c == '.' || c == '[' {
			refEnd = i
			break
		}
	}

	ref := expr[:refEnd]
	rest := expr[refEnd:]

	if rest == "" {
		return ref, nil, nil
	}

	ops, err := parsePath(rest)
	if err != nil {
		return "", nil, err
	}

	return ref, ops, nil
}

func parsePath(path string) ([]pathOp, error) {
	var ops []pathOp
	i := 0

	for i < len(path) {
		switch path[i] {
		case '.':
			i++ // skip dot
			end := i
			for end < len(path) && path[end] != '.' && path[end] != '[' {
				end++
			}
			if end == i {
				return nil, fmt.Errorf("empty field name at position %d", i)
			}
			ops = append(ops, pathOp{kind: "field", field: path[i:end]})
			i = end

		case '[':
			i++ // skip [
			if i >= len(path) {
				return nil, fmt.Errorf("unclosed bracket")
			}

			// Wildcard: [*]
			if path[i] == '*' {
				if i+1 >= len(path) || path[i+1] != ']' {
					return nil, fmt.Errorf("expected ] after *")
				}
				ops = append(ops, pathOp{kind: "wildcard"})
				i += 2 // skip *]
				continue
			}

			// Find closing bracket
			end := strings.IndexByte(path[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("unclosed bracket")
			}
			content := path[i : i+end]
			i += end + 1 // skip past ]

			// Slice: [start:end]
			if colonIdx := strings.IndexByte(content, ':'); colonIdx >= 0 {
				startStr := content[:colonIdx]
				endStr := content[colonIdx+1:]
				start := 0
				sliceEnd := -1 // -1 means "to the end"
				if startStr != "" {
					var err error
					start, err = strconv.Atoi(startStr)
					if err != nil {
						return nil, fmt.Errorf("invalid slice start: %s", startStr)
					}
				}
				if endStr != "" {
					var err error
					sliceEnd, err = strconv.Atoi(endStr)
					if err != nil {
						return nil, fmt.Errorf("invalid slice end: %s", endStr)
					}
				}
				ops = append(ops, pathOp{kind: "slice", start: start, end: sliceEnd})
				continue
			}

			// Index: [N]
			idx, err := strconv.Atoi(content)
			if err != nil {
				return nil, fmt.Errorf("invalid index: %s", content)
			}
			ops = append(ops, pathOp{kind: "index", index: idx})

		default:
			return nil, fmt.Errorf("unexpected character %c at position %d", path[i], i)
		}
	}

	return ops, nil
}

// navigatePath applies a sequence of path operations to a parsed JSON value.
func navigatePath(v interface{}, ops []pathOp) (interface{}, error) {
	current := v

	for _, op := range ops {
		switch op.kind {
		case "field":
			obj, ok := current.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("expected object for field %q, got %T", op.field, current)
			}
			val, ok := obj[op.field]
			if !ok {
				return nil, fmt.Errorf("field %q not found", op.field)
			}
			current = val

		case "index":
			arr, ok := current.([]interface{})
			if !ok {
				return nil, fmt.Errorf("expected array for index %d, got %T", op.index, current)
			}
			if op.index < 0 || op.index >= len(arr) {
				return nil, fmt.Errorf("index %d out of range (len %d)", op.index, len(arr))
			}
			current = arr[op.index]

		case "slice":
			arr, ok := current.([]interface{})
			if !ok {
				return nil, fmt.Errorf("expected array for slice, got %T", current)
			}
			start := op.start
			end := op.end
			if end < 0 || end > len(arr) {
				end = len(arr)
			}
			if start < 0 {
				start = 0
			}
			if start >= len(arr) {
				current = []interface{}{}
			} else {
				current = arr[start:end]
			}

		case "wildcard":
			arr, ok := current.([]interface{})
			if !ok {
				return nil, fmt.Errorf("expected array for wildcard, got %T", current)
			}
			// Wildcard must be followed by a field op (handled by collecting remaining ops)
			// For now, return the array -- field projection happens in subsequent ops
			current = arr
		}
	}

	// Handle wildcard + field projection:
	// If we ended with an array and the last op was wildcard, the caller
	// should have appended a field op. But we handle that case explicitly
	// in navigatePath by checking if there's a following field op after wildcard.
	return current, nil
}

// sliceString returns a substring with pagination metadata.
func sliceString(s string, offset, limit int) (interface{}, *SliceMeta, error) {
	total := len(s)
	if offset >= total {
		return "", &SliceMeta{
			Total:   total,
			Offset:  offset,
			Count:   0,
			HasMore: false,
		}, nil
	}

	end := offset + limit
	if end > total {
		end = total
	}

	return s[offset:end], &SliceMeta{
		Total:   total,
		Offset:  offset,
		Count:   end - offset,
		HasMore: end < total,
	}, nil
}
