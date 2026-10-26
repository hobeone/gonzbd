package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
)

// The loose article record, end to end: what one process writes and flushes,
// and what the next process verifies before it attaches a job's content.
//
// Every job here has two files. File A carries the record under test; file B
// is never delivered, so the job stays at Fetching (or, restored at Assessing,
// is demoted back to it) and no test races its own job into post-processing.

const lrArtLen = 512

// lrArticle is article art of file file: distinct bytes per article, so a CRC
// tells any two apart.
func lrArticle(file, art int) []byte {
	b := make([]byte, lrArtLen)
	for i := range b {
		b[i] = byte(file*97 + art*31 + i*7 + 1)
	}
	return b
}

// lrNZB renders a two-file NZB: A.bin with nA articles and B.bin with nB.
func lrNZB(nA, nB int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="iso-8859-1" ?>` + "\n")
	b.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	for fi, f := range []struct {
		name string
		n    int
	}{{"A.bin", nA}, {"B.bin", nB}} {
		fmt.Fprintf(&b, `<file poster="p@t" date="1700000000" subject="&quot;%s&quot; yEnc (1/%d)">`+"\n", f.name, f.n)
		b.WriteString("<groups><group>alt.bin.test</group></groups>\n<segments>\n")
		for i := 1; i <= f.n; i++ {
			fmt.Fprintf(&b, `<segment bytes="%d" number="%d">f%da%d@t</segment>`+"\n", lrArtLen, i, fi, i)
		}
		b.WriteString("</segments>\n</file>\n")
	}
	b.WriteString("</nzb>\n")
	return []byte(b.String())
}

// lrStage succeeds without doing anything, so a job reaching post-processing
// is filed without touching its files.
type lrStage struct{}

func (lrStage) Name() string                                 { return "lr-noop" }
func (lrStage) Run(_ context.Context, _ *postproc.Job) error { return nil }

// lrEnv is one installation's directories, shared by every Application a test
// builds over them: a second Application on the same env is a restart.
type lrEnv struct {
	dl, comp, admin string
	// stage is the post-processing stage every Application over the env
	// runs; nil is lrStage.
	stage postproc.Stage
}

func newLREnv(t *testing.T) *lrEnv {
	t.Helper()
	return &lrEnv{dl: t.TempDir(), comp: t.TempDir(), admin: t.TempDir()}
}

// lrApp is one process over an lrEnv.
type lrApp struct {
	*Application
	fd   *fakeDownloader
	repo *history.Repository
	st   *durability.Store
}

// newApp builds, but does not start, an Application over e. configure runs
// after New and before anything is started, which is the discipline the
// same-package seams on Application require.
func (e *lrEnv) newApp(t *testing.T, configure ...func(*Application)) *lrApp {
	t.Helper()
	return e.newAppWith(t, nil, configure...)
}

// newAppSyncing is newApp with the assembler's fsync replaced by sync. The
// seam is read when New builds the assembler, so it is set as an option to New
// rather than by configure.
func (e *lrEnv) newAppSyncing(t *testing.T, sync func(*os.File) error) *lrApp {
	t.Helper()
	return e.newAppWith(t, []func(*Application){func(a *Application) { a.syncFile = sync }})
}

// newAppWith is newApp with extra options passed to New.
func (e *lrEnv) newAppWith(t *testing.T, opts []func(*Application), configure ...func(*Application)) *lrApp {
	t.Helper()
	cfg := testConfig(e.dl, e.comp, e.admin)
	db, err := history.Open(t.Context(), filepath.Join(e.admin, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	fd := newFakeDownloader()
	stage := e.stage
	if stage == nil {
		stage = lrStage{}
	}
	a, err := New(cfg, repo, append([]func(*Application){WithDownloader(fd), WithPostProcStages([]postproc.Stage{stage})}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	restoreQueue(t, a)
	// Explicit flushes only, so a test decides what reached SQLite.
	a.recordInterval = time.Hour
	for _, c := range configure {
		c(a)
	}
	return &lrApp{Application: a, fd: fd, repo: repo, st: durability.NewStore(repo.DB())}
}

// start starts a and registers a hard stop for the end of the test.
func (a *lrApp) start(t *testing.T) {
	t.Helper()
	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { a.StopAndJoin(t) })
}

// addJob queues a two-file job with a backup of its NZB, so a retry can
// rebuild it.
func (a *lrApp) addJob(t *testing.T, name string, nA, nB int) *job.Job {
	t.Helper()
	raw := lrNZB(nA, nB)
	parsed, err := nzb.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	j, hdr, err := BuildIngestJob(a.config, parsed, name+".nzb", types.FetchOptions{NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := a.AddJob(t.Context(), j, hdr, raw, true); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	return j
}

// deliver plays the downloader: article art of file A arrives decoded, through
// the real pipeline and assembler.
func (a *lrApp) deliver(t *testing.T, j *job.Job, art int) {
	t.Helper()
	data := lrArticle(0, art)
	a.fd.completions <- &downloader.ArticleResult{
		Job:       j,
		MessageID: fmt.Sprintf("f0a%d@t", art+1),
		FileIdx:   0,
		ArtIdx:    int32(art), //nolint:gosec // G115: a handful of articles
		Subject:   "A.bin",
		Data:      data,
		Offset:    int64(art) * lrArtLen,
		CRC:       crc32.ChecksumIEEE(data),
	}
}

// lrWaitBudget is how long a wait lasts at most in an ordinary run. A wait is
// a hang guard, not a correctness bound, so the budget is generous enough
// that load on the machine does not exhaust it.
const lrWaitBudget = 60 * time.Second

// lrWaitBudgetEnv, when set, replaces lrWaitBudget. scripts/mutate sets it to
// a few seconds on every go test it runs, baseline included, so a mutation
// that removes the awaited event is killed quickly.
const lrWaitBudgetEnv = "GONZBD_TEST_WAIT"

// lrWaitDeadline is when a wait gives up: the budget from now, or earlier
// when the test binary's -timeout is nearer. In that case it stops a fifth of
// the remaining time, at most 30 seconds, short of the binary's deadline, so
// the test reports which wait failed and runs its cleanups before the
// binary's own timeout panics.
func lrWaitDeadline(t *testing.T) time.Time {
	t.Helper()
	budget := lrWaitBudget
	if v, ok := os.LookupEnv(lrWaitBudgetEnv); ok {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			t.Fatalf("%s=%q is not a positive duration", lrWaitBudgetEnv, v)
		}
		budget = d
	}
	deadline := time.Now().Add(budget)
	if d, ok := t.Deadline(); ok {
		if clamp := d.Add(-min(time.Until(d)/5, 30*time.Second)); clamp.Before(deadline) {
			deadline = clamp
		}
	}
	return deadline
}

// lrWaitFor polls cond until it holds or lrWaitDeadline passes.
func lrWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := lrWaitDeadline(t)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lrAwait receives one value from ch, or fails the test at lrWaitDeadline.
func lrAwait[T any](t *testing.T, what string, ch <-chan T) T { //nolint:ireturn // T is the channel's element type, concrete at each call site, not an interface
	t.Helper()
	timer := time.NewTimer(time.Until(lrWaitDeadline(t)))
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func articleDone(j *job.Job, art int) bool {
	p := j.Progress()
	return p != nil && p.ArticleDone(art)
}

// registered returns the instance a's dispatcher holds under id.
func (a *lrApp) registered(t *testing.T, id string) *job.Job {
	t.Helper()
	j, ok := a.dispatcher.Job(id)
	if !ok {
		t.Fatalf("job %s is not registered", id)
	}
	return j
}

// waitRestored waits until the tick's hydration of j has finished. Resident
// alone is not enough: verifyAndAttach attaches the content before it installs
// what it verified, and a Hydrate call made meanwhile waits for the one in
// flight.
func (a *lrApp) waitRestored(t *testing.T, j *job.Job) {
	t.Helper()
	lrWaitFor(t, "the restored job's content", j.Resident)
	if err := a.residency.Hydrate(t.Context(), j.ID()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
}

// filePath is where file A of j lives.
func (e *lrEnv) filePath(j *job.Job) string {
	return filepath.Join(e.dl, j.Name(), "A.bin")
}

// writeFileA puts articles arts of file A on disk at their offsets, over a
// file pre-allocated to the whole file's size, as the assembler leaves it.
func (e *lrEnv) writeFileA(t *testing.T, j *job.Job, nA int, arts ...int) {
	t.Helper()
	path := e.filePath(j)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	buf := make([]byte, nA*lrArtLen)
	for _, a := range arts {
		copy(buf[a*lrArtLen:], lrArticle(0, a))
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// zeroArticle overwrites article art of file A with zeros.
func (e *lrEnv) zeroArticle(t *testing.T, j *job.Job, art int) {
	t.Helper()
	f, err := os.OpenFile(e.filePath(j), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt(make([]byte, lrArtLen), int64(art)*lrArtLen); err != nil {
		t.Fatalf("zero: %v", err)
	}
}

// rowsFor returns the written rows of file A of the job, as SQLite holds them.
func rowsFor(t *testing.T, db *sql.DB, jobID string) map[int32]bool {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT art_idx FROM written_articles WHERE job_id = ? AND file_idx = 0`, jobID)
	if err != nil {
		t.Fatalf("query written_articles: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int32]bool{}
	for rows.Next() {
		var a int32
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[a] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// completeFlag reads job_files.complete for file A.
func completeFlag(t *testing.T, db *sql.DB, jobID string) bool {
	t.Helper()
	var c int
	if err := db.QueryRowContext(t.Context(),
		`SELECT complete FROM job_files WHERE job_id = ? AND file_index = 0`, jobID).Scan(&c); err != nil {
		t.Fatalf("query job_files: %v", err)
	}
	return c != 0
}

// recordFileA records file A's state and rows for arts directly in SQLite,
// the way a flush would have left them.
func (a *lrApp) recordFileA(t *testing.T, jobID string, complete bool, arts ...int) {
	t.Helper()
	b := durability.RecordBatch{
		JobID: jobID,
		Files: []durability.FileState{{FileIdx: 0, Filename: "A.bin", Complete: complete}},
	}
	for _, art := range arts {
		b.Rows = append(b.Rows, durability.WrittenRow{
			FileIdx: 0, ArtIdx: int32(art), //nolint:gosec // G115: a handful of articles
			Offset: int64(art) * lrArtLen, Length: lrArtLen,
			CRC32: crc32.ChecksumIEEE(lrArticle(0, art)),
		})
	}
	if err := a.st.ApplyRecord(t.Context(), []durability.RecordBatch{b}); err != nil {
		t.Fatalf("ApplyRecord: %v", err)
	}
}

// failingFsync is a syncFile seam that fails while armed, for file A only.
type failingFsync struct{ armed atomic.Bool }

func (f *failingFsync) sync(fh *os.File) error {
	if f.armed.Load() && filepath.Base(fh.Name()) == "A.bin" {
		return &os.PathError{Op: "fsync", Path: fh.Name(), Err: syscall.EIO}
	}
	return fh.Sync()
}

// TestLooseRecord_RestartResumesInsideAFile pins the point of the record: an
// article one process wrote and flushed is Done in the next process, after a
// read of its bytes, and the articles it never wrote are Outstanding.
func TestLooseRecord_RestartResumesInsideAFile(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	a1.start(t)
	j := a1.addJob(t, "resume", 6, 2)
	for art := range 3 {
		a1.deliver(t, j, art)
	}
	lrWaitFor(t, "three articles written", func() bool {
		return articleDone(j, 0) && articleDone(j, 1) && articleDone(j, 2)
	})
	if err := a1.recorder.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	a1.StopAndJoin(t)

	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	a2.waitRestored(t, j2)
	p := j2.Progress()
	for art := range 6 {
		if got, want := p.ArticleDone(art), art < 3; got != want {
			t.Errorf("article %d Done = %v after the restart, want %v", art, got, want)
		}
	}
}

// TestLooseRecord_ZeroedRangeIsRefetched pins the read: a row whose bytes are
// no longer on disk costs its own article, and its row is deleted.
func TestLooseRecord_ZeroedRangeIsRefetched(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	a1.start(t)
	j := a1.addJob(t, "zeroed", 6, 2)
	for art := range 3 {
		a1.deliver(t, j, art)
	}
	lrWaitFor(t, "three articles written", func() bool {
		return articleDone(j, 0) && articleDone(j, 1) && articleDone(j, 2)
	})
	if err := a1.recorder.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	a1.StopAndJoin(t)
	env.zeroArticle(t, j, 1)

	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	a2.waitRestored(t, j2)
	p := j2.Progress()
	if p.ArticleDone(1) {
		t.Error("the zeroed article is Done; its row was trusted without a read")
	}
	if !p.ArticleDone(0) || !p.ArticleDone(2) {
		t.Error("an intact article was not resumed")
	}
	if rowsFor(t, a2.repo.DB(), j.ID())[1] {
		t.Error("the zeroed article's row survived verification")
	}
}

// TestLooseRecord_UntrustedSurvivesEviction pins Review Focus 2: a file whose
// completion fsync failed is untrusted in SQLite, not only in memory, before
// the job can be evicted, so a re-hydration cannot bring its articles back.
func TestLooseRecord_UntrustedSurvivesEviction(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	fs := &failingFsync{}
	a := env.newAppSyncing(t, fs.sync)
	a.start(t)
	j := a.addJob(t, "untrust", 3, 2)
	a.deliver(t, j, 0)
	a.deliver(t, j, 1)
	lrWaitFor(t, "two articles written", func() bool { return articleDone(j, 0) && articleDone(j, 1) })
	if err := a.recorder.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := rowsFor(t, a.repo.DB(), j.ID()); len(got) != 2 {
		t.Fatalf("fixture: %d rows in SQLite before the fault, want 2", len(got))
	}

	fs.armed.Store(true)
	a.deliver(t, j, 2)
	lrWaitFor(t, "the completion fault to stall the job", func() bool {
		return a.StallReason(j.ID()).Reason != ""
	})

	a.residency.Evict(j.ID())
	if err := a.residency.Hydrate(t.Context(), j.ID()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if err := a.recorder.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	p := j.Progress()
	for art := range 3 {
		if p.ArticleDone(art) {
			t.Errorf("article %d is Done after an fsync fault and a re-hydration", art)
		}
	}
	if got := rowsFor(t, a.repo.DB(), j.ID()); len(got) != 0 {
		t.Errorf("SQLite still holds %d rows of the untrusted file", len(got))
	}
	if completeFlag(t, a.repo.DB(), j.ID()) {
		t.Error("job_files.complete is 1 for a file whose fsync failed")
	}
}

// TestLooseRecord_CompletionFaultConvergesInProcess pins Review Focus 7: after
// a completion fault the refetched articles open a fresh writer, so the file
// completes without a restart.
func TestLooseRecord_CompletionFaultConvergesInProcess(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	fs := &failingFsync{}
	a := env.newAppSyncing(t, fs.sync)
	a.start(t)
	j := a.addJob(t, "converge", 3, 2)

	fs.armed.Store(true)
	for art := range 3 {
		a.deliver(t, j, art)
	}
	lrWaitFor(t, "the completion fault to stall the job", func() bool {
		return a.StallReason(j.ID()).Reason != ""
	})
	fs.armed.Store(false)
	a.ReevaluateStalls()
	lrWaitFor(t, "the stall to clear", func() bool { return a.StallReason(j.ID()).Reason == "" })

	for art := range 3 {
		a.deliver(t, j, art)
	}
	lrWaitFor(t, "the file to complete", func() bool {
		p := j.Progress()
		return p != nil && p.FileComplete(0)
	})
	if err := a.recorder.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !completeFlag(t, a.repo.DB(), j.ID()) {
		t.Error("job_files.complete is 0 for a file that converged")
	}
	if got := rowsFor(t, a.repo.DB(), j.ID()); len(got) != 3 {
		t.Errorf("%d rows for the converged file, want 3", len(got))
	}
	want := crc32.ChecksumIEEE(append(append(lrArticle(0, 0), lrArticle(0, 1)...), lrArticle(0, 2)...))
	if got := j.Progress().FileAssembledCRC32(0); got != want {
		t.Errorf("whole-file CRC = %08x, want %08x", got, want)
	}
}

// TestLooseRecord_PausedJobReportsEstimateUntilResumed pins the hand-off: a
// Fetching job restored paused reports the recorded-count estimate, and the
// user's resume is what verifies it.
func TestLooseRecord_PausedJobReportsEstimateUntilResumed(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	j := pauseAtFetching(t, env, env.newApp(t), "paused")
	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	if j2.AllVerified() || j2.Resident() {
		t.Fatal("the restored paused job was hydrated before its resume")
	}
	row, ok := a2.dispatcher.Row(j.ID())
	if !ok {
		t.Fatal("the job is not registered")
	}
	if want := row.Header.Bytes - 2*lrArtLen; row.RemainingBytes != want {
		t.Errorf("remaining = %d of %d before the resume, want %d estimated from the 2 recorded articles", row.RemainingBytes, row.Header.Bytes, want)
	}
	if err := a2.dispatcher.ResumeJobByUser(j.ID()); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	lrWaitFor(t, "the resumed job to hold its lease on the verified content", func() bool {
		r, ok := a2.dispatcher.Row(j.ID())
		return ok && r.View.Holds && j2.Resident() && articleDone(j2, 1)
	})
	if !articleDone(j2, 0) {
		t.Error("a verified article is not Done after the resume")
	}
	if !j2.AllVerified() {
		t.Error("the resumed job is not verified after its hydration")
	}
	expected, remaining, _ := j2.ProgressFigures()
	if remaining >= expected {
		t.Errorf("remaining %d of %d after the resume", remaining, expected)
	}
}

// pauseAtFetching adds a job to a with two of file A's four articles written
// and recorded, takes it to Fetching and pauses it there, so that a restart
// over the same env restores it paused at Fetching with rows to verify.
func pauseAtFetching(t *testing.T, env *lrEnv, a *lrApp, name string) *job.Job {
	t.Helper()
	j := a.addJob(t, name, 4, 2)
	env.writeFileA(t, j, 4, 0, 1)
	a.recordFileA(t, j.ID(), false, 0, 1)
	a.dispatcher.Tick(t.Context())
	a.dispatcher.Tick(t.Context())
	if st := j.State().State; st != job.Fetching {
		t.Fatalf("fixture: %s is at %v, want Fetching", name, st)
	}
	if err := a.dispatcher.PauseJob(j.ID()); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	a.dispatcher.Tick(t.Context())
	return j
}

// TestLooseRecord_CrashAfterLeavingFetchingStillReads pins §4's "no outside
// Fetching shortcut": a job restored at Assessing with complete=0 rows is read
// like any other, so a lost range is Outstanding.
func TestLooseRecord_CrashAfterLeavingFetchingStillReads(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	j := a1.addJob(t, "assessing", 3, 2)
	env.writeFileA(t, j, 3, 0, 1, 2)
	a1.recordFileA(t, j.ID(), false, 0, 1, 2)
	if _, err := a1.repo.DB().ExecContext(t.Context(),
		`UPDATE dispatch_jobs SET state = ? WHERE id = ?`, int(job.Assessing), j.ID()); err != nil {
		t.Fatalf("restore at Assessing: %v", err)
	}
	env.zeroArticle(t, j, 2)

	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	lrWaitFor(t, "the restored job's content", func() bool { return articleDone(j2, 0) })
	if articleDone(j2, 2) {
		t.Error("a zeroed article is Done for a job restored outside Fetching")
	}
	if !articleDone(j2, 1) {
		t.Error("an intact article was not resumed")
	}
}

// registeredHeader returns the header a's dispatcher holds for id.
func (a *lrApp) registeredHeader(t *testing.T, id string) dispatch.Header {
	t.Helper()
	row, ok := a.dispatcher.Row(id)
	if !ok {
		t.Fatalf("job %s is not registered", id)
	}
	return row.Header
}

// writeGzNZBInternal replaces the NZB backup name under adminDir with raw.
func writeGzNZBInternal(t *testing.T, adminDir, name string, raw []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(adminDir, "nzb", name), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write NZB backup: %v", err)
	}
}

// lrRetryFixture files a FAILED history entry for a job whose file A has
// bytes and rows, as a failed attempt leaves it: the entry and its kept
// job_files and written_articles rows, the NZB backup, and the bytes.
func lrRetryFixture(t *testing.T, env *lrEnv, nzbArts, nA int, complete bool, arts ...int) (*lrApp, string) {
	t.Helper()
	a1 := env.newApp(t)
	j := a1.addJob(t, "retry", nA, 2)
	env.writeFileA(t, j, nA, arts...)
	a1.recordFileA(t, j.ID(), complete, arts...)
	backup := a1.registeredHeader(t, j.ID()).NZBBackup
	if nzbArts != nA {
		writeGzNZBInternal(t, env.admin, backup, lrNZB(nzbArts, 2))
	}
	if _, err := a1.repo.DB().ExecContext(t.Context(), `DELETE FROM dispatch_jobs WHERE id = ?`, j.ID()); err != nil {
		t.Fatalf("drop the queue row: %v", err)
	}
	if err := a1.repo.Add(t.Context(), history.Entry{
		NzoID: j.ID(), Name: j.Name(), NzbName: "retry.nzb", NZBBackup: backup,
		Status: string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("history Add: %v", err)
	}
	a2 := env.newApp(t)
	a2.dispatcher.Pause()
	return a2, j.ID()
}

// TestLooseRecord_RetryVerifiesWrittenArticles pins the retry path: a failed
// job's written articles are verified and Done in the rebuilt job, without a
// fetch.
func TestLooseRecord_RetryVerifiesWrittenArticles(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a, id := lrRetryFixture(t, env, 4, 4, false, 0, 1)
	if err := a.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}
	j := a.registered(t, id)
	for art := range 4 {
		if got, want := articleDone(j, art), art < 2; got != want {
			t.Errorf("article %d Done = %v after the retry, want %v", art, got, want)
		}
	}
}

// TestLooseRecord_RetryAfterPostProcessingReadsEveryFile pins §3.7's retry
// row: complete=1 is cleared on every file before verification, because
// post-processing may have changed the bytes since.
func TestLooseRecord_RetryAfterPostProcessingReadsEveryFile(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a, id := lrRetryFixture(t, env, 3, 3, true, 0, 1, 2)
	path := filepath.Join(env.dl, "retry", "A.bin")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xFF}, lrArtLen+7); err != nil {
		t.Fatalf("repair-like write: %v", err)
	}
	_ = f.Close()

	if err := a.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}
	j := a.registered(t, id)
	if articleDone(j, 1) {
		t.Error("the article post-processing changed is trusted after a retry")
	}
	if !articleDone(j, 0) || !articleDone(j, 2) {
		t.Error("an unchanged article was not resumed by the retry")
	}
}

