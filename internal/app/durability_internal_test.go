package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
)

// newDurabilityTestApp builds an Application over a real SQLite-backed queue
// and history, with one job of nFiles files each holding nArts articles, and
// the assembler started.
func newDurabilityTestApp(t *testing.T, nFiles, nArts int) (*Application, *job.Job) {
	t.Helper()
	application, _, _ := newLifecycleTestApp(t)
	if err := application.assembler.Start(t.Context()); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.assembler.Stop() })

	parsed := &nzb.NZB{}
	for f := range nFiles {
		file := nzb.File{Subject: fileFixtureName(f), Bytes: int64(nArts) * 100}
		for a := range nArts {
			file.Articles = append(file.Articles, nzb.Article{
				ID: fileFixtureArticleID(f, a), Bytes: 100, Number: a + 1,
			})
		}
		parsed.Files = append(parsed.Files, file)
	}
	j, hdr, err := BuildIngestJob(application.config, parsed, "durability-unit.nzb", types.FetchOptions{NzbName: "durability-unit"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return application, j
}

func fileFixtureName(f int) string { return string(rune('A'+f)) + ".bin" }
func fileFixtureArticleID(f, a int) string {
	return fmt.Sprintf("%c%d@t", 'A'+f, a)
}

// writeFixtureArticle hands one article of one file to the assembler, at the
// offset the fixture's uniform 100-byte articles imply.
func writeFixtureArticle(t *testing.T, application *Application, jobID string, fileIdx, globalArt int) {
	t.Helper()
	j, ok := application.dispatcher.Job(jobID)
	if !ok {
		t.Fatalf("job %s not in the dispatcher", jobID)
	}
	if err := application.pipeline.registerFile(j, fileIdx); err != nil {
		t.Fatalf("registerFile %d: %v", fileIdx, err)
	}
	// Offset 0: this helper writes one article at the start of its file, so
	// the offset is file-local and does not follow the global article index.
	ref, req := assemblerWrite(jobID, fileIdx, globalArt, 0)
	if err := application.assembler.WriteArticle(t.Context(), ref, req); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	// WriteArticle returns once the worker has ACCEPTED the request, not once
	// it has written it. Quiesce waits for the worker to process it, so the
	// file exists before the caller looks at it.
	if err := application.assembler.Quiesce(t.Context()); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
}

// assemblerWrite is one 100-byte article of a fixture file, as the pipeline
// would hand it to the assembler.
func assemblerWrite(jobID string, fileIdx, globalArt int, offset int64) (assembler.ArticleRef, assembler.WriteRequest) {
	return assembler.ArticleRef{
		JobID: jobID, FileIdx: fileIdx, ArtIdx: int32(globalArt), //nolint:gosec // G115: test article counts are tiny
		MessageID: string(rune('a'+globalArt)) + "@t",
	}, assembler.WriteRequest{
		Offset: offset,
		Data:   make([]byte, 100),
	}
}

// ---------- Stall / Fail ----------

// TestStall_PausesTheJobAndSurfacesTheReason pins A1's storage half and R27.
//
// A full disk resolves against storage: the job stops making requests and the
// user is told which file and which condition. No article may be touched —
// marking one failed would burn its retry budget and degrade the job's
// reported health from something the user fixes in seconds.
func TestStall_PausesTheJobAndSurfacesTheReason(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	application.Stall(job.ID(), storagefault.Classify("sync", "/mnt/full/A.bin", syscall.ENOSPC))

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("job left the dispatcher")
	}
	if row.Status() != constants.StatusPaused {
		t.Errorf("status = %v after a retryable fault, want Paused — the job keeps "+
			"dispatching articles into a device that cannot take them", row.Status())
	}
	stallReason := application.StallReason(job.ID()).Reason
	if stallReason == "" {
		t.Fatal("no stall reason was surfaced; the job is paused for no visible reason (R27)")
	}
	for _, want := range []string{"/mnt/full/A.bin", "sync"} {
		if !strings.Contains(stallReason, want) {
			t.Errorf("stall reason %q does not mention %q; the user cannot act on it", stallReason, want)
		}
	}
	for i := range 2 {
		if job.Progress().ArticleFailed(i) {
			t.Errorf("article %d was marked failed by a storage fault (A1, R21)", i)
		}
		if job.Progress().ArticleDone(i) {
			t.Errorf("article %d was marked done by a storage fault", i)
		}
	}
	if job.Progress().FailedBytes() != 0 {
		t.Errorf("failed bytes = %d after a storage fault, want 0 (R21)", job.Progress().FailedBytes())
	}
}

