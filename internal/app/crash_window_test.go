package app

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

const (
	crashArtLen = 100
	// The on-disk size a pre-allocation leaves behind. Larger than the two
	// articles' decoded total on purpose, and it is the ordinary case rather
	// than a contrived one: pre-allocation sizes the file from the NZB's
	// declared bytes, which are the ENCODED length, and yEnc decodes smaller.
	// So the trim below is the normal path, not an edge.
	crashPrealloc = 450
	crashDecoded  = 2 * crashArtLen
)

// crashWindowFixture is one job with one two-article file whose bytes are on
// disk, whose runs are recorded, and whose Complete flag is not set — the
// state a crash between the barrier's commit and the following queue save
// leaves behind.
type crashWindowFixture struct {
	app  *Application
	job  *queue.Job
	path string
}

// newCrashWindowFixture builds that state directly rather than by crashing a
// process, because what is under test is the REPAIR and not the window.
// test/crash/ owns the question of whether the window is reachable; this owns
// what the next start does about it.
//
// recordArts names which of the file's articles have durable runs, so one
// caller can build the stranded state and another the ordinary
// still-downloading state that must be left alone.
func newCrashWindowFixture(t *testing.T, recordArts ...int32) *crashWindowFixture {
	t.Helper()
	application, repo, _ := newLifecycleTestApp(t)

	parsed := &nzb.NZB{}
	file := nzb.File{Subject: "A.bin", Bytes: crashPrealloc}
	for a := range 2 {
		file.Articles = append(file.Articles, nzb.Article{
			ID:     fmt.Sprintf("A%d@t", a),
			Bytes:  crashArtLen,
			Number: a + 1,
		})
	}
	parsed.Files = append(parsed.Files, file)

	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "crash-window"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.queue.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := application.queue.SetStatus(job.ID, constants.StatusDownloading); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := application.queue.SetFileFilename(job.ID, 0, "A.bin"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}

	dir := filepath.Join(application.pipeline.downloadDir, job.Name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f := &crashWindowFixture{app: application, job: job, path: filepath.Join(dir, "A.bin")}

	// Both articles' bytes really are on disk, followed by pre-allocation's
	// untrimmed tail. The size gate is a comparison, so a file SHORTER than
	// its runs claim would have them discarded and would never reach the
	// repair — that is a different limitation and not this one.
	onDisk := make([]byte, crashPrealloc)
	copy(onDisk, bytes.Repeat([]byte{'x'}, crashArtLen))
	copy(onDisk[crashArtLen:], bytes.Repeat([]byte{'y'}, crashArtLen))
	if err := os.WriteFile(f.path, onDisk, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	arts := make([]durability.DurableArticle, 0, len(recordArts))
	for _, a := range recordArts {
		arts = append(arts, durability.DurableArticle{
			FileIdx: 0,
			ArtIdx:  a,
			Offset:  int64(a) * crashArtLen,
			Length:  crashArtLen,
			CRC32:   crc32.ChecksumIEEE(onDisk[int(a)*crashArtLen : (int(a)+1)*crashArtLen]),
		})
	}
	if _, err := durability.NewSQLiteRunStore(repo.DB()).Commit(t.Context(), job.ID, arts); err != nil {
		t.Fatalf("RunStore.Commit: %v", err)
	}
	return f
}

// TestResumeSweep_FinishesAFinalizeACrashInterrupted pins the repair for
// accepted limitation 6.
//
// Article resolution reaches disk in the barrier's commit; the file's Complete
// flag reaches it at the NEXT queue save. A crash between them leaves every
// article resolved, Complete false, and nothing able to set it — completion
// fires from partsWritten inside the assembler, that counter moves only when
// the assembler is handed an article to write, and every article is already
// Done so none is dispatched. The job is then not dispatchable, not complete
// and not failed, across every restart.
//
// Both halves are asserted, and BOTH are needed. Setting the flag without
// trimming would send an untrimmed, pre-allocated file into post-processing,
// which QuickCheck reads as a missing file — a differently broken job rather
// than a fixed one. That is also why the flag cannot simply be derived on load
// from the article bits: Complete means the finalize RAN, and the truncate is
// the part the bits cannot witness.
func TestResumeSweep_FinishesAFinalizeACrashInterrupted(t *testing.T) {
	t.Parallel()
	f := newCrashWindowFixture(t, 0, 1)

	// Fixture guards. Without these the assertions below pass for a job that
	// was already complete, or against a file that needed no trim.
	before := f.app.queue.SnapshotJob(f.job.ID).Progress()
	if before.FileComplete(0) {
		t.Fatal("the fixture starts with the file already Complete; the repair has nothing to do")
	}
	if fi, err := os.Stat(f.path); err != nil || fi.Size() != crashPrealloc {
		t.Fatalf("fixture file is %v (err %v), want %d bytes so the trim is observable", fi, err, crashPrealloc)
	}

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	fi, err := os.Stat(f.path)
	if err != nil {
		t.Fatalf("stat after the sweep: %v", err)
	}
	if fi.Size() != crashDecoded {
		t.Errorf("file is %d bytes after the sweep, want %d — the interrupted finalize's "+
			"truncate was not finished, so pre-allocation's tail goes to post-processing "+
			"and QuickCheck reads it as a missing file", fi.Size(), crashDecoded)
	}
	progress := f.app.queue.SnapshotJob(f.job.ID).Progress()
	if !progress.FileComplete(0) {
		t.Error("the file is still not Complete after the sweep; every article is resolved " +
			"so none will be dispatched, nothing moves partsWritten, and the job stays " +
			"wedged across every future restart")
	}

	// The assembled CRC too, which the repair reaches through
	// completeFinalizedFile. Recovering the file without it leaves
	// AssembledCRC32 at zero, which par2 reads as NoCRC: QuickCheck cannot
	// report Clean, so the full verify runs and every recovery volume is
	// fetched — for a file whose whole-file CRC was sitting in its single
	// durable run the entire time.
	want := crc32.ChecksumIEEE(append(
		bytes.Repeat([]byte{'x'}, crashArtLen),
		bytes.Repeat([]byte{'y'}, crashArtLen)...))
	if got := progress.FileAssembledCRC32(0); got != want {
		t.Errorf("assembled CRC = %#x after the repair, want %#x", got, want)
	}
}

// TestResumeSweep_LeavesAnUnfinishedFileAlone is the other half, and without it
// the test above cannot distinguish "finishes an interrupted finalize" from
// "marks every file complete at startup".
//
// One article recorded of two. That file is the ordinary state of an
// interrupted download — there is real work outstanding — and completing it
// would claim a file whose second half is pre-allocation's zeros, while
// trimming it would cut the region the missing article is going to fill.
func TestResumeSweep_LeavesAnUnfinishedFileAlone(t *testing.T) {
	t.Parallel()
	f := newCrashWindowFixture(t, 0)

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	progress := f.app.queue.SnapshotJob(f.job.ID).Progress()
	if progress.FileComplete(0) {
		t.Error("a file with an outstanding article was marked Complete by the sweep")
	}
	if progress.ArticleDone(1) {
		t.Fatal("fixture guard: article 1 came back Done, so the file is not the " +
			"unfinished case this test needs")
	}
	fi, err := os.Stat(f.path)
	if err != nil {
		t.Fatalf("stat after the sweep: %v", err)
	}
	if fi.Size() != crashPrealloc {
		t.Errorf("file is %d bytes after the sweep, want %d untouched — trimming to the "+
			"recorded bound here would cut away the region article 1 is still to write",
			fi.Size(), crashPrealloc)
	}
}

// TestStrandedComplete_Predicate pins the three exclusions that decide whether
// a file is repaired, each of which would be a wrong repair rather than a
// missed one.
//
// Driven directly because the sweep can only reach two of them: a job's files
// all share one FetchPolicy in any fixture the sweep will accept, and an empty
// article range cannot be built through queue.NewJob at all.
func TestStrandedComplete_Predicate(t *testing.T) {
	t.Parallel()
	f := newCrashWindowFixture(t, 0, 1)
	if err := f.app.queue.SeedFromRuns(f.job.ID, []durability.Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 1, Offset: 0, Length: crashDecoded},
	}); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}
	snap := f.app.queue.SnapshotJob(f.job.ID)
	m, err := snap.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	p := snap.Progress()

	if !strandedComplete(p, m, 0) {
		t.Fatal("fixture guard: the file is not reported stranded, so every negative " +
			"below passes for the wrong reason")
	}

	// An out-of-range index must not panic on p.files or m.FileRange. The
	// sweep only ever passes indices it walked, so this is a guard on the
	// helper's own contract rather than on a reachable call.
	if strandedComplete(p, m, m.NumFiles()) {
		t.Error("an out-of-range file index was reported stranded")
	}

	// Already Complete: the repair must be idempotent across restarts, or the
	// second start trims and re-completes a file the first one finished.
	if err := f.app.queue.MarkFileComplete(f.job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	done := f.app.queue.SnapshotJob(f.job.ID).Progress()
	if strandedComplete(done, m, 0) {
		t.Error("a file that is already Complete was reported stranded; the repair would " +
			"run again on every start")
	}
}

