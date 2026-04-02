package resultstore

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestPutAndGet(t *testing.T) {
	s := New()

	raw, _ := json.Marshal(map[string]interface{}{"title": "Test PR", "number": 42})
	s.Put("s1", &StepResult{
		StepID:    "s1",
		Server:    "github",
		Tool:      "list_prs",
		Raw:       raw,
		Timestamp: time.Now(),
	})

	r, ok := s.Get("s1")
	if !ok {
		t.Fatal("expected to find s1")
	}
	if r.Server != "github" {
		t.Errorf("expected server=github, got %s", r.Server)
	}
	if r.ByteSize != len(raw) {
		t.Errorf("expected byteSize=%d, got %d", len(raw), r.ByteSize)
	}
}

func TestExtractField(t *testing.T) {
	s := New()

	raw, _ := json.Marshal(map[string]interface{}{"title": "Test PR", "number": 42})
	s.Put("s1", &StepResult{
		StepID: "s1",
		Raw:    raw,
	})

	val, err := s.ExtractField("s1.title")
	if err != nil {
		t.Fatal(err)
	}
	if val != "Test PR" {
		t.Errorf("expected 'Test PR', got %v", val)
	}

	val, err = s.ExtractField("s1.number")
	if err != nil {
		t.Fatal(err)
	}
	if val != float64(42) { // JSON numbers are float64
		t.Errorf("expected 42, got %v", val)
	}
}

func TestExtractField_Array(t *testing.T) {
	s := New()

	raw, _ := json.Marshal([]map[string]interface{}{
		{"title": "PR 1", "number": 1},
		{"title": "PR 2", "number": 2},
	})
	s.Put("s1", &StepResult{
		StepID: "s1",
		Raw:    raw,
	})

	val, err := s.ExtractField("s1[0].title")
	if err != nil {
		t.Fatal(err)
	}
	if val != "PR 1" {
		t.Errorf("expected 'PR 1', got %v", val)
	}

	val, err = s.ExtractField("s1[1].number")
	if err != nil {
		t.Fatal(err)
	}
	if val != float64(2) {
		t.Errorf("expected 2, got %v", val)
	}
}

func TestExtractField_Slice(t *testing.T) {
	s := New()

	items := make([]map[string]interface{}, 100)
	for i := range items {
		items[i] = map[string]interface{}{"id": i, "title": "Item"}
	}
	raw, _ := json.Marshal(items)
	s.Put("s1", &StepResult{StepID: "s1", Raw: raw})

	// Slice [10:20]
	val, err := s.ExtractField("s1[10:20]")
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := val.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T", val)
	}
	if len(arr) != 10 {
		t.Errorf("expected 10 elements, got %d", len(arr))
	}
	first := arr[0].(map[string]interface{})
	if first["id"] != float64(10) {
		t.Errorf("expected first element id=10, got %v", first["id"])
	}
}

func TestExtractField_OpenEndedSlice(t *testing.T) {
	s := New()

	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}
	raw, _ := json.Marshal(items)
	s.Put("s1", &StepResult{StepID: "s1", Raw: raw})

	// [45:] -- open-ended, should return elements 45-49
	val, err := s.ExtractField("s1[45:]")
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := val.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T", val)
	}
	if len(arr) != 5 {
		t.Errorf("expected 5 elements, got %d", len(arr))
	}
}

func TestSlice_WithFields(t *testing.T) {
	s := New()

	items := make([]map[string]interface{}, 200)
	for i := range items {
		items[i] = map[string]interface{}{
			"id":     i,
			"title":  "PR Title",
			"body":   "Very long body text that we don't need",
			"author": "user",
		}
	}
	raw, _ := json.Marshal(items)
	s.Put("s1", &StepResult{StepID: "s1", Raw: raw})

	data, meta, err := s.Slice("s1", 50, 25, []string{"id", "title"})
	if err != nil {
		t.Fatal(err)
	}

	if meta.Total != 200 {
		t.Errorf("expected total=200, got %d", meta.Total)
	}
	if meta.Offset != 50 {
		t.Errorf("expected offset=50, got %d", meta.Offset)
	}
	if meta.Count != 25 {
		t.Errorf("expected count=25, got %d", meta.Count)
	}
	if !meta.HasMore {
		t.Error("expected hasMore=true")
	}

	arr, ok := data.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T", data)
	}

	first := arr[0].(map[string]interface{})
	if first["id"] != float64(50) {
		t.Errorf("expected first id=50, got %v", first["id"])
	}
	// Should only have id and title, not body or author
	if _, hasBody := first["body"]; hasBody {
		t.Error("expected body to be filtered out by field projection")
	}
	if _, hasAuthor := first["author"]; hasAuthor {
		t.Error("expected author to be filtered out by field projection")
	}
}