// TestFail_SurfacesTheReasonAndStillFailsNoArticle pins R20 alongside A1. A
// read-only filesystem is permanent, so the job stops rather than waiting —
// but it says nothing about any article's availability, so the health figure
// must keep describing the download rather than the disk.
func TestFail_SurfacesTheReasonAndStillFailsNoArticle(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	application.Fail(job.ID(), storagefault.Classify("write", "/mnt/ro/A.bin", syscall.EROFS))

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		// maybeFinalize can carry the job straight out of the queue, which is
		// the intended terminal path; the article assertions below need a
		// snapshot, so the reason is checked against history instead.
		t.Skip("job left the dispatcher for history; see TestFail on a resident job")
	}
	if !strings.Contains(row.Header.FailReason, "/mnt/ro/A.bin") {
		t.Errorf("fail reason %q does not name the file (R27)", row.Header.FailReason)
	}
	for i := range 2 {
		if job.Progress().ArticleFailed(i) {
			t.Errorf("article %d was marked failed by a permanent storage fault (A1, R20)", i)
		}
	}
	if job.Progress().FailedBytes() != 0 {
		t.Errorf("failed bytes = %d, want 0 (R21)", job.Progress().FailedBytes())
	}
}

// ---------- byte accounting ----------

// ---------- per-job state ----------

// ---------- target construction ----------

// ---------- cadence ----------

// ---------- completion ----------

// ---------- job departure ----------

// jobFilesCount reports how many per-file progress rows a job still has.
func jobFilesCount(t *testing.T, application *Application, jobID string) int {
	t.Helper()
	var n int
	if err := application.historyRepo.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_files WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDropJobAlreadyInHistory_DoesNothingWithoutAHistoryDatabase pins the
// guard inside the method. With no history database there is no history to
// find the job in, and without the guard the call dereferences a nil
// repository.
func TestDropJobAlreadyInHistory_DoesNothingWithoutAHistoryDatabase(t *testing.T) {
	t.Parallel()
	for name, repo := range map[string]*history.Repository{
		"no repository":           nil,
		"repository, no database": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			application := &Application{historyRepo: repo, log: slog.New(slog.DiscardHandler)}
			application.dropJobAlreadyInHistory(t.Context(), "job-a")
		})
	}
}

// ---------- settings ----------

// ---------- error paths ----------

// failingRunStore fails every read and delete of durable_runs, for the paths
// that must degrade to a re-fetch rather than to a wrong answer. The other two
// tables' operations reach the embedded real store, so a test can still watch
// those rows go while the run record refuses.
type failingRunStore struct {
	durabilityStore
	err error
}

func (f failingRunStore) ForFile(context.Context, string, int32) ([]durability.Run, error) {
	return nil, f.err
}

func (f failingRunStore) ForJob(context.Context, string) ([]durability.Run, error) {
	return nil, f.err
}

func (f failingRunStore) DiscardRuns(context.Context, string) error { return f.err }

func (f failingRunStore) Reclaim(context.Context, string, ...string) error { return f.err }

func (f failingRunStore) SweepOrphans(context.Context) error { return f.err }

// TestStall_ReportsRatherThanPanicsOnAJobThatHasLeft pins the case a storage
// fault most easily hits: the barrier finds the fault, and by the time the
// stall reaches the queue the job has been removed. Both queue writes fail and
// neither may take the process down or abort the other.
func TestStall_ReportsRatherThanPanicsOnAJobThatHasLeft(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)

	application.Stall("no-such-job", storagefault.Classify("sync", "/x", syscall.ENOSPC))
	application.Fail("no-such-job", storagefault.Classify("write", "/x", syscall.EROFS))
}

// ---------- event emission ----------

// recordingEmitter captures the events an Application broadcasts.
type recordingEmitter struct{ events []Event }