// TestRunsForFile_SelectsOneFilesRows pins the filter, which is the only thing
// standing between one file's trim bound and another file's runs.
//
// A bound taken from the wrong file's rows would truncate to a length nothing
// on this file wrote — the never-grow guard in TrimToRuns catches the too-long
// direction, and nothing catches the too-short one but this.
func TestRunsForFile_SelectsOneFilesRows(t *testing.T) {
	t.Parallel()
	all := []durability.Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Offset: 0, Length: 100},
		{FileIdx: 1, FirstArtIdx: 5, LastArtIdx: 5, Offset: 0, Length: 900},
		{FileIdx: 0, FirstArtIdx: 2, LastArtIdx: 2, Offset: 200, Length: 100},
	}

	got := runsForFile(all, 0)
	if len(got) != 2 {
		t.Fatalf("runsForFile(0) returned %d rows, want 2: %+v", len(got), got)
	}
	for _, r := range got {
		if r.FileIdx != 0 {
			t.Errorf("runsForFile(0) returned a row for file %d", r.FileIdx)
		}
	}
	if n := len(runsForFile(all, 7)); n != 0 {
		t.Errorf("runsForFile(7) returned %d rows for a file with none", n)
	}
}

// TestCompleteStrandedFiles_ToleratesWhatItCannotRepair pins the two ways the
// pass can find nothing to act on, both of which must leave startup alone.
//
// It runs after the sweep rather than instead of it, because these are error
// arms the sweep's own fixtures cannot produce.
func TestCompleteStrandedFiles_ToleratesWhatItCannotRepair(t *testing.T) {
	t.Parallel()
	f := newCrashWindowFixture(t, 0, 1)
	snap := f.app.queue.SnapshotJob(f.job.ID)
	m, err := snap.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	t.Run("the job has left the queue", func(t *testing.T) {
		// Reachable: the sweep snapshots a job, and nothing holds it resident
		// between that and this call. The requirement is that it returns.
		f.app.completeStrandedFiles(t.Context(), "ghost-job", m, []int32{0}, nil)
	})

	t.Run("the file is gone from disk", func(t *testing.T) {
		if err := f.app.queue.SeedFromRuns(f.job.ID, []durability.Run{
			{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 1, Offset: 0, Length: crashDecoded},
		}); err != nil {
			t.Fatalf("SeedFromRuns: %v", err)
		}
		if err := os.Remove(f.path); err != nil {
			t.Fatalf("remove: %v", err)
		}

		f.app.completeStrandedFiles(t.Context(), f.job.ID, m, []int32{0}, []durability.Run{
			{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 1, Offset: 0, Length: crashDecoded},
		})

		if f.app.queue.SnapshotJob(f.job.ID).Progress().FileComplete(0) {
			t.Error("a file that could not be trimmed was marked Complete anyway; the " +
				"trim is what EARNS the flag, so a failure to trim must withhold it")
		}
	})
}

