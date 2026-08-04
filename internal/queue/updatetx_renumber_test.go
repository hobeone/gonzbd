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

// clearManifestRowsStaleIf must refuse a rewrite that no longer describes the
// job. The retry runs outside the queue lock on a clone, so between the clone
// and the clear another discard can rebuild the live manifest — and that
// discard's own success or failure owns the flag from then on. Clearing it
// here would report rows as reconciled against a file set the job no longer
// has.
func TestClearManifestRowsStaleIf_RefusesAStaleRewrite(t *testing.T) {
	t.Parallel()

	job := &Job{ID: "guard"}
	first := &Manifest{}
	job.setResidency(first, newJobProgress(first))
	job.setManifestRowsStale(true)

	// A discard rebuilds the manifest after the clone was taken.
	second := &Manifest{}
	job.setResidency(second, newJobProgress(second))

	if job.clearManifestRowsStaleIf(first) {
		t.Error("a rewrite of the superseded manifest cleared the flag")
	}
	if !job.ManifestRowsStale() {
		t.Error("the flag was cleared for a file set the job no longer has")
	}
	if !job.clearManifestRowsStaleIf(second) {
		t.Error("a rewrite of the current manifest did not clear the flag")
	}
	if job.ManifestRowsStale() {
		t.Error("the flag survived a rewrite of the job's own manifest")
	}
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
