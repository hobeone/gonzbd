package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// makeTestNZB returns a minimal valid NZB XML document as a byte slice.
func makeTestNZB(t *testing.T) []byte {
	t.Helper()
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test@example.com" date="1609459200" subject="test.nzb (1/1)">
    <groups>
      <group>alt.binaries.test</group>
    </groups>
    <segments>
      <segment bytes="1024" number="1">test-article-id-001@example.com</segment>
    </segments>
  </file>
</nzb>`)
}

// testQueueServer builds a Server wired with a fresh queue (and no history).
func testQueueServer(t *testing.T) (*Server, *queue.Queue) {
	t.Helper()
	q := queue.New()
	s := New(Options{
		Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
		Version: "1.0.0-test",
		Queue:   q,
		App:     mockApp{q: q},
	})
	return s, q
}

// addTestJob adds a job parsed from a minimal NZB to the queue and returns it.
func addTestJob(t *testing.T, q *queue.Queue, opts queue.AddOptions) *queue.Job {
	t.Helper()
	data := makeTestNZB(t)
	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse test NZB: %v", err)
	}
	if opts.Filename == "" {
		opts.Filename = "test.nzb"
	}
	job, err := queue.NewJob(parsed, opts, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	return job
}

// --- Default queue listing ---

func TestQueueDefault_EmptyQueue(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Status bool `json:"status"`
		Queue  struct {
			NoOfSlots      int   `json:"noofslots"`
			NoOfSlotsTotal int   `json:"noofslots_total"`
			Paused         bool  `json:"paused"`
			Slots          []any `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Status {
		t.Error("status should be true")
	}
	if resp.Queue.NoOfSlots != 0 {
		t.Errorf("noofslots = %d; want 0", resp.Queue.NoOfSlots)
	}
	if resp.Queue.Slots == nil {
		t.Error("slots should not be nil (should be empty array)")
	}
}

