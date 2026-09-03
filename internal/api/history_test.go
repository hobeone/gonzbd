package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api/apitest"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

// testHistoryServer builds a Server wired with an in-memory history repository.
func testHistoryServer(t *testing.T) (*Server, *history.Repository) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "hist.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) //nolint:errcheck // test cleanup
	repo := history.NewRepository(db)

	s := New(Options{
		Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
		Version: "1.0.0-test",
		History: repo,
		App:     apitest.NopApp{History: repo},
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
			NoOfSlots int    `json:"noofslots"`
			Slots     []any  `json:"slots"`
			TotalSize string `json:"total_size"`
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
	for i := range 6 {
		seedEntry(t, repo, fmt.Sprintf("Job%d", i), "Completed", "tv", now.Add(-time.Duration(i)*time.Hour))
	}

	rr := apiGet(t, s.Handler(), "/api?mode=history&start=2&limit=3&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var res struct {
		History struct {
			NoOfSlots int           `json:"noofslots"`
			Slots     []historySlot `json:"slots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.History.NoOfSlots != 6 {
		t.Errorf("noofslots = %d; want 6 (total count for pagination)", res.History.NoOfSlots)
	}
	if len(res.History.Slots) != 3 {
		t.Errorf("len(slots) = %d; want 3 (paginated)", len(res.History.Slots))
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

type retryErrApp struct {
	apitest.NopApp
	err error
}

func (a retryErrApp) RetryHistoryJob(_ context.Context, _ string) error {
	return a.err
}

// --- Retry ---

func TestHistoryRetry(t *testing.T) {
	t.Parallel()

	t.Run("missing value", func(t *testing.T) {
		t.Parallel()
		s, _ := testHistoryServer(t)
		rr := apiGet(t, s.Handler(), "/api?mode=history&name=retry&apikey="+testAPIKey)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400", rr.Code)
		}
		m := decodeJSON(t, rr)
		if _, ok := m["error"]; !ok {
			t.Error("expected error field in JSON response")
		}
	})

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		s, _ := testHistoryServer(t)
		rr := apiGet(t, s.Handler(), "/api?mode=history&name=retry&value=job_123&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		m := decodeJSON(t, rr)
		if m["status"] != true {
			t.Errorf("status = %v; want true", m["status"])
		}
		if nzoID, _ := m["nzo_id"].(string); nzoID != "job_123" {
			t.Errorf("nzo_id = %v; want job_123", m["nzo_id"])
		}
	})

	t.Run("retry error", func(t *testing.T) {
		t.Parallel()
		s, repo := testHistoryServer(t)
		s.setAppServices(retryErrApp{History: repo, err: errors.New("simulated retry failure")})

		rr := apiGet(t, s.Handler(), "/api?mode=history&name=retry&value=job_fail&apikey="+testAPIKey)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want 500", rr.Code)
		}
		m := decodeJSON(t, rr)
		errMsg, _ := m["error"].(string)
		if !strings.Contains(errMsg, "simulated retry failure") {
			t.Errorf("error = %q; want to contain 'simulated retry failure'", errMsg)
		}
	})
}

// --- Nil guard ---

func TestHistoryNilGuard(t *testing.T) {
	t.Parallel()
	s := New(Options{
		Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey}},
		Version: "1.0.0-test",
		// History intentionally nil.
	})
	rr := apiGet(t, s.Handler(), "/api?mode=history&apikey="+testAPIKey)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 when history is nil", rr.Code)
	}
}

// --- Sonarr/Radarr compatibility tests ---

// TestHistoryLimitZero verifies that limit=0 returns all entries (Sonarr sends this).
func TestHistoryLimitZero(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	now := time.Now()
	for i := range 5 {
		seedEntry(t, repo, fmt.Sprintf("Job%d", i), "Completed", "tv", now.Add(-time.Duration(i)*time.Hour))
	}

	rr := apiGet(t, s.Handler(), "/api?mode=history&limit=0&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		History struct {
			NoOfSlots int           `json:"noofslots"`
			Slots     []historySlot `json:"slots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.History.NoOfSlots != 5 {
		t.Errorf("noofslots = %d; want 5", resp.History.NoOfSlots)
	}
	if len(resp.History.Slots) != 5 {
		t.Errorf("slots len = %d; want 5 (limit=0 should return all)", len(resp.History.Slots))
	}
}

// TestHistoryStageLog verifies that stage_log is present and is an empty
// array (not null). Sonarr iterates over it and crashes on null.
func TestHistoryStageLog(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	seedEntry(t, repo, "TestJob", "Completed", "tv", time.Now())

	rr := apiGet(t, s.Handler(), "/api?mode=history&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	// Parse raw JSON to check stage_log type.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal root: %v", err)
	}
	var histRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["history"], &histRaw); err != nil {
		t.Fatalf("unmarshal history: %v", err)
	}
	var slots []json.RawMessage
	if err := json.Unmarshal(histRaw["slots"], &slots); err != nil {
		t.Fatalf("unmarshal slots: %v", err)
	}
	if len(slots) == 0 {
		t.Fatal("expected at least one slot")
	}

	var slot map[string]json.RawMessage
	if err := json.Unmarshal(slots[0], &slot); err != nil {
		t.Fatalf("unmarshal slot: %v", err)
	}

	stageLog, ok := slot["stage_log"]
	if !ok {
		t.Fatal("stage_log field missing from history slot")
	}
	// Must be "[]" not "null".
	if string(stageLog) == "null" {
		t.Error("stage_log is null; must be [] (empty array)")
	}
	if string(stageLog) != "[]" {
		t.Errorf("stage_log = %s; want []", stageLog)
	}
}

