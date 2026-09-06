package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// The unit fixture below is two files of two articles each, and in the file it
// puts on disk exactly one article is durable and one is not — the same rule
// the end-to-end tests follow, for the same reason: a fixture whose articles
// are all in one state cannot distinguish the seeding logic from its absence.
const (
	unitArtLen  = 100
	unitFileOne = "A.bin"
)

type resumeUnitFixture struct {
	app      *Application
	repo     *history.Repository
	job      *job.Job
	dir      string
	path     string
	articles [2][]byte
}

// newResumeUnitFixture builds a job whose FIRST file has a resolved on-disk
// name, a file long enough to satisfy the size gate, and one recorded durable
// run covering only its first article — and whose SECOND file has a recorded
// run but no resolved name, so nothing ever opened a path for it.
//
// One article recorded and one not, deliberately: a fixture whose articles are
// all in one state cannot distinguish the seeding logic from its absence.
func newResumeUnitFixture(t *testing.T) *resumeUnitFixture {
	t.Helper()
	application, repo, _ := newLifecycleTestApp(t)

	parsed := &nzb.NZB{}
	for f := range 2 {
		file := nzb.File{Subject: string(rune('A'+f)) + ".bin", Bytes: 2 * unitArtLen}
		for a := range 2 {
			file.Articles = append(file.Articles, nzb.Article{
				ID:     fmt.Sprintf("%c%d@t", 'A'+f, a),
				Bytes:  unitArtLen,
				Number: a + 1,
			})
		}
		parsed.Files = append(parsed.Files, file)
	}
	j, hdr, err := BuildIngestJob(application.config, parsed, "resume-unit.nzb", types.FetchOptions{NzbName: "resume-unit"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.dispatcher.Add(j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if err := j.SetFileFilename(0, unitFileOne); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}

	f := &resumeUnitFixture{app: application, repo: repo, job: j}
	f.articles[0] = bytes.Repeat([]byte{'x'}, unitArtLen)
	f.articles[1] = bytes.Repeat([]byte{'y'}, unitArtLen)
	f.dir = filepath.Join(application.pipeline.downloadDir, j.Name())
	f.path = filepath.Join(f.dir, unitFileOne)
	if err := os.MkdirAll(f.dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pre-allocated to the file's full decoded size, with article 1's region
	// still zero — the hole an article that never landed leaves behind. The
	// gate is a size comparison, so what matters is that the file is at least
	// as long as the recorded run claims.
	onDisk := make([]byte, 2*unitArtLen)
	copy(onDisk, f.articles[0])
	if err := os.WriteFile(f.path, onDisk, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Only article 0 is recorded, because only article 0 was ever written.
	// File 1 has a run of its own that must NOT reach the work set: with no
	// resolved name there is no path the gate could have stat'ed, and adopting
	// it would mark an article Done on the strength of the record alone.
	if _, err := durability.NewSQLiteRunStore(repo.DB()).Commit(t.Context(), j.ID(),
		[]durability.DurableArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: unitArtLen,
				CRC32: crc32.ChecksumIEEE(f.articles[0])},
			{FileIdx: 1, ArtIdx: 2, Offset: 0, Length: unitArtLen, CRC32: 7},
		}); err != nil {
		t.Fatalf("RunStore.Commit: %v", err)
	}
	return f
}

// saveQueue persists the fixture's queue to the store the way a running
// process does.
func (f *resumeUnitFixture) saveQueue(t *testing.T) {
	t.Helper()
	f.app.dispatcher.Tick(t.Context())
}

// nameSecondFile gives file 1 a resolved on-disk name and a file to match, so
// the sweep has two files to walk rather than one. Without it the per-file
// context check has nothing to stop at.
func (f *resumeUnitFixture) nameSecondFile(t *testing.T) {
	t.Helper()
	if err := f.job.SetFileFilename(1, "B.bin"); err != nil {
		t.Fatalf("SetFileFilename(1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "B.bin"), make([]byte, 2*unitArtLen), 0o600); err != nil {
		t.Fatalf("write B.bin: %v", err)
	}
}

// faultOnSecondFile gives file 1 a resolved name pointing at a
// self-referential symlink, so its stat fails with ELOOP — neither
// fs.ErrNotExist nor a listed permanent errno, so it is a retryable storage
// fault. File 0 is untouched and still resumes cleanly.
func (f *resumeUnitFixture) faultOnSecondFile(t *testing.T) {
	t.Helper()
	if err := f.job.SetFileFilename(1, "B.bin"); err != nil {
		t.Fatalf("SetFileFilename(1): %v", err)
	}
	loop := filepath.Join(f.dir, "B.bin")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatalf("symlink loop: %v", err)
	}
}

// liveProgress reads a job's in-memory progress WITHOUT hydrating it.
//
// SnapshotJob hydrates the clone it returns, and hydration re-derives every
// article's state from the durability record — which is the same record a seed
// installs from. So a SnapshotJob-based assertion cannot tell "the sweep
// liveProgress reads a job's in-memory progress WITHOUT hydrating it.
func liveProgress(t *testing.T, app *Application, jobID string) *job.JobProgress {
	t.Helper()
	j, ok := app.dispatcher.Job(jobID)
	if !ok {
		t.Fatalf("liveProgress: job %s is not in the dispatcher", jobID)
	}
	return j.Progress()
}

func (f *resumeUnitFixture) snapshot(t *testing.T) (*job.Job, *job.Manifest) {
	t.Helper()
	j, ok := f.app.dispatcher.Job(f.job.ID())
	if !ok {
		t.Fatal("job vanished from the dispatcher")
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	return j, m
}

// TestResumeJobFiles_SkipsFilesWithNoResolvedName pins both halves of the
// sweep's authority bound: the file that has a name on disk is swept and
// contributes the runs it still holds, and the file that never resolved a name
// appears in NEITHER return despite having a run of its own.
//
// The two returns are separate for exactly this reason. A file whose runs the
// gate discarded also contributes no run, so the runs alone cannot distinguish
// "I looked and found nothing" — which must return its articles to Outstanding
// — from "I never looked", which must leave them alone.
func TestResumeJobFiles_SkipsFilesWithNoResolvedName(t *testing.T) {
	t.Parallel()
	f := newResumeUnitFixture(t)
	snap, m := f.snapshot(t)

	swept, runs, fault, err := f.app.resumeJobFiles(t.Context(), snap.ID(), m, snap.Name(), snap.Progress())
	if err != nil {
		t.Fatalf("resumeJobFiles: %v", err)
	}
	if fault != nil {
		t.Fatalf("fault = %v, want none", fault)
	}
	if len(swept) != 1 || swept[0] != 0 {
		t.Fatalf("swept = %v, want [0] — file 1 has no resolved on-disk name, so nothing "+
			"stat'ed it and it must not be named as looked at", swept)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1 — file 1's run must not be adopted", len(runs))
	}
	if runs[0].FileIdx != 0 {
		t.Errorf("FileIdx = %d, want 0", runs[0].FileIdx)
	}
	if runs[0].FirstArtIdx != 0 || runs[0].LastArtIdx != 0 {
		t.Errorf("adopted run covers [%d,%d], want [0,0] — article 1 was never written "+
			"and nothing recorded it", runs[0].FirstArtIdx, runs[0].LastArtIdx)
	}
	// File 1's run is still on stable storage: the sweep neither adopted it
	// nor discarded it, because it never looked.
	stored, err := f.app.runs.ForFile(t.Context(), f.job.ID(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Errorf("file 1 holds %d runs, want 1 — an unswept file must be left exactly "+
			"as it was found", len(stored))
	}
}

// TestResumeAllJobs_SeedsResidentAndSkipsNonResident pins the residency rule
// in both directions at once.
//
// ReplaceFromRuns installs bits into the LIVE job's progress, which needs a
// resident manifest. A non-resident job cannot be seeded, and the sweep must
// step over it rather than fail the whole startup — while the resident job
// beside it is still seeded.
//
// # Which guard actually stops the second job
//
// The second job is Paused, and since the phase bound was added for the
// post-processing hazard, THE PHASE BOUND is what skips it — a paused job
// never reaches the residency check. So read this as "a job that is neither
// downloading nor resident contributes nothing", not as a pin on the residency
// arm specifically. Verified rather than assumed: replacing that arm with a
// panic leaves the whole of ./internal/app green.
//
// The residency arm has no pin because no reachable state builds one; the
// reachability argument is on resumeAllJobs, at the arm itself. It is a guard,
// not a branch with a test owing against it.
func TestResumeAllJobs_SeedsResidentAndSkipsNonResident(t *testing.T) {
	t.Parallel()
	f := newResumeUnitFixture(t)

	// A second job, non-resident (evicted).
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "B.bin",
		Bytes:    unitArtLen,
		Articles: []nzb.Article{{ID: "b0@t", Bytes: unitArtLen, Number: 1}},
	}}}
	other, hdr, err := BuildIngestJob(f.app.config, parsed, "resume-unit-other.nzb", types.FetchOptions{NzbName: "resume-unit-other"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := f.app.dispatcher.Add(other, hdr); err != nil {
		t.Fatalf("Add(other): %v", err)
	}
	if err := other.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt(other): %v", err)
	}
	if err := other.SetFileFilename(0, "B.bin"); err != nil {
		t.Fatalf("SetFileFilename(other): %v", err)
	}
	// Its file and its recorded run are both real and consistent, so the only
	// thing standing between it and a Done bit is its residency. Without them
	// the assertion below would hold for a sweep that tried to seed it and
	// merely found nothing.
	otherDir := filepath.Join(f.app.pipeline.downloadDir, other.Name())
	if err := os.MkdirAll(otherDir, 0o750); err != nil {
		t.Fatalf("mkdir(other): %v", err)
	}
	otherBytes := bytes.Repeat([]byte{'z'}, unitArtLen)
	otherPath := filepath.Join(otherDir, "B.bin")
	if err := os.WriteFile(otherPath, otherBytes, 0o600); err != nil {
		t.Fatalf("write(other): %v", err)
	}
	if _, err := durability.NewSQLiteRunStore(f.repo.DB()).Commit(t.Context(), other.ID(),
		[]durability.DurableArticle{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: unitArtLen,
			CRC32: crc32.ChecksumIEEE(otherBytes)}}); err != nil {
		t.Fatalf("RunStore.Commit(other): %v", err)
	}
	other.Evict()
	if other.Resident() {
		t.Fatal("evict did not evict the second job's manifest, so it is resident and " +
			"the assertion below says nothing")
	}
	if list := f.app.dispatcher.List(); len(list) != 2 {
		t.Fatalf("fixture guard: %d jobs in the queue, want 2", len(list))
	}

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	seeded := f.job.Progress()
	if !seeded.ArticleDone(0) {
		t.Error("the resident job's durable article was not seeded")
	}
	if seeded.ArticleDone(1) {
		t.Error("an article whose bytes are not on disk came back Done")
	}
	for i := 2; i < 4; i++ {
		if seeded.ArticleDone(i) {
			t.Errorf("article %d belongs to the file with no resolved name and must stay Outstanding", i)
		}
	}
	// A guard rather than a pin on today's code: the sweep steps over a job it
	// cannot read a manifest for, so nothing currently reachable could set
	// this bit. It is here for the change that would — hydrating the queue at
	// startup to widen the sweep's coverage — because that change must bring
	// the stat gate with it rather than installing the record on residency
	// alone.
	//
	// Read through liveProgress, not SnapshotJob: hydration re-derives the
	// article state from the same record, so a hydrated clone reports the bit
	// set whether or not anything seeded it.
	if liveProgress(t, f.app, other.ID()).ArticleDone(0) {
		t.Error("a non-resident job was seeded although nothing stat'ed its file")
	}
}

// countingResumer records every per-file Resume call and lets a test act
// between them.
//
// It exists because R15's "interruptible between FILES, not merely between
// jobs" is a claim about how many files were processed, and nothing outside
// the sweep can observe that otherwise. The previous version of the test
// below asserted only that an error came back, which the store's own read
// produces on its own when the context is already dead — so it passed with
// both context checks deleted.
type countingResumer struct {
	inner  fileResumer
	calls  []int32
	onCall func(fileIdx int32)
}

func (c *countingResumer) Resume(ctx context.Context, jobID string, fileIdx int32, path string) (durability.ResumeResult, error) {
	c.calls = append(c.calls, fileIdx)
	res, err := c.inner.Resume(ctx, jobID, fileIdx, path)
	// AFTER the inner call, not before. Cancelling first makes the inner
	// Resume fail on the dead context, and the sweep's own error branch then
	// aborts — so the count would be 1 whether or not the per-file check
	// exists. That is the inert version of this test, and it was written
	// before it was caught.
	if c.onCall != nil {
		c.onCall(fileIdx)
	}
	return res, err
}

// TestResumeAllJobs_CancelBetweenFilesStopsAtTheNextFile pins R15's per-FILE
// check.
//
// The context is cancelled from inside the first file's Resume, so the job
// loop's own check has already passed and only the per-file check can stop
// the sweep. The assertion is on how many files were resumed, not on the
// existence of an error: an error is produced by the cancelled context
// reaching SQLite whether or not either check exists.
func TestResumeAllJobs_CancelBetweenFilesStopsAtTheNextFile(t *testing.T) {
	t.Parallel()
	f := newResumeUnitFixture(t)
	f.nameSecondFile(t)

	ctx, cancel := context.WithCancel(t.Context())
	counter := &countingResumer{inner: f.app.resumer}
	counter.onCall = func(int32) { cancel() }
	f.app.resumer = counter

	if err := f.app.resumeAllJobs(ctx); err == nil {
		t.Error("resumeAllJobs returned nil after its context was cancelled")
	}
	if len(counter.calls) != 1 {
		t.Errorf("resumed %d files (%v), want 1 — the sweep must stop at the next file "+
			"rather than run the rest of the job to completion (R15)", len(counter.calls), counter.calls)
	}
}

// TestResumeAllJobs_CancelBeforeAnyJobResumesNothing pins the per-JOB check,
// which the per-file check cannot stand in for.
//
// The only job here is non-resident, so the sweep never reaches a file loop
// and the per-file check can never fire. Without the per-job check the sweep
// skips the job, finds nothing else, and returns nil — a cancelled startup
// that reports success.
func TestResumeAllJobs_CancelBeforeAnyJobResumesNothing(t *testing.T) {
	t.Parallel()
	f := newResumeUnitFixture(t)
	f.job.Evict()
	if err := f.app.dispatcher.PauseJob(f.job.ID()); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	counter := &countingResumer{inner: f.app.resumer}
	f.app.resumer = counter

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.app.resumeAllJobs(ctx); err == nil {
		t.Error("resumeAllJobs returned nil on an already-cancelled context, so a shutdown " +
			"racing startup would be reported as a clean sweep")
	}
	// A forward guard rather than a pin, like the one in the residency test
	// above: this fixture's only job is non-resident, so the file loop is
	// never entered and no mutation of either context check can make this
	// count non-zero. It is here to catch a later change that widens the
	// sweep to non-resident jobs without carrying the abort with it.
	if len(counter.calls) != 0 {
		t.Errorf("resumed %d files on a cancelled context, want 0", len(counter.calls))
	}
	// liveProgress rather than SnapshotJob, for the reason its doc gives: a
	// hydrated clone re-derives the article state from the record and would
	// report this bit set whether or not the sweep ran.
	if liveProgress(t, f.app, f.job.ID()).ArticleDone(0) {
		t.Error("an aborted sweep still seeded; the abort must leave the work set untouched")
	}
}

// cancelledDuringResume is a shutdown landing in the middle of one file's
// Resume: it cancels the context and then fails, which is exactly the shape
// durability.Resumer returns when its stat, read or SQLite query is
// interrupted.
//
// A five-line stub rather than a real interruption because the distinction
// under test is entirely in how the sweep READS the pair (error, ctx.Err()),
// and a real race would make which of the two arrived first non-deterministic.
type cancelledDuringResume struct{ cancel context.CancelFunc }

func (c *cancelledDuringResume) Resume(_ context.Context, jobID string, fileIdx int32, _ string) (durability.ResumeResult, error) {
	c.cancel()
	return durability.ResumeResult{}, fmt.Errorf(
		"durability: resume stat job=%s file=%d: %w", jobID, fileIdx, context.Canceled)
}

// TestResumeAllJobs_ShutdownDuringResumeIsNotAStorageFault pins the guard that
// separates the two ways Resume can fail.
//
// They are not interchangeable. A storage fault stalls the job, and a stalled
// job is paused, non-resident, and skipped by every future sweep — which only
// runs at startup. So misreading a shutdown as a storage fault costs that job
// its seed permanently, and persists a "Stalled: context canceled" reason
// describing a condition that never existed. It is the same
// permanent-L3-regression loop as discarding the runs already gathered, reached by
// an ordinary Ctrl-C during startup rather than by a flaking mount.
//
// The assertions are on the distinction, not on the existence of an error:
// that the abort carries context.Canceled, and that the job was NOT stalled.
// "An error came back" is the assertion shape that made the cancellation test
// inert twice on this branch.
func TestResumeAllJobs_ShutdownDuringResumeIsNotAStorageFault(t *testing.T) {
	t.Parallel()
	f := newResumeUnitFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.app.resumer = &cancelledDuringResume{cancel: cancel}

	err := f.app.resumeAllJobs(ctx)

	// The job's state is checked FIRST, and with t.Error rather than t.Fatal,
	// so that a regression reports the distinction it actually broke. Leading
	// with a fatal "an error came back" check would abort the test before
	// these ran, and "no error" is the same message a dozen unrelated defects
	// produce.
	row, ok := f.app.dispatcher.Row(f.job.ID())
	if !ok {
		t.Fatal("job not in dispatcher")
	}
	if row.Status() == constants.StatusPaused {
		t.Errorf("a shutdown during Resume was routed as a storage fault: status = %q. "+
			"A stalled job is non-resident and is skipped by every future sweep, so this "+
			"costs the job its seed permanently", row.Status())
	}
	if strings.HasPrefix(row.Header.Warning, "Stalled: ") {
		t.Errorf("a shutdown during Resume was surfaced as a storage condition that never "+
			"existed: warning = %q", row.Header.Warning)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to carry context.Canceled — the sweep must report a "+
			"shutdown as a shutdown rather than reclassify it as a fault of the device", err)
	}
}