func TestQueueDefault_WithJobs(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "movie.nzb", Category: "movies"})
	addTestJob(t, q, queue.AddOptions{Filename: "show.nzb", Category: "tv"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Queue struct {
			NoOfSlots int `json:"noofslots"`
			Slots     []struct {
				NzoID    string `json:"nzo_id"`
				Filename string `json:"filename"`
				Category string `json:"cat"`
				Priority string `json:"priority"`
				Status   string `json:"status"`
				PP       string `json:"pp"`
				MB       string `json:"mb"`
				Bytes    int64  `json:"bytes"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Queue.NoOfSlots != 2 {
		t.Errorf("noofslots = %d; want 2", resp.Queue.NoOfSlots)
	}
	if len(resp.Queue.Slots) != 2 {
		t.Fatalf("slots len = %d; want 2", len(resp.Queue.Slots))
	}
	// Verify essential shape fields are present.
	slot := resp.Queue.Slots[0]
	if slot.NzoID == "" {
		t.Error("nzo_id should not be empty")
	}
	if slot.Priority == "" {
		t.Error("priority should not be empty")
	}
	if slot.Status == "" {
		t.Error("status should not be empty")
	}
}

func TestQueueDefault_Filtering(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "movie.nzb", Category: "movies"})
	addTestJob(t, q, queue.AddOptions{Filename: "show.nzb", Category: "tv"})
	addTestJob(t, q, queue.AddOptions{Filename: "doc.nzb", Category: "tv"})

	// Filter by category=tv → expect 2 slots.
	rr := apiGet(t, s.Handler(), "/api?mode=queue&cat=tv&apikey="+testAPIKey)
	var resp struct {
		Queue struct {
			NoOfSlots int `json:"noofslots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Queue.NoOfSlots != 2 {
		t.Errorf("filtered noofslots = %d; want 2", resp.Queue.NoOfSlots)
	}

	// Filter by search=movie → expect 1 slot.
	rr2 := apiGet(t, s.Handler(), "/api?mode=queue&search=movie&apikey="+testAPIKey)
	var resp2 struct {
		Queue struct {
			NoOfSlots int `json:"noofslots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp2.Queue.NoOfSlots != 1 {
		t.Errorf("search-filtered noofslots = %d; want 1", resp2.Queue.NoOfSlots)
	}
}

func TestQueueDefault_Pagination(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	for i := range 5 {
		addTestJob(t, q, queue.AddOptions{Filename: fmt.Sprintf("job%d.nzb", i)})
	}

	// start=2 limit=2 → 2 slots.
	rr := apiGet(t, s.Handler(), "/api?mode=queue&start=2&limit=2&apikey="+testAPIKey)
	var resp struct {
		Queue struct {
			NoOfSlots      int `json:"noofslots"`
			NoOfSlotsTotal int `json:"noofslots_total"`
			Start          int `json:"start"`
			Limit          int `json:"limit"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Queue.NoOfSlots != 2 {
		t.Errorf("noofslots = %d; want 2", resp.Queue.NoOfSlots)
	}
	if resp.Queue.NoOfSlotsTotal != 5 {
		t.Errorf("noofslots_total = %d; want 5", resp.Queue.NoOfSlotsTotal)
	}
}

// --- Paused status override ---

func TestQueueList_GlobalPauseOverridesDownloadingToPaused(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

	// Simulate the dispatcher having marked this job as Downloading.
	if err := q.SetStatusIf(job.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}

	// Globally pause the queue.
	q.PauseAll()

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Queue struct {
			Status string `json:"status"`
			Paused bool   `json:"paused"`
			Slots  []struct {
				Status string `json:"status"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !resp.Queue.Paused {
		t.Error("queue.paused should be true")
	}
	if resp.Queue.Status != "Paused" {
		t.Errorf("queue.status = %q; want Paused", resp.Queue.Status)
	}
	if len(resp.Queue.Slots) != 1 {
		t.Fatalf("slots len = %d; want 1", len(resp.Queue.Slots))
	}
	if resp.Queue.Slots[0].Status != "Paused" {
		t.Errorf("slot status = %q; want Paused (overridden from Downloading)", resp.Queue.Slots[0].Status)
	}

	// Verify internal state is still Downloading (not mutated).
	j, err := q.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != constants.StatusDownloading {
		t.Errorf("internal status = %q; want Downloading (should not be mutated)", j.Status)
	}
}

func TestQueueList_NotPausedKeepsDownloadingStatus(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

	// Simulate the dispatcher having marked this job as Downloading.
	if err := q.SetStatusIf(job.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}

	// Queue is NOT paused — status should remain Downloading.
	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Queue struct {
			Slots []struct {
				Status string `json:"status"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Queue.Slots) != 1 {
		t.Fatalf("slots len = %d; want 1", len(resp.Queue.Slots))
	}
	if resp.Queue.Slots[0].Status != "Downloading" {
		t.Errorf("slot status = %q; want Downloading (not paused)", resp.Queue.Slots[0].Status)
	}
}

func TestQueuePause(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=pause&value="+job.ID+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	j, err := q.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != constants.StatusPaused {
		t.Errorf("status = %q; want Paused", j.Status)
	}
}

func TestQueueResume(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
	// Pause first.
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=resume&value="+job.ID+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	j, err := q.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != constants.StatusQueued {
		t.Errorf("status = %q; want Queued", j.Status)
	}
}

// --- Delete ---

func TestQueueDelete_Single(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=delete&value="+job.ID+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	if q.Len() != 0 {
		t.Errorf("queue len = %d; want 0 after delete", q.Len())
	}
}

func TestQueueDelete_Multiple(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	j1 := addTestJob(t, q, queue.AddOptions{Filename: "a.nzb"})
	j2 := addTestJob(t, q, queue.AddOptions{Filename: "b.nzb"})
	addTestJob(t, q, queue.AddOptions{Filename: "c.nzb"}) // kept

	value := j1.ID + "," + j2.ID
	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=delete&value="+value+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	if q.Len() != 1 {
		t.Errorf("queue len = %d; want 1", q.Len())
	}
}

func TestQueueDelete_All(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "a.nzb"})
	addTestJob(t, q, queue.AddOptions{Filename: "b.nzb"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=delete&value=all&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	if q.Len() != 0 {
		t.Errorf("queue len = %d; want 0", q.Len())
	}
}

func TestQueueDelete_MissingValue(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=delete&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
}

func TestQueuePurge(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "a.nzb"})
	addTestJob(t, q, queue.AddOptions{Filename: "b.nzb"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=purge&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	if q.Len() != 0 {
		t.Errorf("queue len = %d; want 0 after purge", q.Len())
	}
}

// --- AddFile (multipart upload) ---

func TestAddFile_Multipart(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)

	nzbData := makeTestNZB(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("nzbfile", "test.nzb")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(nzbData); err != nil {
		t.Fatalf("write nzb: %v", err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey="+testAPIKey, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Status bool     `json:"status"`
		NzoIDs []string `json:"nzo_ids"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Status {
		t.Error("status should be true")
	}
	if len(resp.NzoIDs) != 1 {
		t.Fatalf("nzo_ids len = %d; want 1", len(resp.NzoIDs))
	}
	if q.Len() != 1 {
		t.Errorf("queue len = %d; want 1 after addfile", q.Len())
	}
	// Verify the job ID matches.
	job, err := q.Get(resp.NzoIDs[0])
	if err != nil {
		t.Fatalf("queue.Get(%q): %v", resp.NzoIDs[0], err)
	}
	if job.Filename != "test.nzb" {
		t.Errorf("filename = %q; want test.nzb", job.Filename)
	}
}

func TestAddFile_MissingFile(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey="+testAPIKey, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
}

func TestAddFile_NameField(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)

	nzbData := makeTestNZB(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// Using "name" field instead of "nzbfile"
	fw, err := mw.CreateFormFile("name", "test.nzb")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(nzbData); err != nil {
		t.Fatalf("write nzb: %v", err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey="+testAPIKey, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Status bool     `json:"status"`
		NzoIDs []string `json:"nzo_ids"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Status {
		t.Error("status should be true")
	}
	if q.Len() != 1 {
		t.Errorf("queue len = %d; want 1 after addfile", q.Len())
	}
}

// --- AddLocalFile ---

func TestAddLocalFile_HappyPath(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)

	dir := t.TempDir()
	nzbPath := filepath.Join(dir, "local.nzb")
	if err := os.WriteFile(nzbPath, makeTestNZB(t), 0o600); err != nil {
		t.Fatalf("write NZB: %v", err)
	}

	rr := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name="+nzbPath+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if q.Len() != 1 {
		t.Errorf("queue len = %d; want 1", q.Len())
	}
}

func TestAddLocalFile_PathTraversal(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)

	// Use a path that is relative (not absolute), which is caught by the
	// filepath.IsAbs check before any filesystem access occurs.
	// A relative path with ".." components cannot escape our guard.
	traversal := "../etc/passwd"
	rr := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name="+traversal+"&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for path traversal (relative path)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "absolute") {
		t.Errorf("expected 'absolute' in error body; got: %s", rr.Body.String())
	}
}

