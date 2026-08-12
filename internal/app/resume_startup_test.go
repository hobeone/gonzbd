package app_test

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// The fixture below is a three-article file, and EVERY test that uses it
// leaves at least one of those articles durable and at least one not.
//
// That is a hard requirement, not a style. A fixture in which every article is
// durable is satisfied by a "mark everything Done" mutation, and one in which
// none are is satisfied by a "seed nothing" mutation — so a single-state
// fixture cannot tell the seeding logic from its absence. Both mutations have
// to go red, which takes both states present in one file.
const (
	resumePartLen  = 64
	resumeArts     = 3
	resumeTotal    = int64(resumePartLen * resumeArts)
	resumeFileName = "resume.bin"
)

// resumeFixture is a job that a previous run left half-downloaded: its target
// file exists on disk, Class A facts describe every article's byte range, and
// a Class B extent records which of them a barrier actually fsynced.
//
// Nothing here writes the queue's own articles_done bits. That matters: the
// queue store restores those at load, so a fixture that set them would make
// every ArticleDone assertion below true whether or not the resume sweep ran
// at all.
type resumeFixture struct {
	t           *testing.T
	adminDir    string
	downloadDir string
	completeDir string
	repo        *history.Repository
	server      *nntptest.Scripted

	jobID   string
	jobName string
	dir     string
	path    string
	msgIDs  [resumeArts]string
	parts   [resumeArts][]byte
}

func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	f := &resumeFixture{
		t: t, adminDir: adminDir, downloadDir: downloadDir,
		completeDir: completeDir, repo: repo, server: nntptest.New(t),
	}

	arts := make([]nzb.Article, 0, resumeArts)
	for i := range resumeArts {
		// Distinct and non-zero per article. A zero-filled part would hash to
		// the CRC of the sparse hole the assembler leaves behind, so the
		// recompute path would "verify" an article that never landed.
		f.parts[i] = bytes.Repeat([]byte{byte('A' + i)}, resumePartLen)
		f.msgIDs[i] = randomMsgID(t)
		f.server.AddArticle(f.msgIDs[i],
			yencMultiPart(resumeFileName, f.parts[i], i+1, resumeArts, resumeTotal))
		arts = append(arts, nzb.Article{ID: f.msgIDs[i], Bytes: resumePartLen, Number: i + 1})
	}
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  fmt.Sprintf(`"%s" yEnc (1/%d)`, resumeFileName, resumeArts),
		Bytes:    resumeTotal,
		Articles: arts,
	}}}

	seed := newSeedQueue(t, repo, adminDir)
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "resume-job"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := seed.Add(job); err != nil {
		t.Fatalf("seed.Add: %v", err)
	}
	// The resolved on-disk name is what the sweep uses to find the file, and
	// registerFile is what records it in a real run. Recording it here is the
	// same state a run that had already written the file would leave behind.
	if err := seed.SetFileFilename(job.ID, 0, resumeFileName); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	if err := seed.Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("seed.Save: %v", err)
	}

	f.jobID = job.ID
	f.jobName = job.Name
	f.dir = filepath.Join(downloadDir, job.Name)
	f.path = filepath.Join(f.dir, resumeFileName)
	return f
}

// writePartial lays down the target file at its full expected size with only
// the named articles' bytes present; every other region is left zero, which is
// what the assembler's sparse pre-allocation leaves for an article that never
// arrived.
func (f *resumeFixture) writePartial(present ...int) {
	f.t.Helper()
	if err := os.MkdirAll(f.dir, 0o750); err != nil {
		f.t.Fatalf("mkdir %s: %v", f.dir, err)
	}
	buf := make([]byte, resumeTotal)
	for _, i := range present {
		copy(buf[i*resumePartLen:], f.parts[i])
	}
	if err := os.WriteFile(f.path, buf, 0o600); err != nil {
		f.t.Fatalf("write %s: %v", f.path, err)
	}
}