func (r *recordingEmitter) Broadcast(e Event) { r.events = append(r.events, e) }

// TestEmit_ReachesTheRegisteredEmitter pins the path every UI update takes.
//
// A stall is the case that makes it load-bearing rather than cosmetic: the job
// is paused with a reason nobody asked for, so the only way a user learns of
// it is a pushed queue update. Without the broadcast the queue silently stops
// and the UI keeps showing the pre-stall state until something else happens to
// refresh it.
func TestEmit_ReachesTheRegisteredEmitter(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	rec := &recordingEmitter{}
	application.emitter = rec

	application.emit(Event{Type: "queue_updated", NzoID: "direct"})
	if len(rec.events) != 1 || rec.events[0].Type != "queue_updated" || rec.events[0].NzoID != "direct" {
		t.Fatalf("emit delivered %+v, want one queue_updated for \"direct\"", rec.events)
	}

	application.Stall(job.ID(), storagefault.Classify("sync", "/mnt/full/A.bin", syscall.ENOSPC))
	var sawStallUpdate bool
	for _, e := range rec.events[1:] {
		if e.Type == "queue_updated" && e.NzoID == job.ID() {
			sawStallUpdate = true
		}
	}
	if !sawStallUpdate {
		t.Error("a stall broadcast nothing; the queue halts and the UI shows the " +
			"pre-stall state until an unrelated event refreshes it")
	}
}

// ---------- bound resolution ----------

// wedgeOnFile parks the assembler's worker goroutine when it opens one
// particular file, and never lets it go.
//
// It reproduces a wedged mount without needing one. The worker owns every file
// handle, so anything that blocks it blocks every barrier operation for every
// job — which is exactly the condition barrierOpTimeout exists for, and the
// reason SyncTarget.Files can return "no files" for a reason that is not "no
// files".
type wedgeOnFile struct {
	fileIdx int
	entered chan struct{}
	release chan struct{}
	inner   func(string, int) (assembler.FileInfo, error)
}

func (w *wedgeOnFile) resolve(jobID string, fileIdx int) (assembler.FileInfo, error) {
	if fileIdx == w.fileIdx {
		close(w.entered)
		<-w.release
	}
	return w.inner(jobID, fileIdx)
}

// newWedgedApp builds an Application whose assembler holds file 0 open and
// whose worker is parked inside file 1's open until the returned func is
// called.
//
// Releasing it is what makes the recovery tests real: a stall test that never
// unwedges can only observe a flag, while one that does can watch the job
// actually finish the work the stall interrupted.
//
// The assembler is constructed here rather than taken from New, because the
// resolver has to be substituted before the worker ever runs and Application
// exposes no hook for it — deliberately, since production has no reason to
// swap one.
func newWedgedApp(t *testing.T) (*Application, *job.Job, func()) {
	t.Helper()
	application, job, arm, release := newArmableWedgedApp(t)
	arm()
	return application, job, release
}