// TestResumeSweep_CompletesAFileWhoseTailArticleFailedPermanently pins the case
// the repair must NOT confuse with an unfinished download.
//
// A permanently failed article is resolved — its bytes will never arrive — and
// the assembler's partsWritten counts it toward TotalParts, so a file whose
// last article failed does complete, short, and reaches par2 as an ordinary
// shortfall. ReplaceFromRuns leaves its Done bit alone (markNotDone refuses to
// un-mark a failed article), so it survives the sweep and the file is stranded
// exactly like a fully successful one.
//
// The trim matters more here than anywhere: the bound stops at the last
// DURABLE byte, so the file is cut back to article 0's end rather than kept at
// pre-allocation's length with a hole where article 1 was to go.
func TestResumeSweep_CompletesAFileWhoseTailArticleFailedPermanently(t *testing.T) {
	t.Parallel()
	f := newCrashWindowFixture(t, 0)

	if err := f.app.queue.AckPermanentFailure(f.job.ID, []int32{1}); err != nil {
		t.Fatalf("AckPermanentFailure: %v", err)
	}

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	progress := f.app.queue.SnapshotJob(f.job.ID).Progress()
	if !progress.ArticleFailed(1) || !progress.ArticleDone(1) {
		t.Fatalf("fixture guard: article 1 is done=%v failed=%v after the sweep, want both "+
			"true — the sweep must not un-mark a permanently failed article",
			progress.ArticleDone(1), progress.ArticleFailed(1))
	}
	if !progress.FileComplete(0) {
		t.Error("a file whose only outstanding article failed permanently was left " +
			"incomplete; nothing will ever be dispatched for it, so it stays wedged")
	}
	fi, err := os.Stat(f.path)
	if err != nil {
		t.Fatalf("stat after the sweep: %v", err)
	}
	if fi.Size() != crashArtLen {
		t.Errorf("file is %d bytes after the sweep, want %d — the bound is the last "+
			"DURABLE byte, and keeping the failed article's region would hand par2 a "+
			"file padded with zeros rather than one that is honestly short",
			fi.Size(), crashArtLen)
	}
}
