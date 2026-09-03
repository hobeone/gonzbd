package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/app"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/urlgrabber"
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

func convertQueueManifestToJobManifest(qm *queue.Manifest) *job.Manifest {
	if qm == nil {
		return nil
	}
	var jfiles []job.JobFile
	for fi := range qm.NumFiles() {
		lo, hi := qm.FileRange(fi)
		var arts []job.JobArticle
		for ai := lo; ai < hi; ai++ {
			arts = append(arts, job.JobArticle{
				ID:     qm.ArticleID(ai),
				Bytes:  qm.ArticleBytes(ai),
				Number: ai + 1,
			})
		}
		jfiles = append(jfiles, job.JobFile{
			Subject:        qm.FileSubject(fi),
			Bytes:          qm.FileBytes(fi),
			Articles:       arts,
			IsPar2Recovery: qm.FileIsPar2Recovery(fi),
		})
	}
	return job.NewManifest(jfiles)
}

func newTestQueueBridgeWithApp(t *testing.T, cfg *config.Config, makeApp func(disp *dispatch.Dispatcher, q *queue.Queue) AppServices) (*Server, *queue.Queue) {
	t.Helper()
	disp := newTestAPIDispatcher(t)
	var q *queue.Queue
	q = queue.New(queue.WithHooks(queue.Hooks{
		OnAdd: func(qj *queue.Job, m *queue.Manifest) {
			j := job.New(qj.ID, qj.Name, job.Policy{})
			jm := convertQueueManifestToJobManifest(m)
			if jm != nil {
				_ = j.AttachContent(jm)
			}
			_ = disp.Add(j, dispatch.Header{
				Name:     qj.Name,
				Filename: qj.Filename,
				Category: qj.Category,
				Priority: int(qj.Priority),
				Bytes:    qj.TotalBytes(),
				Warning:  qj.Warning,
				Script:   qj.Script,
				Password: qj.Password,
				PP:       qj.PP,
			})
			_ = j.BeginAttempt(time.Now().UTC())
			disp.Tick(context.Background())
			if qj.Status == constants.StatusPaused {
				_ = disp.PauseJob(qj.ID)
				_ = disp.Yielded(qj.ID)
			}
		},
		OnRemove: func(id string) {
			_ = disp.Remove(context.Background(), id)
		},
		OnPause: func(id string) {
			_ = disp.PauseJob(id)
			_ = disp.Yielded(id)
		},
		OnResume: func(id string) {
			_ = disp.ResumeJob(id)
			disp.Tick(context.Background())
		},
		OnPauseAll: func() {
			disp.Pause()
			for _, row := range disp.List() {
				_ = disp.Yielded(row.ID)
			}
		},
		OnResumeAll: func() {
			disp.Resume()
			disp.Tick(context.Background())
		},
		OnStatus: func(id string, status constants.Status) {
			j, ok := disp.Job(id)
			if !ok {
				return
			}
			switch status {
			case constants.StatusDownloading:
				disp.Tick(context.Background())
			case constants.StatusVerifying:
				_ = j.Transition(job.Assessing)
				disp.Tick(context.Background())
			case constants.StatusRepairing:
				_ = j.Transition(job.Repairing)
				disp.Tick(context.Background())
			case constants.StatusExtracting:
				if j.State().State != job.Extracting {
					if j.State().State == job.Fetching {
						_ = j.Transition(job.Assessing)
					}
					_ = j.SetNext(job.Extracting)
					_, _ = j.Cross(job.Extracting)
				}
				disp.Tick(context.Background())
			}
		},
	}))
	var application AppServices
	if makeApp != nil {
		application = makeApp(disp, q)
	} else {
		application = apitest.NopApp{Dispatcher: disp, Queue: q}
	}
	s := New(Options{
		Config:     cfg,
		Version:    "1.0.0-test",
		Dispatcher: disp,
		Queue:      q,
		App:        application,
	})
	return s, q
}

func newTestQueueBridge(t *testing.T, cfg *config.Config, speed float64) (*Server, *queue.Queue) {
	return newTestQueueBridgeWithApp(t, cfg, func(disp *dispatch.Dispatcher, q *queue.Queue) AppServices {
		return apitest.NopApp{Dispatcher: disp, Queue: q, SpeedVal: speed}
	})
}

// testQueueServer builds a Server wired with a fresh queue (and no history).
func testQueueServer(t *testing.T) (*Server, *queue.Queue) {
	t.Helper()
	return newTestQueueBridge(t, &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}}, 0)
}

// testQueueServerWithRoot builds a Server wired with a fresh queue and root
// configured as DownloadDir, so that browse/addlocalfile's SEC-6 allowlist
// check permits paths under root.
func testQueueServerWithRoot(t *testing.T, root string) (*Server, *queue.Queue) {
	t.Helper()
	return newTestQueueBridge(t, &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey, DownloadDir: root}}, 0)
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

