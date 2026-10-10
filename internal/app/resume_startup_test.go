package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// The fixture below is a three-article file, and EVERY test that uses it
// leaves at least one of those articles written and at least one not.
//
// That is a hard requirement, not a style. A fixture in which every article is
// written is satisfied by a "mark everything Done" mutation, and one in which
// none are is satisfied by a "verify nothing" mutation — so a single-state
// fixture cannot tell the verification from its absence.
const (
	resumePartLen  = 64
	resumeArts     = 3
	resumeTotal    = int64(resumePartLen * resumeArts)
	resumeFileName = "resume.bin"
)

// resumeFixture is a job that a previous run left half-downloaded: its target
// file exists on disk, and written_articles records which of its articles
// were written.
//
// What keeps the assertions attributable is the stall below: a stalled article
// is never written, so nothing in the process can add a Done bit the startup
// verification did not already establish.
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
	row     dispatch.Persisted
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

	cfg := testConfig(downloadDir, completeDir, adminDir, config.ServerConfig{})
	j, hdr := buildTestJob(t, cfg, parsed, types.FetchOptions{NzbName: "resume-job"})
	if err := j.SetFileFilename(0, resumeFileName); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}

	manifestDir := filepath.Join(adminDir, "queue", "manifests")
	if err := os.MkdirAll(manifestDir, 0o750); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := fsutil.WriteGzAtomicBytes(filepath.Join(manifestDir, j.ID()+".json.gz"), data); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	store := dispatchstore.New(repo.DB())
	cp := j.Checkpoint()
	p := dispatch.Persisted{
		ID:      j.ID(),
		SortKey: 1,
		Header:  hdr,
		Policy:  j.Policy(),
		State:   cp.State,
		Intent:  j.Intent(),
	}
	if err := store.Save(t.Context(), p); err != nil {
		t.Fatalf("store.Save: %v", err)
	}
	_, err = repo.DB().ExecContext(t.Context(),
		`INSERT INTO job_files (job_id, file_index, complete, assembled_crc32, fetch_policy, filename)
		VALUES (?, ?, ?, ?, ?, ?)`,
		j.ID(), 0, 0, 0, int(job.FetchAlways), resumeFileName,
	)
	if err != nil {
		t.Fatalf("insert job_files: %v", err)
	}

	f.row = p
	f.jobID = j.ID()
	f.jobName = j.Name()
	f.dir = filepath.Join(downloadDir, j.Name())
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

// recordWritten writes the written_articles rows an earlier run would have
// recorded for the named articles. Written directly because the point of these
// tests is what a LATER process makes of a record an earlier one left.
func (f *resumeFixture) recordWritten(arts ...int) {
	f.t.Helper()
	rows := make([]durability.WrittenRow, 0, len(arts))
	for _, i := range arts {
		rows = append(rows, durability.WrittenRow{
			FileIdx: 0,
			ArtIdx:  int32(i), //nolint:gosec // G115: fixture article counts are tiny
			Offset:  int64(i * resumePartLen),
			Length:  resumePartLen,
			CRC32:   crc32.ChecksumIEEE(f.parts[i]),
		})
	}
	app.SeedWritten(f.t, durability.NewStore(f.repo.DB(), "history.db"), f.jobID, rows)
}

// stall parks the named articles forever, holding their connections open.
//
// It is how these tests keep Done attributable: a stalled article is never
// written, so a Done bit can only have come from the startup verification.
func (f *resumeFixture) stall(arts ...int) {
	f.t.Helper()
	for _, i := range arts {
		f.server.InjectFailure(f.msgIDs[i], nntptest.FailureStall)
	}
}

// start brings the application up over the seeded state.
func (f *resumeFixture) start(conns int) *app.Application {
	f.t.Helper()
	return f.startWith(conns, []postproc.Stage{noOpStage{}}, nil)
}

