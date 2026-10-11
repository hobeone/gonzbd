package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/humanfmt"
	"github.com/hobeone/gonzbd/internal/job"
)

// queueLevel is the queue-level part of a mode=queue listing that these tests
// assert on.
type queueLevel struct {
	Status         string `json:"status"`
	Paused         bool   `json:"paused"`
	Speed          string `json:"speed"`
	KBPerSec       string `json:"kbpersec"`
	MB             string `json:"mb"`
	MBLeft         string `json:"mbleft"`
	Size           string `json:"size"`
	SizeLeft       string `json:"sizeleft"`
	Timeleft       string `json:"timeleft"`
	NoOfSlots      int    `json:"noofslots"`
	NoOfSlotsTotal int    `json:"noofslots_total"`
}

func getQueueLevel(t *testing.T, s *Server, query string) queueLevel {
	t.Helper()
	rr := apiGet(t, s.Handler(), "/api?mode=queue"+query+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		Queue queueLevel `json:"queue"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Queue
}

// TestQueueList_SizeTotalsIgnorePagination pins #749: the queue-level size
// totals describe every slot the filters select, not the page start/limit
// cut out of them.
func TestQueueList_SizeTotalsIgnorePagination(t *testing.T) {
	t.Parallel()
	s, q := testQueueServer(t)
	addNamedLargeTestJob(t, q, "ten", 10)
	addNamedLargeTestJob(t, q, "three", 3)

	const wantBytes = int64(13 << 20)
	got := getQueueLevel(t, s, "&limit=1")
	if got.NoOfSlots != 1 || got.NoOfSlotsTotal != 2 {
		t.Fatalf("noofslots = %d, noofslots_total = %d; want 1 and 2", got.NoOfSlots, got.NoOfSlotsTotal)
	}
	if want := toMBString(wantBytes); got.MB != want || got.MBLeft != want {
		t.Errorf("mb = %q, mbleft = %q; want %q for both (both jobs, not the page)", got.MB, got.MBLeft, want)
	}
	if want := humanfmt.Bytes(wantBytes); got.Size != want || got.SizeLeft != want {
		t.Errorf("size = %q, sizeleft = %q; want %q for both (both jobs, not the page)", got.Size, got.SizeLeft, want)
	}

	// start=1 selects the other job; the totals must not move.
	if got2 := getQueueLevel(t, s, "&start=1&limit=1"); got2.MB != got.MB || got2.Size != got.Size {
		t.Errorf("start=1: mb = %q, size = %q; want %q and %q", got2.MB, got2.Size, got.MB, got.Size)
	}
}

// TestQueueList_SpeedFields pins #734: the queue-level speed, kbpersec and
// timeleft come from the request's speed snapshot, not literals.
func TestQueueList_SpeedFields(t *testing.T) {
	t.Parallel()
	const speed = 2 * 1024 * 1024 // 2 MiB/s
	s, q := testQueueServerWithSpeed(t, speed)
	tj := addNamedLargeTestJob(t, q, "ten", 10) // 10 MiB → 5 s at 2 MiB/s
	if err := q.SetStatusIf(tj.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}

	got := getQueueLevel(t, s, "")
	if got.Speed != "2.0 MB/s" {
		t.Errorf("speed = %q; want %q", got.Speed, "2.0 MB/s")
	}
	if got.KBPerSec != "2048.0" {
		t.Errorf("kbpersec = %q; want %q", got.KBPerSec, "2048.0")
	}
	if got.Timeleft != "0:00:05" {
		t.Errorf("timeleft = %q; want %q", got.Timeleft, "0:00:05")
	}

	// The single-job detail variant reports the same three fields.
	detail := getQueueLevel(t, s, "&nzo_id="+tj.ID+"&files=1")
	if detail.Speed != got.Speed || detail.KBPerSec != got.KBPerSec || detail.Timeleft != got.Timeleft {
		t.Errorf("detail speed/kbpersec/timeleft = %q/%q/%q; want %q/%q/%q",
			detail.Speed, detail.KBPerSec, detail.Timeleft, got.Speed, got.KBPerSec, got.Timeleft)
	}
}

// TestQueueList_TimeleftZeroWhenPaused pins the paused half of #734: a
// globally paused queue reports no ETA, whatever the speed snapshot says.
func TestQueueList_TimeleftZeroWhenPaused(t *testing.T) {
	t.Parallel()
	s, q := testQueueServerWithSpeed(t, 2*1024*1024)
	tj := addNamedLargeTestJob(t, q, "ten", 10)
	if err := q.SetStatusIf(tj.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}
	q.PauseAll()

	if got := getQueueLevel(t, s, ""); got.Timeleft != "0:00:00" {
		t.Errorf("timeleft = %q; want 0:00:00 while paused", got.Timeleft)
	}
}

// TestQueueList_TimeleftZeroWhenNothingDownloads pins that timeleft follows
// the queue status, not only the global pause: with the global pause off and
// a non-zero speed snapshot (a meter still decaying), a queue whose only job
// is individually paused reports no ETA, in the listing and in the detail
// response for that job.
func TestQueueList_TimeleftZeroWhenNothingDownloads(t *testing.T) {
	t.Parallel()
	s, q := testQueueServerWithSpeed(t, 2*1024*1024)
	tj := addNamedLargeTestJob(t, q, "ten", 10)
	if err := q.SetStatusIf(tj.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}
	if err := q.Pause(tj.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	got := getQueueLevel(t, s, "")
	if got.Paused || got.Status != "Paused" {
		t.Fatalf("setup: paused = %v, status = %q; want false and Paused", got.Paused, got.Status)
	}
	if got.Timeleft != "0:00:00" {
		t.Errorf("listing timeleft = %q; want 0:00:00 while nothing downloads", got.Timeleft)
	}
	if detail := getQueueLevel(t, s, "&nzo_id="+tj.ID+"&files=1"); detail.Timeleft != "0:00:00" {
		t.Errorf("detail timeleft = %q; want 0:00:00 for a paused job", detail.Timeleft)
	}
}

// TestQueueList_TimeleftZeroWhileOnlyWaiting pins that timeleft follows a
// job actually downloading, not the status string: a queue whose only job is
// waiting to start reads Downloading but has no ETA.
func TestQueueList_TimeleftZeroWhileOnlyWaiting(t *testing.T) {
	t.Parallel()
	s, q := testUnstartedQueueServer(t, 2*1024*1024)
	// 10 MiB at 2 MiB/s would be a 5 s ETA if timeleft keyed on the status.
	tj := addNamedLargeTestJob(t, q, "waiting", 10)
	if row, ok := q.disp.Row(tj.ID); !ok || row.View.Running || row.View.State != job.StateUnset {
		t.Fatalf("setup: want a job not yet started, got ok=%v view=%+v", ok, row.View)
	}
	got := getQueueLevel(t, s, "")
	if got.Status != "Downloading" {
		t.Fatalf("setup: status = %q; want Downloading", got.Status)
	}
	if got.Timeleft != "0:00:00" {
		t.Errorf("timeleft = %q; want 0:00:00 while no job is fetching", got.Timeleft)
	}
}

// addPostProcJob queues a job and moves it past Fetching into a running
// post-processing state.
func addPostProcJob(t *testing.T, q *testQueueWrapper, filename string) *testJob {
	t.Helper()
	tj := addTestJob(t, q, testAddOptions{Filename: filename})
	if err := q.SetStatusIf(tj.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}
	if _, err := q.SetPostProcStarted(tj.ID); err != nil {
		t.Fatalf("SetPostProcStarted: %v", err)
	}
	row, ok := q.disp.Row(tj.ID)
	if !ok || !row.View.Running || row.View.State == job.Fetching {
		t.Fatalf("setup: want a running job past Fetching, got ok=%v view=%+v", ok, row.View)
	}
	return tj
}

// addHeldJob queues a job and pauses it individually.
func addHeldJob(t *testing.T, q *testQueueWrapper, opts testAddOptions) *testJob {
	t.Helper()
	tj := addTestJob(t, q, opts)
	if err := q.Pause(tj.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	return tj
}

// testUnstartedQueueServer is testQueueServerWithSpeed over a dispatcher whose
// tick loop never runs, so a queued job stays not-yet-started for the whole
// test instead of racing the loop for a lease. The dispatcher is restored, not
// started: Add requires the first and not the second.
func testUnstartedQueueServer(t *testing.T, speed float64) (*Server, *testQueueWrapper) {
	t.Helper()
	d := dispatch.New(2, 2, time.Hour, time.Now, &apiStubWorkers{}, &apiStubResidency{}, &apiStubStore{}, &apiStubRunner{})
	if err := d.Restore(t.Context()); err != nil {
		t.Fatalf("dispatcher.Restore: %v", err)
	}
	s := New(Options{
		Config:     &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}},
		Dispatcher: d,
		App:        apitest.NopApp{Dispatcher: d, SpeedVal: speed},
	})
	return s, &testQueueWrapper{disp: d}
}

// addWaitingJob queues a job that has not started: not running, not paused.
// Use it on a testUnstartedQueueServer, where no tick can start the job.
func addWaitingJob(t *testing.T, q *testQueueWrapper, filename string) *testJob {
	t.Helper()
	tj := addTestJob(t, q, testAddOptions{Filename: filename})
	row, ok := q.disp.Row(tj.ID)
	if !ok || row.View.Running || row.View.Reason.IsPause() || row.View.State != job.StateUnset {
		t.Fatalf("setup: want a job not yet started and not paused, got ok=%v view=%+v", ok, row.View)
	}
	return tj
}

// TestQueueList_StatusFromSlots pins #735: the queue-level status says
// Paused only for a pause (global, or a job held by one), Downloading while a
// job downloads or waits to, and Idle otherwise.
func TestQueueList_StatusFromSlots(t *testing.T) {
	t.Parallel()

	t.Run("empty queue is Idle", func(t *testing.T) {
		t.Parallel()
		s, _ := testQueueServer(t)
		if got := getQueueLevel(t, s, ""); got.Status != "Idle" {
			t.Errorf("status = %q; want Idle", got.Status)
		}
	})

	t.Run("only job individually paused is Paused", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		tj := addTestJob(t, q, testAddOptions{Filename: "held.nzb"})
		if err := q.SetStatusIf(tj.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
			t.Fatalf("SetStatusIf: %v", err)
		}
		if err := q.Pause(tj.ID); err != nil {
			t.Fatalf("pause: %v", err)
		}
		got := getQueueLevel(t, s, "")
		if got.Paused {
			t.Fatal("queue.paused = true; this case needs the global pause off")
		}
		if got.Status != "Paused" {
			t.Errorf("status = %q; want Paused (a job is held)", got.Status)
		}
	})

	t.Run("one downloading job beside a paused one is Downloading", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		dl := addTestJob(t, q, testAddOptions{Filename: "running.nzb", Category: "movies"})
		if err := q.SetStatusIf(dl.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
			t.Fatalf("SetStatusIf: %v", err)
		}
		addHeldJob(t, q, testAddOptions{Filename: "held.nzb", Category: "tv"})
		if got := getQueueLevel(t, s, ""); got.Status != "Downloading" {
			t.Errorf("status = %q; want Downloading", got.Status)
		}
		// The status describes the queue, so a filter that selects only the
		// paused job does not change it.
		if got := getQueueLevel(t, s, "&cat=tv"); got.Status != "Downloading" {
			t.Errorf("cat=tv: status = %q; want Downloading", got.Status)
		}
	})

	t.Run("only job waiting to start is Downloading", func(t *testing.T) {
		t.Parallel()
		s, q := testUnstartedQueueServer(t, 0)
		addWaitingJob(t, q, "waiting.nzb")
		if got := getQueueLevel(t, s, ""); got.Status != "Downloading" {
			t.Errorf("status = %q; want Downloading (a job waits for a slot)", got.Status)
		}
	})

	t.Run("a waiting job beside a paused one is Downloading", func(t *testing.T) {
		t.Parallel()
		s, q := testUnstartedQueueServer(t, 0)
		addHeldJob(t, q, testAddOptions{Filename: "held.nzb"})
		addWaitingJob(t, q, "waiting.nzb")
		if got := getQueueLevel(t, s, ""); got.Status != "Downloading" {
			t.Errorf("status = %q; want Downloading (waiting outranks held)", got.Status)
		}
	})

	t.Run("only job post-processing is Idle", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		addPostProcJob(t, q, "pp.nzb")
		if got := getQueueLevel(t, s, ""); got.Status != "Idle" {
			t.Errorf("status = %q; want Idle (nothing downloads, waits, or is held)", got.Status)
		}
	})

	t.Run("a paused job beside a post-processing one is Paused", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		addPostProcJob(t, q, "pp.nzb")
		addHeldJob(t, q, testAddOptions{Filename: "held.nzb"})
		if got := getQueueLevel(t, s, ""); got.Status != "Paused" {
			t.Errorf("status = %q; want Paused (a job is held)", got.Status)
		}
	})

	t.Run("empty queue under global pause is Paused", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		q.PauseAll()
		if got := getQueueLevel(t, s, ""); got.Status != "Paused" {
			t.Errorf("status = %q; want Paused", got.Status)
		}
	})

	t.Run("global pause is Paused", func(t *testing.T) {
		t.Parallel()
		s, q := testQueueServer(t)
		tj := addTestJob(t, q, testAddOptions{Filename: "job.nzb"})
		if err := q.SetStatusIf(tj.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
			t.Fatalf("SetStatusIf: %v", err)
		}
		q.PauseAll()
		if got := getQueueLevel(t, s, ""); got.Status != "Paused" {
			t.Errorf("status = %q; want Paused", got.Status)
		}
	})
}

// TestQueueStatus covers queueStatus on RenderViews the listing fixtures
// cannot reach: a job at Fetching waiting for a lease, one that finished
// Fetching and waits to move on, a running job whose Reason would read as a
// pause, and a settled job.
func TestQueueStatus(t *testing.T) {
	t.Parallel()
	row := func(v job.RenderView) dispatch.Row { return dispatch.Row{ID: "x", View: v} }
	fetching := job.StateView{State: job.Fetching}
	tests := []struct {
		name            string
		rows            []dispatch.Row
		paused          bool
		wantStatus      string
		wantDownloading bool
	}{
		{"empty", nil, false, "Idle", false},
		{"empty, global pause", nil, true, "Paused", false},
		{"fetching", []dispatch.Row{row(job.RenderView{StateView: fetching, Running: true})}, false, "Downloading", true},
		{"global pause outranks fetching", []dispatch.Row{row(job.RenderView{StateView: fetching, Running: true})}, true, "Paused", false},
		{"at Fetching waiting for a lease", []dispatch.Row{row(job.RenderView{StateView: fetching, Reason: job.NoLease})}, false, "Downloading", false},
		{"not yet started", []dispatch.Row{row(job.RenderView{Reason: job.NoLease})}, false, "Downloading", false},
		{"finished Fetching, waiting to move on", []dispatch.Row{row(job.RenderView{State: job.Fetching, Next: job.Assessing})}, false, "Idle", false},
		{"waiting for a compute slot", []dispatch.Row{row(job.RenderView{State: job.Assessing})}, false, "Idle", false},
		{"running post-processing", []dispatch.Row{row(job.RenderView{State: job.Extracting, Running: true})}, false, "Idle", false},
		{"running, Reason not consulted", []dispatch.Row{row(job.RenderView{State: job.Extracting, Running: true, Reason: job.UserPaused})}, false, "Idle", false},
		{"held", []dispatch.Row{row(job.RenderView{StateView: fetching, Reason: job.UserPaused})}, false, "Paused", false},
		{"settled", []dispatch.Row{row(job.RenderView{State: job.Fetching, Outcome: job.OutcomeFailed})}, false, "Idle", false},
		{"waiting outranks held", []dispatch.Row{
			row(job.RenderView{StateView: fetching, Reason: job.UserPaused}),
			row(job.RenderView{Reason: job.NoLease}),
		}, false, "Downloading", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, downloading := queueStatus(tt.rows, tt.paused)
			if status != tt.wantStatus || downloading != tt.wantDownloading {
				t.Errorf("queueStatus = (%q, %v); want (%q, %v)", status, downloading, tt.wantStatus, tt.wantDownloading)
			}
		})
	}
}

func TestFormatSpeed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		bps  float64
		want string
	}{
		{0, "0 B/s"},
		{512, "512 B/s"},
		{1023.7, "1024 B/s"},
		{1024, "1.0 KB/s"},
		{1536, "1.5 KB/s"},
		{1024 * 1024, "1.0 MB/s"},
		{210 * 1024 * 1024, "210.0 MB/s"},
	} {
		if got := formatSpeed(tt.bps); got != tt.want {
			t.Errorf("formatSpeed(%v) = %q; want %q", tt.bps, got, tt.want)
		}
	}
}