// TestLooseRecord_RetryShapeMismatchDeletesEveryRow pins the shape check: rows
// that do not fit the re-parsed manifest are all deleted, not read.
func TestLooseRecord_RetryShapeMismatchDeletesEveryRow(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a, id := lrRetryFixture(t, env, 2, 3, false, 0, 1, 2)
	if err := a.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}
	rows, err := a.st.WrittenRows(t.Context(), id)
	if err != nil {
		t.Fatalf("WrittenRows: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d rows survived a retry whose NZB no longer matches them", len(rows))
	}
	j := a.registered(t, id)
	for art := range 2 {
		if articleDone(j, art) {
			t.Errorf("article %d is Done from a row of a different manifest", art)
		}
	}
}

// TestLooseRecord_VerificationFaultParksNeverFails pins Review Focus 6: an
// unreadable sector parks the job with a reason naming the file, leaves its
// outcome unset, and a resume after the fault clears verifies and attaches it.
//
// Not parallel: preadAt is a package-level seam.
func TestLooseRecord_VerificationFaultParksNeverFails(t *testing.T) {
	env := newLREnv(t)
	a1 := env.newApp(t)
	j := a1.addJob(t, "eio", 3, 2)
	env.writeFileA(t, j, 3, 0, 1)
	a1.recordFileA(t, j.ID(), false, 0, 1)

	var failing atomic.Bool
	failing.Store(true)
	prev := preadAt
	preadAt = func(f *os.File, b []byte, off int64) (int, error) {
		if failing.Load() {
			return 0, &os.PathError{Op: "pread", Path: f.Name(), Err: syscall.EIO}
		}
		return prev(f, b, off)
	}
	a2 := env.newApp(t)
	t.Cleanup(func() { preadAt = prev })
	a2.start(t)

	path := env.filePath(j)
	j2 := a2.registered(t, j.ID())
	// stall records the reason before PauseInstance pauses the job, and the
	// resume below must not race ahead of that pause: wait for both.
	lrWaitFor(t, "the verification fault to park the job", func() bool {
		return strings.Contains(a2.StallReason(j.ID()).Reason, path) && j2.Intent() == job.IntentPause
	})
	if o := j2.State().Outcome; o != job.OutcomePending {
		t.Fatalf("outcome = %v after a verification fault, want none", o)
	}
	if j2.Resident() {
		t.Fatal("the job is resident after a verification that faulted")
	}

	failing.Store(false)
	// The user's per-job resume, as the API makes it: a re-evaluation alone
	// does not lift a verification fault on a job nobody resumed.
	if err := a2.dispatcher.ResumeJobByUser(j.ID()); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	a2.ReevaluateStalls()
	lrWaitFor(t, "a resume to verify and attach the job", func() bool { return articleDone(j2, 1) })
	if !articleDone(j2, 0) {
		t.Error("an intact article was not resumed after the fault cleared")
	}
}

