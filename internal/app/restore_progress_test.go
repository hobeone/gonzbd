package app

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// The restore pass (restoreProgress) and the hydration that follows it: every
// registered job carries its file table and a progress record seeded from the
// record before the first tick, and its first hydration in the process
// verifies every file.

// lrFileNames are the file names of an lrNZBFiles job, by file index.
var lrFileNames = []string{"A.bin", "B.bin", "C.bin", "D.bin"}

// lrNZBFiles renders an NZB of len(counts) files named from lrFileNames, file
// fi with counts[fi] articles of lrArtLen bytes.
func lrNZBFiles(counts ...int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="iso-8859-1" ?>` + "\n")
	b.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	for fi, n := range counts {
		fmt.Fprintf(&b, `<file poster="p@t" date="1700000000" subject="&quot;%s&quot; yEnc (1/%d)">`+"\n", lrFileNames[fi], n)
		b.WriteString("<groups><group>alt.bin.test</group></groups>\n<segments>\n")
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, `<segment bytes="%d" number="%d">f%da%d@t</segment>`+"\n", lrArtLen, i, fi, i)
		}
		b.WriteString("</segments>\n</file>\n")
	}
	b.WriteString("</nzb>\n")
	return []byte(b.String())
}

// addJobFiles queues an lrNZBFiles job.
func (a *lrApp) addJobFiles(t *testing.T, name string, counts ...int) *job.Job {
	t.Helper()
	raw := lrNZBFiles(counts...)
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

// writeFile puts articles arts of file fi of j on disk at their offsets, over
// a file of n articles.
func (e *lrEnv) writeFile(t *testing.T, j *job.Job, fi, n int, arts ...int) {
	t.Helper()
	path := filepath.Join(e.dl, j.Name(), lrFileNames[fi])
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	buf := make([]byte, n*lrArtLen)
	for _, a := range arts {
		copy(buf[a*lrArtLen:], lrArticle(fi, a))
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// recordFile records file fi of a job in SQLite as a flush would leave it:
// its filename and complete flag, and a row for each of arts, whose global
// index is base+art.
func (a *lrApp) recordFile(t *testing.T, jobID string, fi, base int, complete bool, arts ...int) {
	t.Helper()
	b := durability.RecordBatch{
		JobID: jobID,
		Files: []durability.FileState{{FileIdx: fi, Filename: lrFileNames[fi], Complete: complete}},
	}
	for _, art := range arts {
		b.Rows = append(b.Rows, durability.WrittenRow{
			FileIdx: fi, ArtIdx: int32(base + art), //nolint:gosec // G115: a handful of articles
			Offset: int64(art) * lrArtLen, Length: lrArtLen,
			CRC32: crc32.ChecksumIEEE(lrArticle(fi, art)),
		})
	}
	if err := a.st.ApplyRecord(t.Context(), []durability.RecordBatch{b}); err != nil {
		t.Fatalf("ApplyRecord: %v", err)
	}
}

// doneCount is the number of j's articles whose Done bit is set.
func doneCount(j *job.Job) int {
	p := j.Progress()
	n := 0
	for i := range p.TotalArticles() {
		if p.ArticleDone(i) {
			n++
		}
	}
	return n
}

// assertRestoredUnverified fails unless j carries a file table and progress
// from the restore pass, unverified and not resident.
func assertRestoredUnverified(t *testing.T, what string, j *job.Job) {
	t.Helper()
	if j.Files() == nil || j.Files().NumFiles() == 0 || !j.HasProgress() {
		t.Errorf("%s: Files = %v, HasProgress = %v after the restore pass, want a table and progress", what, j.Files(), j.HasProgress())
		return
	}
	if j.AllVerified() {
		t.Errorf("%s is Verified before any hydration", what)
	}
	if j.Resident() {
		t.Errorf("%s is resident: the restore pass attaches no articles", what)
	}
}

// TestRestoreProgress_EveryRegisteredJobHasFilesAndProgress runs the startup
// hook (reconcileBeforeFirstTick) over a queue holding a job paused before it
// ran, one paused at Fetching, one queued with part of a file recorded, and
// one whose files are all complete awaiting post-processing. No tick runs, so
// nothing hydrates: every job carries a table and an unverified progress
// record from the restore pass alone.
func TestRestoreProgress_EveryRegisteredJobHasFilesAndProgress(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)

	neverRun := a1.addJob(t, "neverrun", 3, 2)
	if err := a1.dispatcher.PauseJob(neverRun.ID()); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	pausedFetching := pauseAtFetching(t, env, a1, "pausedfetching")

	partial := a1.addJob(t, "partial", 4, 2)
	env.writeFileA(t, partial, 4, 0, 1, 2)
	a1.recordFileA(t, partial.ID(), false, 0, 1, 2)

	complete := a1.addJob(t, "complete", 2, 2)
	env.writeFile(t, complete, 0, 2, 0, 1)
	env.writeFile(t, complete, 1, 2, 0, 1)
	a1.recordFile(t, complete.ID(), 0, 0, true, 0, 1)
	a1.recordFile(t, complete.ID(), 1, 2, true, 0, 1)
	if _, err := a1.repo.DB().ExecContext(t.Context(),
		`UPDATE dispatch_jobs SET state = ? WHERE id = ?`, int(job.Assessing), complete.ID()); err != nil {
		t.Fatalf("restore at Assessing: %v", err)
	}

	a2 := env.newApp(t)
	if err := a2.reconcileBeforeFirstTick(t.Context()); err != nil {
		t.Fatalf("reconcileBeforeFirstTick: %v", err)
	}
	for name, j := range map[string]*job.Job{
		"the job paused before it ran": neverRun,
		"the job paused at Fetching":   pausedFetching,
		"the partly recorded job":      partial,
		"the complete job":             complete,
	} {
		assertRestoredUnverified(t, name, a2.registered(t, j.ID()))
	}

	p2 := a2.registered(t, partial.ID())
	ft := p2.Files()
	var want int64
	for fi := range ft.NumFiles() {
		recorded := map[int]int64{0: 3}[fi]
		want += ft.Bytes(fi) - ft.Bytes(fi)*recorded/int64(ft.ArticleCount(fi))
	}
	if got := p2.RemainingBytes(); got != want {
		t.Errorf("the partly recorded job's RemainingBytes = %d, want %d estimated from its recorded count", got, want)
	}
	if got := p2.RemainingBytes(); got == ft.TotalBytes() {
		t.Errorf("the partly recorded job's RemainingBytes = %d, its whole size: the recorded count was not seeded", got)
	}
	if c2 := a2.registered(t, complete.ID()); !c2.IsComplete() {
		t.Error("the complete job does not read complete from its seeded complete flags")
	}
}

// TestRestoreProgress_SeedsNoDoneBit pins durability §3: the restore pass seeds
// a recorded count, never an article bit, so every recorded article is
// Outstanding until a hydration reads it back.
func TestRestoreProgress_SeedsNoDoneBit(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	j := a1.addJob(t, "nobits", 4, 2)
	env.writeFileA(t, j, 4, 0, 1, 2, 3)
	a1.recordFileA(t, j.ID(), true, 0, 1, 2, 3)

	a2 := env.newApp(t)
	if err := a2.restoreProgress(t.Context()); err != nil {
		t.Fatalf("restoreProgress: %v", err)
	}
	j2 := a2.registered(t, j.ID())
	if !j2.HasProgress() {
		t.Fatal("fixture: the restore pass attached no progress")
	}
	if n := doneCount(j2); n != 0 {
		t.Errorf("%d articles Done after the restore pass, want 0: a Done bit exists only after a read-back", n)
	}
	if err := a2.residency.Hydrate(t.Context(), j2.ID()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if n := doneCount(j2); n != 4 {
		t.Errorf("%d articles Done after the hydration, want the 4 recorded", n)
	}
}

// TestRestoreProgress_SettlesAJobWithoutStructureFailed pins the restore
// pass's refusal: a job whose job_files rows are gone, or do not cover its
// file indices, cannot be shown or run, so it is settled Failed with an
// operational error naming the gap, and the others are restored as usual. A
// job that never ran has no attempt to settle; it is left without a table,
// and the tick settles it Failed at its first hydration.
func TestRestoreProgress_SettlesAJobWithoutStructureFailed(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	noRows := a1.addJob(t, "norows", 3, 2)
	gapped := a1.addJob(t, "gapped", 3, 2)
	healthy := a1.addJob(t, "healthy", 3, 2)
	a1.dispatcher.Tick(t.Context())
	a1.dispatcher.Tick(t.Context())
	for _, j := range []*job.Job{noRows, gapped, healthy} {
		if st := j.State().State; st != job.Fetching {
			t.Fatalf("fixture: %s is at %v, want Fetching", j.Name(), st)
		}
	}
	neverRun := a1.addJob(t, "neverrun", 3, 2)
	db := a1.repo.DB()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM written_articles WHERE job_id IN (?, ?)`, []any{noRows.ID(), neverRun.ID()}},
		{`DELETE FROM job_files WHERE job_id IN (?, ?)`, []any{noRows.ID(), neverRun.ID()}},
		{`DELETE FROM job_files WHERE job_id = ? AND file_index = 0`, []any{gapped.ID()}},
	} {
		if _, err := db.ExecContext(t.Context(), q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}

	a2 := env.newApp(t)
	a2.start(t)
	for _, j := range []*job.Job{noRows, gapped} {
		j2 := a2.registered(t, j.ID())
		if o := j2.State().Outcome; o != job.OutcomeFailed {
			t.Errorf("%s: outcome = %v after the restore pass, want Failed", j.Name(), o)
		}
		if j2.Files() != nil {
			t.Errorf("%s: carries a file table built from rows that do not describe it", j.Name())
		}
		if msg := a2.registeredHeader(t, j.ID()).OperationalError; !strings.Contains(msg, "job_files") {
			t.Errorf("%s: operational error = %q, want one naming the job_files gap", j.Name(), msg)
		}
	}
	if h := a2.registered(t, healthy.ID()); !h.HasProgress() || h.Files().NumFiles() != 2 {
		t.Error("the healthy job was not restored beside the ones without structure")
	}
	if o := a2.registered(t, healthy.ID()).State().Outcome; o != job.OutcomePending {
		t.Errorf("the healthy job's outcome = %v, want pending", o)
	}
	nr := a2.registered(t, neverRun.ID())
	lrWaitFor(t, "the never-run job without structure to settle Failed at its first hydration", func() bool {
		return nr.State().Outcome == job.OutcomeFailed
	})
}

