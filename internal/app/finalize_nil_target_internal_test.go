package app

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
)

// shortArticleBytes is less than the fixture file's declared 100 bytes, so the
// file is pre-allocated past its real end and only the finalize's trim removes
// the slack.
const shortArticleBytes = 60

// writeShortArticle writes the one article of a newDurabilityTestApp(t, 1, 1)
// file with fewer bytes than the file was pre-allocated to, and waits for the
// worker to have opened the file.
func writeShortArticle(t *testing.T, application *Application, j *job.Job) {
	t.Helper()
	if err := application.pipeline.registerFile(j.ID(), 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	ref := assembler.ArticleRef{JobID: j.ID(), FileIdx: 0, ArtIdx: 0, MessageID: fileFixtureArticleID(0, 0)}
	req := assembler.WriteRequest{Offset: 0, Data: make([]byte, shortArticleBytes)}
	if err := application.assembler.WriteArticle(t.Context(), ref, req); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tgt := application.syncTargetFor(j.ID()); tgt != nil && slices.Contains(tgt.Files(), 0) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("file 0 never opened after WriteArticle")
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Size()
}

// newEvictedCompletionApp builds a job whose one file has all its parts
// written, pre-allocated past its real end, and whose manifest is then
// evicted — the state a completion meets when the dispatcher drops the job's
// manifest between the file's last write and its finalize. It returns the
// evicted manifest so a test can make the job resident again.
func newEvictedCompletionApp(t *testing.T) (*Application, *job.Job, *job.Manifest, string) {
	t.Helper()
	application, j := newDurabilityTestApp(t, 1, 1)
	writeShortArticle(t, application, j)
	path := application.filePathFor(j.ID(), 0)
	if got := fileSize(t, path); got <= shortArticleBytes {
		t.Fatalf("file size = %d before the finalize, want the pre-allocated size above %d; "+
			"without slack an untrimmed file is indistinguishable from a trimmed one",
			got, shortArticleBytes)
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	j.Evict()
	if application.syncTargetFor(j.ID()) != nil {
		t.Fatal("the evicted job still has a sync target; this fixture is about the nil-target return")
	}
	return application, j, m, path
}

// TestHandleFileComplete_ANonResidentJobsFileIsNotDeliveredUntrimmed drives a
// completion whose finalize met a nil sync target for a job still in the
// queue, through the stall re-evaluation that later delivers it.
//
// No barrier can run over a job with no resident manifest, so nothing has
// trimmed the file. Delivering the completion once the job is resident again
// would hand DirectUnpack and par2 a file carrying pre-allocation's trailing
// bytes. The completion must instead wait for a retry that runs the barrier —
// and waiting must not pause the job, or it stays non-resident until a user
// Resume.
func TestHandleFileComplete_ANonResidentJobsFileIsNotDeliveredUntrimmed(t *testing.T) {
	t.Parallel()
	application, j, m, path := newEvictedCompletionApp(t)

	application.handleFileComplete(t.Context(), FileComplete{JobID: j.ID(), FileIdx: 0})

	if j.Progress().FileComplete(0) {
		t.Fatal("the file was marked complete while the job was not resident")
	}
	if st, ok := application.recoveryFiles(j.ID())[0]; !ok || st != finalizePending {
		t.Errorf("recovery state = %v, want the file pending — recorded as finalized, the "+
			"re-evaluation delivers it without ever running the barrier",
			application.recoveryFiles(j.ID()))
	}
	open, err := application.assembler.OpenFiles(t.Context(), j.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(open, 0) {
		t.Error("the file's handle was released, so the retry can never trim the file and " +
			"the job is left needing a restart")
	}

	// A re-evaluation while the job is still not resident must retry later
	// rather than park the job: a paused job is not promoted again until a
	// user Resume.
	application.reevaluateStalls(t.Context())
	if row, ok := application.dispatcher.Row(j.ID()); !ok || row.Status() == constants.StatusPaused {
		t.Fatalf("status = %v after a retry that met a non-resident job, want it not paused — "+
			"the job was evicted, not faulted, and a pause keeps it non-resident until a user Resume",
			row.Status())
	}
	if st, ok := application.recoveryFiles(j.ID())[0]; !ok || st != finalizePending {
		t.Fatal("the pending finalize was dropped by a retry that could not run")
	}

	// The dispatcher promotes the job again.
	if err := j.RestoreContent(m, j.Progress()); err != nil {
		t.Fatal(err)
	}
	application.reevaluateStalls(t.Context())

	if !j.Progress().FileComplete(0) {
		t.Fatal("the file was not marked complete once the job was resident again; the " +
			"pending finalize is never retried to completion")
	}
	if got := fileSize(t, path); got != shortArticleBytes {
		t.Errorf("the file was marked complete at %d bytes, want %d: its finalize never "+
			"ran, so DirectUnpack and par2 receive pre-allocation's trailing bytes",
			got, shortArticleBytes)
	}
	if !j.Progress().ArticleDone(0) {
		t.Error("the file's article is still Outstanding; the retried finalize never acked it")
	}
}

// TestHandleFileComplete_ANonResidentCompletionDrainedAtShutdown pins the
// shutdown shape of the same path: a completion drained after the assembler
// has stopped, for a job whose manifest is not resident.
//
// Nothing may be marked complete — the file was never trimmed — and nothing
// may be logged as an error: a job evicted while one of its completions was in
// flight is ordinary. The file stays not Complete and its article unacked, so
// the next start's resumeAllJobs re-derives both from the durable runs:
// completeStrandedFiles trims and completes the file when the runs resolve
// every article, and ReplaceFromRuns leaves any article they do not cover
// Outstanding to be fetched again.
func TestHandleFileComplete_ANonResidentCompletionDrainedAtShutdown(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 1)
	writeShortArticle(t, application, j)
	logs := closeFaultLogs(application)

	// Shutdown's order, from the queue pause to the drain: Shutdown pauses the
	// queue, stopWorkers yields every Fetching job once the downloader has
	// stopped, and the tick that yield kicks evicts the job, whose parked
	// lease no longer holds its manifest — which can land before the assembler
	// stops and watchCompletions drains. This fixture's job never took a lease
	// through a tick, so the dispatcher does not count it resident and a tick
	// would not evict it; the eviction reconcileResidency makes is called
	// directly instead.
	application.dispatcher.Pause()
	if err := application.dispatcher.Yielded(j.ID()); err != nil {
		t.Fatal(err)
	}
	application.residency.Evict(j.ID())
	if application.syncTargetFor(j.ID()) != nil {
		t.Fatal("the job still has a sync target; this test is about the nil-target return")
	}
	if err := application.assembler.Stop(); err != nil {
		t.Fatal(err)
	}

	application.handleFileComplete(t.Context(), FileComplete{JobID: j.ID(), FileIdx: 0})

	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a completion drained at shutdown for a non-resident job logged an error; "+
			"logs:\n%s", logs.String())
	}
	if j.Progress().FileComplete(0) {
		t.Error("the file was marked complete although no barrier trimmed it; the queue save " +
			"persists that flag and the next start ships the untrimmed file")
	}
	if j.Progress().ArticleDone(0) {
		t.Error("the article was marked done although no barrier committed it")
	}
	// The queue itself is paused here, so the row's status cannot say whether
	// the job was stalled; the stall record can.
	if got := application.StallReason(j.ID()).Reason; got != "" || application.weParked(j.ID()) {
		t.Errorf("stall reason = %q, parked = %v; want the job not stalled for a residency "+
			"condition", got, application.weParked(j.ID()))
	}
}

// TestRetryFinalize_ADepartedJobIsNotAnsweredAsNonResident pins the retry for a
// job that leaves the queue between reevaluateStall's Dispatcher.Job check and
// the retry itself.
//
// A job that has left the queue has nothing left to finalize. Answered as
// job.ErrNotResident, routeFinalizeFailure would record the file pending again
// and the retry would keep its handle, for a job no promotion will ever make
// resident.
func TestRetryFinalize_ADepartedJobIsNotAnsweredAsNonResident(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 1)
	writeShortArticle(t, application, j)
	if err := application.dispatcher.Remove(t.Context(), j.ID()); err != nil {
		t.Fatal(err)
	}

	err := application.retryFinalize(t.Context(), j.ID(), 0)

	if errors.Is(err, job.ErrNotResident) {
		t.Errorf("retryFinalize = %v for a job that has left the queue; routeFinalizeFailure "+
			"records it pending for a promotion that cannot happen", err)
	}
	open, oerr := application.assembler.OpenFiles(t.Context(), j.ID())
	if oerr != nil {
		t.Fatal(oerr)
	}
	if slices.Contains(open, 0) {
		t.Error("the retry kept the handle of a job that has left the queue")
	}
}