// TestResumeAllJobs_SeedsFilesResumedBeforeAFault pins that a fault on one
// file does not discard the files already resumed.
//
// The cost of discarding them is permanent, not transient: the stall pauses
// the job, a paused job is not resident, and a non-resident job is skipped by
// every future sweep — which only runs at startup. So a momentary NFS error
// on the second file would throw away the first file's seed with no path back
// to it.
//
// Asserting only that the job stalled would hold for both versions. The
// assertion that discriminates is that file 0's durable article is Done
// anyway.
func TestResumeAllJobs_SeedsFilesResumedBeforeAFault(t *testing.T) {
	t.Parallel()
	f := newResumeUnitFixture(t)
	f.faultOnSecondFile(t)

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	row, ok := f.app.dispatcher.Row(f.job.ID())
	if !ok {
		t.Fatal("job not in dispatcher")
	}
	p := f.job.Progress()
	if !p.ArticleDone(0) {
		t.Error("file 0 resumed cleanly, but its durable article was discarded because file 1 " +
			"faulted afterwards — a transient fault must not cost the ground already recovered")
	}
	// Also a forward guard, not a pin. On the fault path Resume returns the
	// zero ResumeResult, so the bitmap is empty and seeds nothing however the
	// conversion is mutated. S3 itself is pinned by assertDone under
	// TestResumeAtStartup_StorageFaultStallsAndDoesNotFailArticles, so nothing
	// is uncovered here — only this line is not what covers it.
	if p.ArticleDone(1) {
		t.Error("an article whose bytes are not on disk came back Done")
	}
	if row.Status() != constants.StatusPaused {
		t.Errorf("status = %q, want %q — the faulting file must still stall the job", row.Status(), constants.StatusPaused)
	}
	if !strings.HasPrefix(row.Header.Warning, "Stalled: ") {
		t.Errorf("warning = %q, want a surfaced stall reason (R27)", row.Header.Warning)
	}
}