// TestRestoreProgress_AnUnreadableRowCostsOnlyItsJob pins the blast radius of
// a job_files row that does not read as its column types: that job is settled
// Failed with an operational error naming the row, and Start still succeeds
// with the other job restored.
func TestRestoreProgress_AnUnreadableRowCostsOnlyItsJob(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	damaged := a1.addJob(t, "damaged", 3, 2)
	healthy := a1.addJob(t, "healthy", 3, 2)
	a1.dispatcher.Tick(t.Context())
	a1.dispatcher.Tick(t.Context())
	for _, j := range []*job.Job{damaged, healthy} {
		if st := j.State().State; st != job.Fetching {
			t.Fatalf("fixture: %s is at %v, want Fetching", j.Name(), st)
		}
	}
	if _, err := a1.repo.DB().ExecContext(t.Context(),
		`UPDATE job_files SET bytes = 'garbled' WHERE job_id = ? AND file_index = 1`, damaged.ID()); err != nil {
		t.Fatalf("damage a row: %v", err)
	}

	a2 := env.newApp(t)
	a2.start(t)
	d := a2.registered(t, damaged.ID())
	if o := d.State().Outcome; o != job.OutcomeFailed {
		t.Errorf("damaged job outcome = %v, want Failed", o)
	}
	if d.Files() != nil {
		t.Error("the damaged job carries a file table")
	}
	if msg := a2.registeredHeader(t, damaged.ID()).OperationalError; !strings.Contains(msg, "file index 1") || !strings.Contains(msg, "bytes") {
		t.Errorf("operational error = %q, want one naming the row's file index and column", msg)
	}
	if h := a2.registered(t, healthy.ID()); !h.HasProgress() || h.Files().NumFiles() != 2 {
		t.Error("the healthy job was not restored beside the damaged one")
	}
}