// newArmableWedgedApp is newWedgedApp with the wedge deferred: file 0 is open
// and the worker is healthy until arm is called, which parks the worker inside
// file 1's open and cuts the assembler's barrier-op bound to 20ms.
//
// arm may be called from inside a barrier callback, which is what lets a test
// wedge the worker AFTER a finalize's own operations have all been answered.
func newArmableWedgedApp(t *testing.T) (application *Application, j *job.Job, arm, release func()) {
	t.Helper()
	application, j = newDurabilityTestApp(t, 2, 1)
	ctx := t.Context()

	// Stop the assembler New built and replace it with one whose resolver
	// wedges. Nothing has been written through the first one yet.
	if err := application.assembler.Stop(); err != nil {
		t.Fatalf("stop the original assembler: %v", err)
	}
	wedge := &wedgeOnFile{
		fileIdx: 1,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		inner:   application.pipeline.resolveFileInfo,
	}
	application.assembler = assembler.New(assembler.Options{
		FileInfo: wedge.resolve,
	}, slog.New(slog.DiscardHandler))
	application.closeHandlesTimeout = 20 * time.Millisecond
	application.pipeline.assembler = application.assembler
	if err := application.assembler.Start(ctx); err != nil {
		t.Fatalf("start the wedging assembler: %v", err)
	}
	// Registered in this order so it runs FIRST: t.Cleanup is LIFO, and
	// Assembler.Stop joins the worker, which is parked on this channel. The
	// other way round the test deadlocks in its own teardown.
	t.Cleanup(func() { _ = application.assembler.Stop() })
	var once sync.Once
	release = func() { once.Do(func() { close(wedge.release) }) }
	t.Cleanup(release)

	// File 0 opens normally and stays open.
	writeFixtureArticle(t, application, j.ID(), 0, 0)

	arm = func() {
		application.assembler.SetBarrierOpTimeout(20 * time.Millisecond)
		// File 1's open parks the worker, so no control message can be answered.
		if err := application.pipeline.registerFile(j, 1); err != nil {
			t.Errorf("registerFile 1: %v", err)
			return
		}
		// File 1 explicitly: sending this to file 0 would never reach the
		// wedge. It is not routed through writeFixtureArticle because that
		// waits for the file to open, and the whole point here is that the
		// open never returns.
		ref, req := assemblerWrite(j.ID(), 1, 1, 0)
		if err := application.assembler.WriteArticle(ctx, ref, req); err != nil {
			t.Errorf("WriteArticle 1: %v", err)
			return
		}
		select {
		case <-wedge.entered:
		case <-time.After(5 * time.Second):
			t.Error("the worker never reached the wedge; the fixture is not wedged and the " +
				"assertions that follow would pass against a healthy assembler")
		}
	}
	return application, j, arm, release
}

// ---------- fault routing on the completion path ----------

