package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// erofsSyncTarget is the assembler's real target for a job, except that every
// fsync answers EROFS, which storagefault classifies permanent.
type erofsSyncTarget struct {
	durability.SyncTarget
}

func (erofsSyncTarget) Sync(context.Context, int32) error { return syscall.EROFS }

// stageRecorder is a post-processing stage that records every job it runs for,
// and holds the one named by hold until its context ends. processJob runs no
// stage for a job whose FailMsg is set, so a recorded job is one that was
// handed to repair and unpack.
type stageRecorder struct {
	hold    string
	holding chan struct{}

	mu   sync.Mutex
	runs []string
}

func newStageRecorder(hold string) *stageRecorder {
	return &stageRecorder{hold: hold, holding: make(chan struct{})}
}

func (*stageRecorder) Name() string { return "recorder" }

func (s *stageRecorder) Run(ctx context.Context, j *postproc.Job) error {
	s.mu.Lock()
	s.runs = append(s.runs, j.JobID())
	s.mu.Unlock()
	if j.JobID() == s.hold {
		close(s.holding)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (s *stageRecorder) ran() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.runs)
}

// buildShutdownFailJob builds a one-file job of nArts 100-byte articles.
func buildShutdownFailJob(t *testing.T, cfg *config.Config, name string, nArts int) (*job.Job, dispatch.Header) {
	t.Helper()
	file := nzb.File{Subject: name + ".bin", Bytes: int64(nArts) * 100}
	for a := range nArts {
		file.Articles = append(file.Articles, nzb.Article{ID: name + string(rune('0'+a)) + "@t", Bytes: 100, Number: a + 1})
	}
	j, hdr, err := BuildIngestJob(cfg, &nzb.NZB{Files: []nzb.File{file}}, name+".nzb", types.FetchOptions{NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	return j, hdr
}

// TestFail_InTheCleanShutdownBarrier_DoesNotPersistAPartialJobForPostProcessing
// drives a permanent storage fault through Barrier.routeFault into
// Application.Fail while Shutdown is in its clean-shutdown checkpoint — stopping
// set, app.ctx live, the assembler still running, and CloseJobHandles giving
// up as it does against a wedged mount — and then reads what Shutdown
// persisted.
//
// The job has one of its two articles written and neither acked. What must not
// happen is the job being persisted positioned for post-processing with that
// article still unfinished, or reaching a post-processing stage during the
// shutdown: that is a partial download handed to par2. The two acceptable
// outcomes are the ones Fail and a clean stop each intend — the job in history
// as Failed, or back in the queue at Fetching with its outstanding articles
// offered to the downloader.
//
// Which one happens depends on whether the post-processor reaches the job
// before Shutdown stops it, so both are driven: an idle post-processor files
// it, and one busy with another job leaves it pending when it stops.
func TestFail_InTheCleanShutdownBarrier_DoesNotPersistAPartialJobForPostProcessing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		postProcBusy  bool
		wantInHistory bool
	}{
		{name: "post-processor idle", postProcBusy: false, wantInHistory: true},
		{name: "post-processor busy", postProcBusy: true, wantInHistory: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runShutdownFailRestart(t, tc.postProcBusy, tc.wantInHistory)
		})
	}
}

