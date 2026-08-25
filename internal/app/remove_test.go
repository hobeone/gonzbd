package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// newRemoveJobTestApp builds an Application wired for RemoveJob tests:
// a fresh download/admin dir pair, an open history repo, and default config.
func newRemoveJobTestApp(t *testing.T) *Application {
	t.Helper()

	dir := t.TempDir()
	downloadDir := filepath.Join(dir, "download")
	adminDir := filepath.Join(dir, "admin")
	if err := os.MkdirAll(downloadDir, 0o750); err != nil {
		t.Fatalf("create download directory: %v", err)
	}
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		t.Fatalf("create admin directory: %v", err)
	}

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("open history database: %v", err)
	}
	repo := history.NewRepository(db)
	cfg := testConfig(
		downloadDir,
		filepath.Join(dir, "complete"),
		adminDir,
		config.ServerConfig{Name: "test"},
	)
	a, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New application failed: %v", err)
	}
	return a
}

func TestRemoveJob(t *testing.T) {
	a := newRemoveJobTestApp(t)
	downloadDir := a.config.GetGeneral().DownloadDir

	parsed := &nzb.NZB{}
	job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "to-delete"}, fsutil.SanitizeOptions{})
	_ = a.queue.Add(job)

	// Create a dummy download directory
	jobDir := filepath.Join(downloadDir, "to-delete")
	_ = os.MkdirAll(jobDir, 0o750)
	dummyFile := filepath.Join(jobDir, "data.bin")
	_ = os.WriteFile(dummyFile, []byte("data"), 0o600)

	// 1. Remove with deleteFiles=true
	err := a.RemoveJob(t.Context(), job.ID, true)
	if err != nil {
		t.Fatalf("RemoveJob failed: %v", err)
	}
	if a.queue.Len() != 0 {
		t.Errorf("queue len = %d, want 0", a.queue.Len())
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Errorf("job directory was NOT deleted but should have been")
	}

	// 2. Add again and remove
	job2, _ := queue.NewJob(parsed, queue.AddOptions{Name: "to-delete-files"}, fsutil.SanitizeOptions{})
	_ = a.queue.Add(job2)
	jobDir2 := filepath.Join(downloadDir, "to-delete-files")
	_ = os.MkdirAll(jobDir2, 0o750)

	err = a.RemoveJob(t.Context(), job2.ID, false)
	if err != nil {
		t.Fatalf("RemoveJob failed: %v", err)
	}
	if _, err := os.Stat(jobDir2); os.IsNotExist(err) {
		t.Errorf("job directory was deleted but should have been kept (deleteFiles=false)")
	}
}

// TestRemoveJob_NilDownloader pins the nil-guard on RemoveJob's final
// DisconnectAll call. New() always wires a real downloader, so app.downloader
// is nilled out directly (white-box, same package) after construction to
// exercise the branch — the same technique TestBuildDownloaderOptions_Defaults
// uses elsewhere in this package. Before the #98 fix this call was
// unsynchronized and unconditional (app.downloader.DisconnectAll()), which
// would have panicked here.
func TestRemoveJob_NilDownloader(t *testing.T) {
	a := newRemoveJobTestApp(t)
	a.downloader = nil

	parsed := &nzb.NZB{}
	job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "to-delete"}, fsutil.SanitizeOptions{})
	_ = a.queue.Add(job)

	if err := a.RemoveJob(t.Context(), job.ID, false); err != nil {
		t.Fatalf("RemoveJob with nil downloader: %v", err)
	}
}

// errStoreDeleteRefused is the injected store-level failure. A distinct
// sentinel rather than a bare errors.New so the assertion below can tell
// RemoveJob's propagated error apart from any other failure on the path.
var errStoreDeleteRefused = errors.New("store refused the delete")

// removeRefusingStore fails Remove and delegates everything else to a real
// store, modelling the store-level delete failure RemoveJob has to survive
// without having already destroyed the job's files.
//
// Remove is the only method overridden: the fixture needs Add, List and
// IsPaused to behave normally so queue.Load and queue.Add work, and the
// embedded interface supplies them.
type removeRefusingStore struct {
	queue.Store
}

func (removeRefusingStore) Remove(context.Context, string) error {
	return errStoreDeleteRefused
}

