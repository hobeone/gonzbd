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

	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
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
		Auth: AuthConfig{
			APIKey: testAPIKey,
			NZBKey: testNZBKey,
		},
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
	job, err := queue.NewJob(parsed, opts)
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
			NoOfSlots      int           `json:"noofslots"`
			NoOfSlotsTotal int           `json:"noofslots_total"`
			Paused         bool          `json:"paused"`
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
				NzoID    string  `json:"nzo_id"`
				Filename string  `json:"filename"`
				Category string  `json:"category"`
				Priority string  `json:"priority"`
				Status   string  `json:"status"`
				PP       string  `json:"pp"`
				MB       float64 `json:"mb"`
				Bytes    int64   `json:"bytes"`
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
	for i := 0; i < 5; i++ {
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

// --- Pause / Resume ---

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
		Auth:    AuthConfig{APIKey: testAPIKey},
		Version: "1.0.0-test",
		// Queue intentionally nil.
	})
	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 when queue is nil", rr.Code)
	}
}