// TestRestoreProgress_NoStoreIsANoop pins the no-history case: with no record
// store there is no restored job, and the pass attaches nothing.
func TestRestoreProgress_NoStoreIsANoop(t *testing.T) {
	t.Parallel()
	a, err := New(testConfig(t.TempDir(), t.TempDir(), t.TempDir()), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	restoreQueue(t, a)
	j := job.New("bare", "bare", job.PolicyFromPP(3))
	if err := a.dispatcher.Add(t.Context(), j, dispatch.Header{Name: "bare"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := a.restoreProgress(t.Context()); err != nil {
		t.Fatalf("restoreProgress with no store = %v, want nil", err)
	}
	if j.Files() != nil || j.HasProgress() {
		t.Error("the restore pass attached content with no record store to read")
	}
}

// TestHydrate_FirstHydrationVerifiesEveryFile restores a job with a complete=1
// file whose rows all verify, a file with no rows, a partly recorded file,
// and a complete=1 file missing a row. Its first hydration verifies every
// file: afterwards each is Verified, the Done bits are the verified rows, and
// each complete file carries what only its install puts there — a settled CRC
// for the whole one and a failed set for the gapped one.
func TestHydrate_FirstHydrationVerifiesEveryFile(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	// Global article ranges: A 0-2, B 3-4, C 5-7, D 8-10.
	j := a1.addJobFiles(t, "everyfile", 3, 2, 3, 3)
	env.writeFile(t, j, 0, 3, 0, 1, 2)
	env.writeFile(t, j, 2, 3, 0, 1)
	env.writeFile(t, j, 3, 3, 0, 2)
	a1.recordFile(t, j.ID(), 0, 0, true, 0, 1, 2)
	a1.recordFile(t, j.ID(), 2, 5, false, 0, 1)
	a1.recordFile(t, j.ID(), 3, 8, true, 0, 2)

	a2 := env.newApp(t)
	if err := a2.restoreProgress(t.Context()); err != nil {
		t.Fatalf("restoreProgress: %v", err)
	}
	j2 := a2.registered(t, j.ID())
	if j2.AllVerified() || doneCount(j2) != 0 {
		t.Fatalf("fixture: before hydration AllVerified = %v with %d Done, want unverified with none", j2.AllVerified(), doneCount(j2))
	}
	if err := a2.residency.Hydrate(t.Context(), j2.ID()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if !j2.AllVerified() {
		t.Fatal("a file is unverified after the job's first hydration")
	}
	p := j2.Progress()
	wantDone := map[int]bool{0: true, 1: true, 2: true, 5: true, 6: true, 8: true, 10: true}
	for i := range p.TotalArticles() {
		if p.ArticleFailed(i) {
			continue // a failed article reads Done too (setFailedBits)
		}
		if got := p.ArticleDone(i); got != wantDone[i] {
			t.Errorf("article %d Done = %v, want %v", i, got, wantDone[i])
		}
	}
	if !p.ArticleFailed(9) {
		t.Error("the complete=1 file's article with no row is not failed: its failed set was not installed")
	}
	for i := range 9 {
		if p.ArticleFailed(i) {
			t.Errorf("article %d is failed; only file D's missing row is", i)
		}
	}
	whole := make([]byte, 0, 3*lrArtLen)
	for art := range 3 {
		whole = append(whole, lrArticle(0, art)...)
	}
	if got, want := p.FileAssembledCRC32(0), crc32.ChecksumIEEE(whole); got != want {
		t.Errorf("the complete file's CRC = %08x, want %08x settled from its rows", got, want)
	}
	if !p.FileComplete(0) || !p.FileComplete(3) || p.FileComplete(1) || p.FileComplete(2) {
		t.Error("the complete flags after hydration are not the recorded ones")
	}
}

// TestHydrate_VerifiedJobRehydratesWithoutVerifying pins the articles-only
// branch: a job whose files were all verified in this process is rehydrated
// from its articles alone. Its record reader is swapped for one that fails
// between the two hydrations, so a second hydration that read the record to
// verify it would fault.
func TestHydrate_VerifiedJobRehydratesWithoutVerifying(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	j := a1.addJob(t, "rehydrate", 4, 2)
	env.writeFileA(t, j, 4, 0, 1)
	a1.recordFileA(t, j.ID(), false, 0, 1)

	a2 := env.newApp(t)
	if err := a2.restoreProgress(t.Context()); err != nil {
		t.Fatalf("restoreProgress: %v", err)
	}
	j2 := a2.registered(t, j.ID())
	if err := a2.residency.Hydrate(t.Context(), j2.ID()); err != nil {
		t.Fatalf("first Hydrate: %v", err)
	}
	if !j2.AllVerified() || !articleDone(j2, 0) || !articleDone(j2, 1) {
		t.Fatal("fixture: the first hydration did not verify the job")
	}
	a2.residency.Evict(j2.ID())
	if j2.Resident() {
		t.Fatal("fixture: the job is resident after Evict")
	}

	readErr := errors.New("the record was read")
	a2.residency.store = failingRecordReader{filesErr: readErr, rowsErr: readErr}
	if err := a2.residency.Hydrate(t.Context(), j2.ID()); err != nil {
		t.Fatalf("second Hydrate = %v: a verified job's rehydration read its record", err)
	}
	if !j2.Resident() || !articleDone(j2, 0) || !articleDone(j2, 1) || articleDone(j2, 2) {
		t.Error("the rehydrated job lost or gained Done bits")
	}
}

// TestHydrate_RefusesAnUnverifiedJobWithoutAStore pins the no-store arm of
// the verifying branch: verification reads the record, so an unverified job
// with no store to read cannot hydrate, and nothing is attached.
func TestHydrate_RefusesAnUnverifiedJobWithoutAStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	ft := job.NewFileTable([]job.FileSpec{{Subject: "test.rar", Bytes: 100, ArticleCount: 1}})
	if err := j.AttachFiles(ft, []job.FileSeed{{}}); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), j)
	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, dir, nil, nil)

	err := r.Hydrate(context.Background(), "abc123")
	if err == nil || !strings.Contains(err.Error(), "no record store") {
		t.Fatalf("Hydrate = %v, want an error saying there is no record store", err)
	}
	if j.Resident() || j.AllVerified() {
		t.Error("a refused hydration attached the articles or verified the job")
	}
}