// TestRemoveJob_StoreDeleteFailureLeavesTheJobsFilesOnDisk pins the ordering
// #376 is about: the step that can fail must run before the step that cannot
// be undone.
//
// Before the fix RemoveJob ran assembler.CancelJob first, whose control arm
// unlinks every file it currently holds open for the job, and only then
// called queue.Remove. A store-delete failure therefore returned an error
// having already deleted the job's bytes, leaving the job resident and still
// dispatchable with nothing on disk to write into.
//
// The assertion that discriminates is the os.Stat on the file: the job
// staying resident is true both before and after the fix, because the store
// delete fails either way. Only the file's survival tells them apart.
func TestRemoveJob_StoreDeleteFailureLeavesTheJobsFilesOnDisk(t *testing.T) {
	application, repo, adminDir := newLifecycleTestApp(t)
	ctx := t.Context()

	// New applies its option funcs before it builds the queue, so a store
	// substitution cannot ride in as an option — the queue is replaced after
	// construction instead. pipeline holds its own *queue.Queue pointer, so
	// both are swapped together; leaving them pointing at different queues
	// would make the fixture's write path and RemoveJob disagree about which
	// jobs exist.
	//
	// Three components keep the queue New built: app.downloader, app.barrier,
	// and the pipeline's onJobHopeless closure, which captures the queue
	// directly rather than reading p.queue. None of them runs here — this
	// test starts no dispatch and no completion consumer — so the fixture is
	// sound for what it asserts. Anything that later extends it onto either
	// of those paths must swap them too, or it will exercise the original
	// queue and pass while testing nothing.
	stateDir := filepath.Join(adminDir, "queue")
	refusing := removeRefusingStore{Store: queue.NewSQLiteStore(repo.DB(), stateDir, repo)}
	q, err := queue.Load(stateDir, queue.WithStore(refusing), queue.WithLogger(application.log))
	if err != nil {
		t.Fatalf("queue.Load: %v", err)
	}
	application.queue = q
	application.pipeline.queue = q

	job, path := removeJobFixture(t, application, "refused")

	// deleteFiles=TRUE, and that is load-bearing rather than incidental.
	//
	// This test pins an ORDERING: queue.Remove must precede the assembler
	// cancel, so a store-delete failure returns with the job's bytes intact.
	// It can only detect a violation if the cancel is destructive when it
	// runs, and since #433 that is exactly what deleteFiles selects. Passing
	// false would make the cancel keep the file, the assertion below would
	// hold whatever the order, and the pin would be silently gone.
	//
	// The sweep this also enables is not reached: RemoveJob returns at
	// queue.Remove, well above it.
	err = application.RemoveJob(ctx, job.ID, true)
	if !errors.Is(err, errStoreDeleteRefused) {
		t.Fatalf("RemoveJob err = %v, want it to propagate %v — the fixture did not "+
			"reach the failure it exists to create", err, errStoreDeleteRefused)
	}
	if application.queue.SnapshotJob(job.ID) == nil {
		t.Error("the job left the queue although its store delete failed; " +
			"the error RemoveJob returned would be describing a removal that happened")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the job's file is gone after a failed RemoveJob (stat: %v). The job is "+
			"still resident and still dispatchable, so the downloader will write into a "+
			"file the assembler has to recreate from zero, against progress counting "+
			"bytes that no longer exist", err)
	}
}

// removeJobFixture adds a two-article job whose single file the assembler is
// holding open, and returns the job with the resolved path of that file.
//
// Two articles of which one is written, so the file is mid-download rather
// than finished — the state a removal actually interrupts.
//
// That is realism, not the mechanism. What keeps the file in the assembler's
// open map is that nothing has closed it: a FINISHED file's handle is retained
// too, because OnFileComplete's contract leaves it open until CloseFile, and
// both production callers of CloseFile — `git grep -n 'CloseFile(' -- '*.go'`
// outside tests returns finalizeCompletedFile's defer at durability.go and the
// stall retry at stall.go — sit behind goroutines that only Application.Start
// launches, which these tests never call. So one article would serve equally.
//
// The path must be resolved before RemoveJob runs: RemoveJob's
// pipeline.forgetJob drops the cached FileInfo it comes from.
func removeJobFixture(t *testing.T, application *Application, name string) (*queue.Job, string) {
	t.Helper()

	if err := application.assembler.Start(t.Context()); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.assembler.Stop() })

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: name + ".bin",
		Bytes:   200,
		Articles: []nzb.Article{
			{ID: name + "0@t", Bytes: 100, Number: 1},
			{ID: name + "1@t", Bytes: 100, Number: 2},
		},
	}}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: name}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.queue.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Blocks until the assembler's own view says the file is open, rather
	// than statting the path — the open map is the set the cancel arm acts
	// on, so it is the condition these tests actually depend on.
	writeFixtureArticle(t, application, job.ID, 0, 0)
	info, err := application.pipeline.resolveFileInfo(job.ID, 0)
	if err != nil {
		t.Fatalf("resolveFileInfo: %v", err)
	}
	return job, info.Path
}

// TestRemoveJob_KeepingFilesLeavesAFileTheAssemblerHoldsOpen pins #433.
//
// deleteFiles=false gates exactly one step of RemoveJob — the whole-directory
// sweep at the end. The assembler cancel that runs before it unlinked every
// file it held open for the job regardless of the flag, so a caller asking to
// keep the job's bytes got the in-flight file destroyed anyway.
//
// The os.Stat is the assertion that discriminates. The job leaves the queue
// on both the fixed and the unfixed code, so only the file's survival tells
// them apart.
func TestRemoveJob_KeepingFilesLeavesAFileTheAssemblerHoldsOpen(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	job, path := removeJobFixture(t, application, "kept")

	if err := application.RemoveJob(t.Context(), job.ID, false); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the job's in-flight file is gone after RemoveJob(deleteFiles=false) "+
			"(stat: %v). The caller asked to keep the job's bytes and the only file "+
			"still being written was unlinked anyway, which is the one whose bytes "+
			"nothing else holds a copy of", err)
	}
}

// TestRemoveJob_DeletingFilesRemovesAFileTheAssemblerHoldsOpen is the
// companion to the test above, and pins the user-visible outcome of
// deleteFiles=true: the bytes are gone.
//
// It deliberately does NOT pin the assembler's own unlink, and cannot —
// RemoveJob's whole-directory sweep removes the file whatever the cancel arm
// did, so neutering that unlink entirely leaves this green. Measured, not
// assumed. TestCancelJob_ClosesOpenFileHandles is what pins the arm, at the
// layer where the sweep is not in the way.
//
// What this covers instead is the pair: that fixing #433 did not quietly
// invert the true case while making the false case work.
func TestRemoveJob_DeletingFilesRemovesAFileTheAssemblerHoldsOpen(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	job, path := removeJobFixture(t, application, "swept")

	if err := application.RemoveJob(t.Context(), job.ID, true); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the job's in-flight file survives RemoveJob(deleteFiles=true) "+
			"(stat err: %v, want IsNotExist). The caller asked for the job's bytes "+
			"to be removed", err)
	}
}