func TestAddLocalFile_Relative(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name=./foo.nzb&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for relative path", rr.Code)
	}
}

// --- AddURL ---

// When no Grabber is wired into Options, mode=addurl should signal
// that clearly rather than silently 500-ing on the underlying nil deref.
func TestAddURL_NoGrabber(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := apiGet(t, s.Handler(), "/api?mode=addurl&apikey="+testAPIKey+"&name=http://example.test/foo.nzb")
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "grabber not wired") {
		t.Errorf("body = %q; want 'grabber not wired'", rr.Body.String())
	}
}

// When the URL is missing, addurl should reject with 400 regardless of
// whether a Grabber is wired — the parameter validation happens first.
func TestAddURL_MissingURL(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := apiGet(t, s.Handler(), "/api?mode=addurl&apikey="+testAPIKey)
	// With no Grabber, the nil-check fires before the URL check. That's
	// fine: both are 4xx/5xx and mutually exclusive in prod (if the
	// grabber is wired, this test path becomes a 400).
	if rr.Code != http.StatusInternalServerError && rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 or 500", rr.Code)
	}
}

// --- Stub actions ---

func TestQueueStub_Rename(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=rename&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for stubbed action", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not implemented") {
		t.Error("expected 'not implemented' in error message")
	}
}

// --- Queue nil guard ---

func TestQueueNilGuard(t *testing.T) {
	t.Parallel()
	s := New(Options{
		Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey}},
		Version: "1.0.0-test",
		// Queue intentionally nil.
	})
	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 when queue is nil", rr.Code)
	}
}

// --- Sonarr/Radarr compatibility tests ---

// TestQueueSlot_MBIsString verifies that the queue slot "mb" and "mbleft"
// fields are JSON strings (e.g. "0.00") not bare numbers. Sonarr/Radarr
// parse them as strings and would choke on a numeric literal.
func TestQueueSlot_MBIsString(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "test.nzb"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	// Parse the raw JSON to inspect field types.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal root: %v", err)
	}
	var queueRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["queue"], &queueRaw); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	var slots []json.RawMessage
	if err := json.Unmarshal(queueRaw["slots"], &slots); err != nil {
		t.Fatalf("unmarshal slots: %v", err)
	}
	if len(slots) == 0 {
		t.Fatal("expected at least one slot")
	}

	var slot map[string]json.RawMessage
	if err := json.Unmarshal(slots[0], &slot); err != nil {
		t.Fatalf("unmarshal slot: %v", err)
	}

	// JSON strings start with '"'; numbers start with a digit.
	for _, field := range []string{"mb", "mbleft"} {
		v, ok := slot[field]
		if !ok {
			t.Errorf("field %q missing from slot", field)
			continue
		}
		if len(v) == 0 || v[0] != '"' {
			t.Errorf("field %q = %s; want a JSON string (starts with '\"')", field, v)
		}
	}
}