func TestParseStageLog_Empty(t *testing.T) {
	result := parseStageLog("")
	if len(result) != 0 {
		t.Errorf("parseStageLog(\"\") returned %d entries; want 0", len(result))
	}
}

func TestParseStageLog_Null(t *testing.T) {
	result := parseStageLog("null")
	if len(result) != 0 {
		t.Errorf("parseStageLog(\"null\") returned %d entries; want 0", len(result))
	}
}

func TestParseStageLog_InvalidJSON(t *testing.T) {
	result := parseStageLog("{broken")
	if len(result) != 0 {
		t.Errorf("parseStageLog(broken) returned %d entries; want 0", len(result))
	}
}

func TestParseStageLog_ValidEntries(t *testing.T) {
	input := `[{"Stage":"repair","Elapsed":2500000000,"Err":null,"Lines":["All files are correct"]},{"Stage":"unpack","Elapsed":5300000000,"Err":null,"Lines":["Extracted: movie.mkv"]}]`
	result := parseStageLog(input)
	if len(result) != 2 {
		t.Fatalf("parseStageLog returned %d entries; want 2", len(result))
	}
	if result[0].Name != "repair" {
		t.Errorf("result[0].Name = %q; want %q", result[0].Name, "repair")
	}
	if result[1].Name != "unpack" {
		t.Errorf("result[1].Name = %q; want %q", result[1].Name, "unpack")
	}
	// First action should be the duration, second the par2 output line.
	if len(result[0].Actions) < 2 {
		t.Fatalf("result[0].Actions has %d entries; want >= 2", len(result[0].Actions))
	}
	if result[0].Actions[0] != "Completed in 2.5s" {
		t.Errorf("result[0].Actions[0] = %q; want %q", result[0].Actions[0], "Completed in 2.5s")
	}
	if result[0].Actions[1] != "All files are correct" {
		t.Errorf("result[0].Actions[1] = %q; want %q", result[0].Actions[1], "All files are correct")
	}
}

func TestParseStageLog_WithError(t *testing.T) {
	errMsg := "repair failed: too many blocks missing"
	input := fmt.Sprintf(`[{"Stage":"repair","Elapsed":1000000000,"Err":%q,"Lines":[]}]`, errMsg)
	result := parseStageLog(input)
	if len(result) != 1 {
		t.Fatalf("parseStageLog returned %d entries; want 1", len(result))
	}
	found := slices.Contains(result[0].Actions, "Error: "+errMsg)
	if !found {
		t.Errorf("expected error action in %v", result[0].Actions)
	}
}