func TestFileTableFromRows_RefusesAGapAndADuplicate(t *testing.T) {
	t.Parallel()
	row := func(idx int, subject string) durability.FileRow {
		return durability.FileRow{FileIndex: idx, Subject: subject, Bytes: int64(idx+1) * 100, ArticleCount: idx + 1}
	}
	for name, rows := range map[string][]durability.FileRow{
		"a gap":       {row(0, "a"), row(2, "c")},
		"a duplicate": {row(0, "a"), row(1, "b"), row(1, "b")},
		"no first":    {row(1, "b")},
	} {
		if ft, err := fileTableFromRows(rows); err == nil {
			t.Errorf("fileTableFromRows(%s) = %d files, nil; want an error", name, ft.NumFiles())
		}
	}

	rows := []durability.FileRow{row(1, "b"), row(0, "a")}
	ft, err := fileTableFromRows(rows)
	if err != nil {
		t.Fatalf("fileTableFromRows(out of order) = %v, want the rows sorted", err)
	}
	if ft.Subject(0) != "a" || ft.Subject(1) != "b" || ft.ArticleCount(1) != 2 || ft.Bytes(0) != 100 {
		t.Errorf("table = {%q %q}, want rows ordered by FileIndex with their figures", ft.Subject(0), ft.Subject(1))
	}
	if rows[0].FileIndex != 1 {
		t.Error("fileTableFromRows reordered the caller's slice")
	}

	empty, err := fileTableFromRows(nil)
	if err != nil || empty.NumFiles() != 0 {
		t.Errorf("fileTableFromRows(nil) = %v files, %v; want a table of no files", empty, err)
	}
}