// TestJobFilePath_ResolvesUnderTheJobDirectory pins that the sweep looks for a
// file where the assembler wrote it, and that a name recorded in the queue
// cannot walk out of the job's own directory.
func TestJobFilePath_ResolvesUnderTheJobDirectory(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	root := application.pipeline.downloadDir

	if got, want := application.pipeline.jobFilePath("a job", "file.bin"),
		filepath.Join(root, "a job", "file.bin"); got != want {
		t.Errorf("jobFilePath = %q, want %q", got, want)
	}

	escape := application.pipeline.jobFilePath("a job", "../../etc/passwd")
	jobDir := filepath.Join(root, "a job")
	if !bytes.HasPrefix([]byte(escape), []byte(jobDir+string(os.PathSeparator))) {
		t.Errorf("jobFilePath = %q, want it confined to %q", escape, jobDir)
	}
}

// replacedUnderneath makes file 0 look like something outside the assembler
// rewrote it: the file on disk is now SHORTER than the runs recorded for it,
// which is the one thing §3.4's gate treats as a disproof.
//
// par2 repairing a file in place is the case this stands in for. The repaired
// bytes are correct output, but they are not what the download recorded, and a
// sweep that ran over a job in post-processing would discard the record for a
// file that is fine.
//
// It also records the state a fully downloaded job carries into
// post-processing — both articles Done, the file Complete, an assembled CRC —
// because that is what the sweep would destroy.
func (f *resumeUnitFixture) replacedUnderneath(t *testing.T) {
	t.Helper()
	// Both articles recorded and both installed, so the state the sweep would
	// destroy is a complete one. The fixture's own commit covers article 0
	// only; this adds article 1.
	if _, err := f.app.runs.Commit(t.Context(), f.job.ID(), []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 1, Offset: unitArtLen, Length: unitArtLen,
			CRC32: crc32.ChecksumIEEE(f.articles[1])},
	}); err != nil {
		t.Fatalf("RunStore.Commit: %v", err)
	}
	if err := f.job.ReplaceFromRuns([]int32{0}, []durability.Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 1, Offset: 0, Length: 2 * unitArtLen},
	}); err != nil {
		t.Fatalf("ReplaceFromRuns: %v", err)
	}
	if err := f.job.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	seedFileCRC(t, f.job, 0, 0xC0FFEE)
	// Shorter than the 200 bytes the runs claim, so the gate discards them.
	if err := os.WriteFile(f.path, bytes.Repeat([]byte{'r'}, 50), 0o600); err != nil {
		t.Fatalf("rewrite the replaced file: %v", err)
	}
	p := f.job.Progress()
	if !p.ArticleDone(0) || !p.ArticleDone(1) || !p.FileComplete(0) {
		t.Fatalf("fixture did not reach the post-download state: done=%v/%v complete=%v",
			p.ArticleDone(0), p.ArticleDone(1), p.FileComplete(0))
	}
}