// appendFacts records Class A for every article. A fact is written when an
// article is DECODED, so it exists for articles whose write was later lost —
// which is exactly what makes the recompute path able to disprove a stale
// extent rather than merely fail to confirm it.
func (f *resumeFixture) appendFacts() {
	f.t.Helper()
	facts := make([]durability.ArticleFact, 0, resumeArts)
	for i := range resumeArts {
		facts = append(facts, durability.ArticleFact{
			FileIdx: 0,
			ArtIdx:  int32(i),
			Offset:  int64(i * resumePartLen),
			Length:  resumePartLen,
			CRC32:   crc32.ChecksumIEEE(f.parts[i]),
		})
	}
	fl := durability.NewSQLiteFactLog(f.repo.DB())
	if err := fl.Append(f.t.Context(), f.jobID, facts); err != nil {
		f.t.Fatalf("FactLog.Append: %v", err)
	}
}

// commitExtent stamps a Class B record over the file as it exists right now,
// claiming the named articles durable. Committed here rather than through a
// barrier because the point of these tests is what a LATER process makes of a
// record an earlier one left.
func (f *resumeFixture) commitExtent(durableArts ...int) {
	f.t.Helper()
	fi, err := os.Stat(f.path)
	if err != nil {
		f.t.Fatalf("stat %s: %v", f.path, err)
	}
	bm := durability.NewBitmap(resumeArts)
	for _, i := range durableArts {
		bm.Set(i)
	}
	es := durability.NewSQLiteExtentStore(f.repo.DB())
	err = es.Commit(f.t.Context(), f.jobID, []durability.FileExtent{{
		FileIdx:      0,
		Durable:      bm,
		VerifiedTo:   resumePartLen,
		PrefixCRC:    crc32.ChecksumIEEE(f.parts[0]),
		BytesDurable: int64(len(durableArts)) * resumePartLen,
		Size:         fi.Size(),
		ModTimeNs:    fi.ModTime().UnixNano(),
	}})
	if err != nil {
		f.t.Fatalf("ExtentStore.Commit: %v", err)
	}
}

// stall parks the named articles forever, holding their connections open.
//
// It is how these tests keep Done attributable. The checkpoint cadence is
// pinned open below, so the only other thing that can ack an article is the
// file-completion finalize — and a file with a permanently stalled part never
// completes. With this in place, a Done bit can only have come from the
// startup sweep.
func (f *resumeFixture) stall(arts ...int) {
	f.t.Helper()
	for _, i := range arts {
		f.server.InjectFailure(f.msgIDs[i], nntptest.FailureStall)
	}
}

// start brings the application up over the seeded state. Both checkpoint
// bounds are pinned far out of reach so no barrier runs during the test.
func (f *resumeFixture) start(conns int) *app.Application {
	f.t.Helper()
	srvCfg := f.server.ServerConfig("resume", conns)
	srvCfg.Timeout = 120 // a stall must outlast the assertions
	a, err := app.New(testConfig(f.downloadDir, f.completeDir, f.adminDir, srvCfg),
		f.repo,
		app.WithPostProcStages([]postproc.Stage{noOpStage{}}),
		app.WithCheckpointInterval(time.Hour),
		app.WithCheckpointBytes(1<<40),
	)
	if err != nil {
		f.t.Fatalf("app.New: %v", err)
	}
	ctx, cancel := context.WithCancel(f.t.Context())
	f.t.Cleanup(func() {
		a.ForceStopWorkers()
		cancel()
	})
	if err := a.Start(ctx); err != nil {
		f.t.Fatalf("app.Start: %v", err)
	}
	go drainAny(ctx, a.JobComplete())
	go drainAny(ctx, a.PostProcComplete())
	return a
}

// assertDone checks the full per-article done vector in one shot, so a test
// can never assert only the half that happens to agree with it.
func (f *resumeFixture) assertDone(a *app.Application, want [resumeArts]bool) {
	f.t.Helper()
	snap := a.Queue().SnapshotJob(f.jobID)
	if snap == nil {
		f.t.Fatal("job left the queue before it could be inspected")
	}
	var got [resumeArts]bool
	for i := range resumeArts {
		got[i] = snap.Progress().ArticleDone(i)
	}
	if got != want {
		f.t.Errorf("article done vector = %v, want %v", got, want)
	}
}