// TestLooseRecord_CloseTimeFsyncFaultUntrusts pins §3.3 step 3: a file whose
// fsync fails at clean shutdown has no rows afterwards, so the next start
// cannot trust bytes the kernel reported lost.
func TestLooseRecord_CloseTimeFsyncFaultUntrusts(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	fs := &failingFsync{}
	a1 := env.newAppSyncing(t, fs.sync)
	a1.start(t)
	j := a1.addJob(t, "closefault", 4, 2)
	a1.deliver(t, j, 0)
	a1.deliver(t, j, 1)
	lrWaitFor(t, "two articles written", func() bool { return articleDone(j, 0) && articleDone(j, 1) })
	if err := a1.recorder.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	fs.armed.Store(true)
	if err := a1.Shutdown(); err != nil {
		t.Logf("Shutdown: %v", err)
	}
	if got := rowsFor(t, a1.repo.DB(), j.ID()); len(got) != 0 {
		t.Errorf("%d rows survived a close-time fsync fault", len(got))
	}

	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	a2.waitRestored(t, j2)
	if articleDone(j2, 0) || articleDone(j2, 1) {
		t.Error("an article of a file whose close-time fsync failed is Done after the restart")
	}
}

// TestLooseRecord_FailedCloseSyncReturnsArticlesToOutstanding pins the
// in-process half: a close-handles fsync that fails untrusts the file, so the
// written articles are Outstanding again in this process — offered for
// dispatch, neither Done nor Emitted — and their buffered rows never reach
// SQLite.
func TestLooseRecord_FailedCloseSyncReturnsArticlesToOutstanding(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	fs := &failingFsync{}
	a := env.newAppSyncing(t, fs.sync)
	a.start(t)
	j := a.addJob(t, "closesync", 4, 2)
	for art := range 2 {
		// As dispatch does, so the write's Done is what clears Emitted.
		if err := j.MarkArticleEmitted(art); err != nil {
			t.Fatalf("fixture: MarkArticleEmitted(%d): %v", art, err)
		}
		a.deliver(t, j, art)
	}
	lrWaitFor(t, "two articles written", func() bool { return articleDone(j, 0) && articleDone(j, 1) })

	fs.armed.Store(true)
	if err := a.assembler.CloseJobHandles(t.Context(), j.ID()); err == nil {
		t.Fatal("fixture: CloseJobHandles reported no fault for a failed fsync")
	}
	if articleDone(j, 0) || articleDone(j, 1) {
		t.Error("an article whose fsync failed is still Done in this process")
	}
	var offered []int32
	if err := j.ForEachUnfinishedArticle(func(fi int, art int32, _ string, _, _ int, _ string) bool {
		if fi == 0 {
			offered = append(offered, art)
		}
		return true
	}); err != nil {
		t.Fatalf("ForEachUnfinishedArticle: %v", err)
	}
	if !slices.Contains(offered, 0) || !slices.Contains(offered, 1) {
		t.Errorf("file A's unfinished articles = %v, want 0 and 1 offered for refetch", offered)
	}
	if err := a.recorder.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := rowsFor(t, a.repo.DB(), j.ID()); len(got) != 0 {
		t.Errorf("%d buffered rows of an untrusted file reached SQLite", len(got))
	}
}