// TestResumeAllJobs_SkipsAJobPastDownloading pins the phase bound, and both
// halves are needed: the guard is only meaningful if the very same on-disk
// state WOULD have been cleared without it.
//
// JobPhase.IsResident is true for the post-processing statuses too, so
// residency is not the bound this sweep needs now that it is authoritative.
// par2 repairs a file in place and the move relocates it, so the file the
// sweep would stat is no longer the file the download recorded — and the gate
// would discard its runs, clear real progress, drop Complete and throw away
// the assembled CRC on a file that is fine.
func TestResumeAllJobs_SkipsAJobPastDownloading(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		setup      func(f *resumeUnitFixture)
		wantSwept  bool
		wantReason string
	}{{
		name:       "downloading is still swept",
		setup:      nil,
		wantSwept:  true,
		wantReason: "the sweep must still be authoritative over a job it is downloading",
	}, {
		name: "paused is swept",
		setup: func(f *resumeUnitFixture) {
			if err := f.app.dispatcher.PauseJob(f.job.ID()); err != nil {
				t.Fatalf("PauseJob: %v", err)
			}
		},
		wantSwept:  true,
		wantReason: "a paused job is mid-download and the assembler wrote every byte in its files",
	}, {
		name: "verifying is skipped",
		setup: func(f *resumeUnitFixture) {
			if err := f.job.SetNext(job.Assessing); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			if err := f.job.Transition(job.Assessing); err != nil {
				t.Fatalf("Transition: %v", err)
			}
		},
		wantSwept:  false,
		wantReason: "par2 rewrote this file and its bytes are correct; the durable runs describe what the assembler wrote, not what the repair produced",
	}, {
		name: "moving is skipped",
		setup: func(f *resumeUnitFixture) {
			if err := f.job.SetNext(job.Assessing); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			if err := f.job.Transition(job.Assessing); err != nil {
				t.Fatalf("Transition: %v", err)
			}
			if err := f.job.SetNext(job.Extracting); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			if _, err := f.job.Cross(job.Extracting); err != nil {
				t.Fatalf("Cross: %v", err)
			}
			if err := f.job.SetNext(job.Finalizing); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			if err := f.job.Transition(job.Finalizing); err != nil {
				t.Fatalf("Transition: %v", err)
			}
		},
		wantSwept:  false,
		wantReason: "the file is being relocated out of the download directory, so what is or is not there is not evidence about any article",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newResumeUnitFixture(t)
			f.replacedUnderneath(t)
			f.saveQueue(t)
			if tt.setup != nil {
				tt.setup(f)
			}

			if err := f.app.resumeAllJobs(t.Context()); err != nil {
				t.Fatalf("resumeAllJobs: %v", err)
			}

			p := f.job.Progress()
			gotSwept := !p.ArticleDone(0) || !p.ArticleDone(1) || !p.FileComplete(0)
			if gotSwept != tt.wantSwept {
				t.Errorf("swept = %v, want %v (done=%v/%v complete=%v crc=%#x) — %s",
					gotSwept, tt.wantSwept, p.ArticleDone(0), p.ArticleDone(1),
					p.FileComplete(0), p.FileAssembledCRC32(0), tt.wantReason)
			}
			if tt.wantSwept {
				return
			}
			if got := p.FileAssembledCRC32(0); got != 0xC0FFEE {
				t.Errorf("assembled CRC = %#x, want 0xC0FFEE — %s", got, tt.wantReason)
			}
		})
	}
}

// TestSweptState enumerates the bound directly over AllStates(), so a state
// added to job without a decision here shows up as a failing case rather than as
// silent coverage or silent exclusion.
func TestSweptState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state job.State
		want  bool
		why   string
	}{
		{job.StateUnset, false, "never run; assembler has written nothing"},
		{job.Fetching, true, "PhaseActive: the assembler is the only writer"},
		{job.Assessing, false, "par2 repairs the file in place"},
		{job.Repairing, false, "par2 repairs the file in place"},
		{job.Extracting, false, "unpack reads it and writes elsewhere"},
		{job.Finalizing, false, "the file is being relocated out of the download directory"},
	} {
		got := tc.state == job.Fetching
		if got != tc.want {
			t.Errorf("swept(%v) = %v, want %v — %s", tc.state, got, tc.want, tc.why)
		}
	}
}
