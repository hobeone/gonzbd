package app_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestCheckpoint_SurvivesCrashMidDownload verifies that a crash mid-job
// loses at most one checkpoint interval of per-article/per-file progress
// rather than the entire in-memory state.
//
// The basic checkpoint machinery (dirty flag + ticker) is tested by
// TestCheckpointFires_AfterMutation and TestCheckpointSkips_WhenClean in
// checkpoint_test.go.  This scenario-level test exercises the full
// crash-recovery path (no Shutdown call, reload from disk).
func TestCheckpoint_SurvivesCrashMidDownload(t *testing.T) {
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	const conns = 4
	server := nntptest.New(t)

	const n = 3
	msgIDs := make([]string, 0, n)
	files := make([]nzb.File, 0, n)
	var totalBytes int64
	for i := range n {
		msgID := randomMsgID(t)
		msgIDs = append(msgIDs, msgID)
		raw := fmt.Appendf(nil, "content %d", i)
		fileName := fmt.Sprintf("file%d.bin", i)
		server.AddArticle(msgID, yencSinglePart(fileName, raw))
		files = append(files, nzb.File{
			Subject:  fmt.Sprintf(`"%s" yEnc (1/1)`, fileName),
			Articles: []nzb.Article{{ID: msgID, Bytes: len(raw), Number: 1}},
			Bytes:    int64(len(raw)),
		})
		totalBytes += int64(len(raw))
	}

	// Stall article 1 mid-download.
	server.InjectFailure(msgIDs[1], nntptest.FailureStall)

	const checkInterval = 50 * time.Millisecond
	srvCfg := server.ServerConfig("scenario-checkpoint", conns)
	srvCfg.Timeout = 60 // Ensure stalled read outlasts crash-and-restart polling window.
	cfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		srvCfg,
	)

	a1, err := app.New(cfg, repo,
		app.WithCheckpointInterval(checkInterval),
		app.WithPostProcStages([]postproc.Stage{noOpStage{}}),
	)
	if err != nil {
		t.Fatalf("app.New 1: %v", err)
	}

	_, cancel1 := startAppAndDrain(t, a1)

	parsed := &nzb.NZB{Files: files}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "crash-recovery", Filename: "crash.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := a1.Queue().Add(job); err != nil {
		t.Fatalf("a1.Queue().Add: %v", err)
	}

	// Wait until stall has fired and checkpoint on disk captures Articles 0 and 2 complete.
	if !waitUntil(5*time.Second, func() bool {
		if server.StallCount() < 1 {
			return false
		}
		store := queue.NewSQLiteStore(repo.DB(), filepath.Join(adminDir, "queue"), repo)
		q, err := queue.Load(filepath.Join(adminDir, "queue"), queue.WithStore(store))
		if err != nil {
			return false
		}
		snap := q.SnapshotJob(job.ID)
		if snap == nil {
			return false
		}
		p := snap.Progress()
		return p.ArticleDone(0) && !p.ArticleDone(1) && p.ArticleDone(2)
	}) {
		t.Fatal("timed out waiting for checkpoint on disk to capture mid-download state")
	}

	// Simulate an ungraceful hard crash: stop downloader and assembler workers
	// first (so in-flight events are delivered while watchCompletions is running),
	// then cancel the application context. We deliberately do NOT call a1.Shutdown()
	// so queue.Save() is never invoked, preserving true no-flush hard-crash semantics.
	a1.ForceStopWorkers()
	cancel1()

	// Verify on disk: Articles 0 and 2 are durably marked Done, while Article 1 is NOT Done.
	diskQ := loadTestQueue(t, repo, adminDir)
	snap := diskQ.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("job missing from on-disk queue after crash")
	}
	if !snap.Progress().ArticleDone(0) {
		t.Fatal("expected Article 0 to be durably marked Done on disk after crash")
	}
	if snap.Progress().ArticleDone(1) {
		t.Fatal("expected Article 1 to NOT be marked Done on disk after crash")
	}
	if !snap.Progress().ArticleDone(2) {
		t.Fatal("expected Article 2 to be durably marked Done on disk after crash")
	}

	// Restart app.New() against clean NNTP server (stall is one-shot in nntptest).
	a2, err := app.New(cfg, repo,
		app.WithCheckpointInterval(checkInterval),
		app.WithPostProcStages([]postproc.Stage{noOpStage{}}),
	)
	if err != nil {
		t.Fatalf("app.New 2: %v", err)
	}

	ctx2, cancel2 := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel2()
		if err := a2.Shutdown(); err != nil {
			t.Errorf("a2.Shutdown: %v", err)
		}
	})
	if err := a2.Start(ctx2); err != nil {
		t.Fatalf("a2.Start: %v", err)
	}
	go drainAny(ctx2, a2.JobComplete())
	go drainAny(ctx2, a2.PostProcComplete())

	waitForHistoryAndQueueCleanup(t, repo, a2, job.ID)

	if c := server.FetchCount(msgIDs[0]); c != 1 {
		t.Errorf("expected msgID 0 to be fetched exactly once, got %d", c)
	}
	if c := server.FetchCount(msgIDs[1]); c != 2 {
		t.Errorf("expected msgID 1 to be fetched twice (once before crash, once after restart), got %d", c)
	}
	if c := server.FetchCount(msgIDs[2]); c != 1 {
		t.Errorf("expected msgID 2 to be fetched exactly once, got %d", c)
	}
}

