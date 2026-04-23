package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/history"
)

// testHistoryServer builds a Server wired with an in-memory history repository.
func testHistoryServer(t *testing.T) (*Server, *history.Repository) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "hist.db")
	db, err := history.Open(dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) //nolint:errcheck // test cleanup
	repo := history.NewRepository(db)

	s := New(Options{
		Auth: AuthConfig{
			APIKey: testAPIKey,
			NZBKey: testNZBKey,
		},
		Version: "1.0.0-test",
		History: repo,
	})
	return s, repo
}

// seedEntry inserts a history entry and returns its NzoID.
func seedEntry(t *testing.T, repo *history.Repository, name, status, cat string, completed time.Time) string {
	t.Helper()
	nzoID := fmt.Sprintf("nzo%d", time.Now().UnixNano())
	e := history.Entry{
		NzoID:     nzoID,
		Name:      name,
		Status:    status,
		Category:  cat,
		Completed: completed,
		Bytes:     1024 * 1024 * 100, // 100 MiB
	}
	if err := repo.Add(t.Context(), e); err != nil {
		t.Fatalf("Add history entry %q: %v", nzoID, err)
	}
	return nzoID
}

// --- Default history listing ---

func TestHistoryDefault_EmptyRepo(t *testing.T) {
	t.Parallel()
	s, _ := testHistoryServer(t)

	rr := apiGet(t, s.Handler(), "/api?mode=history&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Status  bool `json:"status"`
		History struct {
			NoOfSlots int           `json:"noofslots"`
			Slots     []any `json:"slots"`
			TotalSize string        `json:"total_size"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Status {
		t.Error("status should be true")
	}
	if resp.History.NoOfSlots != 0 {
		t.Errorf("noofslots = %d; want 0", resp.History.NoOfSlots)
	}
	if resp.History.Slots == nil {
		t.Error("slots should not be nil")
	}
}

func TestHistoryDefault_WithEntries(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	now := time.Now()
	seedEntry(t, repo, "Movie1", "Completed", "movies", now)
	seedEntry(t, repo, "Show1", "Completed", "tv", now.Add(-time.Hour))
	seedEntry(t, repo, "Show2", "Failed", "tv", now.Add(-2*time.Hour))
	seedEntry(t, repo, "Doc1", "Completed", "docs", now.Add(-3*time.Hour))
	seedEntry(t, repo, "Game1", "Failed", "games", now.Add(-4*time.Hour))

	rr := apiGet(t, s.Handler(), "/api?mode=history&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		History struct {
			NoOfSlots int `json:"noofslots"`
			Slots     []struct {
				NzoID     string `json:"nzo_id"`
				Name      string `json:"name"`
				Status    string `json:"status"`
				Category  string `json:"category"`
				Size      string `json:"size"`
				Bytes     int64  `json:"bytes"`
				Completed int64  `json:"completed"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.History.NoOfSlots != 5 {
		t.Errorf("noofslots = %d; want 5", resp.History.NoOfSlots)
	}
	// Verify slot shape.
	if len(resp.History.Slots) == 0 {
		t.Fatal("expected at least one slot")
	}
	slot := resp.History.Slots[0]
	if slot.NzoID == "" {
		t.Error("nzo_id should not be empty")
	}
	if slot.Status == "" {
		t.Error("status should not be empty")
	}
	if slot.Bytes == 0 {
		t.Error("bytes should not be zero")
	}
	if slot.Completed == 0 {
		t.Error("completed should not be zero")
	}
}

func TestHistoryDefault_Pagination(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	now := time.Now()
	for i := 0; i < 6; i++ {
		seedEntry(t, repo, fmt.Sprintf("Job%d", i), "Completed", "tv", now.Add(-time.Duration(i)*time.Hour))
	}

	rr := apiGet(t, s.Handler(), "/api?mode=history&start=2&limit=3&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		History struct {
			NoOfSlots int `json:"noofslots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.History.NoOfSlots != 3 {
		t.Errorf("noofslots = %d; want 3 (paginated)", resp.History.NoOfSlots)
	}
}

func TestHistoryDefault_StatusFilter(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	now := time.Now()
	seedEntry(t, repo, "Done1", "Completed", "tv", now)
	seedEntry(t, repo, "Done2", "Completed", "tv", now.Add(-time.Hour))
	seedEntry(t, repo, "Fail1", "Failed", "tv", now.Add(-2*time.Hour))

	rr := apiGet(t, s.Handler(), "/api?mode=history&status=Failed&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		History struct {
			NoOfSlots int `json:"noofslots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.History.NoOfSlots != 1 {
		t.Errorf("noofslots = %d; want 1 (failed only)", resp.History.NoOfSlots)
	}
}

func TestHistoryDefault_SearchFilter(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	now := time.Now()
	seedEntry(t, repo, "Breaking Bad", "Completed", "tv", now)
	seedEntry(t, repo, "Better Call Saul", "Completed", "tv", now.Add(-time.Hour))
	seedEntry(t, repo, "The Wire", "Completed", "tv", now.Add(-2*time.Hour))

	rr := apiGet(t, s.Handler(), "/api?mode=history&search=Breaking&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		History struct {
			NoOfSlots int `json:"noofslots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.History.NoOfSlots != 1 {
		t.Errorf("noofslots = %d; want 1 (search match)", resp.History.NoOfSlots)
	}
}

// --- Delete ---

func TestHistoryDelete_Single(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	id := seedEntry(t, repo, "ToDelete", "Completed", "tv", time.Now())

	rr := apiGet(t, s.Handler(), "/api?mode=history&name=delete&value="+id+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Status  bool `json:"status"`
		Deleted int  `json:"deleted"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Status {
		t.Error("status should be true")
	}
	if resp.Deleted != 1 {
		t.Errorf("deleted = %d; want 1", resp.Deleted)
	}
}

func TestHistoryDelete_All(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	now := time.Now()
	seedEntry(t, repo, "Job1", "Completed", "tv", now)
	seedEntry(t, repo, "Job2", "Failed", "tv", now.Add(-time.Hour))
	seedEntry(t, repo, "Job3", "Completed", "tv", now.Add(-2*time.Hour))

	rr := apiGet(t, s.Handler(), "/api?mode=history&name=delete&value=all&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Deleted int `json:"deleted"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Deleted != 3 {
		t.Errorf("deleted = %d; want 3", resp.Deleted)
	}
}

func TestHistoryDelete_Failed(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	now := time.Now()
	seedEntry(t, repo, "Good", "Completed", "tv", now)
	seedEntry(t, repo, "Bad1", "Failed", "tv", now.Add(-time.Hour))
	seedEntry(t, repo, "Bad2", "Failed", "tv", now.Add(-2*time.Hour))

	rr := apiGet(t, s.Handler(), "/api?mode=history&name=delete&value=failed&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Deleted int `json:"deleted"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Deleted != 2 {
		t.Errorf("deleted = %d; want 2 (failed only)", resp.Deleted)
	}
}

func TestHistoryDelete_Completed(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	now := time.Now()
	seedEntry(t, repo, "Ok1", "Completed", "tv", now)
	seedEntry(t, repo, "Ok2", "Completed", "tv", now.Add(-time.Hour))
	seedEntry(t, repo, "Fail", "Failed", "tv", now.Add(-2*time.Hour))

	rr := apiGet(t, s.Handler(), "/api?mode=history&name=delete&value=completed&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Deleted int `json:"deleted"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Deleted != 2 {
		t.Errorf("deleted = %d; want 2 (completed only)", resp.Deleted)
	}
}

func TestHistoryDelete_MissingValue(t *testing.T) {
	t.Parallel()
	s, _ := testHistoryServer(t)

	rr := apiGet(t, s.Handler(), "/api?mode=history&name=delete&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
}

// --- MarkCompleted ---

func TestHistoryMarkCompleted(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	id := seedEntry(t, repo, "ToComplete", "Failed", "tv", time.Now())

	rr := apiGet(t, s.Handler(), "/api?mode=history&name=mark_as_completed&value="+id+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	// Verify the entry is now Completed.
	e, err := repo.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Status != "Completed" {
		t.Errorf("status = %q; want Completed", e.Status)
	}
}

// --- Nil guard ---

func TestHistoryNilGuard(t *testing.T) {
	t.Parallel()
	s := New(Options{
		Auth:    AuthConfig{APIKey: testAPIKey},
		Version: "1.0.0-test",
		// History intentionally nil.
	})
	rr := apiGet(t, s.Handler(), "/api?mode=history&apikey="+testAPIKey)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 when history is nil", rr.Code)
	}
}