// TestDropJobAlreadyInHistory_CancellationAfterRemoveStillClearsDurability is
// the startup reconcile's copy of RemoveJob's detachment pin, and it exists
// separately because the two are separate call sites of the same fix: reverting
// one says nothing about the other.
//
// The context here is the startup context rather than an API request's, so what
// cancels it is a SIGINT or a startup deadline rather than a browser tab —
// a restart interrupted by another restart. Past dispatcher.Remove the outcome
// is identical either way: the queue row is gone, and rows this call fails to
// remove wait for the next start's sweep.
//
// Note which steps are NOT detached. The history Get and dispatcher.Remove run
// on the caller's context on purpose, because a cancellation there destroys
// nothing and the next startup reconciles the job again.
func TestDropJobAlreadyInHistory_CancellationAfterRemoveStillClearsDurability(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	d := dispatch.New(
		1, 1, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		disconnectingStore{Store: store.New(repo.DB()), cancel: cancel},
		application.runner,
	)
	application.dispatcher = d
	application.pipeline.dispatcher = d
	application.runner.report = d

	j, _ := removeJobFixture(t, application, "reconciled")

	// COMPLETED, so the retention rule says the rows go. A FAILED entry would
	// keep them and the assertion below could not tell a working detachment
	// from the retention.
	if err := application.historyRepo.Add(ctx, history.Entry{
		NzoID: j.ID(), Name: "reconciled", Status: string(constants.StatusCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	seedDurability(t, application, j.ID())
	if _, err := repo.DB().ExecContext(ctx,
		`INSERT OR REPLACE INTO job_files (job_id, file_index, complete) VALUES (?, 0, 0)`, j.ID()); err != nil {
		t.Fatalf("seed job files: %v", err)
	}
	if nr, nf := durabilityRowCounts(t, application, j.ID()); nr != 1 || nf != 1 {
		t.Fatalf("fixture recorded %d runs and %d failed rows, want 1 and 1; "+
			"the test would pass vacuously", nr, nf)
	}
	if n := jobFilesCount(t, application, j.ID()); n == 0 {
		t.Fatal("no job_files rows to delete, so this test would pass vacuously")
	}

	application.dropJobAlreadyInHistory(ctx, j.ID())
	if _, ok := application.Dispatcher().Job(j.ID()); ok {
		t.Fatal("the job is still in the dispatcher although it is in history")
	}
	if ctx.Err() == nil {
		t.Fatal("the store never cancelled, so this never entered the window under test")
	}

	if nr, nf := durabilityRowCounts(t, application, j.ID()); nr != 0 || nf != 0 {
		t.Errorf("%d durable runs and %d failed-article rows survive a reconcile that was "+
			"cancelled after the job left the dispatcher", nr, nf)
	}
	if n := jobFilesCount(t, application, j.ID()); n != 0 {
		t.Errorf("%d job_files rows survive a reconcile that was cancelled after the job "+
			"left the dispatcher", n)
	}
}

// TestDropJobAlreadyInHistory_KeepsEverythingWhenTheDispatcherRemoveFails pins
// #376's ordering on the reconcile path: a Remove that fails leaves the queue
// row, and the manifest and every durability row stay with it, so the next
// startup can reconcile the job against them. The job stays cancelled, so the
// dispatcher does not route it onward in the meantime.
func TestDropJobAlreadyInHistory_KeepsEverythingWhenTheDispatcherRemoveFails(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	ctx := t.Context()

	d := dispatch.New(
		1, 1, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		removeRefusingStore{Store: store.New(repo.DB())},
		application.runner,
	)
	application.dispatcher = d
	application.pipeline.dispatcher = d
	application.runner.report = d

	j, _ := removeJobFixture(t, application, "unremovable")

	if err := application.historyRepo.Add(ctx, history.Entry{
		NzoID: j.ID(), Name: "unremovable", Status: string(constants.StatusCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	seedDurability(t, application, j.ID())
	if _, err := repo.DB().ExecContext(ctx,
		`INSERT OR REPLACE INTO job_files (job_id, file_index, complete) VALUES (?, 0, 0)`, j.ID()); err != nil {
		t.Fatalf("seed job files: %v", err)
	}
	mpath := filepath.Join(manifestDir(adminDir), j.ID()+".json.gz")
	if err := os.MkdirAll(manifestDir(adminDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mpath, []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}

	application.dropJobAlreadyInHistory(ctx, j.ID())
	if got := j.Intent(); got != job.IntentCancel {
		t.Errorf("intent = %v after a failed Remove, want IntentCancel; a job that is "+
			"already filed would be routed onward and post-processed again", got)
	}

	if _, err := os.Stat(mpath); err != nil {
		t.Errorf("the manifest was deleted although the job is still in the queue (stat: %v); "+
			"the next startup has nothing to reconcile it against", err)
	}
	if nr, nf := durabilityRowCounts(t, application, j.ID()); nr != 1 || nf != 1 {
		t.Errorf("%d durable runs and %d failed-article rows left after a failed Remove, "+
			"want 1 and 1 — the row outlived its own state", nr, nf)
	}
	if n := jobFilesCount(t, application, j.ID()); n != 1 {
		t.Errorf("%d job_files rows left after a failed Remove, want 1", n)
	}
	if _, ok := application.Dispatcher().Job(j.ID()); !ok {
		t.Error("the job left the dispatcher although its store delete failed")
	}
}

// TestDropJobAlreadyInHistory_KeepsTheJobWhenTheHistoryLookupFails pins that a
// lookup which establishes nothing deletes nothing and routes nothing: the
// job, its manifest and its rows stay, and the job is paused with an
// operational error so that no tick post-processes it.
func TestDropJobAlreadyInHistory_KeepsTheJobWhenTheHistoryLookupFails(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	ctx := t.Context()

	j, _ := removeJobFixture(t, application, "unlookupable")
	seedDurability(t, application, j.ID())
	mpath := filepath.Join(manifestDir(adminDir), j.ID()+".json.gz")
	if err := os.MkdirAll(manifestDir(adminDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mpath, []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A lookup failure that is not ErrNotFound. Taking the table away is the
	// available lever: historyRepo.Get runs SQL against a real database and
	// there is no seam to inject at.
	if _, err := repo.DB().ExecContext(ctx,
		`ALTER TABLE history RENAME TO history_hidden`); err != nil {
		t.Fatalf("hide history: %v", err)
	}
	t.Cleanup(func() {
		_, _ = repo.DB().ExecContext(context.WithoutCancel(ctx),
			`ALTER TABLE history_hidden RENAME TO history`)
	})

	application.dropJobAlreadyInHistory(ctx, j.ID())
	if _, err := os.Stat(mpath); err != nil {
		t.Errorf("the manifest was deleted although nothing was established about the "+
			"job (stat: %v)", err)
	}
	if _, ok := application.Dispatcher().Job(j.ID()); !ok {
		t.Error("the job left the dispatcher on a lookup that established nothing")
	}
	if got := j.Intent(); got != job.IntentPause {
		t.Errorf("intent = %s after a failed lookup, want %s: the first tick would "+
			"post-process a job that may already be filed", got, job.IntentPause)
	}
	row, ok := application.Dispatcher().Row(j.ID())
	if !ok {
		t.Fatal("Row: job not registered")
	}
	if !strings.Contains(row.Header.OperationalError, "history lookup failed") {
		t.Errorf("operational error = %q, want it to say the history lookup failed",
			row.Header.OperationalError)
	}
}

// TestHoldUnreconciledJob_LeavesACancelledJobAlone pins the two cases the
// hold cannot pause: with no dispatcher there is nothing to pause, and a
// cancelled job's intent is latched. Neither is routed onward, and a job the
// hold did not pause gets no note saying it did.
func TestHoldUnreconciledJob_LeavesACancelledJobAlone(t *testing.T) {
	t.Parallel()
	(&Application{log: slog.New(slog.DiscardHandler)}).holdUnreconciledJob("job-a", errors.New("lookup failed"))

	application, _, _ := newLifecycleTestApp(t)
	j, _ := removeJobFixture(t, application, "cancelledhold")
	if err := application.Dispatcher().Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	application.holdUnreconciledJob(j.ID(), errors.New("lookup failed"))
	if got := j.Intent(); got != job.IntentCancel {
		t.Errorf("intent = %s, want %s", got, job.IntentCancel)
	}
	row, ok := application.Dispatcher().Row(j.ID())
	if !ok {
		t.Fatal("Row: job not registered")
	}
	if row.Header.OperationalError != "" {
		t.Errorf("operational error = %q on a job the hold could not pause, want none",
			row.Header.OperationalError)
	}
}

// TestDropJobAlreadyInHistory_AppliesTheFailedRetentionRule pins the startup
// reconcile's reclaim, in both directions. The path is reached by a job that
// crashed between MoveToHistory and the queue removal that follows it.
//
// Both directions fail differently. Keeping a completed job's rows leaks one
// set per crash of this kind. Dropping a FAILED job's runs is worse: its retry
// reuses the job ID over the same partial file, and the runs are what bound
// FinalizeFile's truncate to the whole file (#422).
func TestDropJobAlreadyInHistory_AppliesTheFailedRetentionRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		status   constants.Status
		wantRuns int // written_articles and job_files rows alike
	}{
		{name: "a completed entry takes its rows with it", status: constants.StatusCompleted},
		{name: "a failed entry keeps its record", status: constants.StatusFailed, wantRuns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			application, job := newDurabilityTestApp(t, 1, 2)

			seedDurability(t, application, job.ID())
			if nr, nf := durabilityRowCounts(t, application, job.ID()); nr != 1 || nf != 1 || jobFilesCount(t, application, job.ID()) != 1 {
				t.Fatalf("fixture recorded %d runs, %d failed rows and %d job_files rows, want 1 "+
					"of each; the test would pass vacuously", nr, nf, jobFilesCount(t, application, job.ID()))
			}

			if err := application.historyRepo.Add(t.Context(), history.Entry{
				NzoID: job.ID(), Name: "reconciled", Status: string(tc.status),
			}); err != nil {
				t.Fatal(err)
			}

			application.dropJobAlreadyInHistory(t.Context(), job.ID())
			if _, ok := application.Dispatcher().Job(job.ID()); ok {
				t.Fatal("the job is still in the dispatcher although it is in history")
			}

			nr, nf := durabilityRowCounts(t, application, job.ID())
			if nr != tc.wantRuns {
				t.Errorf("%d written rows after reconciling a %s entry, want %d", nr, tc.status, tc.wantRuns)
			}
			if nf != 0 {
				t.Errorf("%d failed-article rows survive reconciliation; nothing reads them "+
					"once the job has left the queue", nf)
			}
			if n := jobFilesCount(t, application, job.ID()); n != tc.wantRuns {
				t.Errorf("%d job_files rows after reconciling a %s entry, want %d", n, tc.status, tc.wantRuns)
			}
		})
	}
}
