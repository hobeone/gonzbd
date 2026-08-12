package app

import (
	"bytes"
	"context"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

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
				ID:     string(rune('A'+f)) + string(rune('0'+a)) + "@t",
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
// SeedFromExtents installs bits into the LIVE job's progress, which needs a
// resident manifest. A non-resident job cannot be seeded, and the sweep must
// step over it rather than fail the whole startup — while the resident job
// beside it is still seeded.
func TestResumeAllJobs_SeedsResidentAndSkipsNonResident(t *testing.T) {
	f := newResumeUnitFixture(t)

	// A second job, paused so the queue evicts its manifest. It carries a
	// committed extent of its own; nothing may reach it.
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
	// A guard rather than a pin on today's code: SeedFromExtents rejects a
	// non-resident job outright, so nothing currently reachable could set this
	// bit. It is here for the change that would — hydrating the queue at
	// startup to widen the sweep's coverage — because that change must bring
	// a verified path with it, not adopt the cache on residency alone.
	if got := f.app.queue.SnapshotJob(other.ID); got.Progress().ArticleDone(0) {
		t.Error("a non-resident job was seeded from a cache nothing verified")
	}
}

// TestResumeAllJobs_AbortsOnCancelledContext pins R15: a shutdown arriving
// during the sweep stops it rather than waiting out a whole-file CRC walk.
func TestResumeAllJobs_AbortsOnCancelledContext(t *testing.T) {
	f := newResumeUnitFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := f.app.resumeAllJobs(ctx); err == nil {
		t.Fatal("resumeAllJobs returned nil on a cancelled context, so a shutdown would wait for it")
	}
	if f.app.queue.SnapshotJob(f.job.ID).Progress().ArticleDone(0) {
		t.Error("an aborted sweep still seeded; the abort must leave the work set untouched")
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