// TestLooseRecord_ResumedCompletionSurvivesEviction pins that a file the
// verifier finished by path stays complete when the job is evicted before its
// Resumed completion is consumed: the hydration marks it, and the consumer
// needs no manifest for what is left.
func TestLooseRecord_ResumedCompletionSurvivesEviction(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	j := a1.addJob(t, "evicted", 2, 2)
	env.writeFileA(t, j, 2, 0, 1)
	a1.recordFileA(t, j.ID(), false, 0, 1)

	// Not started, so no tick reconciles the job behind the test: LoadProgress
	// marks it resident in the dispatcher, a tick then evicts it as unpaused
	// and holding nothing (reconcileResidency), and that eviction would cancel
	// the re-hydration below if it landed inside it.
	var owed []int
	a2 := env.newApp(t, func(a *Application) {
		a.residency.finished = func(_ *job.Job, fi int, _ string) { owed = append(owed, fi) }
	})
	id := j.ID()
	if err := a2.restoreProgress(t.Context()); err != nil {
		t.Fatalf("restoreProgress: %v", err)
	}
	if err := a2.dispatcher.LoadProgress(t.Context(), id); err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if !slices.Equal(owed, []int{0}) {
		t.Fatalf("fixture: the verifier finished %v, want file A", owed)
	}
	a2.residency.Evict(id)
	j2 := a2.registered(t, id)
	if j2.Resident() {
		t.Fatal("fixture: the job is still resident after Evict")
	}

	// The consumer's own door, called directly: its error is what a completion
	// that needed the manifest would return for an evicted job.
	if err := a2.completeFinalizedFile(FileComplete{Job: j2, FileIdx: 0, Resumed: true}); err != nil {
		t.Fatalf("a resumed completion of an evicted job was not delivered: %v", err)
	}
	a2.recorder.mu.Lock()
	_, dirtied := a2.recorder.dirty[j2][0]
	a2.recorder.mu.Unlock()
	if dirtied {
		t.Error("a resumed completion dirtied file A; the SetComplete verdict is the one writer of its complete flag")
	}
	if err := a2.residency.Hydrate(t.Context(), id); err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}
	if !j2.Progress().FileComplete(0) {
		t.Error("file A is not complete after an evicted job's resumed completion was consumed")
	}
}