func TestSlice_StringPagination(t *testing.T) {
	s := New()

	longString := ""
	for i := 0; i < 1000; i++ {
		longString += "abcdefghij" // 10 chars each, 10000 total
	}
	raw, _ := json.Marshal(longString)
	s.Put("s1", &StepResult{StepID: "s1", Raw: raw})

	data, meta, err := s.Slice("s1", 5000, 2000, nil)
	if err != nil {
		t.Fatal(err)
	}

	if meta.Total != 10000 {
		t.Errorf("expected total=10000, got %d", meta.Total)
	}
	if meta.Count != 2000 {
		t.Errorf("expected count=2000, got %d", meta.Count)
	}
	if !meta.HasMore {
		t.Error("expected hasMore=true")
	}

	str, ok := data.(string)
	if !ok {
		t.Fatalf("expected string, got %T", data)
	}
	if len(str) != 2000 {
		t.Errorf("expected 2000 chars, got %d", len(str))
	}
}

func TestPutRaw(t *testing.T) {
	s := New()

	raw, _ := json.Marshal(map[string]interface{}{"data": "test"})
	ref := s.PutRaw("github", "get_issue", raw)

	if ref == "" {
		t.Fatal("expected non-empty ref")
	}

	r, ok := s.Get(ref)
	if !ok {
		t.Fatalf("expected to find result by ref %s", ref)
	}
	if r.Server != "github" {
		t.Errorf("expected server=github, got %s", r.Server)
	}
}

func TestPlanScopedRef(t *testing.T) {
	s := New()

	raw, _ := json.Marshal("result1")
	s.Put("s1", &StepResult{PlanID: "p1", StepID: "s1", Raw: raw})
	raw2, _ := json.Marshal("result2")
	s.Put("s1", &StepResult{PlanID: "p2", StepID: "s1", Raw: raw2})

	// Should find p1:s1
	r, ok := s.Get("p1:s1")
	if !ok {
		t.Fatal("expected to find p1:s1")
	}
	if r.PlanID != "p1" {
		t.Errorf("expected planID=p1, got %s", r.PlanID)
	}

	// Should find p2:s1
	r, ok = s.Get("p2:s1")
	if !ok {
		t.Fatal("expected to find p2:s1")
	}
	if r.PlanID != "p2" {
		t.Errorf("expected planID=p2, got %s", r.PlanID)
	}
}

func TestTTLEviction(t *testing.T) {
	s := NewWithConfig(1*time.Millisecond, DefaultMaxStorageBytes)

	raw, _ := json.Marshal("test")
	s.Put("s1", &StepResult{StepID: "s1", Raw: raw})

	// Result should exist immediately
	_, ok := s.Get("s1")
	if !ok {
		t.Fatal("expected to find s1 immediately")
	}

	// Wait for TTL to expire
	time.Sleep(5 * time.Millisecond)

	_, ok = s.Get("s1")
	if ok {
		t.Error("expected s1 to be expired")
	}
}

func TestShapeAnalysis(t *testing.T) {
	s := New()

	// Array result
	arr, _ := json.Marshal([]int{1, 2, 3, 4, 5})
	s.Put("arr", &StepResult{StepID: "arr", Raw: arr})
	r, _ := s.Get("arr")
	if !r.IsArray || r.ArrayLen != 5 {
		t.Errorf("expected isArray=true, arrayLen=5, got isArray=%v, arrayLen=%d", r.IsArray, r.ArrayLen)
	}

	// String result
	str, _ := json.Marshal("hello world")
	s.Put("str", &StepResult{StepID: "str", Raw: str})
	r, _ = s.Get("str")
	if !r.IsString || r.StringLen != 11 {
		t.Errorf("expected isString=true, stringLen=11, got isString=%v, stringLen=%d", r.IsString, r.StringLen)
	}
}

