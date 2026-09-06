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
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
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
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	server := nntptest.New(t)

	// Three 1-article files: 0, 1, 2.
	// We will stall article 1 on the server so 0 and 2 complete, then crash.
	msgIDs := make([]string, 3)
	parts := make([][]byte, 3)
	files := make([]nzb.File, 3)
	for i := range 3 {
		msgIDs[i] = randomMsgID(t)
		parts[i] = []byte(fmt.Sprintf("part-%d-payload-padding-data", i))
		filename := fmt.Sprintf("file%d.bin", i)
		server.AddArticle(msgIDs[i], yencSinglePart(filename, parts[i]))
		files[i] = nzb.File{
			Subject:  filename,
			Bytes:    int64(len(parts[i])),
			Articles: []nzb.Article{{ID: msgIDs[i], Bytes: len(parts[i]), Number: 1}},
		}
	}

	// Article 1 stalls — worker holds the connection open, no complete event arrives.
	server.InjectFailure(msgIDs[1], nntptest.FailureStall)

	// Checkpoint interval must be short enough to reliably save before crash.
	const checkInterval = 20 * time.Millisecond

	srvCfg := server.ServerConfig("srv", 2)
	srvCfg.Timeout = 120 // stall must outlast the test

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
	j, hdr := buildTestJob(t, cfg, parsed, types.FetchOptions{NzbName: "crash-recovery"})
	hdr.Filename = "crash.nzb"
	if err := a1.AddJob(t.Context(), j, hdr, []byte("<nzb/>"), false); err != nil {
		t.Fatalf("a1.AddJob: %v", err)
	}

	// Wait until stall has fired and the state ON DISK is the one this test
	// means to crash into: files 0 and 2 finished and marked complete, file 1
	// still outstanding.
	if !waitUntil(5*time.Second, func() bool {
		if server.StallCount() < 1 {
			return false
		}
		jobInst, ok := a1.Dispatcher().Job(j.ID())
		if !ok {
			return false
		}
		p := jobInst.Progress()
		if !p.ArticleDone(0) || p.ArticleDone(1) || !p.ArticleDone(2) {
			return false
		}
		runs, err := durability.NewSQLiteRunStore(repo.DB()).ForJob(t.Context(), j.ID())
		if err != nil {
			return false
		}
		var has0, has2 bool
		for _, r := range runs {
			if r.FileIdx == 0 {
				has0 = true
			}
			if r.FileIdx == 2 {
				has2 = true
			}
		}
		if !has0 || !has2 {
			return false
		}
		var c0, c2 int
		_ = repo.DB().QueryRowContext(t.Context(),
			`SELECT complete FROM job_files WHERE job_id = ? AND file_index = 0`, j.ID()).Scan(&c0)
		_ = repo.DB().QueryRowContext(t.Context(),
			`SELECT complete FROM job_files WHERE job_id = ? AND file_index = 2`, j.ID()).Scan(&c2)
		return c0 == 1 && c2 == 1
	}) {
		jobInst, _ := a1.Dispatcher().Job(j.ID())
		var p0, p1, p2 bool
		if jobInst != nil {
			p := jobInst.Progress()
			p0, p1, p2 = p.ArticleDone(0), p.ArticleDone(1), p.ArticleDone(2)
		}
		runs, _ := durability.NewSQLiteRunStore(repo.DB()).ForJob(t.Context(), j.ID())
		t.Fatalf("timed out waiting for checkpoint on disk to capture mid-download state: stalls=%d p0=%v p1=%v p2=%v f0=%d f1=%d f2=%d runs=%+v",
			server.StallCount(), p0, p1, p2,
			server.FetchCount(msgIDs[0]), server.FetchCount(msgIDs[1]), server.FetchCount(msgIDs[2]), runs)
	}

	// Simulate an ungraceful hard crash: stop downloader and assembler workers
	// first (so in-flight events are delivered while watchCompletions is running),
	// then cancel the application context. We deliberately do NOT call a1.Shutdown()
	// so no quiet shutdown flush runs, preserving true no-flush hard-crash semantics.
	a1.ForceStopWorkers()
	cancel1()

	// Verify on disk: Articles 0 and 2 are durably marked Done, while Article 1 is NOT Done.
	runs, err := durability.NewSQLiteRunStore(repo.DB()).ForJob(t.Context(), j.ID())
	if err != nil {
		t.Fatalf("load runs from disk: %v", err)
	}
	var has0, has1, has2 bool
	for _, r := range runs {
		if r.FileIdx == 0 {
			has0 = true
		}
		if r.FileIdx == 1 {
			has1 = true
		}
		if r.FileIdx == 2 {
			has2 = true
		}
	}
	if !has0 {
		t.Fatal("expected Article 0 to be durably marked Done on disk after crash")
	}
	if has1 {
		t.Fatal("expected Article 1 to NOT be marked Done on disk after crash")
	}
	if !has2 {
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

	waitForHistoryAndQueueCleanup(t, repo, a2, j.ID())

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
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// TestCheckpoint_SurvivesCrashMidPostProc verifies mid-post-processing crash
// recovery by injecting a custom stage chain that blocks on its first run,
// simulating an ungraceful crash.
func TestCheckpoint_SurvivesCrashMidPostProc(t *testing.T) {
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	const jobID = "recover-postproc-0001"

	jobDlDir := filepath.Join(downloadDir, "recovery-mid-pp")
	if err := os.MkdirAll(jobDlDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jobDlDir, "dummy.bin"), []byte("test"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	seedCompletedJob(t, repo, adminDir, jobID, "recovery-mid-pp", job.Repairing)

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
	// a1.Shutdown() so no quiet shutdown flush runs.
	a1.ForceStopWorkers()
	cancel1()

	// Verify on-disk queue state after crash: job must still exist in dispatch_jobs.
	store := dispatchstore.New(repo.DB())
	rows, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ID == jobID {
			found = true
			if r.State.State != job.Repairing {
				t.Fatalf("expected state Repairing on disk after crash, got %v", r.State.State)
			}
			break
		}
	}
	if !found {
		t.Fatal("job missing from on-disk queue after crash")
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
