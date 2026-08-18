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
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
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
	job      *queue.Job
	dir      string
	path     string
	articles [2][]byte
}

// newResumeUnitFixture builds a job whose FIRST file has a resolved on-disk
// name, a partially written file, Class A facts for both its articles and a
// Class B extent claiming only the first durable — and whose SECOND file has
// a committed extent but no resolved name, so nothing ever opened a path for
// it.
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
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "resume-unit"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.queue.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := application.queue.SetFileFilename(job.ID, 0, unitFileOne); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}

	f := &resumeUnitFixture{app: application, repo: repo, job: job}
	f.articles[0] = bytes.Repeat([]byte{'x'}, unitArtLen)
	f.articles[1] = bytes.Repeat([]byte{'y'}, unitArtLen)
	f.dir = filepath.Join(application.pipeline.downloadDir, job.Name)
	f.path = filepath.Join(f.dir, unitFileOne)
	if err := os.MkdirAll(f.dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Article 1's region is left zero — the sparse hole an article that never
	// landed leaves behind — so its recorded CRC cannot match.
	onDisk := make([]byte, 2*unitArtLen)
	copy(onDisk, f.articles[0])
	if err := os.WriteFile(f.path, onDisk, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	facts := make([]durability.ArticleFact, 0, 2)
	for a := range 2 {
		facts = append(facts, durability.ArticleFact{
			FileIdx: 0,
			ArtIdx:  int32(a),
			Offset:  int64(a * unitArtLen),
			Length:  unitArtLen,
			CRC32:   crc32.ChecksumIEEE(f.articles[a]),
		})
	}
	if err := durability.NewSQLiteFactLog(repo.DB()).Append(t.Context(), job.ID, facts); err != nil {
		t.Fatalf("FactLog.Append: %v", err)
	}

	fi, err := os.Stat(f.path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	first := durability.NewBitmap(2)
	first.Set(0)
	// File 1's extent claims a durable article too. It must not reach the
	// work set: with no resolved name there is no path to have verified it
	// against, and adopting it would mark an article Done on the strength of
	// a cache alone.
	second := durability.NewBitmap(2)
	second.Set(0)
	err = durability.NewSQLiteExtentStore(repo.DB()).Commit(t.Context(), job.ID, []durability.FileExtent{
		{FileIdx: 0, Durable: first, VerifiedTo: unitArtLen, BytesDurable: unitArtLen,
			Size: fi.Size(), ModTimeNs: fi.ModTime().UnixNano()},
		{FileIdx: 1, Durable: second, BytesDurable: unitArtLen, Size: 2 * unitArtLen},
	})
	if err != nil {
		t.Fatalf("ExtentStore.Commit: %v", err)
	}
	return f
}

// saveQueue persists the fixture's queue to the store the way a running
// process does.
//
// It exists because a non-resident job's progress is re-read from the store on
// hydration, so a filename that only ever lived in memory is gone by the time
// the sweep looks — and the sweep then skips the file for having no resolved
// name, which makes a paused-job assertion pass for a reason that has nothing
// to do with the phase bound. A real process has already run both the periodic
// save and the shutdown save.
func (f *resumeUnitFixture) saveQueue(t *testing.T) {
	t.Helper()
	adminDir := f.app.config.GetGeneral().AdminDir
	if err := f.app.queue.Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// nameSecondFile gives file 1 a resolved on-disk name and a file to match, so
// the sweep has two files to walk rather than one. Without it the per-file
// context check has nothing to stop at.
func (f *resumeUnitFixture) nameSecondFile(t *testing.T) {
	t.Helper()
	if err := f.app.queue.SetFileFilename(f.job.ID, 1, "B.bin"); err != nil {
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
	if err := f.app.queue.SetFileFilename(f.job.ID, 1, "B.bin"); err != nil {
		t.Fatalf("SetFileFilename(1): %v", err)
	}
	loop := filepath.Join(f.dir, "B.bin")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatalf("symlink loop: %v", err)
	}
}

func (f *resumeUnitFixture) snapshot(t *testing.T) (*queue.Job, *queue.Manifest) {
	t.Helper()
	snap := f.app.queue.SnapshotJob(f.job.ID)
	if snap == nil {
		t.Fatal("job vanished from the queue")
	}
	m, err := snap.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	return snap, m
}

// TestResumeJobFiles_SkipsFilesWithNoResolvedName pins both halves of the
// conversion: the file that has a name on disk contributes exactly the
// articles its bytes prove, and the file that never resolved a name
// contributes nothing at all despite having a committed extent.
func TestResumeJobFiles_SkipsFilesWithNoResolvedName(t *testing.T) {
	f := newResumeUnitFixture(t)
	snap, m := f.snapshot(t)

	exts, fault, err := f.app.resumeJobFiles(t.Context(), snap, m)
	if err != nil {
		t.Fatalf("resumeJobFiles: %v", err)
	}
	if fault != nil {
		t.Fatalf("fault = %v, want none", fault)
	}
	if len(exts) != 1 {
		t.Fatalf("got %d extents, want 1 — file 1 has no resolved on-disk name, so nothing "+
			"verified it and its committed extent must not be adopted", len(exts))
	}
	if exts[0].FileIdx != 0 {
		t.Errorf("FileIdx = %d, want 0", exts[0].FileIdx)
	}
	if !exts[0].Durable.Get(0) {
		t.Error("article 0's bytes are on disk and hash to their recorded CRC, but it is not durable")
	}
	if exts[0].Durable.Get(1) {
		t.Error("article 1's region on disk is a hole, but it came back durable")
	}
	// Only FileIdx and Durable may be set: SeedFromExtents reads nothing else,
	// and ResumeResult carries no stat for the rest to have come from.
	if exts[0].Size != 0 || exts[0].ModTimeNs != 0 || exts[0].BytesDurable != 0 ||
		exts[0].VerifiedTo != 0 || exts[0].PrefixCRC != 0 || exts[0].HasPrefixCRC {
		t.Errorf("converted extent carries fields no resume established: %+v", exts[0])
	}
}

// TestResumeAllJobs_SeedsResidentAndSkipsNonResident pins the residency rule
// in both directions at once.
//
// ReplaceFromResume installs bits into the LIVE job's progress, which needs a
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
	f := newResumeUnitFixture(t)

	// A second job, paused so the queue evicts its manifest — and so the
	// phase bound skips it. It carries a committed extent of its own; nothing
	// may reach it.
	other, err := queue.NewJob(&nzb.NZB{Files: []nzb.File{{
		Subject:  "B.bin",
		Bytes:    unitArtLen,
		Articles: []nzb.Article{{ID: "b0@t", Bytes: unitArtLen, Number: 1}},
	}}}, queue.AddOptions{Name: "resume-unit-other"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob(other): %v", err)
	}
	if err := f.app.queue.Add(other); err != nil {
		t.Fatalf("Add(other): %v", err)
	}
	if err := f.app.queue.SetFileFilename(other.ID, 0, "B.bin"); err != nil {
		t.Fatalf("SetFileFilename(other): %v", err)
	}
	// Its file, facts and extent are all real and all consistent, so the only
	// thing standing between it and a Done bit is its residency. Without them
	// the assertion below would hold for a sweep that tried to seed it and
	// merely found nothing.
	otherDir := filepath.Join(f.app.pipeline.downloadDir, other.Name)
	if err := os.MkdirAll(otherDir, 0o750); err != nil {
		t.Fatalf("mkdir(other): %v", err)
	}
	otherBytes := bytes.Repeat([]byte{'z'}, unitArtLen)
	otherPath := filepath.Join(otherDir, "B.bin")
	if err := os.WriteFile(otherPath, otherBytes, 0o600); err != nil {
		t.Fatalf("write(other): %v", err)
	}
	if err := durability.NewSQLiteFactLog(f.repo.DB()).Append(t.Context(), other.ID,
		[]durability.ArticleFact{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: unitArtLen,
			CRC32: crc32.ChecksumIEEE(otherBytes)}}); err != nil {
		t.Fatalf("FactLog.Append(other): %v", err)
	}
	otherFI, err := os.Stat(otherPath)
	if err != nil {
		t.Fatalf("stat(other): %v", err)
	}
	otherBM := durability.NewBitmap(1)
	otherBM.Set(0)
	if err := durability.NewSQLiteExtentStore(f.repo.DB()).Commit(t.Context(), other.ID,
		[]durability.FileExtent{{FileIdx: 0, Durable: otherBM, VerifiedTo: unitArtLen,
			BytesDurable: unitArtLen, Size: otherFI.Size(), ModTimeNs: otherFI.ModTime().UnixNano()}}); err != nil {
		t.Fatalf("ExtentStore.Commit(other): %v", err)
	}
	if err := f.app.queue.SetStatus(other.ID, constants.StatusPaused); err != nil {
		t.Fatalf("SetStatus(other, Paused): %v", err)
	}
	// Read through Snapshot rather than SnapshotJob: SnapshotJob hydrates the
	// CLONE it returns, so it reports a manifest for an evicted job and would
	// make this guard unfalsifiable. Snapshot does not hydrate, and it is also
	// the exact list resumeAllJobs walks.
	var otherResident bool
	for _, snap := range f.app.queue.Snapshot() {
		if snap.ID != other.ID {
			continue
		}
		_, mErr := snap.Manifest()
		otherResident = mErr == nil
	}
	if otherResident {
		t.Fatal("pausing did not evict the second job's manifest, so it is resident and " +
			"the assertion below says nothing")
	}
	if snap := f.app.queue.Snapshot(); len(snap) != 2 {
		t.Fatalf("fixture guard: %d jobs in the queue, want 2", len(snap))
	}

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	seeded := f.app.queue.SnapshotJob(f.job.ID).Progress()
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
	// A guard rather than a pin on today's code: the sweep steps over a job
	// it cannot read a manifest for, so nothing currently reachable could set
	// this bit. It is here for the change that would — hydrating the queue at
	// startup to widen the sweep's coverage — because that change must bring
	// a verified path with it, not adopt the cache on residency alone.
	if got := f.app.queue.SnapshotJob(other.ID); got.Progress().ArticleDone(0) {
		t.Error("a non-resident job was seeded from a cache nothing verified")
	}
}

// countingResumer records every per-file Resume call and lets a test act
// between them.
//
// It exists because R15's "interruptible between FILES, not merely between
// jobs" is a claim about how many files were processed, and nothing outside
// the sweep can observe that otherwise. The previous version of the test
// below asserted only that an error came back, which ExtentStore.Load
// produces on its own when the context is already dead — so it passed with
// both context checks deleted.
type countingResumer struct {
	inner  fileResumer
	calls  []int32
	onCall func(fileIdx int32)
}

func (c *countingResumer) Resume(ctx context.Context, jobID string, fileIdx int32, path string, firstArtIdx int32, artCount int) (durability.ResumeResult, error) {
	c.calls = append(c.calls, fileIdx)
	res, err := c.inner.Resume(ctx, jobID, fileIdx, path, firstArtIdx, artCount)
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
	f := newResumeUnitFixture(t)
	if err := f.app.queue.SetStatus(f.job.ID, constants.StatusPaused); err != nil {
		t.Fatalf("SetStatus(Paused): %v", err)
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
	if f.app.queue.SnapshotJob(f.job.ID).Progress().ArticleDone(0) {
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

func (c *cancelledDuringResume) Resume(_ context.Context, jobID string, fileIdx int32, _ string, _ int32, _ int) (durability.ResumeResult, error) {
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
// permanent-L3-regression loop as discarding the partial extents, reached by
// an ordinary Ctrl-C during startup rather than by a flaking mount.
//
// The assertions are on the distinction, not on the existence of an error:
// that the abort carries context.Canceled, and that the job was NOT stalled.
// "An error came back" is the assertion shape that made the cancellation test
// inert twice on this branch.
func TestResumeAllJobs_ShutdownDuringResumeIsNotAStorageFault(t *testing.T) {
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
	snap := f.app.queue.SnapshotJob(f.job.ID)
	if snap.Status == constants.StatusPaused {
		t.Errorf("a shutdown during Resume was routed as a storage fault: status = %q. "+
			"A stalled job is non-resident and is skipped by every future sweep, so this "+
			"costs the job its seed permanently", snap.Status)
	}
	if strings.HasPrefix(snap.Warning, "Stalled: ") {
		t.Errorf("a shutdown during Resume was surfaced as a storage condition that never "+
			"existed: warning = %q", snap.Warning)
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
	f := newResumeUnitFixture(t)
	f.faultOnSecondFile(t)

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	snap := f.app.queue.SnapshotJob(f.job.ID)
	if !snap.Progress().ArticleDone(0) {
		t.Error("file 0 resumed cleanly, but its durable article was discarded because file 1 " +
			"faulted afterwards — a transient fault must not cost the ground already recovered")
	}
	// Also a forward guard, not a pin. On the fault path Resume returns the
	// zero ResumeResult, so the bitmap is empty and seeds nothing however the
	// conversion is mutated. S3 itself is pinned by assertDone under
	// TestResumeAtStartup_StorageFaultStallsAndDoesNotFailArticles, so nothing
	// is uncovered here — only this line is not what covers it.
	if snap.Progress().ArticleDone(1) {
		t.Error("an article whose bytes are not on disk came back Done")
	}
	if snap.Status != constants.StatusPaused {
		t.Errorf("status = %q, want %q — the faulting file must still stall the job", snap.Status, constants.StatusPaused)
	}
	if !strings.HasPrefix(snap.Warning, "Stalled: ") {
		t.Errorf("warning = %q, want a surfaced stall reason (R27)", snap.Warning)
	}
}

// TestJobFilePath_ResolvesUnderTheJobDirectory pins that the sweep looks for a
// file where the assembler wrote it, and that a name recorded in the queue
// cannot walk out of the job's own directory.
func TestJobFilePath_ResolvesUnderTheJobDirectory(t *testing.T) {
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

// repairedInPlace makes file 0 look like par2 repaired it: the bytes on disk
// are correct output but they are not the bytes the fact log recorded at
// download time, and the extent's stamp no longer matches, so the fast path is
// refused and a recomputation can prove nothing about the file.
//
// It also records the state a fully downloaded job carries into
// post-processing — both articles Done, the file Complete, an assembled CRC —
// because that is what the sweep would destroy.
func (f *resumeUnitFixture) repairedInPlace(t *testing.T) {
	t.Helper()
	both := durability.NewBitmap(2)
	both.Set(0)
	both.Set(1)
	if err := f.app.queue.SeedFromExtents(f.job.ID, []durability.FileExtent{
		{FileIdx: 0, Durable: both},
	}); err != nil {
		t.Fatalf("SeedFromExtents: %v", err)
	}
	if err := f.app.queue.MarkFileComplete(f.job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	if err := f.app.queue.SetFileCRC32(f.job.ID, 0, 0xC0FFEE); err != nil {
		t.Fatalf("SetFileCRC32: %v", err)
	}
	repaired := bytes.Repeat([]byte{'r'}, 2*unitArtLen)
	if err := os.WriteFile(f.path, repaired, 0o600); err != nil {
		t.Fatalf("rewrite repaired file: %v", err)
	}
	// Explicit rather than relying on the write moving it: a coarse timestamp
	// granularity could leave the stamp matching, the fast path would adopt
	// the cached bitmap, and both cases below would pass for the wrong reason.
	stamp := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(f.path, stamp, stamp); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	p := f.app.queue.SnapshotJob(f.job.ID).Progress()
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
// par2 repairs a file in place, and the repaired bytes will not match the CRCs
// the fact log recorded at download time — so a recomputation over them proves
// nothing and would clear real progress, drop Complete and discard the
// assembled CRC on a file that is fine.
func TestResumeAllJobs_SkipsAJobPastDownloading(t *testing.T) {
	tests := []struct {
		name string
		// walk is the transition path from Downloading, because SetStatus
		// enforces the legal-edge table and Downloading -> Moving is not one
		// of its edges.
		walk       []constants.Status
		wantSwept  bool
		wantReason string
	}{{
		// The control, and it is what stops the two cases below from passing
		// against a sweep that does nothing at all: the SAME on-disk state
		// must be cleared here. Downloading is the phase where the assembler
		// is the only writer, so a file whose bytes stopped matching its facts
		// really did lose them.
		name:       "downloading is still swept",
		walk:       nil,
		wantSwept:  true,
		wantReason: "the sweep must still be authoritative over a job it is downloading",
	}, {
		// A paused job is mid-download: nothing but the assembler has ever
		// written its files. Skipping it let #362 survive in that branch —
		// the disproven Done bits were never corrected, and priorExtent ORs
		// the stored bitmap, so the next checkpoint re-committed them with a
		// fresh matching stamp and the file finalized over a hole. It is also
		// the branch Application.Stall leaves jobs in, which is what made
		// stallLost's "restart gonzbd to resume this job" unable to work.
		//
		// It is NOT resident, so this case also covers the hydration
		// ReplaceFromResume does for the duration of the correction.
		name:       "paused is swept",
		walk:       []constants.Status{constants.StatusPaused},
		wantSwept:  true,
		wantReason: "a paused job is mid-download and the assembler wrote every byte in its files",
	}, {
		name:       "verifying is skipped",
		walk:       []constants.Status{constants.StatusVerifying},
		wantSwept:  false,
		wantReason: "par2 rewrote this file and its bytes are correct; the fact log describes what the assembler wrote, not what the repair produced",
	}, {
		name:       "moving is skipped",
		walk:       []constants.Status{constants.StatusVerifying, constants.StatusMoving},
		wantSwept:  false,
		wantReason: "the file is being relocated out of the download directory, so what is or is not there is not evidence about any article",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newResumeUnitFixture(t)
			f.repairedInPlace(t)
			// Persisted before the status walk. A non-resident job's progress
			// is re-read from the store on hydration, so a filename that only
			// ever lived in memory is gone by the time the sweep looks — and
			// then the sweep skips the file for having no resolved name,
			// which would make the paused case pass for a reason that has
			// nothing to do with the phase bound. In a real process the
			// periodic save and the shutdown save have both already run.
			f.saveQueue(t)
			want := constants.StatusDownloading
			for _, st := range tt.walk {
				if err := f.app.queue.SetStatus(f.job.ID, st); err != nil {
					t.Fatalf("SetStatus(%s): %v", st, err)
				}
				want = st
			}
			if got := f.app.queue.SnapshotJob(f.job.ID); got == nil || got.Status != want {
				t.Fatalf("the job did not reach %s", want)
			}

			if err := f.app.resumeAllJobs(t.Context()); err != nil {
				t.Fatalf("resumeAllJobs: %v", err)
			}

			p := f.app.queue.SnapshotJob(f.job.ID).Progress()
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

// TestSweptStatus enumerates the bound directly, so a status added to
// constants without a decision here shows up as a failing case rather than as
// silent coverage or silent exclusion.
//
// The two directions cost different things. Sweeping a status the assembler no
// longer owns clears real progress — par2 repairs a file in place, and a
// recomputation over the repaired bytes proves nothing. NOT sweeping one it
// does own leaves a disproven Done bit to be re-committed by the next
// checkpoint, and the file finalizes over a hole (#362).
func TestSweptStatus(t *testing.T) {
	for _, tc := range []struct {
		status constants.Status
		want   bool
		why    string
	}{
		{constants.StatusDownloading, true, "the assembler is the only writer"},
		{constants.StatusFetching, true, "PhaseActive, and nothing assigns it — see the note on resumeAllJobs"},
		{constants.StatusPaused, true, "mid-download, and where Application.Stall leaves a job"},
		{constants.StatusQueued, false, "nothing has been written for it yet"},
		{constants.StatusVerifying, false, "par2 repairs the file in place"},
		{constants.StatusRepairing, false, "par2 repairs the file in place"},
		{constants.StatusExtracting, false, "unpack reads it and writes elsewhere"},
		{constants.StatusMoving, false, "the file is being relocated out of the download directory"},
		{constants.StatusCompleted, false, "the job has left the download stage entirely"},
		{constants.StatusFailed, false, "the job has left the download stage entirely"},
	} {
		if got := sweptStatus(tc.status); got != tc.want {
			t.Errorf("sweptStatus(%v) = %v, want %v — %s", tc.status, got, tc.want, tc.why)
		}
	}
}