// TestFileOwedUnwantedFailures_CompleteJobWithUnreadableManifestIsFiled pins
// the filing a job owes when its hydration fails. Every file of the job is
// complete in the record, so the restore pass makes it read complete although
// it is not resident; its manifest is gone, so neither the startup sweep nor
// the first tick can hydrate it, and no Assessing worker will ever run for it.
// The filing must therefore be made at once, with the unwanted-extension
// message, rather than deferred to an Assessing the job cannot reach.
func TestFileOwedUnwantedFailures_CompleteJobWithUnreadableManifestIsFiled(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	owed := a1.addJob(t, "owed", 2, 1)
	a1.dispatcher.Tick(t.Context())
	a1.dispatcher.Tick(t.Context())
	if st := owed.State().State; st != job.Fetching {
		t.Fatalf("fixture: the owed job is at %v, want Fetching", st)
	}
	if moved, err := a1.dispatcher.BlockUnwanted(owed, false); err != nil || !moved {
		t.Fatalf("BlockUnwanted = (%v, %v)", moved, err)
	}
	a1.recordFile(t, owed.ID(), 0, 0, true, 0, 1)
	a1.recordFile(t, owed.ID(), 1, 2, true, 0)
	a1.dispatcher.Tick(t.Context()) // persists the block
	path, err := manifestPath(env.admin, owed.ID())
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}

	a2 := env.newApp(t)
	a2.start(t)

	lrWaitFor(t, "the owed filing in history", func() bool {
		e, err := a2.repo.Get(t.Context(), owed.ID())
		return err == nil && e.NzoID != ""
	})
	e, err := a2.repo.Get(t.Context(), owed.ID())
	if err != nil {
		t.Fatalf("history Get: %v", err)
	}
	if e.Status != "Failed" || e.FailMessage != unwantedFailPrefix {
		t.Errorf("history Status, FailMessage = %q, %q, want Failed, %q", e.Status, e.FailMessage, unwantedFailPrefix)
	}
	if _, ok := a2.dispatcher.Job(owed.ID()); ok {
		t.Error("the filed job is still registered in the dispatcher")
	}
}