// TestLooseRecord_CompleteFileInstallsItsFailedSet pins open design item 1: a
// complete=1 file's failed set is every article of its range without a row,
// installed with the rows, so hasFailedArticle answers before anything that
// consults it runs.
func TestLooseRecord_CompleteFileInstallsItsFailedSet(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	j := a1.addJob(t, "failedset", 3, 2)
	env.writeFileA(t, j, 3, 0, 2)
	a1.recordFileA(t, j.ID(), true, 0, 2)

	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	a2.waitRestored(t, j2)
	m, err := j2.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	p := j2.Progress()
	if !p.FileComplete(0) {
		t.Fatal("fixture: file A is not Complete after the restart")
	}
	if !hasFailedArticle(m, p, 0) || !p.ArticleFailed(1) {
		t.Error("the complete file's article with no row is not failed")
	}
	if !p.ArticleDone(0) || !p.ArticleDone(2) || p.ArticleFailed(0) || p.ArticleFailed(2) {
		t.Error("the complete file's rows are not installed as Done")
	}
	// The byte half: the one article without a row is the whole failed total.
	if got := p.FailedBytes(); got != lrArtLen {
		t.Errorf("FailedBytes = %d, want %d (the one article with no row)", got, lrArtLen)
	}
}

// TestLooseRecord_CorruptRowCostsOnlyItself pins open design item 3: a row of
// a complete=1 file that does not belong to it is dropped, and the file's
// other rows are installed.
func TestLooseRecord_CorruptRowCostsOnlyItself(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	j := a1.addJob(t, "corrupt", 3, 2)
	env.writeFileA(t, j, 3, 0, 1, 2)
	a1.recordFileA(t, j.ID(), true, 0, 1, 2)
	// Article 3 belongs to file B, so as a row of file A it is corrupt.
	if err := a1.st.ApplyRecord(t.Context(), []durability.RecordBatch{{JobID: j.ID(), Rows: []durability.WrittenRow{
		{FileIdx: 0, ArtIdx: 3, Offset: 3 * lrArtLen, Length: lrArtLen, CRC32: 1},
	}}}); err != nil {
		t.Fatalf("ApplyRecord: %v", err)
	}

	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	a2.waitRestored(t, j2)
	p := j2.Progress()
	for art := range 3 {
		if !p.ArticleDone(art) || p.ArticleFailed(art) {
			t.Errorf("article %d of the complete file is not Done: one corrupt row cost it", art)
		}
	}
	if p.ArticleDone(3) {
		t.Error("the corrupt row's article is Done")
	}
	// Design item 2: the file's CRC is settled at install from the rows it
	// kept, and those rows are then released.
	whole := make([]byte, 0, 3*lrArtLen)
	for art := range 3 {
		whole = append(whole, lrArticle(0, art)...)
	}
	if got, want := p.FileAssembledCRC32(0), crc32.ChecksumIEEE(whole); got != want {
		t.Errorf("complete file's CRC = %08x after the restart, want %08x settled from its rows", got, want)
	}
	if rows := j2.FileRows(0); rows != nil {
		t.Errorf("complete file still holds %d resident rows after its CRC settled", len(rows))
	}
}

