package queue

import (
	"database/sql"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// storedFile is one job_files row, read back for the identity-versus-state
// comparison these tests are about.
type storedFile struct {
	idx          int
	subject      string
	isPar2       bool
	complete     bool
	deferred     bool
	articlesDone string
}

func readJobFiles(t *testing.T, db *sql.DB, jobID string) []storedFile {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT file_index, subject, is_par2_recovery, complete, deferred, COALESCE(articles_done, '')
		 FROM job_files WHERE job_id = ? ORDER BY file_index`, jobID)
	if err != nil {
		t.Fatalf("query job_files: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []storedFile
	for rows.Next() {
		var f storedFile
		var isPar2, complete, deferred int
		if err := rows.Scan(&f.idx, &f.subject, &isPar2, &complete, &deferred, &f.articlesDone); err != nil {
			t.Fatalf("scan job_files: %v", err)
		}
		f.isPar2, f.complete, f.deferred = isPar2 == 1, complete == 1, deferred == 1
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate job_files: %v", err)
	}
	return out
}

// renumberNZB places the deferred recovery volume between two content files,
// so discarding it shifts content-2 from index 2 to index 1. The shift is the
// whole point: a discard of the final file renumbers nothing and none of this
// arises.
func renumberNZB() *nzb.NZB {
	return &nzb.NZB{
		Files: []nzb.File{
			{Subject: "content-1.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "c1@x", Bytes: 1000, Number: 1}}},
			{Subject: `"content.vol000+01.par2" yEnc`, Bytes: 500, Articles: []nzb.Article{{ID: "v@x", Bytes: 500, Number: 1}}},
			{Subject: "content-2.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "c2@x", Bytes: 1000, Number: 1}}},
		},
	}
}

// failedDiscardFixture builds the state #310 is about: a three-file job whose
// middle file has been discarded in memory and whose persistence failed, so
// the stored rows keep the pre-discard shape.
func failedDiscardFixture(t *testing.T) (*Queue, *failingStore, *sql.DB, *Job, string) {
	t.Helper()
	real, dir, db := setupResidencyTestStoreWithDB(t)
	fs := &failingStore{Store: real}
	q := New(WithStore(fs), WithStateDir(dir))

	job, err := NewJob(renumberNZB(), AddOptions{Filename: "renumber.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if pre := mustManifest(t, job); !pre.FileIsPar2Recovery(1) {
		t.Fatal("fixture guard: expected the deferred recovery volume at index 1, so discarding it renumbers content-2")
	}
	if err := q.MarkArticlesDone(job.ID, []string{"c2@x"}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}

	fs.failReplaceMani = true
	if err := q.DiscardDeferredPar2(job.ID); err == nil {
		t.Fatal("fixture guard: DiscardDeferredPar2 was expected to report the persistence failure")
	}
	if !job.ManifestRowsStale() {
		t.Fatal("fixture guard: the job is not flagged, so nothing below is about the flag")
	}
	return q, fs, db, job, dir
}

// A checkpoint that finds the flag raised must retry the wholesale rewrite,
// not just decline to write. Skipping alone freezes the job's per-file state
// on disk for the life of the process, so recovery is the other half of the
// fix and not an optimisation.
func TestCheckpoint_ReconcilesOnceTheRewriteSucceeds(t *testing.T) {
	q, fs, db, job, dir := failedDiscardFixture(t)

	// A checkpoint while the store is still failing: rows untouched, flag
	// still raised.
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save during the failure: %v", err)
	}
	if !job.ManifestRowsStale() {
		t.Error("the flag was cleared by a checkpoint that never reconciled the rows")
	}

	// The next checkpoint, with the store healthy again.
	fs.failReplaceMani = false
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save after recovery: %v", err)
	}
	if job.ManifestRowsStale() {
		t.Error("the flag is still raised after a successful rewrite; this job's per-file state would never be written again")
	}

	m := mustManifest(t, job)
	stored := readJobFiles(t, db, job.ID)
	if len(stored) != m.NumFiles() {
		t.Fatalf("job_files holds %d rows, want %d", len(stored), m.NumFiles())
	}
	for i, f := range stored {
		if f.idx != i || f.subject != m.FileSubject(i) {
			t.Errorf("row %d is %q, want index %d subject %q", f.idx, f.subject, i, m.FileSubject(i))
		}
		if f.deferred {
			t.Errorf("row %d (%q) is still deferred after the discard was persisted", f.idx, f.subject)
		}
	}
	// content-2 shifted from index 2 to 1 and must carry its own done bit.
	if stored[1].subject != "content-2.bin" {
		t.Fatalf("fixture guard: expected content-2 at index 1, got %q", stored[1].subject)
	}
	if stored[1].articlesDone == "" || stored[1].articlesDone == "0000" {
		t.Errorf("content-2's row has articles_done=%q; its downloaded article was lost in the reconcile", stored[1].articlesDone)
	}
}

// retryManifestRewrites' own branches, driven directly rather than through
// Save, which only ever reaches the success path.
func TestRetryManifestRewrites(t *testing.T) {
	t.Run("skips a job that is not flagged", func(t *testing.T) {
		real, dir, db := setupResidencyTestStoreWithDB(t)
		fs := &failingStore{Store: real}
		q := New(WithStore(fs), WithStateDir(dir))
		job := makeMultiFileJob(t, "retry-unflagged", 2, 1)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		before := readJobFiles(t, db, job.ID)

		// A failing store proves the call never reached it.
		fs.failReplaceMani = true
		q.retryManifestRewrites(t.Context(), []*Job{cloneJob(job)})

		if got := readJobFiles(t, db, job.ID); len(got) != len(before) {
			t.Errorf("rows changed for an unflagged job: %d -> %d", len(before), len(got))
		}
		if job.ManifestRowsStale() {
			t.Error("an unflagged job came back flagged")
		}
	})

	t.Run("leaves a non-resident job flagged", func(t *testing.T) {
		real, dir, _ := setupResidencyTestStoreWithDB(t)
		q := New(WithStore(real), WithStateDir(dir), WithMaxActiveJobs(1))
		filler := makeMultiFileJob(t, "retry-filler", 1, 1)
		if err := q.Add(filler); err != nil {
			t.Fatalf("Add filler: %v", err)
		}
		job := makeMultiFileJob(t, "retry-nonresident", 2, 1)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if manifestResident(job) {
			t.Fatal("fixture guard: the job is resident, so this exercises the wrong branch")
		}
		job.setManifestRowsStale(true)

		q.retryManifestRewrites(t.Context(), []*Job{cloneJob(job)})

		// No manifest means no file set to write the rows from. Leaving the
		// rows untouched is the point; clearing the flag would claim they had
		// been reconciled against a shape this process cannot see.
		if !job.ManifestRowsStale() {
			t.Error("the flag was cleared for a job whose manifest could not be read")
		}
	})

	t.Run("leaves the flag raised when the rewrite fails", func(t *testing.T) {
		real, dir, _ := setupResidencyTestStoreWithDB(t)
		fs := &failingStore{Store: real}
		q := New(WithStore(fs), WithStateDir(dir))
		job := makeMultiFileJob(t, "retry-fails", 2, 1)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		job.setManifestRowsStale(true)

		fs.failReplaceMani = true
		q.retryManifestRewrites(t.Context(), []*Job{cloneJob(job)})

		if !job.ManifestRowsStale() {
			t.Error("the flag was cleared by a rewrite that failed; the next checkpoint would resume writing job_files by a renumbered index")
		}
	})

	t.Run("clears the flag on the live job when the rewrite lands", func(t *testing.T) {
		real, dir, _ := setupResidencyTestStoreWithDB(t)
		q := New(WithStore(real), WithStateDir(dir))
		job := makeMultiFileJob(t, "retry-succeeds", 2, 1)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		job.setManifestRowsStale(true)

		q.retryManifestRewrites(t.Context(), []*Job{cloneJob(job)})

		// The live job, not the clone: clearing only the snapshot would leave
		// updateTx skipping this job's rows for the rest of the process.
		if job.ManifestRowsStale() {
			t.Error("the live job is still flagged after a successful rewrite")
		}
	})
}

// clearManifestRowsStaleIfGen must refuse a rewrite whose file set the job no
// longer has, and accept one that merely raced ordinary residency churn.
//
// The retry runs outside the queue lock on a clone, so two different things
// can happen in between and they need opposite answers. A second discard
// rebuilds the file set and raises the flag for its own attempt — that
// attempt owns it, and a rewrite of the file set it replaced must not clear
// it. Eviction changes nothing about which files the job has, so a rewrite
// that raced it is still valid.
//
// Keying this on manifest pointer identity got the second case wrong: eviction
// nils the pointer and rehydration installs a freshly deserialized one, so a
// job that churned through the active set would refuse every later clear and
// keep manifestRowsStale raised for the life of the process. updateTx would
// then skip its job_files half forever, so the job's per-file progress would
// stop persisting incrementally.
func TestClearManifestRowsStaleIfGen(t *testing.T) {
	t.Parallel()

	newFlaggedJob := func() (*Job, uint64) {
		j := &Job{ID: "guard"}
		m := &Manifest{}
		j.setResidency(m, newJobProgress(m))
		j.setManifestRowsStale(true)
		return j, j.FileSetGen()
	}

	t.Run("a rebuilt file set refuses the clear", func(t *testing.T) {
		t.Parallel()
		job, gen := newFlaggedJob()
		job.bumpFileSetGen() // a second discard

		if job.clearManifestRowsStaleIfGen(gen) {
			t.Error("a rewrite of the superseded file set cleared the flag")
		}
		if !job.ManifestRowsStale() {
			t.Error("the flag was cleared for a file set the job no longer has")
		}
		if !job.clearManifestRowsStaleIfGen(job.FileSetGen()) {
			t.Error("a rewrite of the current file set did not clear the flag")
		}
	})

	t.Run("eviction and rehydration do not refuse the clear", func(t *testing.T) {
		t.Parallel()
		job, gen := newFlaggedJob()

		// Ordinary residency churn: the job leaves the active set and comes
		// back with a manifest deserialized afresh from disk.
		job.setResidency(nil, job.progress)
		rehydrated := &Manifest{}
		job.setResidency(rehydrated, newJobProgress(rehydrated))

		if !job.clearManifestRowsStaleIfGen(gen) {
			t.Fatal("eviction invalidated a rewrite that was still valid; the job's per-file state would never persist incrementally again")
		}
		if job.ManifestRowsStale() {
			t.Error("the flag survived a valid rewrite")
		}
	})
}

// TestCheckpoint_AfterAFailedDiscardDoesNotSpliceRows reproduces #310.
//
// updateTx rewrites job_files with UPDATE ... WHERE file_index = ?, taking
// every value from the live manifest and never touching the identity columns
// (subject, bytes, is_par2_recovery, date). That is safe only while the rows
// describe the same file set the manifest does.
//
// #308 keeps them in step on the success path — DiscardDeferredPar2 persists
// the rebuilt manifest and rows together. When that persist fails, the job is
// left with a shrunk in-memory manifest and pre-discard rows, and every
// checkpoint tick then writes each surviving file's state onto the row of its
// pre-discard neighbour. The recovery volume's row picks up content-2's done
// bit and loses its deferral, so on the next hydration the volume reads as
// already downloaded and is never fetched, while content-2 reads as still
// needing download.
//
// The failure is silent and repeats on every tick until the process restarts.
func TestCheckpoint_AfterAFailedDiscardDoesNotSpliceRows(t *testing.T) {
	real, dir, db := setupResidencyTestStoreWithDB(t)
	fs := &failingStore{Store: real}
	q := New(WithStore(fs), WithStateDir(dir))

	job, err := NewJob(renumberNZB(), AddOptions{Filename: "renumber.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if pre := mustManifest(t, job); !pre.FileIsPar2Recovery(1) {
		t.Fatal("fixture guard: expected the deferred recovery volume at index 1, so discarding it renumbers content-2")
	}
	if err := q.MarkArticlesDone(job.ID, []string{"c2@x"}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}

	// The discard succeeds in memory and fails to persist. That is the state
	// #308 leaves behind and explicitly declines to roll back.
	fs.failReplaceMani = true
	if err := q.DiscardDeferredPar2(job.ID); err == nil {
		t.Fatal("fixture guard: DiscardDeferredPar2 was expected to report the persistence failure")
	}

	// The periodic checkpoint. Nothing about it knows the shapes disagree.
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	for _, f := range readJobFiles(t, db, job.ID) {
		if !f.isPar2 {
			continue
		}
		// The recovery volume's row. It was deferred and had nothing
		// downloaded; only a splice from a neighbouring index can change that.
		if f.deferred == false {
			t.Errorf("row %d (%q) is no longer deferred after a checkpoint; the recovery volume will never be fetched yet counts as present for repair",
				f.idx, f.subject)
		}
		if f.articlesDone != "" && f.articlesDone != "0000" {
			t.Errorf("row %d (%q) has articles_done=%q; a deferred volume is never dispatched, so this is another file's progress written onto its row",
				f.idx, f.subject, f.articlesDone)
		}
	}
}
