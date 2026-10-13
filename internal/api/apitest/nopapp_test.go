package apitest

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appkg "github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

// stubWorkers models a worker that exits when aborted: it reports the yield
// that clears the job's launch claim. Without that report, removing a launched
// job waits for the claim until its context ends (#814). The report runs on
// its own goroutine because sched calls Abort holding its lock, and
// sched.Workers says Abort must not block.
type stubWorkers struct {
	d  *dispatch.Dispatcher
	wg sync.WaitGroup
}

func (w *stubWorkers) Abort(j *job.Job) {
	w.wg.Go(func() {
		// ErrNotFound means the job is no longer registered, and its claim
		// went with it.
		_ = w.d.YieldedJob(j)
	})
}

type stubResidency struct{}

func (r *stubResidency) Hydrate(context.Context, string) error { return nil }
func (r *stubResidency) Evict(string)                          {}

type stubStore struct{}

func (s *stubStore) Load(context.Context) ([]dispatch.Persisted, error) { return nil, nil }
func (s *stubStore) Save(context.Context, dispatch.Persisted) error     { return nil }
func (s *stubStore) Delete(context.Context, string) error               { return nil }

// stubRunner reports each launch on launched and starts no work.
type stubRunner struct{ launched chan string }

func (r *stubRunner) Run(_ context.Context, id string, _ job.State) {
	select {
	case r.launched <- id:
	default:
	}
}