func TestSummary(t *testing.T) {
	s := New()
	raw, _ := json.Marshal("test")
	s.Put("s1", &StepResult{StepID: "s1", Server: "a", Tool: "b", Raw: raw})
	s.Put("s2", &StepResult{StepID: "s2", Server: "c", Tool: "d", Raw: raw})

	summary := s.Summary()
	if len(summary) != 2 {
		t.Errorf("expected 2 entries, got %d", len(summary))
	}
}

func TestClear(t *testing.T) {
	s := New()
	raw, _ := json.Marshal("test")
	s.Put("s1", &StepResult{StepID: "s1", Raw: raw})

	s.Clear()
	if _, ok := s.Get("s1"); ok {
		t.Error("expected store to be empty after clear")
	}
}

func TestResultsPersistAcrossPlans(t *testing.T) {
	s := New()

	// Simulate plan 1
	raw1, _ := json.Marshal([]int{1, 2, 3})
	s.Put("s1", &StepResult{PlanID: "plan1", StepID: "s1", Server: "a", Tool: "b", Raw: raw1})

	// Simulate plan 2 (no Clear() -- this is the new behavior)
	raw2, _ := json.Marshal([]int{4, 5, 6})
	s.Put("s1", &StepResult{PlanID: "plan2", StepID: "s1", Server: "a", Tool: "b", Raw: raw2})

	// Both should be accessible
	r1, ok := s.Get("plan1:s1")
	if !ok {
		t.Fatal("expected plan1:s1 to persist")
	}
	if r1.PlanID != "plan1" {
		t.Errorf("expected planID=plan1, got %s", r1.PlanID)
	}

	r2, ok := s.Get("plan2:s1")
	if !ok {
		t.Fatal("expected plan2:s1 to exist")
	}
	if r2.PlanID != "plan2" {
		t.Errorf("expected planID=plan2, got %s", r2.PlanID)
	}
}

func TestDiskPersistence(t *testing.T) {
	dir := t.TempDir()

	// Create a disk-backed store and put some data
	s1 := NewDiskBacked(dir)

	raw1, _ := json.Marshal(map[string]interface{}{"title": "Issue 1", "number": 1})
	s1.Put("s1", &StepResult{PlanID: "p1", StepID: "s1", Server: "github", Tool: "get_issue", Raw: raw1})

	raw2, _ := json.Marshal([]int{1, 2, 3, 4, 5})
	ref := s1.PutRaw("test", "list", raw2)

	// Verify data is on disk
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 files on disk, got %d", len(entries))
	}

	// Create a new store from the same path -- should reload
	s2 := NewDiskBacked(dir)

	r, ok := s2.Get("p1:s1")
	if !ok {
		t.Fatal("expected p1:s1 to be loaded from disk")
	}
	if r.Server != "github" {
		t.Errorf("expected server=github, got %s", r.Server)
	}
	if r.Tool != "get_issue" {
		t.Errorf("expected tool=get_issue, got %s", r.Tool)
	}

	// Raw result should also be loaded
	r2, ok := s2.Get(ref)
	if !ok {
		t.Fatalf("expected %s to be loaded from disk", ref)
	}
	if !r2.IsArray || r2.ArrayLen != 5 {
		t.Errorf("expected reloaded result to be array of 5, got isArray=%v arrayLen=%d", r2.IsArray, r2.ArrayLen)
	}
}

func TestDiskPersistenceExpiry(t *testing.T) {
	dir := t.TempDir()

	// Create a store with very short TTL
	s1 := &Store{
		results:  make(map[string]*StepResult),
		order:    make([]string, 0),
		ttl:      1 * time.Millisecond,
		maxBytes: DefaultMaxStorageBytes,
		diskPath: dir,
	}

	raw, _ := json.Marshal("test")
	s1.Put("s1", &StepResult{StepID: "s1", Raw: raw})

	// Wait for TTL
	time.Sleep(5 * time.Millisecond)

	// Reload: expired results should be skipped and cleaned up
	s2 := NewDiskBacked(dir)
	if _, ok := s2.Get("s1"); ok {
		t.Error("expected expired result to not be loaded from disk")
	}

	// File should have been cleaned up
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected expired files to be cleaned up, got %d files", len(entries))
	}
}
