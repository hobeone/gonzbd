package api

import (
	"net/http"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// testServerWithQueue builds a Server wired with a queue.
func testServerWithQueue(t *testing.T, q *queue.Queue) *Server {
	t.Helper()
	return New(Options{
		Auth: AuthConfig{
			APIKey: testAPIKey,
			NZBKey: testNZBKey,
		},
		Version: "1.0.0-test",
		Queue:   q,
	})
}

func TestModeFullStatus_OK(t *testing.T) {
	t.Parallel()
	q := queue.New()
	s := testServerWithQueue(t, q)

	rr := apiGet(t, s.Handler(), "/api?mode=fullstatus&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)

	// Response structure: {"status": true, "status": {...}}
	// When decoded, the second "status" key overwrites the first, giving us the inner object
	// So m["status"] should be a map with {paused, noofslots, last_warning}
	statusVal, ok := m["status"]
	if !ok {
		t.Fatalf("status field missing from response")
	}

	statusData, ok := statusVal.(map[string]any)
	if !ok {
		t.Fatalf("status field is not an object (got %T: %v)", statusVal, statusVal)
	}

	if paused, ok := statusData["paused"].(bool); !ok {
		t.Errorf("paused not a bool or missing")
	} else if paused {
		t.Errorf("paused = %v; want false (queue not paused)", paused)
	}

	if noofslots, ok := statusData["noofslots"].(float64); !ok {
		t.Errorf("noofslots not a float64 (JSON number)")
	} else if noofslots != 0 {
		t.Errorf("noofslots = %v; want 0 (empty queue)", noofslots)
	}
}

func TestModeStatus_SubActionNotImplemented(t *testing.T) {
	t.Parallel()
	q := queue.New()
	s := testServerWithQueue(t, q)

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=unblock_server&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != false {
		t.Errorf("status = %v; want false", m["status"])
	}
}

func TestModeStatus_NoAction_FallsToFullStatus(t *testing.T) {
	t.Parallel()
	q := queue.New()
	s := testServerWithQueue(t, q)

	rr := apiGet(t, s.Handler(), "/api?mode=status&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	// Response has colliding "status" keys: {"status": true, "status": {...}}
	// The inner object wins in JSON, so we get the full status object
	statusVal, ok := m["status"]
	if !ok {
		t.Fatalf("status field missing")
	}
	statusData, ok := statusVal.(map[string]any)
	if !ok {
		t.Fatalf("status field is not an object (got %T)", statusVal)
	}
	// Verify it has the fullstatus fields
	if _, ok := statusData["paused"]; !ok {
		t.Errorf("paused field missing from fallback fullstatus response")
	}
}

func TestModeWarnings_Empty(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=warnings&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	warnings, ok := m["warnings"].([]any)
	if !ok {
		t.Fatalf("warnings not an array")
	}
	if len(warnings) != 0 {
		t.Errorf("warnings length = %d; want 0", len(warnings))
	}
}

func TestModeWarnings_Clear(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=warnings&name=clear&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}
}

func TestModeWarnings_Populated(t *testing.T) {
	t.Parallel()
	s := testServer()
	s.AddWarning("test warning 1")
	s.AddWarning("test warning 2")

	rr := apiGet(t, s.Handler(), "/api?mode=warnings&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	warnings, ok := m["warnings"].([]any)
	if !ok {
		t.Fatalf("warnings not an array")
	}
	if len(warnings) != 2 {
		t.Errorf("warnings length = %d; want 2", len(warnings))
	}
	if warnings[0] != "test warning 1" || warnings[1] != "test warning 2" {
		t.Errorf("warnings content mismatch: %v", warnings)
	}

	// Test clear
	rr = apiGet(t, s.Handler(), "/api?mode=warnings&name=clear&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("clear status = %d; want 200", rr.Code)
	}
	rr = apiGet(t, s.Handler(), "/api?mode=warnings&apikey="+testAPIKey)
	m = decodeJSON(t, rr)
	warnings = m["warnings"].([]any)
	if len(warnings) != 0 {
		t.Errorf("warnings length after clear = %d; want 0", len(warnings))
	}
}

func TestModeServerStats_Default(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=server_stats&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	if total, ok := m["total"].(float64); !ok {
		t.Errorf("total not a number")
	} else if total != 0 {
		t.Errorf("total = %v; want 0", total)
	}

	if servers, ok := m["servers"].(map[string]any); !ok {
		t.Errorf("servers not an object")
	} else if len(servers) != 0 {
		t.Errorf("servers length = %d; want 0", len(servers))
	}
}