// dumpExtents serialises every file_extents row for the job, for a
// before/after comparison.
func (f *resumeFixture) dumpExtents() string {
	f.t.Helper()
	rows, err := f.repo.DB().QueryContext(f.t.Context(), `
SELECT job_id, file_idx, hex(durable_bitmap), verified_to, prefix_crc,
       has_prefix_crc, bytes_durable, size, mod_time_ns
FROM file_extents ORDER BY job_id, file_idx`)
	if err != nil {
		f.t.Fatalf("query file_extents: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out strings.Builder
	for rows.Next() {
		var jobID, bmHex string
		var fileIdx, verifiedTo, prefixCRC, hasCRC, bytesDurable, size, modTime int64
		if err := rows.Scan(&jobID, &fileIdx, &bmHex, &verifiedTo, &prefixCRC,
			&hasCRC, &bytesDurable, &size, &modTime); err != nil {
			f.t.Fatalf("scan file_extents: %v", err)
		}
		fmt.Fprintf(&out, "%s|%d|%s|%d|%d|%d|%d|%d|%d\n", jobID, fileIdx, bmHex,
			verifiedTo, prefixCRC, hasCRC, bytesDurable, size, modTime)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate file_extents: %v", err)
	}
	return out.String()
}

// TestResumeAtStartup_SeedsDurableArticles is the base claim: what a previous
// run fsynced comes back Done, and what it did not comes back Outstanding.
func TestResumeAtStartup_SeedsDurableArticles(t *testing.T) {
	f := newResumeFixture(t)
	f.writePartial(0, 2)
	f.appendFacts()
	f.commitExtent(0, 2)
	f.stall(1) // the one Outstanding article must not be able to become Done

	a := f.start(2)
	f.assertDone(a, [resumeArts]bool{true, false, true})
}

// TestResumeAtStartup_DurableArticlesAreNeverRefetched is the ordering pin,
// and the one that actually states L3.
//
// Marking the right bits is not the property; not asking for the bytes is. A
// sweep that runs after the downloader has started passes the test above and
// fails this one, because the request for a durable article is already on the
// wire by the time its bit is set.
func TestResumeAtStartup_DurableArticlesAreNeverRefetched(t *testing.T) {
	f := newResumeFixture(t)
	f.writePartial(0, 2)
	f.appendFacts()
	f.commitExtent(0, 2)
	f.stall(1)

	a := f.start(2)

	// Wait for the Outstanding article to actually be asked for. Without this
	// the assertions below would pass against a downloader that had not yet
	// issued a single request — inert by timing.
	if !waitUntil(10*time.Second, func() bool { return f.server.FetchCount(f.msgIDs[1]) > 0 }) {
		t.Fatal("the outstanding article was never requested; nothing here proves what was skipped")
	}
	for _, i := range []int{0, 2} {
		if n := f.server.FetchCount(f.msgIDs[i]); n != 0 {
			t.Errorf("article %d was fetched %d time(s); its bytes were already fsynced by an earlier run (L3)", i, n)
		}
	}
	_ = a
}

// TestResumeAtStartup_MismatchedFileIsNotAdopted pins S7: the committed extent
// is a cache stamped against a file, and when the file no longer matches the
// stamp the cache is worthless.
//
// The extent here over-claims — it says all three articles are durable — while
// the file has been truncated so that only the first one's bytes are present
// and inside it. Adopting the cache marks all three Done; recomputing from the
// bytes marks exactly one.
func TestResumeAtStartup_MismatchedFileIsNotAdopted(t *testing.T) {
	f := newResumeFixture(t)
	f.writePartial(0)
	f.appendFacts()
	f.commitExtent(0, 1, 2)
	if err := os.Truncate(f.path, resumePartLen*2); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.stall(0, 1, 2)

	a := f.start(1)
	// Article 1's region is inside the truncated file but holds zeros, so its
	// CRC does not match; article 2's region is past the new end entirely.
	// Both are Outstanding, and the stale cache's claim about them is ignored.
	f.assertDone(a, [resumeArts]bool{true, false, false})
}

// TestResumeAtStartup_MissingFileLeavesEverythingOutstanding pins S3 for the
// restart case: with no file to verify against, nothing can be proven, and
// unproven means fetch it again.
//
// The articles are all stalled, so nothing else in the process can set a Done
// bit. If the missing file were ignored and the committed extent adopted
// anyway, two of them would come back Done.
func TestResumeAtStartup_MissingFileLeavesEverythingOutstanding(t *testing.T) {
	f := newResumeFixture(t)
	f.writePartial(0, 2)
	f.appendFacts()
	f.commitExtent(0, 2)
	if err := os.Remove(f.path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	f.stall(0, 1, 2)

	a := f.start(1)
	f.assertDone(a, [resumeArts]bool{false, false, false})
}

// TestResumeAtStartup_StorageFaultStallsAndDoesNotFailArticles pins A1, and
// the deliberate divergence from Barrier.routeFault that goes with it.
//
// A failure to READ the disk is a condition of the device and says nothing
// about any article's availability. Attributing it to the articles would burn
// their retry budget and make the job's reported health describe the disk
// instead of the download.
//
// Both classifications are exercised, and the permanent one is the point. The
// barrier routes a permanent fault to Fail; the startup sweep routes it to
// Stall, because at startup nothing has been downloaded yet, so failing
// protects no work while it does send the job to history and discard the
// bytes an earlier run left on disk. That divergence is invisible in a
// retryable-only fixture — restoring `if fault.Permanent { app.Fail(...) }`
// leaves every other test in this package green — so a later "harmonise the
// fault routing" change would silently start binning recoverable jobs on a
// boot-order EROFS.
func TestResumeAtStartup_StorageFaultStallsAndDoesNotFailArticles(t *testing.T) {
	tests := []struct {
		name string
		// fault makes the target file's stat fail, and reports whether the
		// resulting errno classifies permanent.
		fault func(t *testing.T, f *resumeFixture)
	}{{
		// ELOOP: not fs.ErrNotExist, so it cannot be confused with the
		// missing-file case, and not a listed permanent errno.
		name: "retryable ELOOP",
		fault: func(t *testing.T, f *resumeFixture) {
			t.Helper()
			if err := os.Symlink(f.path, f.path); err != nil {
				t.Fatalf("symlink loop: %v", err)
			}
		},
	}, {
		// EACCES, which storagefault classifies PERMANENT. The barrier would
		// Fail on this; the sweep must not.
		name: "permanent EACCES",
		fault: func(t *testing.T, f *resumeFixture) {
			t.Helper()
			if os.Geteuid() == 0 {
				t.Skip("running as root: mode bits do not deny access, so no EACCES can be produced")
			}
			f.writePartial(0, 2)
			f.commitExtent(0, 2)
			if err := os.Chmod(f.dir, 0o000); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(f.dir, 0o750) })
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newResumeFixture(t)
			f.appendFacts()
			if err := os.MkdirAll(f.dir, 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			tt.fault(t, f)

			a := f.start(1)

			snap := a.Queue().SnapshotJob(f.jobID)
			if snap == nil {
				t.Fatal("the job left the queue on a storage fault; it must stall, not be " +
					"failed into history — the bytes an earlier run left on disk go with it")
			}
			if snap.Status != constants.StatusPaused {
				t.Errorf("status = %q, want %q — a storage fault must park the job rather than "+
					"fail it or let it keep dispatching into a device that cannot be read",
					snap.Status, constants.StatusPaused)
			}
			if !strings.HasPrefix(snap.Warning, "Stalled: ") {
				t.Errorf("warning = %q, want a surfaced stall reason beginning \"Stalled: \" (R27); "+
					"a \"Failed: \" reason here means the fault was routed as terminal", snap.Warning)
			}
			for i := range resumeArts {
				if snap.Progress().ArticleFailed(i) {
					t.Errorf("article %d was marked permanently failed by a DISK read failure (A1)", i)
				}
			}
			f.assertDone(a, [resumeArts]bool{false, false, false})
		})
	}
}

// TestResumeAtStartup_DoesNotCommitClassB pins the rule that the barrier is
// the only writer of Class B.
//
// A committed extent asserts that a completed fsync stands behind it. A resume
// proves what is on disk without performing that fsync, so it has no licence
// to mint one — re-verifying on the next restart is bounded rework and is the
// correct cost.
func TestResumeAtStartup_DoesNotCommitClassB(t *testing.T) {
	f := newResumeFixture(t)
	f.writePartial(0, 2)
	f.appendFacts()
	f.commitExtent(0, 2)
	f.stall(1)

	before := f.dumpExtents()
	if before == "" {
		t.Fatal("no committed extent to compare against; the fixture proves nothing")
	}

	a := f.start(2)
	// The sweep is synchronous in Start, and the checkpoint bounds above put
	// the barrier out of reach, so anything that changed here was written by
	// the resume.
	f.assertDone(a, [resumeArts]bool{true, false, true})

	if after := f.dumpExtents(); after != before {
		t.Errorf("resume rewrote Class B:\n before %q\n after  %q", before, after)
	}
}