func TestHistorySlot_CompletenessAndDownloaded(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)

	e := history.Entry{
		NzoID:        "completeness-test-001",
		Name:         "Test.Job",
		Status:       "Completed",
		Completed:    time.Now(),
		Bytes:        100_000_000,
		Downloaded:   99_000_000,
		Completeness: 98,
		DownloadTime: 60,
		PostprocTime: 15,
	}
	if err := repo.Add(t.Context(), e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rr := apiGet(t, s.Handler(), fmt.Sprintf("/api?mode=history&apikey=%s&nzo_ids=%s", testAPIKey, e.NzoID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	var resp struct {
		History struct {
			Slots []struct {
				Completeness int64 `json:"completeness"`
				Downloaded   int64 `json:"downloaded"`
				PostprocTime int64 `json:"postproc_time"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.History.Slots) != 1 {
		t.Fatalf("got %d slots; want 1", len(resp.History.Slots))
	}

	slot := resp.History.Slots[0]
	if slot.Completeness != 98 {
		t.Errorf("completeness = %d; want 98", slot.Completeness)
	}
	if slot.Downloaded != 99_000_000 {
		t.Errorf("downloaded = %d; want 99000000", slot.Downloaded)
	}
	if slot.PostprocTime != 15 {
		t.Errorf("postproc_time = %d; want 15", slot.PostprocTime)
	}
}

// --- PostProc jobs stay in the queue, not history ---

// TestHistoryList_PostProcJobsNotInjected verifies that jobs currently in
// post-processing (PostProc=true in the queue) do NOT appear in the history
// listing. Such jobs remain visible in the queue (see queue_test.go) with
// their live status until OnJobDone moves them to history; injecting a
// synthetic duplicate here previously caused the job to render in both the
// queue and history tables simultaneously.
func TestHistoryList_PostProcJobsNotInjected(t *testing.T) {
	t.Parallel()

	// Build a server with both a queue and a history repo.
	dbPath := filepath.Join(t.TempDir(), "hist.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	d := newTestAPIDispatcher(t)
	s := New(Options{
		Config:     &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
		Version:    "1.0.0-test",
		Dispatcher: d,
		History:    repo,
		App:        apitest.NopApp{Dispatcher: d, History: repo},
	})

	// Seed a completed history entry.
	seedEntry(t, repo, "Completed.Job", "Completed", "tv", time.Now().Add(-time.Hour))

	// Add an active post-processing job to the dispatcher.
	ppJob := job.New("j-pp", "PostProc Job", job.Policy{})
	if err := d.Add(ppJob, dispatch.Header{
		Name:     "PostProc Job",
		Filename: "postproc.nzb",
	}); err != nil {
		t.Fatalf("d.Add: %v", err)
	}

	rr := apiGet(t, s.Handler(), "/api?mode=history&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	var resp struct {
		History struct {
			NoOfSlots int `json:"noofslots"`
			Slots     []struct {
				NzoID  string `json:"nzo_id"`
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Should have only the one completed DB entry; the PP job stays in the
	// queue and must not be duplicated here.
	if resp.History.NoOfSlots != 1 {
		t.Errorf("noofslots = %d; want 1", resp.History.NoOfSlots)
	}
	if len(resp.History.Slots) != 1 {
		t.Fatalf("got %d slots; want 1", len(resp.History.Slots))
	}
	if resp.History.Slots[0].NzoID == ppJob.ID() {
		t.Errorf("PP job %s leaked into history listing", ppJob.ID())
	}
	if resp.History.Slots[0].Status != "Completed" {
		t.Errorf("db slot status = %s; want Completed", resp.History.Slots[0].Status)
	}
}

func TestFetchEntriesByIDs_Internal(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)
	ctx := t.Context()

	id1 := seedEntry(t, repo, "Ubuntu Linux", "Completed", "os", time.Now())
	id2 := seedEntry(t, repo, "Debian Linux", "Failed", "os", time.Now())
	id3 := seedEntry(t, repo, "Windows 11", "Completed", "os", time.Now())

	t.Run("empty ids string", func(t *testing.T) {
		got := s.fetchEntriesByIDs(ctx, "", "", "", "", false)
		if len(got) != 0 {
			t.Errorf("expected empty, got %d entries", len(got))
		}
	})

	t.Run("invalid and valid csv ids", func(t *testing.T) {
		csv := fmt.Sprintf("%s,nonexistent,%s", id1, id2)
		got := s.fetchEntriesByIDs(ctx, csv, "", "", "", false)
		if len(got) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(got))
		}
		if got[0].NzoID != id1 || got[1].NzoID != id2 {
			t.Errorf("unexpected entries order/ids: %v", got)
		}
	})

	t.Run("category filter", func(t *testing.T) {
		csv := fmt.Sprintf("%s,%s", id1, id2)
		got := s.fetchEntriesByIDs(ctx, csv, "movies", "", "", false)
		if len(got) != 0 {
			t.Errorf("expected 0 entries with category 'movies', got %v", got)
		}
		got = s.fetchEntriesByIDs(ctx, csv, "os", "", "", false)
		if len(got) != 2 {
			t.Errorf("expected 2 entries with category 'os', got %v", got)
		}
	})

	t.Run("status filter", func(t *testing.T) {
		csv := fmt.Sprintf("%s,%s,%s", id1, id2, id3)
		got := s.fetchEntriesByIDs(ctx, csv, "", "Failed", "", false)
		if len(got) != 1 || got[0].NzoID != id2 {
			t.Errorf("expected only failed entry id2, got %v", got)
		}
	})

	t.Run("failedOnly filter", func(t *testing.T) {
		csv := fmt.Sprintf("%s,%s,%s", id1, id2, id3)
		got := s.fetchEntriesByIDs(ctx, csv, "", "", "", true)
		if len(got) != 1 || got[0].NzoID != id2 {
			t.Errorf("expected only failed entry id2, got %v", got)
		}
	})

	t.Run("search filter case insensitive and field selection", func(t *testing.T) {
		csv := fmt.Sprintf("%s,%s,%s", id1, id2, id3)
		// Match Ubuntu by "ubuntu" (name)
		got := s.fetchEntriesByIDs(ctx, csv, "", "", "ubuntu", false)
		if len(got) != 1 || got[0].NzoID != id1 {
			t.Errorf("search for 'ubuntu' expected id1, got %v", got)
		}

		// Match nothing
		got = s.fetchEntriesByIDs(ctx, csv, "", "", "macOS", false)
		if len(got) != 0 {
			t.Errorf("search for 'macOS' expected 0 results, got %v", got)
		}
	})
}

type removeHistoryErrApp struct {
	apitest.NopApp
	errByID map[string]error
}

func (a removeHistoryErrApp) RemoveHistoryJob(ctx context.Context, id string, deleteFiles bool) error {
	if err, ok := a.errByID[id]; ok && err != nil {
		return err
	}
	return a.NopApp.RemoveHistoryJob(ctx, id, deleteFiles)
}

func TestHistoryDelete_RemoveHistoryJobErrorLog(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "hist.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	rec := &recordHandler{}
	logger := slog.New(rec)
	s := New(Options{
		Logger:  logger,
		Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
		Version: "1.0.0-test",
		History: repo,
		App: removeHistoryErrApp{
			NopApp:  apitest.NopApp{History: repo},
			errByID: map[string]error{"hist_fail": errors.New("simulated history delete failure")},
		},
	})

	id1 := seedEntry(t, repo, "ToKeepOrDelete", "Completed", "tv", time.Now())
	value := id1 + ",hist_fail"

	rr := apiGet(t, s.Handler(), "/api?mode=history&name=delete&value="+value+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}
	if deleted, ok := m["deleted"].(float64); !ok || deleted != 1 {
		t.Errorf("deleted = %v; want 1", m["deleted"])
	}

	if !rec.hasWarn("failed to remove job during bulk delete", "hist_fail", "simulated history delete failure") {
		t.Errorf("expected warning log not found in records: %v", rec.records)
	}
}

func TestHistoryDelete_ContextCancelledErrors(t *testing.T) {
	s, _ := testHistoryServer(t)
	for _, val := range []string{"all", "failed", "completed"} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		req := httptest.NewRequest(http.MethodGet, "/api?mode=history&name=delete&value="+val+"&apikey="+testAPIKey, nil)
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("value=%s with cancelled context: got status %d, want 500", val, rr.Code)
		}
	}
}

func TestHistoryMarkCompleted_ContextCancelledError(t *testing.T) {
	s, _ := testHistoryServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=history&name=mark_as_completed&value=nzo123&apikey="+testAPIKey, nil)
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("got status %d, want 500", rr.Code)
	}
}