// startWith is start with the post-processing stages chosen by the caller, and
// beforeStart run on the built application before Start is called.
func (f *resumeFixture) startWith(conns int, stages []postproc.Stage, beforeStart func(*app.Application)) *app.Application {
	f.t.Helper()
	srvCfg := f.server.ServerConfig("resume", conns)
	srvCfg.Timeout = 120 // a stall must outlast the assertions
	a, err := app.New(testConfig(f.downloadDir, f.completeDir, f.adminDir, srvCfg),
		f.repo,
		app.WithPostProcStages(stages),
	)
	if err != nil {
		f.t.Fatalf("app.New: %v", err)
	}
	if beforeStart != nil {
		beforeStart(a)
	}
	ctx, cancel := context.WithCancel(f.t.Context())
	f.t.Cleanup(func() {
		a.StopAndJoin(f.t)
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
	j, ok := a.Dispatcher().Job(f.jobID)
	if !ok {
		f.t.Fatal("job left the dispatcher before it could be inspected")
	}
	// The verification runs when a tick first makes the job resident.
	if !waitUntil(10*time.Second, j.Resident) {
		f.t.Fatal("the job never became resident, so it was never verified")
	}
	if err := a.AwaitHydration(f.t.Context(), f.jobID); err != nil {
		f.t.Fatalf("AwaitHydration: %v", err)
	}
	p := j.Progress()
	if p == nil {
		f.t.Fatal("job has no progress")
	}
	var got [resumeArts]bool
	for i := range resumeArts {
		got[i] = p.ArticleDone(i)
	}
	if got != want {
		f.t.Errorf("article done vector = %v, want %v", got, want)
	}
}

// dumpRows serialises every written_articles row, for a before/after
// comparison.
func (f *resumeFixture) dumpRows() string {
	f.t.Helper()
	rows, err := f.repo.DB().QueryContext(f.t.Context(), `
SELECT job_id, file_idx, art_idx, offset, length, crc32
FROM written_articles ORDER BY job_id, file_idx, offset`)
	if err != nil {
		f.t.Fatalf("query written_articles: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out strings.Builder
	for rows.Next() {
		var jobID string
		var fileIdx, art, offset, length, crc int64
		if err := rows.Scan(&jobID, &fileIdx, &art, &offset, &length, &crc); err != nil {
			f.t.Fatalf("scan written_articles: %v", err)
		}
		fmt.Fprintf(&out, "%s|%d|%d|%d|%d|%d\n", jobID, fileIdx, art, offset, length, crc)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate written_articles: %v", err)
	}
	return out.String()
}

// TestResumeAtStartup_RestoresVerifiedArticles is the base claim: what a
// previous run wrote, and whose bytes still match, comes back Done, and what
// it did not write comes back Outstanding.
func TestResumeAtStartup_RestoresVerifiedArticles(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.writePartial(0, 2)
	f.recordWritten(0, 2)
	f.stall(1) // the one Outstanding article must not be able to become Done

	a := f.start(2)
	f.assertDone(a, [resumeArts]bool{true, false, true})
}

// TestResumeAtStartup_VerifiedArticlesAreNeverRefetched is the ordering pin.
//
// Marking the right bits is not the property; not asking for the bytes is. A
// verification that runs after the job is dispatched passes the test above
// and fails this one, because the request for a written article is already on
// the wire by the time its bit is set.
func TestResumeAtStartup_VerifiedArticlesAreNeverRefetched(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.writePartial(0, 2)
	f.recordWritten(0, 2)
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
			t.Errorf("article %d was fetched %d time(s); its bytes were already written and verified", i, n)
		}
	}
	_ = a
}

// TestResumeAtStartup_ShortFileKeepsOnlyTheArticlesItHolds: the record says
// all three articles were written, but the file was truncated to two
// articles' length and only article 0's bytes were ever there. Each row is
// read back on its own, so article 0 is kept, article 1 (zeros) fails its CRC
// and article 2 (past the end) is a short read.
func TestResumeAtStartup_ShortFileKeepsOnlyTheArticlesItHolds(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.writePartial(0)
	f.recordWritten(0, 1, 2)
	if err := os.Truncate(f.path, resumePartLen*2); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.stall(0, 1, 2)

	a := f.start(1)
	f.assertDone(a, [resumeArts]bool{true, false, false})
}

// TestResumeAtStartup_MissingFileLeavesEverythingOutstanding: with no file to
// verify against, nothing can be proven, and unproven means fetch it again.
//
// The articles are all stalled, so nothing else in the process can set a Done
// bit. If the missing file were ignored and the recorded rows adopted anyway,
// two of them would come back Done.
func TestResumeAtStartup_MissingFileLeavesEverythingOutstanding(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.writePartial(0, 2)
	f.recordWritten(0, 2)
	if err := os.Remove(f.path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	f.stall(0, 1, 2)

	a := f.start(1)
	f.assertDone(a, [resumeArts]bool{false, false, false})
}

// TestResumeAtStartup_StorageFaultStallsAndDoesNotFailArticles: a failure to
// READ the disk is a condition of the device and says nothing about any
// article. It parks the job, fails no article, and leaves the record exactly
// as it was. Both classifications are exercised, because a permanent errno is
// the one a later "route permanent faults to Fail" change would break.
func TestResumeAtStartup_StorageFaultStallsAndDoesNotFailArticles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// fault makes opening the target file fail.
		fault func(t *testing.T, f *resumeFixture)
	}{{
		// ELOOP: not fs.ErrNotExist, so it cannot be confused with the
		// missing-file case, and not a listed permanent errno.
		name: "retryable ELOOP",
		fault: func(t *testing.T, f *resumeFixture) {
			t.Helper()
			f.recordWritten(0, 2)
			if err := os.Symlink(f.path, f.path); err != nil {
				t.Fatalf("symlink loop: %v", err)
			}
		},
	}, {
		// EACCES, which storagefault classifies PERMANENT.
		name: "permanent EACCES",
		fault: func(t *testing.T, f *resumeFixture) {
			t.Helper()
			if os.Geteuid() == 0 {
				t.Skip("running as root: mode bits do not deny access, so no EACCES can be produced")
			}
			f.writePartial(0, 2)
			f.recordWritten(0, 2)
			if err := os.Chmod(f.dir, 0o000); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(f.dir, 0o750) })
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newResumeFixture(t)
			if err := os.MkdirAll(f.dir, 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			tt.fault(t, f)
			before := f.dumpRows()
			if before == "" {
				t.Fatal("no recorded row to compare against; the fixture proves nothing")
			}

			a := f.start(1)

			// The verification runs when the first tick hydrates the job.
			if !waitUntil(10*time.Second, func() bool { return a.StallReason(f.jobID).Reason != "" }) {
				t.Fatal("the read fault never parked the job")
			}
			if stallReason := a.StallReason(f.jobID).Reason; !strings.HasPrefix(stallReason, "Stalled: ") {
				t.Errorf("stall reason = %q, want a surfaced stall reason beginning \"Stalled: \" (R27); "+
					"a \"Failed: \" reason here means the fault was routed as terminal", stallReason)
			}
			row, ok := a.Dispatcher().Row(f.jobID)
			if !ok {
				t.Fatal("the job left the dispatcher on a storage fault; it must stall, not be " +
					"failed into history — the bytes an earlier run left on disk go with it")
			}
			if row.Status() != constants.StatusPaused {
				t.Errorf("status = %q, want %q — a storage fault must park the job rather than "+
					"fail it or let it keep dispatching into a device that cannot be read",
					row.Status(), constants.StatusPaused)
			}
			j, _ := a.Dispatcher().Job(f.jobID)
			if o := j.State().Outcome; o != job.OutcomePending {
				t.Errorf("outcome = %v, want none: a DISK read failure decided the job (A1)", o)
			}
			if after := f.dumpRows(); after != before {
				t.Errorf("a read fault changed the record:\n before %q\n after  %q", before, after)
			}
		})
	}
}

// TestResumeAtStartup_LeavesTheRecordAloneWhenItAdopts: a verification whose
// rows all read back deletes nothing and rewrites nothing.
func TestResumeAtStartup_LeavesTheRecordAloneWhenItAdopts(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.writePartial(0, 2)
	f.recordWritten(0, 2)
	f.stall(1)

	before := f.dumpRows()
	if before == "" {
		t.Fatal("no recorded row to compare against; the fixture proves nothing")
	}

	a := f.start(2)
	f.assertDone(a, [resumeArts]bool{true, false, true})

	if after := f.dumpRows(); after != before {
		t.Errorf("an adopting verification rewrote the record:\n before %q\n after  %q",
			before, after)
	}
}