func newTestDispatcher(t *testing.T) (*dispatch.Dispatcher, <-chan string) {
	t.Helper()
	run := &stubRunner{launched: make(chan string, 1)}
	w := &stubWorkers{}
	d := dispatch.New(2, 2, time.Hour, time.Now, w, &stubResidency{}, &stubStore{}, run)
	w.d = d
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("dispatcher.Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	t.Cleanup(w.wg.Wait)
	return d, run.launched
}

// awaitLaunch wakes the dispatcher until it launches id. Opening an attempt
// and granting the lease take one tick each, and a wake that lands while one
// is already pending is dropped, so a single Wake is not enough.
func awaitLaunch(t *testing.T, d *dispatch.Dispatcher, launched <-chan string, id string) {
	t.Helper()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	deadline := time.After(10 * time.Second)
	for {
		d.Wake()
		select {
		case got := <-launched:
			if got != id {
				t.Fatalf("dispatcher launched %q, want %q", got, id)
			}
			return
		case <-poll.C:
		case <-deadline:
			t.Fatalf("dispatcher did not launch %q within 10s", id)
		}
	}
}

func TestNopApp_Contract(t *testing.T) {
	ctx := context.Background()
	snaps := []downloader.ServerSnapshot{{Name: "srv1", ActiveConns: 2}}
	app := NopApp{
		SpeedVal:           12.34,
		ServerSnapshotsVal: snaps,
	}

	// 1. Verify stub method return values
	if err := app.ReloadDownloader(nil); err != nil {
		t.Errorf("ReloadDownloader() = %v, want nil", err)
	}
	if res, err := app.TestNNTPServer(ctx, config.Server{}); err != nil || res != (appkg.NNTPTestResult{}) {
		t.Errorf("TestNNTPServer() = %+v, %v, want the zero result and nil", res, err)
	}
	if err := app.RetryHistoryJob(ctx, "job1"); err != nil {
		t.Errorf("RetryHistoryJob() = %v, want nil", err)
	}
	if name, err := app.RenameJob("job1", "n"); err != nil || name != "n" {
		t.Errorf("RenameJob() = %q, %v, want n, nil", name, err)
	}
	if err := app.RetryHistoryJobAllowingUnwanted(ctx, "job1"); err != nil {
		t.Errorf("RetryHistoryJobAllowingUnwanted() = %v, want nil", err)
	}
	if !app.UnblockServer("srv1") {
		t.Error("UnblockServer() = false, want true")
	}
	if status := app.ServerStatus(); len(status) != 1 || status[0].Name != "srv1" {
		t.Errorf("ServerStatus() = %v, want %v", status, snaps)
	}
	if sp := app.Speed(); sp != 12.34 {
		t.Errorf("Speed() = %v, want 12.34", sp)
	}
	if _, ok := app.DirectUnpackStatus("job1"); ok {
		t.Error("DirectUnpackStatus() ok = true, want false")
	}
	if statuses := app.DirectUnpackStatuses(); statuses != nil {
		t.Errorf("DirectUnpackStatuses() = %v, want nil", statuses)
	}
	if states := app.StallReasons(); states != nil {
		t.Errorf("StallReasons() = %v on an unconfigured NopApp, want nil", states)
	}
	if st := app.StallReason("job1"); st != (appkg.StallInfo{}) {
		t.Errorf("StallReason() = %+v on an unconfigured NopApp, want the zero value", st)
	}
	configured := NopApp{StallReasonsVal: map[string]string{
		"job1": "Stalled: disk full",
	}}
	if st := configured.StallReason("job1"); st.Reason == "" {
		t.Errorf("StallReason(job1) = %+v, want the configured figures — the queue detail "+
			"endpoint reads this one, and a stub that dropped them would make its assertions "+
			"pass against a handler that sends nothing", st)
	}
	// ReevaluateStalls must tolerate a nil counter — most tests do not wire
	// one — and must count when one is present, or the R19 "on user action"
	// assertions in internal/api would pass against a handler that never asks.
	app.ReevaluateStalls()
	var calls atomic.Int64
	counting := NopApp{ReevaluatedVal: &calls}
	counting.ReevaluateStalls()
	if got := calls.Load(); got != 1 {
		t.Errorf("ReevaluateStalls calls = %d, want 1", got)
	}

	if free, err := app.DownloadDirFreeBytes(ctx); free != 0 || err != nil {
		t.Errorf("DownloadDirFreeBytes() = (%d, %v), want (0, nil)", free, err)
	}
	if speed, err := app.TestDownloadDirWriteSpeedMBPerSec(ctx); speed != 0.0 || err != nil {
		t.Errorf("TestDownloadDirWriteSpeedMBPerSec() = (%v, %v), want (0.0, nil)", speed, err)
	}
	if err := app.PingDB(ctx); err != nil {
		t.Errorf("PingDB() = %v, want nil", err)
	}
	if !app.IsPipelineHealthy(ctx) {
		t.Error("IsPipelineHealthy() = false, want true")
	}

	// 2. Safe no-op calls (verify no panic)
	app.SetSpeedLimit(100)
	app.SetBandwidthMax(200)
	app.SetBandwidthPerc(50)
	_ = app.SetDownloadDir("/tmp/down")
	app.SetCompleteDir("/tmp/comp")
	app.PauseDownloads()
	app.ResumeDownloads()
	app.DisconnectAll()
	app.ReloadPostProcOptions(config.PostProcConfig{}, "/tmp/scripts")
	app.ReloadDownloadOptions(config.DownloadConfig{})
	app.ReloadGeneralOptions(config.GeneralConfig{})

	// 3. Nil Dispatcher and History safety
	if err := app.AddJob(ctx, nil, dispatch.Header{}, nil, false); err != nil {
		t.Errorf("AddJob(nil dispatcher) = %v, want nil", err)
	}
	if err := app.RemoveJob(ctx, "job1", false); err != nil {
		t.Errorf("RemoveJob(nil dispatcher) = %v, want nil", err)
	}
	if err := app.RemoveHistoryJob(ctx, "job1", false); err != nil {
		t.Errorf("RemoveHistoryJob(nil history) = %v, want nil", err)
	}
	if err := app.MarkHistoryCompleted(ctx, "job1"); err != nil {
		t.Errorf("MarkHistoryCompleted(nil history) = %v, want nil", err)
	}

	// 4. Wired Dispatcher and History delegation
	disp, launched := newTestDispatcher(t)
	j := job.New("job1", "Test Job", job.Policy{})
	if err := disp.Add(context.Background(), j, dispatch.Header{Name: "Test Job"}); err != nil {
		t.Fatalf("disp.Add failed: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "hist.db")
	db, err := history.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("history.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	if err := repo.Add(ctx, history.Entry{NzoID: "hjob1", Name: "History Job"}); err != nil {
		t.Fatalf("repo.Add failed: %v", err)
	}

	wiredApp := NopApp{
		Dispatcher: disp,
		History:    repo,
	}

	wiredApp.PauseDownloads()
	if !disp.Paused() {
		t.Error("disp.Paused() = false after wired PauseDownloads(), want true")
	}
	wiredApp.ResumeDownloads()
	if disp.Paused() {
		t.Error("disp.Paused() = true after wired ResumeDownloads(), want false")
	}

	if name, err := wiredApp.RenameJob("job1", "Renamed"); err != nil || name != "Renamed" {
		t.Errorf("wired RenameJob() = %q, %v; want Renamed, nil", name, err)
	}
	if j.Name() != "Renamed" {
		t.Errorf("job1 name after wired RenameJob() = %q, want Renamed", j.Name())
	}

	// Remove a job the dispatcher has launched, which is the case that waits
	// for a worker to report back. Left to the race between the tick and this
	// goroutine, the launch happens only sometimes (#814).
	awaitLaunch(t, disp, launched, "job1")
	removeCtx, cancelRemove := context.WithTimeout(ctx, 10*time.Second)
	defer cancelRemove()
	if err := wiredApp.RemoveJob(removeCtx, "job1", false); err != nil {
		t.Errorf("wired RemoveJob() = %v, want nil", err)
	}
	if _, ok := disp.Job("job1"); ok {
		t.Error("job1 still in dispatcher after wired RemoveJob()")
	}

	if err := wiredApp.MarkHistoryCompleted(ctx, "hjob1"); err != nil {
		t.Errorf("wired MarkHistoryCompleted() = %v, want nil", err)
	}
	if e, err := repo.Get(ctx, "hjob1"); err != nil || e.Status != "Completed" {
		t.Errorf("hjob1 after wired MarkHistoryCompleted() = %+v, %v; want status Completed", e, err)
	}

	if err := wiredApp.RemoveHistoryJob(ctx, "hjob1", false); err != nil {
		t.Errorf("wired RemoveHistoryJob() = %v, want nil", err)
	}
	if _, err := repo.Get(ctx, "hjob1"); !errors.Is(err, history.ErrNotFound) {
		t.Errorf("hjob1 get err = %v; want history.ErrNotFound", err)
	}
}