// TestRestoreProgress_SeedsFilenameAndFetchPolicy pins two columns of the
// seed that no other restore test reads: before any hydration, a restored
// file carries the filename its row recorded and the fetch policy its row
// held (job_files.fetch_policy 1 is FetchIfNeeded).
func TestRestoreProgress_SeedsFilenameAndFetchPolicy(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	j := a1.addJob(t, "columns", 2, 2)
	a1.recordFile(t, j.ID(), 0, 0, false, 0)
	if _, err := a1.repo.DB().ExecContext(t.Context(),
		`UPDATE job_files SET fetch_policy = ? WHERE job_id = ? AND file_index = 1`,
		int(job.FetchIfNeeded), j.ID()); err != nil {
		t.Fatalf("set the fetch policy: %v", err)
	}

	a2 := env.newApp(t)
	if err := a2.restoreProgress(t.Context()); err != nil {
		t.Fatalf("restoreProgress: %v", err)
	}
	p := a2.registered(t, j.ID()).Progress()
	if p == nil || a2.registered(t, j.ID()).Resident() {
		t.Fatal("fixture: want restored progress on a job that is not resident")
	}
	if got := p.FileFilename(0); got != lrFileNames[0] {
		t.Errorf("FileFilename(0) = %q before any hydration, want the recorded %q", got, lrFileNames[0])
	}
	if got := p.FileFetchPolicy(1); got != job.FetchIfNeeded {
		t.Errorf("FileFetchPolicy(1) = %v before any hydration, want FetchIfNeeded", got)
	}
	if got := p.FileFetchPolicy(0); got != job.FetchAlways {
		t.Errorf("FileFetchPolicy(0) = %v, want FetchAlways", got)
	}
}