func TestQueueDefault_StatusFilter(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "movie.nzb", Category: "movies"})
	paused := addTestJob(t, q, queue.AddOptions{Filename: "show.nzb", Category: "tv"})
	if err := q.Pause(paused.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Filter by status=Paused → expect exactly the paused job, not the
	// unfiltered total of 2.
	rr := apiGet(t, s.Handler(), "/api?mode=queue&status=Paused&apikey="+testAPIKey)
	var resp struct {
		Queue struct {
			NoOfSlots int `json:"noofslots"`
			Slots     []struct {
				NzoID string `json:"nzo_id"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Queue.NoOfSlots != 1 {
		t.Fatalf("status-filtered noofslots = %d; want 1", resp.Queue.NoOfSlots)
	}
	if len(resp.Queue.Slots) != 1 || resp.Queue.Slots[0].NzoID != paused.ID {
		t.Errorf("status-filtered slots = %+v; want only paused job %q", resp.Queue.Slots, paused.ID)
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
	j := q.SnapshotJob(job.ID)
	if j == nil {
		t.Fatalf("SnapshotJob(%s): job not in queue", job.ID)
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

// --- Issue #3: in-progress state fields ---

// testQueueServerWithSpeed mirrors testQueueServer but plumbs a configurable
// download speed through mockApp so ETA computation can be exercised.
func testQueueServerWithSpeed(t *testing.T, speed float64) (*Server, *queue.Queue) {
	t.Helper()
	return newTestQueueBridge(t, &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}}, speed)
}

// makeLargeTestNZB builds an NZB with a single file split across numSegs
// segments of 1 MiB each. Used to exercise ETA arithmetic where the default
// 1024-byte fixture rounds to zero seconds. Segment size stays under the
// parser's per-article 8 MiB cap.
func makeLargeTestNZB(t *testing.T, numSegs int) []byte {
	t.Helper()
	const segBytes = 1024 * 1024 // 1 MiB; well below maxArticleSize (8 MiB)
	var segs strings.Builder
	for i := 1; i <= numSegs; i++ {
		fmt.Fprintf(&segs, `      <segment bytes="%d" number="%d">big-article-%03d@example.com</segment>`+"\n",
			segBytes, i, i)
	}
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test@example.com" date="1609459200" subject="big.bin (1/%d)">
    <groups>
      <group>alt.binaries.test</group>
    </groups>
    <segments>
%s    </segments>
  </file>
</nzb>`, numSegs, segs.String())
}

// addLargeTestJob enqueues a job with numSegs × 1 MiB segments.
func addLargeTestJob(t *testing.T, q *queue.Queue, numSegs int) *queue.Job {
	t.Helper()
	parsed, err := nzb.Parse(bytes.NewReader(makeLargeTestNZB(t, numSegs)))
	if err != nil {
		t.Fatalf("parse large NZB: %v", err)
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "big.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	return job
}

func TestQueueList_InProgressFields_Downloading(t *testing.T) {
	t.Parallel()
	// 1 MiB/s and a 10-segment (10 MiB) job → ETA = 10 s.
	const speed = 1024.0 * 1024.0
	s, q := testQueueServerWithSpeed(t, speed)
	job := addLargeTestJob(t, q, 10)
	if err := q.SetStatusIf(job.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Queue struct {
			Slots []struct {
				Status            string `json:"status"`
				CurrentStage      string `json:"current_stage"`
				ArticlesRemaining int    `json:"articles_remaining"`
				ETASeconds        int    `json:"eta_seconds"`
				CurrentFile       string `json:"current_file"`
				RemainingBytes    int64  `json:"remaining_bytes"`
				Timeleft          string `json:"timeleft"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Queue.Slots) != 1 {
		t.Fatalf("slots len = %d; want 1", len(resp.Queue.Slots))
	}
	slot := resp.Queue.Slots[0]
	if slot.CurrentStage != "download" {
		t.Errorf("current_stage = %q; want download", slot.CurrentStage)
	}
	if slot.ArticlesRemaining <= 0 {
		t.Errorf("articles_remaining = %d; want > 0 for in-progress job", slot.ArticlesRemaining)
	}
	wantETA := int(float64(slot.RemainingBytes) / speed)
	if slot.ETASeconds != wantETA {
		t.Errorf("eta_seconds = %d; want %d (speed=%.0f, remaining=%d)",
			slot.ETASeconds, wantETA, speed, slot.RemainingBytes)
	}
	if wantETA > 0 && slot.Timeleft == "0:00:00" {
		t.Errorf("timeleft should be populated when ETA is computable, got %q", slot.Timeleft)
	}
	if slot.CurrentFile == "" {
		t.Errorf("current_file should be the first incomplete file's subject, got empty")
	}
}

func TestQueueList_InProgressFields_PausedSuppressesETA(t *testing.T) {
	t.Parallel()
	const speed = 1024.0 * 1024.0
	s, q := testQueueServerWithSpeed(t, speed)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
	if err := q.SetStatusIf(job.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}
	q.PauseAll()

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	var resp struct {
		Queue struct {
			Slots []struct {
				CurrentStage string `json:"current_stage"`
				ETASeconds   int    `json:"eta_seconds"`
				Timeleft     string `json:"timeleft"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Queue.Slots) != 1 {
		t.Fatalf("slots len = %d; want 1", len(resp.Queue.Slots))
	}
	slot := resp.Queue.Slots[0]
	if slot.ETASeconds != 0 {
		t.Errorf("eta_seconds = %d; want 0 when paused", slot.ETASeconds)
	}
	if slot.Timeleft != "0:00:00" {
		t.Errorf("timeleft = %q; want 0:00:00 when paused", slot.Timeleft)
	}
	// current_stage reflects the paused override (not "download").
	if slot.CurrentStage != "paused" {
		t.Errorf("current_stage = %q; want paused", slot.CurrentStage)
	}
}

func TestQueueList_InProgressFields_NoSpeedSuppressesETA(t *testing.T) {
	t.Parallel()
	// Speed below noiseFloor (1 KiB/s) — ETA should be zero, not absurd.
	s, q := testQueueServerWithSpeed(t, 100.0)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
	if err := q.SetStatusIf(job.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	var resp struct {
		Queue struct {
			Slots []struct {
				ETASeconds int `json:"eta_seconds"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Queue.Slots[0].ETASeconds != 0 {
		t.Errorf("eta_seconds = %d; want 0 when speed below noise floor", resp.Queue.Slots[0].ETASeconds)
	}
}

func TestQueueList_StageMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status    constants.Status
		wantStage string
	}{
		{constants.StatusDownloading, "download"},
		{constants.StatusVerifying, "repair"},
		{constants.StatusRepairing, "repair"},
		{constants.StatusExtracting, "unpack"},
		{constants.StatusMoving, "move"},
		{constants.StatusQueued, "queued"},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			t.Parallel()
			got := stageFromStatus(tt.status)
			if got != tt.wantStage {
				t.Errorf("stageFromStatus(%q) = %q; want %q", tt.status, got, tt.wantStage)
			}
		})
	}
}

// --- Issue #4: per-file detail endpoint ---

func TestQueueDetail_FilesIncludedWhenRequested(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addLargeTestJob(t, q, 4) // 4 segments × 1 MiB

	// Mark 2 of 4 articles done so file shows partial progress.
	jobInternal := q.SnapshotJob(job.ID)
	if jobInternal == nil {
		t.Fatalf("SnapshotJob(%s): job not in queue", job.ID)
	}
	dispJob, ok := s.dispatcher.Job(job.ID)
	if !ok {
		t.Fatalf("dispatcher.Job(%s): not in dispatcher", job.ID)
	}
	m := mustManifest(t, dispJob)
	if m.NumFiles() != 1 {
		t.Fatalf("expected 1 file, got %d", m.NumFiles())
	}
	doneIDs := []string{m.ArticleID(0), m.ArticleID(1)}
	ackDone(t, s.dispatcher, job.ID, doneIDs...)

	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&nzo_id="+job.ID+"&files=1&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	var resp struct {
		Queue struct {
			Slots []struct {
				NzoID string `json:"nzo_id"`
				Files []struct {
					Name            string `json:"name"`
					Bytes           int64  `json:"bytes"`
					BytesDownloaded int64  `json:"bytes_downloaded"`
					State           string `json:"state"`
				} `json:"files"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Queue.Slots) != 1 {
		t.Fatalf("slots len = %d; want 1", len(resp.Queue.Slots))
	}
	slot := resp.Queue.Slots[0]
	if slot.NzoID != job.ID {
		t.Errorf("nzo_id = %q; want %q", slot.NzoID, job.ID)
	}
	if len(slot.Files) != 1 {
		t.Fatalf("files len = %d; want 1", len(slot.Files))
	}
	f := slot.Files[0]
	if f.Bytes != 4*1024*1024 {
		t.Errorf("file bytes = %d; want %d (4 MiB)", f.Bytes, 4*1024*1024)
	}
	if f.BytesDownloaded != 2*1024*1024 {
		t.Errorf("file bytes_downloaded = %d; want %d (2 MiB)", f.BytesDownloaded, 2*1024*1024)
	}
	if f.State != "downloading" {
		t.Errorf("file state = %q; want downloading", f.State)
	}
	if f.Name == "" {
		t.Errorf("file name should not be empty")
	}
}

func TestQueueDetail_DefaultListingExcludesFiles(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addLargeTestJob(t, q, 2)

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	// The default listing must not include per-file arrays — the JSON
	// must omit the "files" key (omitempty) so payloads stay small.
	body := rr.Body.String()
	if strings.Contains(body, `"files":`) {
		t.Errorf("default listing must omit files key, body contains it:\n%s", body)
	}
}

func TestQueueDetail_UnknownNzoIDReturnsEmpty(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&nzo_id=does-not-exist&files=1&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Queue struct {
			Slots []any `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Queue.Slots) != 0 {
		t.Errorf("slots len = %d; want 0 for unknown nzo_id", len(resp.Queue.Slots))
	}
}

func TestQueueDetail_FileStateClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, disp *dispatch.Dispatcher, jobID string, j *job.Job)
		want  string
	}{
		{"queued", func(*testing.T, *dispatch.Dispatcher, string, *job.Job) {}, "queued"},
		{"downloading", func(t *testing.T, disp *dispatch.Dispatcher, jobID string, j *job.Job) {
			ackDone(t, disp, jobID, "a0@t")
		}, "downloading"},
		{"done", func(t *testing.T, disp *dispatch.Dispatcher, jobID string, j *job.Job) {
			ackDone(t, disp, jobID, "a0@t", "a1@t")
			if err := j.MarkFileComplete(0); err != nil {
				t.Fatalf("MarkFileComplete: %v", err)
			}
		}, "done"},
		{"failed", func(t *testing.T, disp *dispatch.Dispatcher, jobID string, j *job.Job) {
			ackDone(t, disp, jobID, "a0@t")
			ackFailed(t, disp, jobID, "a1@t")
			if err := j.MarkFileComplete(0); err != nil {
				t.Fatalf("MarkFileComplete: %v", err)
			}
		}, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			disp := newTestAPIDispatcher(t)
			m := makeJobManifest(t, []string{"f"}, []int64{1000}, [][]int64{{500, 500}}, [][]string{{"a0@t", "a1@t"}})
			j := job.New("j_class", "f.nzb", job.Policy{})
			_ = j.AttachContent(m)
			_ = disp.Add(j, dispatch.Header{Name: "f.nzb", Bytes: 1000})

			tt.setup(t, disp, "j_class", j)
			if got := fileState(m, j.Progress(), 0); got != tt.want {
				t.Errorf("fileState = %q; want %q", got, tt.want)
			}
		})
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
	j := q.SnapshotJob(job.ID)
	if j == nil {
		t.Fatalf("SnapshotJob(%s): job not in queue", job.ID)
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
	j := q.SnapshotJob(job.ID)
	if j == nil {
		t.Fatalf("SnapshotJob(%s): job not in queue", job.ID)
	}
	if j.Status != constants.StatusDownloading {
		t.Errorf("status = %q; want Downloading", j.Status)
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
	job := q.SnapshotJob(resp.NzoIDs[0])
	if job == nil {
		t.Fatalf("SnapshotJob(%q)", resp.NzoIDs[0])
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
	dir := t.TempDir()
	s, q := testQueueServerWithRoot(t, dir)

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

func TestModeAddLocalFile_RejectsPathTraversal(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)

	// Test that absolute paths with ".." are rejected. filepath.Clean
	// resolves ".." components on absolute paths, but the explicit
	// strings.Contains(clean, "..") guard provides defense-in-depth.
	// Either way, the request must fail (either the ".." check fires,
	// or the resolved path doesn't exist/isn't a valid NZB).
	tests := []struct {
		name    string
		path    string
		wantSub string // substring expected in body; "" means any 400 is fine
	}{
		{"traversal up from root", "/../../../etc/passwd", ""},
		{"traversal mid-path", "/safe/../../../etc/passwd", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rr := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name="+tt.path+"&apikey="+testAPIKey)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("status = %d; want 400 for path %q (body: %s)", rr.Code, tt.path, rr.Body.String())
			}
		})
	}
}

func TestModeAddLocalFile_RequiresAdmin(t *testing.T) {
	t.Parallel()
	s := testServer()
	// testNZBKey is LevelProtected only (upload-only key) — must now be
	// rejected since addlocalfile is LevelAdmin.
	rr := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name=/tmp/x.nzb&apikey="+testNZBKey)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (nzbkey is not admin)", rr.Code)
	}
}

func TestModeAddLocalFile_RejectsPathOutsideConfiguredRoots(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	// testServerWithConfig doesn't wire a Queue, and requireQueue is checked
	// before the path/roots validation in modeAddLocalFile -- use
	// testQueueServerWithRoot so the request reaches the SEC-6 check we're
	// actually exercising here instead of failing earlier with 500.
	s, _ := testQueueServerWithRoot(t, downloadDir)

	rr := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name=/etc/hosts&apikey="+testAPIKey)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (path outside configured roots)", rr.Code)
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

// When the URL is missing, addurl should reject with 400 specifically —
// the grabber-wired check happens first, so a Grabber must be wired for
// this test to reach the URL param check rather than short-circuiting on
// the nil-grabber path (see TestAddURL_NoGrabber for that path).
func TestAddURL_MissingURL(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	s.grabber = urlgrabber.New(urlgrabber.Config{AllowPrivateIPs: true}, mockGrabberHandler{id: "nzo_test1"})

	rr := apiGet(t, s.Handler(), "/api?mode=addurl&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for missing url", rr.Code)
	}
}

// --- Stub actions ---

func TestQueueRename_MissingValue(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=rename&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for missing value", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "missing value") {
		t.Error("expected 'missing value' in error message")
	}
}

func TestQueueRename_Success(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "original.nzb"})

	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=rename&value="+job.ID+"&value2=NewName&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	updated := q.SnapshotJob(job.ID)
	if updated == nil {
		t.Fatalf("SnapshotJob(%s): job not in queue", job.ID)
	}
	if updated.Name != "NewName" {
		t.Errorf("Name = %q; want %q", updated.Name, "NewName")
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
	updated := q.SnapshotJob(job.ID)
	if updated == nil {
		t.Fatalf("SnapshotJob(%s): job not in queue", job.ID)
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
	_ = q.SetStatus(ppJob.ID, constants.StatusDownloading)
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
			if slot.Status != "Downloading" {
				t.Errorf("normal slot status = %q; want Downloading", slot.Status)
			}
		}
	}
	if !found {
		t.Error("post-processing job should be visible in queue")
	}
}

// --- change_opts (PP change) ---

func TestQueueChangeOpts_Success(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb", PP: 1})

	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_opts&value="+job.ID+"&value2=3&apikey="+testAPIKey)
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

	// Verify the queue was updated.
	got := q.SnapshotJob(job.ID)
	if got == nil {
		t.Fatalf("SnapshotJob(%s): job not in queue", job.ID)
	}
	if got.PP != 3 {
		t.Errorf("PP = %d; want 3", got.PP)
	}
}

func TestQueueChangeOpts_MissingValue(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_opts&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
}

func TestQueueChangeOpts_MissingValue2(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_opts&value="+job.ID+"&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
}

func TestQueueChangeOpts_InvalidPP(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})
	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_opts&value="+job.ID+"&value2=5&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for out-of-range PP", rr.Code)
	}
}

func TestQueueChangeOpts_UnknownJob(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)
	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_opts&value=nonexistent&value2=2&apikey="+testAPIKey)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 for unknown job", rr.Code)
	}
}

// --- change_cat ---

// testQueueServerWithCats builds a Server wired with the given categories.
func testQueueServerWithCats(t *testing.T, cats []config.CategoryConfig) (*Server, *queue.Queue) {
	t.Helper()
	cfg := &config.Config{
		General:    config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey},
		Categories: cats,
	}
	return newTestQueueBridge(t, cfg, 0)
}

func TestQueueChangeCat_Success(t *testing.T) {
	t.Parallel()
	cats := []config.CategoryConfig{
		{Name: "Default", PP: 3, Script: "", Priority: int(constants.NormalPriority)},
		{Name: "movies", PP: 2, Script: "movies.sh", Priority: int(constants.HighPriority)},
	}
	s, q := testQueueServerWithCats(t, cats)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb", Category: "Default"})

	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_cat&value="+job.ID+"&value2=movies&apikey="+testAPIKey)
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

	// Verify queue was updated with the new category's inherited settings.
	got := q.SnapshotJob(job.ID)
	if got == nil {
		t.Fatalf("SnapshotJob(%s): job not in queue", job.ID)
	}
	if got.Category != "movies" {
		t.Errorf("Category = %q; want movies", got.Category)
	}
	if got.PP != 2 {
		t.Errorf("PP = %d; want 2 (from movies category)", got.PP)
	}
	if got.Script != "movies.sh" {
		t.Errorf("Script = %q; want movies.sh", got.Script)
	}
	if got.Priority != constants.HighPriority {
		t.Errorf("Priority = %d; want HighPriority", got.Priority)
	}
}

func TestQueueChangeCat_EmptyValue2FallsBackToDefault(t *testing.T) {
	t.Parallel()
	cats := []config.CategoryConfig{
		{Name: "Default", PP: 3, Script: "", Priority: int(constants.NormalPriority)},
		{Name: "movies", PP: 2, Script: "movies.sh", Priority: int(constants.HighPriority)},
	}
	s, q := testQueueServerWithCats(t, cats)
	// Start with "movies" category.
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb", Category: "movies", PP: 2})

	// Send empty value2 → should reset to Default.
	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_cat&value="+job.ID+"&value2=&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	got := q.SnapshotJob(job.ID)
	if got == nil {
		t.Fatalf("SnapshotJob(%s): job not in queue", job.ID)
	}
	if got.Category != "Default" {
		t.Errorf("Category = %q; want Default", got.Category)
	}
	if got.PP != 3 {
		t.Errorf("PP = %d; want 3 (Default PP)", got.PP)
	}
}

func TestQueueChangeCat_MissingValue(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServerWithCats(t, nil)
	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_cat&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 when value (nzo_id) is missing", rr.Code)
	}
}

func TestQueueChangeCat_UnknownJob(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServerWithCats(t, nil)
	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_cat&value=nonexistent&value2=movies&apikey="+testAPIKey)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 for unknown job", rr.Code)
	}
}

// TestQueueChangeCat_ListReflectsNewCategory verifies that the queue listing
// returns the updated category after a successful change_cat call.
func TestQueueChangeCat_ListReflectsNewCategory(t *testing.T) {
	t.Parallel()
	cats := []config.CategoryConfig{
		{Name: "Default", PP: 3, Script: "", Priority: int(constants.NormalPriority)},
		{Name: "tv", PP: 1, Script: "", Priority: int(constants.LowPriority)},
	}
	s, q := testQueueServerWithCats(t, cats)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb", Category: "Default"})

	// Change to "tv".
	rr := apiGet(t, s.Handler(),
		"/api?mode=queue&name=change_cat&value="+job.ID+"&value2=tv&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("change_cat status = %d", rr.Code)
	}

	// Confirm via queue listing.
	rr2 := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr2.Code != http.StatusOK {
		t.Fatalf("queue list status = %d", rr2.Code)
	}
	var resp struct {
		Queue struct {
			Slots []struct {
				NzoID    string `json:"nzo_id"`
				Category string `json:"cat"`
				PP       string `json:"pp"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Queue.Slots) != 1 {
		t.Fatalf("slots = %d; want 1", len(resp.Queue.Slots))
	}
	slot := resp.Queue.Slots[0]
	if slot.Category != "tv" {
		t.Errorf("cat = %q; want tv", slot.Category)
	}
	if slot.PP != "1" {
		t.Errorf("pp = %q; want 1 (from tv category)", slot.PP)
	}
}

// TestQueuePause_UnknownIDDoesNotPanic exercises the nil branch of
// queueSetPaused's job-lookup: pausing/resuming an ID with no matching job
// must be handled gracefully (loop just skips logging), not dereference the
// nil *Job for the log line's job.Name.
func TestQueuePause_UnknownIDDoesNotPanic(t *testing.T) {
	t.Parallel()
	s, _ := testQueueServer(t)

	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=pause&value=nonexistent_id&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	rr = apiGet(t, s.Handler(), "/api?mode=queue&name=resume&value=nonexistent_id&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
}

// TestQueueSetPaused_ConcurrentMutationRace proves that the pause/resume
// handlers (queuePauseJobs/queueResumeJobs -> queueSetPaused) do not read
// mutable *Job fields (job.Name) off a live pointer while the download
// pipeline concurrently mutates the same job. TRACE-1: before the fix,
// queueSetPaused called s.queue.Get(id) (which hands back the live *Job)
// and then read job.Name with no lock held, while a concurrent SetName
// call mutates job.Name under the queue's write lock. That is a data race
// caught by `go test -race`.
//
// RACE-ONLY: this test has no assertion of its own — its only signal is
// `go test -race` catching the data race above. It relies entirely on the
// project's mandatory `-race` gate; without it, this test always passes
// regardless of whether the race exists.
func TestQueueSetPaused_ConcurrentMutationRace(t *testing.T) {
	s, q := testQueueServer(t)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb"})

	const iterations = 300
	stop := make(chan struct{})
	var mutatorWG sync.WaitGroup

	// Simulates the download pipeline concurrently renaming the job
	// (e.g. on dupe-resolution or a rename triggered elsewhere). Runs
	// until told to stop, not for a fixed iteration count, so it stays
	// active for the entire HTTP loop below — a mutator that races to
	// finish its own fixed loop can exit before the (slower) HTTP loop
	// reaches its later iterations, leaving no concurrent mutation to
	// detect for the remainder of the run and weakening this as a
	// regression proof.
	mutatorWG.Go(func() {
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				_ = q.SetName(job.ID, fmt.Sprintf("job-%d.nzb", i))
				i++
			}
		}
	})

	// Simulates a client repeatedly pausing/resuming the job, which
	// (pre-fix) read job.Name off the live pointer for the log line.
	for range iterations {
		apiGet(t, s.Handler(), "/api?mode=queue&name=pause&value="+job.ID+"&apikey="+testAPIKey)
		apiGet(t, s.Handler(), "/api?mode=queue&name=resume&value="+job.ID+"&apikey="+testAPIKey)
	}

	close(stop)
	mutatorWG.Wait()
}

// TestQueueChangeCat_ConcurrentMutationRace proves that queueChangeCat does
// not read job.Category/PP/Script/Priority off a live pointer. TRACE-1:
// before the fix it called s.queue.Get(nzoID) and read those fields
// unsynchronized after the call returned, while a concurrent SetPP call
// (simulating another actor mutating job state, e.g. change_opts from a
// second client) mutates job.PP under the queue's write lock.
//
// RACE-ONLY: this test has no assertion of its own — its only signal is
// `go test -race` catching the data race above. It relies entirely on the
// project's mandatory `-race` gate; without it, this test always passes
// regardless of whether the race exists.
func TestQueueChangeCat_ConcurrentMutationRace(t *testing.T) {
	cats := []config.CategoryConfig{
		{Name: "Default", PP: 3, Script: "", Priority: int(constants.NormalPriority)},
		{Name: "movies", PP: 2, Script: "movies.sh", Priority: int(constants.HighPriority)},
	}
	s, q := testQueueServerWithCats(t, cats)
	job := addTestJob(t, q, queue.AddOptions{Filename: "job.nzb", Category: "Default"})

	const iterations = 300
	stop := make(chan struct{})
	var mutatorWG sync.WaitGroup

	// Runs until told to stop (see TestQueueSetPaused_ConcurrentMutationRace
	// for why a fixed iteration count is not enough to stay active for the
	// full HTTP loop below).
	mutatorWG.Go(func() {
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				_ = q.SetPP(job.ID, i%4)
				i++
			}
		}
	})

	for i := range iterations {
		cat := "movies"
		if i%2 == 0 {
			cat = "Default"
		}
		apiGet(t, s.Handler(),
			"/api?mode=queue&name=change_cat&value="+job.ID+"&value2="+cat+"&apikey="+testAPIKey)
	}

	close(stop)
	mutatorWG.Wait()
}

type mockApp struct {
	apitest.NopApp
	statuses map[string]directunpack.Status
}

func (m mockApp) DirectUnpackStatus(jobID string) (directunpack.Status, bool) {
	st, ok := m.statuses[jobID]
	return st, ok
}

func (m mockApp) DirectUnpackStatuses() map[string]directunpack.Status {
	return m.statuses
}

func TestQueueList_WithDirectUnpackStatus(t *testing.T) {
	t.Parallel()

	statuses := map[string]directunpack.Status{
		"test-job-id": {
			Active:           true,
			CurrentSet:       "movie_set",
			CompletedVolumes: 2,
			TotalVolumes:     5,
			SuccessSets:      []string{"set1"},
			FailedSets:       []string{"set2"},
		},
	}
	cfg := &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}}
	s, q := newTestQueueBridgeWithApp(t, cfg, func(disp *dispatch.Dispatcher, q *queue.Queue) AppServices {
		return mockApp{
			NopApp:   apitest.NopApp{Dispatcher: disp, Queue: q},
			statuses: statuses,
		}
	})

	data := makeTestNZB(t)
	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse test NZB: %v", err)
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "movie.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	job.ID = "test-job-id"
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}

	// 1. Verify queue list endpoint
	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	var resp struct {
		Queue struct {
			Slots []struct {
				NzoID        string               `json:"nzo_id"`
				DirectUnpack *directunpack.Status `json:"direct_unpack"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Queue.Slots) != 1 {
		t.Fatalf("expected 1 slot, got %d", len(resp.Queue.Slots))
	}

	slot := resp.Queue.Slots[0]
	if slot.DirectUnpack == nil {
		t.Fatal("expected non-nil direct_unpack in response")
	}
	if !slot.DirectUnpack.Active {
		t.Error("expected direct_unpack.active to be true")
	}
	if slot.DirectUnpack.CurrentSet != "movie_set" {
		t.Errorf("expected direct_unpack.current_set to be 'movie_set', got %q", slot.DirectUnpack.CurrentSet)
	}

	// 2. Verify queue job detail endpoint (files=1 is required for details)
	rrDetail := apiGet(t, s.Handler(), "/api?mode=queue&nzo_id=test-job-id&files=1&apikey="+testAPIKey)
	if rrDetail.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rrDetail.Code)
	}

	var respDetail struct {
		Queue struct {
			Slots []struct {
				NzoID        string               `json:"nzo_id"`
				DirectUnpack *directunpack.Status `json:"direct_unpack"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rrDetail.Body).Decode(&respDetail); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(respDetail.Queue.Slots) != 1 {
		t.Fatalf("expected 1 slot in detail response, got %d", len(respDetail.Queue.Slots))
	}
	slotDetail := respDetail.Queue.Slots[0]
	if slotDetail.DirectUnpack == nil {
		t.Fatal("expected non-nil direct_unpack in detail response")
	}
	if !slotDetail.DirectUnpack.Active {
		t.Error("expected direct_unpack.active to be true in detail response")
	}
}

// callCountingDUApp is a fake App that panics if DirectUnpackStatus (the
// per-job singular lookup) is ever called, and counts calls to
// DirectUnpackStatuses (the request-scoped snapshot). It is a regression
// guard for OPT-12: queueList must snapshot direct-unpack statuses once per
// request, never once per job.
type callCountingDUApp struct {
	apitest.NopApp
	statuses         map[string]directunpack.Status
	statusesCallsPtr *int
}

func (a callCountingDUApp) DirectUnpackStatus(string) (directunpack.Status, bool) {
	panic("DirectUnpackStatus (per-job) must not be called by queueList; use DirectUnpackStatuses instead (OPT-12)")
}

func (a callCountingDUApp) DirectUnpackStatuses() map[string]directunpack.Status {
	*a.statusesCallsPtr++
	return a.statuses
}

func TestQueueList_UsesSingleDirectUnpackSnapshot(t *testing.T) {
	t.Parallel()

	statuses := map[string]directunpack.Status{
		"job-1": {Active: true, CurrentSet: "set1"},
		"job-2": {Active: true, CurrentSet: "set2"},
		"job-3": {Active: true, CurrentSet: "set3"},
	}

	calls := 0
	cfg := &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}}
	s, q := newTestQueueBridgeWithApp(t, cfg, func(disp *dispatch.Dispatcher, q *queue.Queue) AppServices {
		return callCountingDUApp{
			NopApp:           apitest.NopApp{Dispatcher: disp, Queue: q},
			statuses:         statuses,
			statusesCallsPtr: &calls,
		}
	})

	data := makeTestNZB(t)
	for _, id := range []string{"job-1", "job-2", "job-3"} {
		parsed, err := nzb.Parse(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("parse test NZB: %v", err)
		}
		job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "movie.nzb"}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("NewJob: %v", err)
		}
		job.ID = id
		if err := q.Add(job); err != nil {
			t.Fatalf("queue.Add: %v", err)
		}
	}

	rr := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	var resp struct {
		Queue struct {
			Slots []struct {
				NzoID        string               `json:"nzo_id"`
				DirectUnpack *directunpack.Status `json:"direct_unpack"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Queue.Slots) != 3 {
		t.Fatalf("expected 3 slots, got %d", len(resp.Queue.Slots))
	}
	for _, slot := range resp.Queue.Slots {
		if slot.DirectUnpack == nil || !slot.DirectUnpack.Active {
			t.Errorf("slot %s: expected active direct_unpack status", slot.NzoID)
		}
	}

	if calls != 1 {
		t.Errorf("DirectUnpackStatuses call count = %d; want 1 (queueList must snapshot once per request, not once per job)", calls)
	}
}

// --- change_script and script parameter sanitization ---

func TestQueueChangeScript_Sanitization(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		input      string
		wantScript string
	}{
		{"PathTraversal", "../../../../etc/passwd", "passwd"},
		{"AbsolutePath", "/bin/malicious.sh", "malicious.sh"},
		{"WindowsTraversal", "..\\..\\windows\\win.ini", "win.ini"},
		{"SimpleScript", "sonarr.sh", "sonarr.sh"},
		{"NoneCaseInsensitive", "none", "none"},
		{"DefaultCaseInsensitive", "Default", "Default"},
		{"Empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, q := testQueueServer(t)
			job := addTestJob(t, q, queue.AddOptions{Filename: "test.nzb"})

			urlStr := "/api?mode=queue&name=change_script&value=" + job.ID + "&value2=" + url.QueryEscape(tt.input) + "&apikey=" + testAPIKey
			rr := apiGet(t, s.Handler(), urlStr)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200", rr.Code)
			}

			got := q.SnapshotJob(job.ID)
			if got == nil {
				t.Fatalf("SnapshotJob(%s): job not in queue", job.ID)
			}
			if got.Script != tt.wantScript {
				t.Errorf("Script = %q; want %q", got.Script, tt.wantScript)
			}
		})
	}
}

func TestQueueAddLocalFile_Sanitization(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, q := testQueueServerWithRoot(t, dir)

	nzbPath := filepath.Join(dir, "local.nzb")
	if err := os.WriteFile(nzbPath, makeTestNZB(t), 0o600); err != nil {
		t.Fatalf("write NZB: %v", err)
	}

	urlStr := "/api?mode=addlocalfile&name=" + url.QueryEscape(nzbPath) + "&script=" + url.QueryEscape("../../../../etc/passwd") + "&apikey=" + testAPIKey
	rr := apiGet(t, s.Handler(), urlStr)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	if q.Len() != 1 {
		t.Fatalf("queue len = %d; want 1", q.Len())
	}
	jobs := q.Snapshot()
	if jobs[0].Script != "passwd" {
		t.Errorf("Script = %q; want passwd", jobs[0].Script)
	}
}

type mockGrabberHandler struct {
	id  string
	err error
}

func (m mockGrabberHandler) HandleNZB(ctx context.Context, filename string, data []byte, opts types.FetchOptions) (string, error) {
	return m.id, m.err
}

type errorAddJobApp struct {
	apitest.NopApp
	err error
}

func (e errorAddJobApp) AddJob(ctx context.Context, job *queue.Job, rawNZB []byte, force bool) error {
	return e.err
}

func TestQueue_CoverageGaps(t *testing.T) {
	t.Parallel()

	t.Run("change_script errors", func(t *testing.T) {
		s, _ := testQueueServer(t)
		// Missing value (nzo_id)
		rr1 := apiGet(t, s.Handler(), "/api?mode=queue&name=change_script&apikey="+testAPIKey)
		if rr1.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400 when value missing", rr1.Code)
		}
		// Non-existent job ID
		rr2 := apiGet(t, s.Handler(), "/api?mode=queue&name=change_script&value=nonexistent&value2=test.sh&apikey="+testAPIKey)
		if rr2.Code != http.StatusNotFound {
			t.Errorf("status = %d; want 404 when job not found", rr2.Code)
		}
	})

	t.Run("addurl with wired grabber", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/err.nzb" {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-nzb")
			_, _ = w.Write(makeTestNZB(t))
		}))
		defer ts.Close()

		s, _ := testQueueServer(t)
		grabber := urlgrabber.New(urlgrabber.Config{AllowPrivateIPs: true}, mockGrabberHandler{id: "nzo_test1"})
		s.grabber = grabber

		// Success
		rr1 := apiGet(t, s.Handler(), "/api?mode=addurl&name="+url.QueryEscape(ts.URL+"/good.nzb")+"&apikey="+testAPIKey)
		if rr1.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr1.Code)
		}
		if !strings.Contains(rr1.Body.String(), "nzo_test1") {
			t.Errorf("body = %q; want nzo_test1", rr1.Body.String())
		}

		// Fetch error
		rr2 := apiGet(t, s.Handler(), "/api?mode=addurl&name="+url.QueryEscape(ts.URL+"/err.nzb")+"&apikey="+testAPIKey)
		if rr2.Code != http.StatusBadGateway {
			t.Errorf("status = %d; want 502 on fetch error", rr2.Code)
		}
	})

	t.Run("addfile and addlocalfile error branches", func(t *testing.T) {
		dir := t.TempDir()
		s, _ := testQueueServerWithRoot(t, dir)
		badPath := filepath.Join(dir, "bad.nzb")
		if err := os.WriteFile(badPath, []byte("invalid xml"), 0o600); err != nil {
			t.Fatalf("write bad NZB: %v", err)
		}

		// addlocalfile with bad xml
		rr1 := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name="+url.QueryEscape(badPath)+"&apikey="+testAPIKey)
		if rr1.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400 on bad XML", rr1.Code)
		}

		// addfile with errorApp on AddJob
		q := queue.New()
		errApp := errorAddJobApp{Queue: q, err: errors.New("add job failed")}
		dir2 := t.TempDir()
		sErr := New(Options{
			Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey, DownloadDir: dir2}},
			Version: "1.0.0-test",
			Queue:   q,
			App:     errApp,
		})
		goodPath := filepath.Join(dir2, "good.nzb")
		if err := os.WriteFile(goodPath, makeTestNZB(t), 0o600); err != nil {
			t.Fatalf("write good NZB: %v", err)
		}
		rr2 := apiGet(t, sErr.Handler(), "/api?mode=addlocalfile&name="+url.QueryEscape(goodPath)+"&apikey="+testAPIKey)
		if rr2.Code != http.StatusInternalServerError {
			t.Errorf("status = %d; want 500 on AddJob error", rr2.Code)
		}

		// addlocalfile with non-absolute path
		rrNonAbs := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name=relative/path.nzb&apikey="+testAPIKey)
		if rrNonAbs.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400 on non-absolute path", rrNonAbs.Code)
		}

		// addlocalfile with traversal path containing ..
		rrTraversal := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name=/absolute/path/../../etc/passwd&apikey="+testAPIKey)
		if rrTraversal.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400 on traversal path containing ..", rrTraversal.Code)
		}

		// addfile with missing multipart file fields
		rrAddFileMissing := apiGet(t, s.Handler(), "/api?mode=addfile&apikey="+testAPIKey)
		if rrAddFileMissing.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400 on missing file fields in addfile", rrAddFileMissing.Code)
		}

		// addfile with malformed multipart parsing
		reqFailParse := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey="+testAPIKey, strings.NewReader("bad-body"))
		reqFailParse.Header.Set("Content-Type", "multipart/form-data; boundary=invalid-boundary")
		rrFailParse := httptest.NewRecorder()
		s.Handler().ServeHTTP(rrFailParse, reqFailParse)
		if rrFailParse.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400 on invalid multipart body", rrFailParse.Code)
		}

		// addlocalfile with non-existent file (within the configured root, so
		// this exercises the "file not found" branch, not the SEC-6 check).
		missingPath := filepath.Join(dir, "missing.nzb")
		rrMissing := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name="+url.QueryEscape(missingPath)+"&apikey="+testAPIKey)
		if rrMissing.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400 on missing file", rrMissing.Code)
		}

		// addlocalfile with file too large (51 MiB sparse file)
		largePath := filepath.Join(dir, "large.nzb")
		lf, err := os.Create(largePath)
		if err != nil {
			t.Fatalf("create large file: %v", err)
		}
		if err := lf.Truncate(51 * 1024 * 1024); err != nil {
			_ = lf.Close()
			t.Fatalf("truncate large file: %v", err)
		}
		_ = lf.Close()
		rrLarge := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name="+url.QueryEscape(largePath)+"&apikey="+testAPIKey)
		if rrLarge.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d; want 413 on file too large, got %d", rrLarge.Code, rrLarge.Code)
		}

		// addlocalfile with nil queue
		sNoQueue := New(Options{
			Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
			Version: "1.0.0-test",
		})
		rrNoQueue := apiGet(t, sNoQueue.Handler(), "/api?mode=addlocalfile&name=/abs/path.nzb&apikey="+testAPIKey)
		if rrNoQueue.Code != http.StatusInternalServerError {
			t.Errorf("status = %d; want 500 on nil queue", rrNoQueue.Code)
		}

		// addlocalfile with directory to trigger read error
		rrDir := apiGet(t, s.Handler(), "/api?mode=addlocalfile&name="+url.QueryEscape(dir)+"&apikey="+testAPIKey)
		if rrDir.Code != http.StatusInternalServerError {
			t.Errorf("status = %d; want 500 on directory read error", rrDir.Code)
		}
	})
}

type queueSpyApp struct {
	apitest.NopApp
	mu      sync.Mutex
	paused  int
	resumed int
}

func (a *queueSpyApp) PauseDownloads() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.paused++
}

func (a *queueSpyApp) ResumeDownloads() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resumed++
}

func TestModeQueue_Comprehensive(t *testing.T) {
	t.Parallel()

	t.Run("queue_not_wired", func(t *testing.T) {
		t.Parallel()
		s := New(Options{Version: "1.0.0"})
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api?mode=queue", nil)
		s.modeQueue(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want 500", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "queue not wired") {
			t.Errorf("body = %q; want to contain 'queue not wired'", rr.Body.String())
		}
	})

	t.Run("pause_all_and_resume_all_with_app", func(t *testing.T) {
		t.Parallel()
		q := queue.New()
		spy := &queueSpyApp{Queue: q}
		s := New(Options{
			Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey}},
			Version: "1.0.0-test",
			Queue:   q,
			App:     spy,
		})

		rr := apiGet(t, s.Handler(), "/api?mode=queue&name=pause_all&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		if !q.IsPaused() {
			t.Error("expected queue to be paused")
		}
		spy.mu.Lock()
		p := spy.paused
		spy.mu.Unlock()
		if p != 1 {
			t.Errorf("paused = %d; want 1", p)
		}

		rr = apiGet(t, s.Handler(), "/api?mode=queue&name=resume_all&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		if q.IsPaused() {
			t.Error("expected queue to be resumed")
		}
		spy.mu.Lock()
		r := spy.resumed
		spy.mu.Unlock()
		if r != 1 {
			t.Errorf("resumed = %d; want 1", r)
		}
	})

	t.Run("pause_all_and_resume_all_nil_app", func(t *testing.T) {
		t.Parallel()
		q := queue.New()
		s := New(Options{
			Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey}},
			Version: "1.0.0-test",
			Queue:   q,
			// App intentionally nil
		})

		rr := apiGet(t, s.Handler(), "/api?mode=queue&name=pause_all&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		if !q.IsPaused() {
			t.Error("expected queue to be paused")
		}

		rr = apiGet(t, s.Handler(), "/api?mode=queue&name=resume_all&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		if q.IsPaused() {
			t.Error("expected queue to be resumed")
		}
	})

	t.Run("stubbed_actions", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServer(t)

		for _, action := range []string{"sort", "delete_nzf"} {
			rr := apiGet(t, s.Handler(), "/api?mode=queue&name="+action+"&apikey="+testAPIKey)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("action %s status = %d; want 400", action, rr.Code)
			}
			if !strings.Contains(rr.Body.String(), "not implemented in this build") {
				t.Errorf("action %s body = %q; want 'not implemented in this build'", action, rr.Body.String())
			}
		}
	})

	t.Run("change_complete_action", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServer(t)

		rr := apiGet(t, s.Handler(), "/api?mode=queue&name=change_complete_action&value=shutdown&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		m := decodeJSON(t, rr)
		if m["status"] != true {
			t.Errorf("status = %v; want true", m["status"])
		}
	})

	t.Run("rename_alias", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		job := addTestJob(t, q, queue.AddOptions{Name: "OldName"})

		rr := apiGet(t, s.Handler(), "/api?mode=queue&name=rename&value="+job.ID+"&value2=NewName&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		m := decodeJSON(t, rr)
		if m["status"] != true {
			t.Errorf("status = %v; want true", m["status"])
		}
		got := q.SnapshotJob(job.ID)
		if got == nil {
			t.Fatalf("SnapshotJob(%s)", job.ID)
		}
		if got.Name != "NewName" {
			t.Errorf("got.Name = %q; want NewName", got.Name)
		}
	})

	t.Run("unknown_action", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServer(t)

		rr := apiGet(t, s.Handler(), "/api?mode=queue&name=bogus_action&apikey="+testAPIKey)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d; want 400", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "unknown queue action: bogus_action") {
			t.Errorf("body = %q; want to contain 'unknown queue action: bogus_action'", rr.Body.String())
		}
	})
}

func TestStageFromStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status constants.Status
		want   string
	}{
		{constants.StatusDownloading, "download"},
		{constants.StatusFetching, "download"},
		{constants.StatusGrabbing, "download"},
		{constants.StatusVerifying, "repair"},
		{constants.StatusRepairing, "repair"},
		{constants.StatusChecking, "repair"},
		{constants.StatusQuickCheck, "repair"},
		{constants.StatusExtracting, "unpack"},
		{constants.StatusMoving, "move"},
		{constants.StatusRunning, "script"},
		{constants.StatusPaused, "paused"},
		{constants.StatusQueued, "queued"},
		{constants.StatusIdle, "queued"},
		{constants.StatusPropagating, "propagating"},
		{constants.StatusCompleted, "completed"},
		{constants.StatusFailed, "failed"},
		{constants.StatusDeleted, "deleted"},
		{"UNKNOWN_STATUS", "unknown_status"},
	}
	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()
			if got := stageFromStatus(tc.status); got != tc.want {
				t.Errorf("stageFromStatus(%q) = %q; want %q", tc.status, got, tc.want)
			}
		})
	}
}

type removeJobErrApp struct {
	apitest.NopApp
	errByID map[string]error
}

func (a removeJobErrApp) RemoveJob(ctx context.Context, id string, deleteFiles bool) error {
	if err, ok := a.errByID[id]; ok && err != nil {
		return err
	}
	return a.NopApp.RemoveJob(ctx, id, deleteFiles)
}

func TestQueueDelete_RemoveJobErrorLog(t *testing.T) {
	t.Parallel()
	q := queue.New()
	rec := &recordHandler{}
	logger := slog.New(rec)
	s := New(Options{
		Logger:  logger,
		Config:  &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
		Version: "1.0.0-test",
		Queue:   q,
		App: removeJobErrApp{
			NopApp:  apitest.NopApp{Queue: q},
			errByID: map[string]error{"job_fail": errors.New("simulated queue delete failure")},
		},
	})

	j1 := addTestJob(t, q, queue.AddOptions{Filename: "a.nzb"})
	value := j1.ID + ",job_fail"

	rr := apiGet(t, s.Handler(), "/api?mode=queue&name=delete&value="+value+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}
	nzoIDs, ok := m["nzo_ids"].([]any)
	if !ok || len(nzoIDs) != 1 || nzoIDs[0] != j1.ID {
		t.Errorf("nzo_ids = %v; want [%s]", m["nzo_ids"], j1.ID)
	}

	if !rec.hasWarn("failed to remove job during bulk delete", "job_fail", "simulated queue delete failure") {
		t.Errorf("expected warning log not found in records: %v", rec.records)
	}
}

// TestFilterQueueSlots covers filterQueueSlots directly (extracted from
// queueList in OPT-9): category filter, status filter, search filter, and
// the duStatuses lookup all apply independently and in combination.
func TestFilterQueueSlots(t *testing.T) {
	t.Parallel()

	newFilterRow := func(id, name, filename, category string, running bool, reason job.WaitReason, state job.State) dispatch.Row {
		return dispatch.Row{
			ID: id,
			Header: dispatch.Header{
				Name:     name,
				Filename: filename,
				Category: category,
			},
			View: job.RenderView{
				StateView: job.StateView{
					State: state,
				},
				Running: running,
				Reason:  reason,
			},
		}
	}
	rows := []dispatch.Row{
		newFilterRow("job1", "Ubuntu ISO", "ubuntu.nzb", "linux", true, 0, job.Fetching),
		newFilterRow("job2", "Debian ISO", "debian.nzb", "linux", false, job.UserPaused, job.Fetching),
		newFilterRow("job3", "Some Movie", "movie.nzb", "movies", true, 0, job.Fetching),
	}
	duStatuses := map[string]directunpack.Status{
		"job1": {CurrentSet: "vol01"},
	}
	s := &Server{}

	t.Run("no filters returns everything", func(t *testing.T) {
		t.Parallel()
		slots := s.filterQueueSlots(rows, "", "", "", false, 0, duStatuses, nil)
		if len(slots) != 3 {
			t.Fatalf("len(slots) = %d, want 3", len(slots))
		}
	})

	t.Run("category filter", func(t *testing.T) {
		t.Parallel()
		slots := s.filterQueueSlots(rows, "movies", "", "", false, 0, duStatuses, nil)
		if len(slots) != 1 || slots[0].NzoID != "job3" {
			t.Fatalf("filterQueueSlots(category=movies, nil) = %+v, want only job3", slots)
		}
	})

	t.Run("status filter", func(t *testing.T) {
		t.Parallel()
		slots := s.filterQueueSlots(rows, "", string(constants.StatusPaused), "", false, 0, duStatuses, nil)
		if len(slots) != 1 || slots[0].NzoID != "job2" {
			t.Fatalf("filterQueueSlots(status=Paused, nil) = %+v, want only job2", slots)
		}
	})

	t.Run("search filter matches name or filename, case-insensitively", func(t *testing.T) {
		t.Parallel()
		slots := s.filterQueueSlots(rows, "", "", "debian", false, 0, duStatuses, nil)
		if len(slots) != 1 || slots[0].NzoID != "job2" {
			t.Fatalf("filterQueueSlots(search=debian, nil) = %+v, want only job2", slots)
		}
	})

	t.Run("duStatuses populates DirectUnpack for matching job only", func(t *testing.T) {
		t.Parallel()
		slots := s.filterQueueSlots(rows, "", "", "", false, 0, duStatuses, nil)
		for _, sl := range slots {
			switch sl.NzoID {
			case "job1":
				if sl.DirectUnpack == nil || sl.DirectUnpack.CurrentSet != "vol01" {
					t.Errorf("job1.DirectUnpack = %+v, want CurrentSet=vol01", sl.DirectUnpack)
				}
			default:
				if sl.DirectUnpack != nil {
					t.Errorf("%s.DirectUnpack = %+v, want nil", sl.NzoID, sl.DirectUnpack)
				}
			}
		}
	})

	t.Run("combined filters narrow to nothing", func(t *testing.T) {
		t.Parallel()
		slots := s.filterQueueSlots(rows, "movies", string(constants.StatusPaused), "", false, 0, duStatuses, nil)
		if len(slots) != 0 {
			t.Fatalf("filterQueueSlots(category=movies, status=Paused, nil) = %+v, want empty", slots)
		}
	})
}

// TestBuildSlot_MapsJobFields calls buildSlot directly and pins the mapping
// for a job that is NOT in the simplest state: it is Downloading but the
// queue-wide pause flag is set (exercising the Paused status/stage override),
// it has PP/Password/Script/Warning set, and it has a failed article
// (exercising FailedBytes) and a direct-unpack status attached.
func TestBuildSlot_MapsJobFields(t *testing.T) {
	t.Parallel()

	articles := make([]job.JobArticle, 10)
	for i := range articles {
		articles[i] = job.JobArticle{
			ID:     fmt.Sprintf("big-article-%03d@example.com", i+1),
			Bytes:  1024 * 1024,
			Number: i + 1,
		}
	}
	files := []job.JobFile{
		{
			Subject:  "large.file",
			Bytes:    10 * 1024 * 1024,
			Articles: articles,
		},
	}
	m := job.NewManifest(files)
	j := job.New("job-1", "large.file", job.Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := j.BeginAttempt(time.Now().UTC()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if err := j.MarkArticleFailed(0); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}

	row := dispatch.Row{
		ID: j.ID(),
		Header: dispatch.Header{
			Name:     "large.file",
			Filename: "large.file.nzb",
			Bytes:    m.TotalBytes(),
			PP:       3,
			Password: "hunter2",
			Script:   "post.py",
			Warning:  "low disk space",
		},
		View: job.RenderView{
			StateView: j.State(),
			Running:   false,
			Reason:    job.GlobalPause,
		},
	}

	const speed = 1024.0 * 1024.0 // 1 MiB/s; well above the noise floor
	duStatus := &directunpack.Status{CurrentSet: "vol01"}

	slot := buildSlot(row, j, true /* queue-wide paused */, speed, 2, duStatus, app.JobCheckpointState{})

	if slot.NzoID != j.ID() {
		t.Errorf("NzoID = %q, want %q", slot.NzoID, j.ID())
	}
	if slot.Index != 2 {
		t.Errorf("Index = %d, want 2", slot.Index)
	}
	if slot.Status != string(constants.StatusPaused) {
		t.Errorf("Status = %q, want %q (paused override of Downloading)", slot.Status, constants.StatusPaused)
	}
	if slot.CurrentStage != "paused" {
		t.Errorf("CurrentStage = %q, want %q", slot.CurrentStage, "paused")
	}
	if slot.PP != "3" {
		t.Errorf("PP = %q, want %q", slot.PP, "3")
	}
	if slot.Password != "hunter2" {
		t.Errorf("Password = %q, want %q", slot.Password, "hunter2")
	}
	if slot.Script != "post.py" {
		t.Errorf("Script = %q, want %q", slot.Script, "post.py")
	}
	if slot.Warning != "low disk space" {
		t.Errorf("Warning = %q, want %q", slot.Warning, "low disk space")
	}
	if slot.FailedBytes == 0 {
		t.Error("FailedBytes = 0, want > 0 after MarkArticleFailed")
	}
	if slot.Bytes != 10*1024*1024 {
		t.Errorf("Bytes = %d, want %d", slot.Bytes, 10*1024*1024)
	}
	// The queue-wide pause override must suppress ETA computation even
	// though speed and remaining bytes would otherwise produce one.
	if slot.ETASeconds != 0 {
		t.Errorf("ETASeconds = %d, want 0 when paused", slot.ETASeconds)
	}
	if slot.Timeleft != "0:00:00" {
		t.Errorf("Timeleft = %q, want %q when paused", slot.Timeleft, "0:00:00")
	}
	if slot.ETA != "unknown" {
		t.Errorf("ETA = %q, want %q when paused", slot.ETA, "unknown")
	}
	if slot.DirectUnpack == nil || slot.DirectUnpack.CurrentSet != "vol01" {
		t.Errorf("DirectUnpack = %+v, want CurrentSet=vol01", slot.DirectUnpack)
	}
}

// TestQueueList_Direct_PaginationAndPausedFlag calls s.queueList directly
// (not through the mode= dispatcher) and pins the paging parameters and the
// queue-wide paused flag in the response envelope. Field names are asserted
// via explicit json tags on an anonymous struct — this is the SABnzbd-
// compatible shape third-party clients (Sonarr, Radarr) parse.
func TestQueueList_Direct_PaginationAndPausedFlag(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addTestJob(t, q, queue.AddOptions{Filename: "a.nzb"})
	jobB := addTestJob(t, q, queue.AddOptions{Filename: "b.nzb"})
	addTestJob(t, q, queue.AddOptions{Filename: "c.nzb"})
	q.PauseAll()

	req := httptest.NewRequest(http.MethodGet, "/api?start=1&limit=1", nil)
	rr := httptest.NewRecorder()
	s.queueList(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Status bool `json:"status"`
		Queue  struct {
			Paused         bool `json:"paused"`
			Start          int  `json:"start"`
			Limit          int  `json:"limit"`
			NoOfSlots      int  `json:"noofslots"`
			NoOfSlotsTotal int  `json:"noofslots_total"`
			Slots          []struct {
				NzoID string `json:"nzo_id"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Status {
		t.Error("status = false, want true")
	}
	if !resp.Queue.Paused {
		t.Error("paused = false, want true (q.PauseAll was called)")
	}
	if resp.Queue.Start != 1 {
		t.Errorf("start = %d, want 1", resp.Queue.Start)
	}
	if resp.Queue.Limit != 1 {
		t.Errorf("limit = %d, want 1", resp.Queue.Limit)
	}
	if resp.Queue.NoOfSlotsTotal != 3 {
		t.Errorf("noofslots_total = %d, want 3 (all jobs, unpaginated)", resp.Queue.NoOfSlotsTotal)
	}
	if resp.Queue.NoOfSlots != 1 {
		t.Errorf("noofslots = %d, want 1 (paginated page size)", resp.Queue.NoOfSlots)
	}
	if len(resp.Queue.Slots) != 1 {
		t.Fatalf("len(slots) = %d, want 1", len(resp.Queue.Slots))
	}
	// start=1 must skip the first job (a.nzb) and return the second (b.nzb) —
	// asserting the count alone wouldn't catch a broken/no-op start offset,
	// since limit=1 would still truncate to one slot either way.
	if resp.Queue.Slots[0].NzoID != jobB.ID {
		t.Errorf("slots[0].nzo_id = %q, want %q (job b, the start=1 offset)", resp.Queue.Slots[0].NzoID, jobB.ID)
	}
}

// TestModeAddLocalFile_Direct calls s.modeAddLocalFile directly (not through
// the mode= dispatcher), covering the happy path (a valid absolute path
// under a configured root gets parsed and enqueued) and its failure mode
// for bad input (a relative path is rejected before any filesystem access).
func TestModeAddLocalFile_Direct(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, q := testQueueServerWithRoot(t, dir)

	t.Run("happy path enqueues job", func(t *testing.T) {
		nzbPath := filepath.Join(dir, "direct.nzb")
		if err := os.WriteFile(nzbPath, makeTestNZB(t), 0o600); err != nil {
			t.Fatalf("write NZB: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api?name="+nzbPath, nil)
		rr := httptest.NewRecorder()
		s.modeAddLocalFile(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		if q.Len() != 1 {
			t.Errorf("queue len = %d; want 1", q.Len())
		}
	})

	t.Run("relative path rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api?name=relative.nzb", nil)
		rr := httptest.NewRecorder()
		s.modeAddLocalFile(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d; want 400 for relative path (body: %s)", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "absolute") {
			t.Errorf("expected 'absolute' in error body; got: %s", rr.Body.String())
		}
	})
}

// TestUnixOrZero_RendersNeverAsZero pins the encoding of an absent timestamp.
// time.Time's zero value goes through Unix() as -6795364578871, which a client
// renders as a date in 1754 rather than as "this job has never checkpointed".
func TestUnixOrZero_RendersNeverAsZero(t *testing.T) {
	t.Parallel()
	if got := unixOrZero(time.Time{}); got != 0 {
		t.Errorf("unixOrZero(zero) = %d, want 0", got)
	}
	at := time.Now().Truncate(time.Second)
	if got := unixOrZero(at); got != at.Unix() {
		t.Errorf("unixOrZero(%v) = %d, want %d", at, got, at.Unix())
	}
}
