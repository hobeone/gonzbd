package api

import (
	"net/http"
	"testing"

	"github.com/hobeone/gonzbd/internal/api/apitest"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/queue"
)

// testServerWithQueue builds a Server wired with a queue.
func testServerWithQueue(t *testing.T, q *queue.Queue) *Server {
	t.Helper()
	return New(Options{
		Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
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

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=delete_orphan&apikey="+testAPIKey)
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

	// servers is now an array (empty when mockApp returns nil).
	servers, ok := m["servers"].([]any)
	if !ok {
		t.Fatalf("servers not an array; got %T", m["servers"])
	}
	if len(servers) != 0 {
		t.Errorf("servers length = %d; want 0", len(servers))
	}
}

// --- Sonarr compatibility tests ---

// TestFullStatus_SonarrCompleteDir verifies fullstatus includes the
// completedir field that Sonarr reads to resolve category paths.
func TestFullStatus_SonarrCompleteDir(t *testing.T) {
	t.Parallel()
	q := queue.New()
	s := New(Options{
		Config: &config.Config{General: config.GeneralConfig{
			APIKey:      testAPIKey,
			NZBKey:      testNZBKey,
			CompleteDir: "/data/complete",
		}},
		Version: "1.0.0-test",
		Queue:   q,
	})

	rr := apiGet(t, s.Handler(), "/api?mode=fullstatus&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	statusVal, ok := m["status"]
	if !ok {
		t.Fatal("status field missing")
	}
	statusData, ok := statusVal.(map[string]any)
	if !ok {
		t.Fatalf("status is not an object (got %T)", statusVal)
	}

	completeDir, ok := statusData["completedir"].(string)
	if !ok {
		t.Fatal("completedir field missing or not a string in fullstatus response")
	}
	if completeDir != "/data/complete" {
		t.Errorf("completedir = %q; want %q", completeDir, "/data/complete")
	}
}

type unblockSpyApp struct {
	apitest.NopApp
	unblockResult bool
	calledName    string
}

func (u *unblockSpyApp) UnblockServer(name string) bool {
	u.calledName = name
	return u.unblockResult
}

func TestModeStatus_UnblockServer(t *testing.T) {
	t.Parallel()

	// 1. Missing value param -> 400
	s1 := testServer()
	h1 := s1.Handler()
	rr1 := apiGet(t, h1, "/api?mode=status&name=unblock_server&apikey="+testAPIKey)
	if rr1.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (missing value)", rr1.Code)
	}

	// 2. Happy path (app returns true) -> 200
	app2 := &unblockSpyApp{unblockResult: true}
	s2 := New(Options{
		Version: "1.0.0-test",
		Config: &config.Config{General: config.GeneralConfig{
			APIKey: testAPIKey,
			NZBKey: testNZBKey,
		}},
		App: app2,
	})
	rr2 := apiGet(t, s2.Handler(), "/api?mode=status&name=unblock_server&value=my-server&apikey="+testAPIKey)
	if rr2.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (happy path); body = %s", rr2.Code, rr2.Body.String())
	}
	if app2.calledName != "my-server" {
		t.Errorf("calledName = %q; want my-server", app2.calledName)
	}
	m2 := decodeJSON(t, rr2)
	if m2["status"] != true {
		t.Errorf("response status = %v; want true", m2["status"])
	}

	// 3. Server not found (app returns false) -> 404
	app3 := &unblockSpyApp{unblockResult: false}
	s3 := New(Options{
		Version: "1.0.0-test",
		Config: &config.Config{General: config.GeneralConfig{
			APIKey: testAPIKey,
			NZBKey: testNZBKey,
		}},
		App: app3,
	})
	rr3 := apiGet(t, s3.Handler(), "/api?mode=status&name=unblock_server&value=unknown-server&apikey="+testAPIKey)
	if rr3.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (not found); body = %s", rr3.Code, rr3.Body.String())
	}
}