// TestLooseRecord_ResumedCompletionIsNotFedToDirectUnpack pins FileComplete's
// Resumed: a completion the verifier produced skips the DirectUnpack feed.
func TestLooseRecord_ResumedCompletionIsNotFedToDirectUnpack(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a := env.newApp(t)
	a.config.With(func(c *config.Config) {
		c.PostProc.DirectUnpack = true
		c.PostProc.EnableUnrar = true
	})
	raw := strings.ReplaceAll(string(lrNZB(1, 1)), "A.bin", "set.part01.rar")
	parsed, err := nzb.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	j, hdr, err := BuildIngestJob(a.config, parsed, "du.nzb", types.FetchOptions{NzbName: "du", PP: 3}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := a.AddJob(t.Context(), j, hdr, []byte(raw), true); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	a.InjectPipelineFileInfo(j.ID(), 0, filepath.Join(env.dl, j.Name(), "set.part01.rar"))

	a.handleFileComplete(t.Context(), FileComplete{Job: j, FileIdx: 0, Resumed: true})
	if _, ok := a.DirectUnpackStatus(j.ID()); ok {
		t.Error("a resumed completion started a DirectUnpacker")
	}
	t.Cleanup(a.duOrch.abortAll)
}

// TestLooseRecord_RestoresTheStoredFetchPolicy pins hydration's policy
// restore: the job_files fetch policy is the current verdict, so a restarted
// job reads it rather than the policy its construction derived.
func TestLooseRecord_RestoresTheStoredFetchPolicy(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	a1.start(t)
	j := a1.addJob(t, "policy", 4, 2)
	a1.StopAndJoin(t)
	if got := j.FileFetchPolicy(1); got != job.FetchAlways {
		t.Fatalf("file B was built with policy %v, want FetchAlways for the restore to move", got)
	}
	if err := a1.st.ApplyRecord(t.Context(), []durability.RecordBatch{{
		JobID: j.ID(),
		Files: []durability.FileState{{FileIdx: 1, Filename: "B.bin", FetchPolicy: uint8(job.FetchNever)}},
	}}); err != nil {
		t.Fatalf("ApplyRecord: %v", err)
	}

	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	a2.waitRestored(t, j2)
	if got := j2.FileFetchPolicy(1); got != job.FetchNever {
		t.Errorf("file B's policy after the restart = %v, want the stored FetchNever", got)
	}
}

// lrFuncStage is a post-processing stage that runs fn.
type lrFuncStage struct{ fn func(*postproc.Job) error }

func (lrFuncStage) Name() string                                   { return "lr-func" }
func (s lrFuncStage) Run(_ context.Context, j *postproc.Job) error { return s.fn(j) }

// deliverAll plays the downloader for every article of both files of a job
// built by addJob(…, nA, nB).
func (a *lrApp) deliverAll(t *testing.T, j *job.Job, nA, nB int) {
	t.Helper()
	for fi, n := range []int{nA, nB} {
		for art := range n {
			data := lrArticle(fi, art)
			a.fd.completions <- &downloader.ArticleResult{
				Job:       j,
				MessageID: fmt.Sprintf("f%da%d@t", fi, art+1),
				FileIdx:   fi,
				ArtIdx:    int32(fi*nA + art), //nolint:gosec // G115: a handful of articles
				Subject:   []string{"A.bin", "B.bin"}[fi],
				Data:      data,
				Offset:    int64(art) * lrArtLen,
				CRC:       crc32.ChecksumIEEE(data),
			}
		}
	}
}

// lrWaitFinalized waits for the finalizer to report the job's
// post-processing complete and returns its history entry's status.
// finalize sends PostProcComplete after persistAndCommit has filed the entry,
// flushed the recorder and called RemoveJob; it is the one sender
// (`git grep -n 'postProcComplete <-' -- '*.go' ':!*_test.go'` returns 1 line).
func (a *lrApp) lrWaitFinalized(t *testing.T, id string) string {
	t.Helper()
	for {
		done := lrAwait(t, "the job's post-processing to complete", a.PostProcComplete())
		if done.JobID == id {
			break
		}
	}
	e, err := a.repo.Get(t.Context(), id)
	if err != nil || e == nil {
		t.Fatalf("no history entry for a job whose post-processing completed: %v", err)
	}
	return e.Status
}

// failFileA hands j over from Fetching with a permanent fault on file A, after
// file A's first n articles are written. The download-complete report flushes
// the recorder only when every file of the job completes (reportDownloadComplete,
// reached from completeFinalizedFile at app.go:1738), and file B never does, so
// the hand-over's flush is the first flush that can write these rows.
func (a *lrApp) failFileA(t *testing.T, j *job.Job, n int) {
	t.Helper()
	for art := range n {
		a.deliver(t, j, art)
	}
	lrWaitFor(t, "file A's articles written", func() bool {
		for art := range n {
			if !articleDone(j, art) {
				return false
			}
		}
		return true
	})
	a.Fail(j, storagefault.Classify("write", filepath.Join(j.Name(), "A.bin"), syscall.EIO))
}

// TestLooseRecord_HandOverFlushesBeforePostProcessing pins enqueuePostProc's
// synchronous flush: every written row is in SQLite before post-processing
// can change the bytes it describes. A failed job skips its stages, so the
// test reads SQLite while the job's finalizer cannot run: a blocker job holds
// postproc's single worker (postproc.go:54) and the handed-over job waits
// queued behind it. Dropping the hand-over flush is killed by cutover.spec.
func TestLooseRecord_HandOverFlushesBeforePostProcessing(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseStage := func() { releaseOnce.Do(func() { close(release) }) }
	env.stage = lrFuncStage{fn: func(pj *postproc.Job) error {
		if pj.Job.Name() == "blocker" {
			close(entered)
			<-release
		}
		return nil
	}}
	a := env.newApp(t)
	a.start(t)
	// Registered after start, so it runs before start's StopAndJoin: a
	// failure before the release must not leave the worker parked.
	t.Cleanup(releaseStage)
	blocker := a.addJob(t, "blocker", 3, 2)
	a.deliverAll(t, blocker, 3, 2)
	lrAwait(t, "the blocker to reach its stage", entered)

	j := a.addJob(t, "handover", 3, 2)
	a.failFileA(t, j, 3)
	if got := len(rowsFor(t, a.repo.DB(), j.ID())); got != 3 {
		t.Errorf("after the hand-over, SQLite holds %d of file A's 3 rows, want all 3", got)
	}
	releaseStage()
	a.lrWaitFinalized(t, j.ID())
}

// failFirstRecord fails the first ApplyRecord and passes every later one. The
// finalizer test relies on that first call being the hand-over flush, which
// holds because failFileA leaves file B incomplete (see failFileA's doc).
type failFirstRecord struct {
	recordStore
	failed atomic.Bool
}

func (f *failFirstRecord) ApplyRecord(ctx context.Context, b []durability.RecordBatch) error {
	if f.failed.CompareAndSwap(false, true) {
		return errors.New("injected record failure")
	}
	return f.recordStore.ApplyRecord(ctx, b)
}

// TestLooseRecord_FinalizerFlushesBeforeTheJobLeaves pins the finalizer's
// flush: rows a failed hand-over flush put back are written while the instance
// is still the dispatcher's, so a FAILED entry's retry starts from them. Once
// RemoveJob runs, the instance check drops them. The hand-over flush is the
// first ApplyRecord (failFirstRecord fails it), because no download-complete
// report precedes it.
func TestLooseRecord_FinalizerFlushesBeforeTheJobLeaves(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a := env.newApp(t, func(app *Application) {
		app.recorder.st = &failFirstRecord{recordStore: app.recorder.st}
	})
	a.start(t)
	j := a.addJob(t, "finflush", 3, 2)
	a.failFileA(t, j, 3)
	if status := a.lrWaitFinalized(t, j.ID()); status != "Failed" {
		t.Fatalf("fixture: the job was filed %q, want Failed so its rows are kept", status)
	}
	if _, held := a.dispatcher.Job(j.ID()); held {
		t.Fatal("fixture: the job is still in the dispatcher after its post-processing completed")
	}
	if got := len(rowsFor(t, a.repo.DB(), j.ID())); got != 3 {
		t.Errorf("the FAILED entry keeps %d of file A's 3 rows, want all 3", got)
	}
}
