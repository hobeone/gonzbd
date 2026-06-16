package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/queue"
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
		App:     NopApp{History: repo},
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

// --- PostProc injection into history ---

// TestHistoryList_PostProcJobsInjected verifies that jobs currently in
// post-processing (from the queue with PostProc=true) appear in the
// history listing as synthetic entries, matching SABnzbd lifecycle (§11.3).
func TestHistoryList_PostProcJobsInjected(t *testing.T) {
	t.Parallel()

	// Build a server with both a queue and a history repo.
	dbPath := filepath.Join(t.TempDir(), "hist.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	q := queue.New()
	s := New(Options{
		Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
		Version: "1.0.0-test",
		Queue:   q,
		History: repo,
		App:     NopApp{Queue: q, History: repo},
	})

	// Seed a completed history entry.
	seedEntry(t, repo, "Completed.Job", "Completed", "tv", time.Now().Add(-time.Hour))

	// Add a post-processing job to the queue.
	ppJob := addTestJob(t, q, queue.AddOptions{Filename: "postproc.nzb"})
	ppJob.Status = constants.StatusRepairing
	_, err = q.SetPostProcStarted(ppJob.ID)
	if err != nil {
		t.Fatalf("SetPostProcStarted: %v", err)
	}
	// Set status after SetPostProcStarted (which resets to Verifying).
	if err := q.SetStatus(ppJob.ID, constants.StatusRepairing); err != nil {
		t.Fatalf("SetStatus: %v", err)
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

	// Should have 2 slots: the PP job (first) + the completed entry.
	if resp.History.NoOfSlots != 2 {
		t.Errorf("noofslots = %d; want 2", resp.History.NoOfSlots)
	}
	if len(resp.History.Slots) != 2 {
		t.Fatalf("got %d slots; want 2", len(resp.History.Slots))
	}

	// The post-processing job should be first with dynamic status.
	ppSlot := resp.History.Slots[0]
	if ppSlot.NzoID != ppJob.ID {
		t.Errorf("pp slot nzo_id = %s; want %s", ppSlot.NzoID, ppJob.ID)
	}
	if ppSlot.Status != "Repairing" {
		t.Errorf("pp slot status = %s; want Repairing", ppSlot.Status)
	}

	// The completed DB entry should be second.
	dbSlot := resp.History.Slots[1]
	if dbSlot.Status != "Completed" {
		t.Errorf("db slot status = %s; want Completed", dbSlot.Status)
	}
}

// TestHistoryList_PostProcNotInjectedForNzoIDs verifies that synthetic
// PP entries are NOT injected when the client requests specific nzo_ids.
func TestHistoryList_PostProcNotInjectedForNzoIDs(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "hist.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	q := queue.New()
	s := New(Options{
		Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
		Version: "1.0.0-test",
		Queue:   q,
		History: repo,
		App:     NopApp{Queue: q, History: repo},
	})

	// Add a PP job to the queue.
	ppJob := addTestJob(t, q, queue.AddOptions{Filename: "pp.nzb"})
	_, _ = q.SetPostProcStarted(ppJob.ID)

	// Request the PP job by nzo_id — should NOT find it (it's not in history DB).
	rr := apiGet(t, s.Handler(), fmt.Sprintf("/api?mode=history&apikey=%s&nzo_ids=%s", testAPIKey, ppJob.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	var resp struct {
		History struct {
			Slots []struct {
				NzoID string `json:"nzo_id"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// nzo_ids lookup goes to DB only, so no PP injection.
	if len(resp.History.Slots) != 0 {
		t.Errorf("got %d slots; want 0 (nzo_ids should skip PP injection)", len(resp.History.Slots))
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

func TestInjectPostProcJobs_Internal(t *testing.T) {
	t.Parallel()
	q := queue.New()
	s := Server{
		queue: q,
	}

	now := time.Now()
	// Add PP job
	j1 := addTestJob(t, q, queue.AddOptions{Filename: "show.nzb"})
	j1.PostProc = true
	j1.Status = constants.StatusRepairing
	j1.Category = "tv"
	j1.Name = "Show.S01E01"
	j1.TotalBytes = 1000
	j1.FailedBytes = 100
	j1.RemainingBytes = 200
	j1.DownloadStarted = now
	j1.DownloadFinished = now.Add(10 * time.Second)

	// Add non-PP job
	j2 := addTestJob(t, q, queue.AddOptions{Filename: "show2.nzb"})
	j2.PostProc = false
	j2.Status = constants.StatusDownloading
	j2.Category = "tv"
	j2.Name = "Show.S01E02"

	t.Run("no queue server", func(t *testing.T) {
		sNoQueue := Server{}
		slots, count, bytes := sNoQueue.injectPostProcJobs(nil, 0, 0, "", "", "", false)
		if len(slots) != 0 || count != 0 || bytes != 0 {
			t.Errorf("expected empty, got %v, %v, %v", slots, count, bytes)
		}
	})

	t.Run("default injects PP job only", func(t *testing.T) {
		slots, count, bytes := s.injectPostProcJobs(nil, 0, 0, "", "", "", false)
		if len(slots) != 1 {
			t.Fatalf("expected 1 injected slot, got %d", len(slots))
		}
		slot := slots[0]
		if slot.NzoID != j1.ID {
			t.Errorf("expected injected slot to be j1, got %v", slot)
		}
		if count != 1 {
			t.Errorf("expected count=1, got %d", count)
		}
		if bytes != 1000 {
			t.Errorf("expected bytes=1000, got %d", bytes)
		}
		if slot.Downloaded != 700 {
			t.Errorf("expected Downloaded=700 (1000-100-200), got %d", slot.Downloaded)
		}
		if slot.DownloadTime != 10 {
			t.Errorf("expected DownloadTime=10, got %d", slot.DownloadTime)
		}
	})

	t.Run("category filter mismatch", func(t *testing.T) {
		slots, _, _ := s.injectPostProcJobs(nil, 0, 0, "movies", "", "", false)
		if len(slots) != 0 {
			t.Errorf("expected 0 slots on category mismatch, got %v", slots)
		}
	})

	t.Run("failedOnly skips PP", func(t *testing.T) {
		slots, _, _ := s.injectPostProcJobs(nil, 0, 0, "", "", "", true)
		if len(slots) != 0 {
			t.Errorf("expected 0 slots on failedOnly, got %v", slots)
		}
	})

	t.Run("status filter mismatch", func(t *testing.T) {
		slots, _, _ := s.injectPostProcJobs(nil, 0, 0, "", "Downloading", "", false)
		if len(slots) != 0 {
			t.Errorf("expected 0 slots on status mismatch, got %v", slots)
		}
	})

	t.Run("search query", func(t *testing.T) {
		slots, _, _ := s.injectPostProcJobs(nil, 0, 0, "", "", "Show", false)
		if len(slots) != 1 {
			t.Errorf("expected 1 slot on query 'Show', got %v", slots)
		}

		var count int
		var bytes int64
		slots, count, bytes = s.injectPostProcJobs(nil, 0, 0, "", "", "debian", false)
		_ = count
		_ = bytes
		if len(slots) != 0 {
			t.Errorf("expected 0 slots on query 'debian', got %v", slots)
		}
	})
}