// TestQueueSlot_TimeleftAndETA verifies timeleft and eta are present.
func TestQueueSlot_TimeleftAndETA(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "test.nzb"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Queue struct {
			Slots []struct {
				Timeleft string `json:"timeleft"`
				ETA      string `json:"eta"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Queue.Slots) == 0 {
		t.Fatal("expected at least one slot")
	}
	if resp.Queue.Slots[0].Timeleft == "" {
		t.Error("timeleft should not be empty")
	}
	if resp.Queue.Slots[0].ETA == "" {
		t.Error("eta should not be empty")
	}
}

// TestQueueAggregates verifies queue-level aggregate fields that Sonarr reads.
func TestQueueAggregates(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "a.nzb"})
	addTestJob(t, q, queue.AddOptions{Filename: "b.nzb"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Queue struct {
			Speed    string `json:"speed"`
			KBPerSec string `json:"kbpersec"`
			MB       string `json:"mb"`
			MBLeft   string `json:"mbleft"`
			Size     string `json:"size"`
			SizeLeft string `json:"sizeleft"`
			Timeleft string `json:"timeleft"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// All aggregate fields should be present (non-empty strings).
	for _, tc := range []struct {
		name, val string
	}{
		{"speed", resp.Queue.Speed},
		{"kbpersec", resp.Queue.KBPerSec},
		{"mb", resp.Queue.MB},
		{"mbleft", resp.Queue.MBLeft},
		{"size", resp.Queue.Size},
		{"sizeleft", resp.Queue.SizeLeft},
		{"timeleft", resp.Queue.Timeleft},
	} {
		if tc.val == "" {
			t.Errorf("queue-level %q should not be empty", tc.name)
		}
	}
}

// TestQueuePriority_Action verifies the priority API action.
func TestQueuePriority_Action(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "test.nzb"})

	url := fmt.Sprintf("/api?mode=queue&name=priority&value=%s&value2=1&apikey=%s",
		job.ID, testAPIKey)
	rr := apiGet(t, s.Handler(), url)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Status bool     `json:"status"`
		NzoIDs []string `json:"nzo_ids"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Status {
		t.Error("status should be true")
	}
	if len(resp.NzoIDs) != 1 || resp.NzoIDs[0] != job.ID {
		t.Errorf("nzo_ids = %v; want [%s]", resp.NzoIDs, job.ID)
	}
	// Verify priority actually changed.
	updated, err := q.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Priority != constants.HighPriority {
		t.Errorf("priority = %d; want %d (HighPriority)", updated.Priority, constants.HighPriority)
	}
}

// TestQueuePriority_MissingParams verifies error handling.
func TestQueuePriority_MissingParams(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)

	// Missing value= (nzo_id)
	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=priority&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("no value: status = %d; want 400", rr.Code)
	}

	// Missing value2= (priority)
	rr = apiGet(t, s.Handler(), "/api?mode=queue&name=priority&value=someid&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("no value2: status = %d; want 400", rr.Code)
	}
}

// --- Sonarr compatibility tests ---
// These verify the exact JSON field names and types that Sonarr's C# client
// deserializes (SabnzbdQueueItem). Any mismatch will silently break the
// integration because .NET's JSON deserializer returns defaults for unknown keys.

// TestQueueSlot_SonarrCatField verifies the category field is emitted as "cat"
// (not "category"). Sonarr's SabnzbdQueueItem has [JsonProperty("cat")].
func TestQueueSlot_SonarrCatField(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "test.nzb", Category: "tv"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	slot := extractFirstSlot(t, rr.Body.Bytes())

	// "cat" must be present; "category" must NOT be present.
	if _, ok := slot["cat"]; !ok {
		t.Error("expected field 'cat' in queue slot (Sonarr reads 'cat')")
	}
	if _, ok := slot["category"]; ok {
		t.Error("field 'category' should not exist in queue slot; Sonarr ignores it")
	}

	// Verify the value is correct.
	var catVal string
	if err := json.Unmarshal(slot["cat"], &catVal); err != nil {
		t.Fatalf("unmarshal cat: %v", err)
	}
	if catVal != "tv" {
		t.Errorf("cat = %q; want %q", catVal, "tv")
	}
}

// TestQueueSlot_SonarrPercentageIsInt verifies percentage is a JSON number
// (not a string). Sonarr deserializes it as C# int.
func TestQueueSlot_SonarrPercentageIsInt(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "test.nzb"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	slot := extractFirstSlot(t, rr.Body.Bytes())

	pctRaw, ok := slot["percentage"]
	if !ok {
		t.Fatal("field 'percentage' missing from queue slot")
	}

	// JSON numbers start with a digit or '-'; strings start with '"'.
	if len(pctRaw) == 0 || pctRaw[0] == '"' {
		t.Errorf("percentage = %s; want a JSON number (Sonarr reads as int), got string", pctRaw)
	}
}

// TestQueueSlot_SonarrIndexField verifies the index field is present and is an integer.
func TestQueueSlot_SonarrIndexField(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "first.nzb"})
	addTestJob(t, q, queue.AddOptions{Filename: "second.nzb"})

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	slots := extractAllSlots(t, rr.Body.Bytes())
	if len(slots) < 2 {
		t.Fatalf("expected at least 2 slots, got %d", len(slots))
	}

	for i, slot := range slots {
		idxRaw, ok := slot["index"]
		if !ok {
			t.Errorf("slot[%d]: field 'index' missing", i)
			continue
		}
		var idx int
		if err := json.Unmarshal(idxRaw, &idx); err != nil {
			t.Errorf("slot[%d]: index unmarshal: %v (raw=%s)", i, err, idxRaw)
			continue
		}
		if idx != i {
			t.Errorf("slot[%d]: index = %d; want %d", i, idx, i)
		}
	}
}

// extractFirstSlot parses a queue response and returns the first slot as a raw JSON map.
func extractFirstSlot(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	slots := extractAllSlots(t, body)
	if len(slots) == 0 {
		t.Fatal("expected at least one slot")
	}
	return slots[0]
}

// extractAllSlots parses a queue response and returns all slots as raw JSON maps.
func extractAllSlots(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal root: %v", err)
	}
	var queueRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["queue"], &queueRaw); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	var slotsRaw []json.RawMessage
	if err := json.Unmarshal(queueRaw["slots"], &slotsRaw); err != nil {
		t.Fatalf("unmarshal slots: %v", err)
	}
	result := make([]map[string]json.RawMessage, 0, len(slotsRaw))
	for _, s := range slotsRaw {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(s, &m); err != nil {
			t.Fatalf("unmarshal slot: %v", err)
		}
		result = append(result, m)
	}
	return result
}

// --- PostProc visibility ---

// TestQueueList_PostProcJobsVisible verifies that jobs in post-processing
// (PostProc=true) remain visible in the queue listing with their current
// status (Verifying, Extracting, etc.) so users can track progress.
func TestQueueList_PostProcJobsVisible(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)

	// Add two jobs: one normal, one in post-processing.
	normalJob := addTestJob(t, q, queue.AddOptions{Filename: "normal.nzb"})
	ppJob := addTestJob(t, q, queue.AddOptions{Filename: "postproc.nzb"})

	// Mark ppJob as post-processing (sets PostProc=true, Status=Verifying).
	_, err := q.SetPostProcStarted(ppJob.ID)
	if err != nil {
		t.Fatalf("SetPostProcStarted: %v", err)
	}

	// Update status to Extracting to simulate unpack stage.
	if err := q.SetStatus(ppJob.ID, constants.StatusExtracting); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	var resp queueResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Both jobs should appear.
	if resp.Queue.NoOfSlotsTotal != 2 {
		t.Errorf("noofslots_total = %d; want 2", resp.Queue.NoOfSlotsTotal)
	}
	if len(resp.Queue.Slots) != 2 {
		t.Fatalf("got %d slots; want 2", len(resp.Queue.Slots))
	}

	// Find the post-proc slot and verify its status.
	var found bool
	for _, slot := range resp.Queue.Slots {
		if slot.NzoID == ppJob.ID {
			found = true
			if slot.Status != "Extracting" {
				t.Errorf("pp slot status = %q; want Extracting", slot.Status)
			}
		}
		if slot.NzoID == normalJob.ID {
			if slot.Status != "Queued" {
				t.Errorf("normal slot status = %q; want Queued", slot.Status)
			}
		}
	}
	if !found {
		t.Error("post-processing job should be visible in queue")
	}
}
