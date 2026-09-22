package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These call the history sub-action handlers directly rather than through the
// router, so each one's own contract is pinned apart from the dispatch in
// modeHistory.

func historyRequest(t *testing.T, query string) *http.Request {
	t.Helper()
	return httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api?"+query, nil)
}

// TestModeHistory_RefusesAnUnknownAction pins the dispatch's fall-through: an
// action it does not know is a client error, not an empty listing.
func TestModeHistory_RefusesAnUnknownAction(t *testing.T) {
	t.Parallel()
	s, _ := testHistoryServer(t)
	rr := httptest.NewRecorder()
	s.modeHistory(rr, historyRequest(t, "mode=history&name=frobnicate"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown history action", rr.Code)
	}
}

// TestHistoryList_ListsASeededEntry pins that the listing reports what the
// repository holds.
func TestHistoryList_ListsASeededEntry(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)
	id := seedEntry(t, repo, "Listed", "Completed", "tv", time.Now())

	rr := httptest.NewRecorder()
	s.historyList(rr, historyRequest(t, "mode=history"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	h, _ := decodeJSON(t, rr)["history"].(map[string]any)
	slots, _ := h["slots"].([]any)
	if len(slots) != 1 {
		t.Fatalf("slots = %v, want the one seeded entry", h["slots"])
	}
	if got, _ := slots[0].(map[string]any)["nzo_id"].(string); got != id {
		t.Errorf("nzo_id = %q, want %q", got, id)
	}
}

// TestHistoryDelete_RemovesTheNamedEntry pins that a delete reaches the job
// manager for each named id and reports how many went.
func TestHistoryDelete_RemovesTheNamedEntry(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)
	id := seedEntry(t, repo, "Deleted", "Failed", "tv", time.Now())

	rr := httptest.NewRecorder()
	s.historyDelete(rr, historyRequest(t, "mode=history&name=delete&value="+id))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := decodeJSON(t, rr)["deleted"]; got != float64(1) {
		t.Errorf("deleted = %v, want 1", got)
	}
	if _, err := repo.Get(t.Context(), id); err == nil {
		t.Error("the entry is still in history after its delete was reported")
	}
}

// TestHistoryMarkCompleted_MarksTheNamedEntry pins that the handler reaches
// the job manager, which marks the entry and reclaims what it kept.
func TestHistoryMarkCompleted_MarksTheNamedEntry(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)
	id := seedEntry(t, repo, "Marked", "Failed", "tv", time.Now())

	rr := httptest.NewRecorder()
	s.historyMarkCompleted(rr, historyRequest(t, "mode=history&name=mark_as_completed&value="+id))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	e, err := repo.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != "Completed" {
		t.Errorf("status = %q, want Completed", e.Status)
	}
}

// TestHistoryRetry_ReportsTheRetriedID pins the retry handler's success shape.
func TestHistoryRetry_ReportsTheRetriedID(t *testing.T) {
	t.Parallel()
	s, _ := testHistoryServer(t)
	rr := httptest.NewRecorder()
	s.historyRetry(rr, historyRequest(t, "mode=history&name=retry&value=job_123"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got, _ := decodeJSON(t, rr)["nzo_id"].(string); got != "job_123" {
		t.Errorf("nzo_id = %q, want job_123", got)
	}
}