type trackingStage struct {
	name  string
	count *int32
}

func (s *trackingStage) Name() string { return s.name }
func (s *trackingStage) Run(_ context.Context, _ *postproc.Job) error {
	atomic.AddInt32(s.count, 1)
	return nil
}

type crashStage struct {
	name      string
	count     *int32
	entered   chan struct{}
	blockOnce *uint32
}

func (s *crashStage) Name() string { return s.name }
func (s *crashStage) Run(ctx context.Context, _ *postproc.Job) error {
	atomic.AddInt32(s.count, 1)
	if atomic.CompareAndSwapUint32(s.blockOnce, 0, 1) {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		// Block until the application context is ungracefully cancelled.
		// In post-processing, returning an error only logs a warning and allows
		// downstream stages to proceed. By blocking on ctx.Done(), we simulate an
		// abrupt process kill mid-stage where onJobDone is never reached, leaving
		// PostProc=true in queue.json.gz for startup recovery.
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// TestCheckpoint_SurvivesCrashMidPostProc verifies mid-post-processing crash
// recovery by injecting a custom stage chain that blocks on its first run,
// simulating an ungraceful crash. Upon restart, Application.Start rescans the
// queue, sees PostProc == true, and re-enqueues the job to the post-processor,
// which restarts its stage chain from the beginning (stages[0]).
func TestCheckpoint_SurvivesCrashMidPostProc(t *testing.T) {
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	const jobID = "recover-postproc-0001"

	jobDlDir := filepath.Join(downloadDir, "recovery-mid-pp")
	if err := os.MkdirAll(jobDlDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jobDlDir, "dummy.bin"), []byte("test"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := queue.NewSQLiteStore(repo.DB(), filepath.Join(adminDir, "queue"), repo)
	seed := queue.New(queue.WithStore(store))
	seedCompletedJob(t, seed, jobID, "recovery-mid-pp", true)
	if err := seed.Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("seed.Save: %v", err)
	}

	cfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		nntptest.New(t).ServerConfig("recovery-mid-pp", 1),
	)

	var stage0Count int32
	var stage1Count int32
	enteredChan := make(chan struct{}, 1)

	stages := []postproc.Stage{
		&trackingStage{name: "stage0", count: &stage0Count},
		&crashStage{name: "crash-stage", count: &stage1Count, entered: enteredChan, blockOnce: new(uint32)},
	}

	a1, err := app.New(cfg, repo, app.WithPostProcStages(stages))
	if err != nil {
		t.Fatalf("app.New a1: %v", err)
	}

	ctx1, cancel1 := context.WithCancel(t.Context())
	if err := a1.Start(ctx1); err != nil {
		t.Fatalf("a1.Start: %v", err)
	}
	go drainAny(ctx1, a1.JobComplete())
	go drainAny(ctx1, a1.PostProcComplete())

	// Wait until post-processing stage 1 is entered and blocked.
	select {
	case <-enteredChan:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for stage 1 entry before crash")
	}

	// Verify stage 0 and 1 executed once before crash.
	if c := atomic.LoadInt32(&stage0Count); c != 1 {
		t.Errorf("expected stage0 count 1 before crash, got %d", c)
	}
	if c := atomic.LoadInt32(&stage1Count); c != 1 {
		t.Errorf("expected stage1 count 1 before crash, got %d", c)
	}

	// Simulate an ungraceful hard crash: stop downloader and assembler workers
	// first, then cancel the application context. We deliberately do NOT call
	// a1.Shutdown() so queue.Save() is never invoked.
	a1.ForceStopWorkers()
	cancel1()

	// Verify on-disk queue state after crash: job must still exist and have PostProc == true.
	diskQ := loadTestQueue(t, repo, adminDir)
	snap := diskQ.SnapshotJob(jobID)
	if snap == nil {
		t.Fatal("job missing from on-disk queue after crash")
	}
	if !snap.PostProc {
		t.Fatal("expected PostProc=true on disk after crash, got false")
	}

	// Attempt 2: Restart application with the same stage chain.
	a2, err := app.New(cfg, repo, app.WithPostProcStages(stages))
	if err != nil {
		t.Fatalf("app.New a2: %v", err)
	}

	_, cancel2 := startAppAndDrain(t, a2)
	t.Cleanup(func() {
		cancel2()
		if err := a2.Shutdown(); err != nil {
			t.Errorf("a2.Shutdown: %v", err)
		}
	})

	waitForHistoryAndQueueCleanup(t, repo, a2, jobID)

	// Verify both stages ran again from the beginning (stages[0]).
	if val := atomic.LoadInt32(&stage0Count); val != 2 {
		t.Errorf("expected stage0Count=2 after restart, got %d", val)
	}
	if val := atomic.LoadInt32(&stage1Count); val != 2 {
		t.Errorf("expected stage1Count=2 after restart, got %d", val)
	}
}