func runShutdownFailRestart(t *testing.T, postProcBusy, wantInHistory bool) {
	t.Helper()
	dl, comp, admin := t.TempDir(), t.TempDir(), t.TempDir()
	cfg := testConfig(dl, comp, admin)
	db, err := history.Open(t.Context(), filepath.Join(admin, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	j, hdr := buildShutdownFailJob(t, cfg, "faulted", 2)
	blocker, blockerHdr := buildShutdownFailJob(t, cfg, "blocker", 1)
	rec1 := newStageRecorder(blocker.ID())
	a1, err := New(cfg, repo, WithDownloader(newFakeDownloader()),
		WithPostProcStages([]postproc.Stage{rec1}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A wedged mount holds the assembler worker, so CloseJobHandles waits out
	// its budget and returns an error. A budget of one nanosecond takes that
	// branch without the wait.
	a1.closeHandlesTimeout = time.Nanosecond
	for _, add := range []struct {
		j *job.Job
		h dispatch.Header
	}{{j, hdr}, {blocker, blockerHdr}} {
		if err := a1.AddJob(t.Context(), add.j, add.h, nil, false); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
	}
	ctx1, cancel1 := context.WithCancel(t.Context())
	defer cancel1()
	if err := a1.Start(ctx1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	writeFixtureArticle(t, a1, j.ID(), 0, 0)
	// Launched, so the job has an attempt at Fetching for anything on the
	// Fail path to advance — which is the hazard.
	waitFor(t, func() bool {
		row, ok := a1.Dispatcher().Row(j.ID())
		return ok && row.View.State == job.Fetching
	})

	if postProcBusy {
		// A non-empty download directory, or processJob skips every stage.
		dir := filepath.Join(dl, blocker.Name())
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x.bin"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		a1.maybeFinalize(blocker.ID(), "")
		select {
		case <-rec1.holding:
		case <-time.After(10 * time.Second):
			t.Fatal("the post-processor never started the blocking job")
		}
	}

	var failRouted bool
	a1.checkpointHook = func() {
		if !a1.stopping.Load() || a1.ctx.Err() != nil {
			t.Error("the hook is not in the window this test is about: stopping set, ctx live")
		}
		mu := a1.jobBarrierLock(j.ID())
		mu.Lock()
		runErr := a1.barrier.Run(context.Background(), j.ID(), erofsSyncTarget{a1.syncTargetFor(j.ID())})
		mu.Unlock()
		a1.releaseJobBarrierLock(j.ID())
		failRouted = runErr != nil
		if !postProcBusy {
			// Held here, inside the barrier window, until the idle
			// post-processor has filed the job — so the variant does not
			// depend on how fast the rest of Shutdown reaches its Stop.
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := repo.Get(context.Background(), j.ID()); err == nil {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Error("the idle post-processor never filed the failed job")
		}
	}
	if err := a1.Shutdown(); err != nil {
		t.Logf("Shutdown: %v", err)
	}
	if !failRouted {
		t.Fatal("the barrier did not fail, so no permanent fault reached Application.Fail")
	}
	if slices.Contains(rec1.ran(), j.ID()) {
		t.Error("a post-processing stage ran during shutdown for a job with an unfinished article")
	}

	// What Shutdown persisted, read from the store before anything restarts:
	// a restart acts on it, so an assertion made afterwards would see the
	// restart's own moves.
	persisted, err := dispatchstore.New(repo.DB(), nil).Load(t.Context())
	if err != nil {
		t.Fatalf("load persisted queue: %v", err)
	}
	var queued *dispatch.Persisted
	for i := range persisted {
		if persisted[i].ID == j.ID() {
			queued = &persisted[i]
		}
	}
	entry, histErr := repo.Get(t.Context(), j.ID())
	inHistory := histErr == nil
	if (queued != nil) == inHistory {
		t.Fatalf("queued=%v, in history=%v; want exactly one", queued != nil, inHistory)
	}
	// Fetching is where the fixture launched it; anything further, or a
	// recorded next, positions a partial download for post-processing.
	if queued != nil && (queued.State.State != job.Fetching || queued.State.Next != job.StateUnset) {
		t.Fatalf("the job was persisted at %+v with an article never written; the "+
			"restart positions a partial download for post-processing", queued.State)
	}
	if inHistory != wantInHistory {
		t.Fatalf("in history = %v, want %v; the fixture did not drive the ordering it names",
			inHistory, wantInHistory)
	}
	if inHistory {
		if entry.Status != string(constants.StatusFailed) {
			t.Errorf("history status = %s, want Failed", entry.Status)
		}
		return
	}

	// Restarted, to show the persisted job is offered to the downloader again.
	a2, err := New(cfg, repo, WithDownloader(newFakeDownloader()),
		WithPostProcStages([]postproc.Stage{newStageRecorder("")}))
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	if err := a2.Start(ctx2); err != nil {
		t.Fatalf("Start after restart: %v", err)
	}
	t.Cleanup(func() { _ = a2.Shutdown() })
	rj, ok := a2.Dispatcher().Job(j.ID())
	if !ok {
		t.Fatal("the persisted job is not queued after the restart")
	}
	// The downloader reads articles of a job that holds a lease, which the
	// dispatcher hydrates; the startup sweep releases the hydration it made, so the job is loaded only
	// once the dispatcher grants it a lease.
	waitFor(t, rj.Resident)
	var offered int
	if err := rj.ForEachUnfinishedArticle(func(int, int32, string, int, int, string) bool {
		offered++
		return true
	}); err != nil {
		t.Fatalf("ForEachUnfinishedArticle: %v", err)
	}
	if offered == 0 {
		t.Error("the restored job offers no unfinished article to the downloader")
	}
}
