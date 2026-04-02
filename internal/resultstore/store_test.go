package resultstore

import (
	"encoding/json"
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
