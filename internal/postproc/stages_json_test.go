package postproc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStageLogEntry_MarshalJSON_NilError(t *testing.T) {
	t.Parallel()
	entry := StageLogEntry{
		Stage:   "repair",
		Elapsed: 2 * time.Second,
		Err:     nil,
		Lines:   []string{"all good"},
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(s, `"Err":{}`) {
		t.Error("nil error serialized as empty object; expected null")
	}
	if !strings.Contains(s, `"Err":null`) {
		t.Errorf("expected null Err, got: %s", s)
	}
}

func TestStageLogEntry_MarshalJSON_WithError(t *testing.T) {
	t.Parallel()
	entry := StageLogEntry{
		Stage:   "unpack",
		Elapsed: 500 * time.Millisecond,
		Err:     errors.New("unrar: not found"),
		Lines:   nil,
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)
	// Error must be serialized as a string, not {}.
	if strings.Contains(s, `"Err":{}`) {
		t.Errorf("error serialized as empty object: %s", s)
	}
	if !strings.Contains(s, `"unrar: not found"`) {
		t.Errorf("expected error string in JSON, got: %s", s)
	}
}

func TestStageLogEntry_MarshalJSON_Roundtrip(t *testing.T) {
	t.Parallel()
	entries := []StageLogEntry{
		{Stage: "repair", Elapsed: 3 * time.Second, Err: errors.New("par2 failed"), Lines: []string{"checking", "failed"}},
		{Stage: "unpack", Elapsed: 0, Err: nil, Lines: []string{"Skipped: repair failed"}},
		{Stage: "finalize", Elapsed: 100 * time.Millisecond, Err: nil, Lines: nil},
	}
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify the deserialization path used by parseStageLog in the API
	// works with this JSON.
	type parsed struct {
		Stage   string   `json:"Stage"`
		Elapsed float64  `json:"Elapsed"` // nanoseconds
		Err     *string  `json:"Err"`
		Lines   []string `json:"Lines"`
	}
	var got []parsed
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal failed (this was the bug!): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got[0].Err == nil || *got[0].Err != "par2 failed" {
		t.Errorf("entry[0].Err = %v, want 'par2 failed'", got[0].Err)
	}
	if got[1].Err != nil {
		t.Errorf("entry[1].Err = %v, want nil", got[1].Err)
	}
	if len(got[1].Lines) != 1 || got[1].Lines[0] != "Skipped: repair failed" {
		t.Errorf("entry[1].Lines = %v, want [Skipped: repair failed]", got[1].Lines)
	}
}
