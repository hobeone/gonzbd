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
	idx      int
	subject  string
	isPar2   bool
	complete bool
	// fetch is the raw stored policy; deferred collapses it to a bool for
	// callers that only care whether the row is held back at all, not by
	// which policy. Comparisons that need to distinguish "still awaiting the
	// CRC verdict" (FetchIfNeeded) from "discarded" (FetchNever) must use
	// fetch directly — deferred is true for both.
	fetch        FetchPolicy
	deferred     bool
	articlesDone string
}

func readJobFiles(t *testing.T, db *sql.DB, jobID string) []storedFile {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT file_index, subject, is_par2_recovery, complete, fetch_policy, COALESCE(articles_done, '')
		 FROM job_files WHERE job_id = ? ORDER BY file_index`, jobID)
	if err != nil {
		t.Fatalf("query job_files: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []storedFile
	for rows.Next() {
		var f storedFile
		var isPar2, complete, fetch int
		if err := rows.Scan(&f.idx, &f.subject, &isPar2, &complete, &fetch, &f.articlesDone); err != nil {
			t.Fatalf("scan job_files: %v", err)
		}
		f.isPar2, f.complete, f.fetch = isPar2 == 1, complete == 1, FetchPolicy(fetch)
		f.deferred = f.fetch != FetchAlways
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate job_files: %v", err)
	}
	return out
}

// renumberNZB places the deferred recovery volume between two content files.
// The name and the middle placement predate this task: a discard used to
// shift content-2 from index 2 to index 1, and several fixtures below still
// rely on the recovery volume sitting at index 1 with a surviving file on
// either side of it, now to pin that nothing shifts.
func renumberNZB() *nzb.NZB {
	return &nzb.NZB{
		Files: []nzb.File{
			{Subject: "content-1.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "c1@x", Bytes: 1000, Number: 1}}},
			{Subject: `"content.vol000+01.par2" yEnc`, Bytes: 500, Articles: []nzb.Article{{ID: "v@x", Bytes: 500, Number: 1}}},
			{Subject: "content-2.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "c2@x", Bytes: 1000, Number: 1}}},
		},
	}
}

// failedDiscardFixture builds a three-file job whose recovery volume has
// been discarded and whose job_files rows are flagged stale, as if some
// reconciliation attempt still owed the store a wholesale rewrite.
//
// This used to be produced by running DiscardDeferredPar2 against a failing
// store: the discard's own rebuild bumped fileSetGen, raised the flag, and
// then failed to persist via ReplaceManifest, leaving the flag stuck raised.
// This task removes all three of those calls from DiscardDeferredPar2 (see
// its doc comment) — a discard can no longer raise the flag, let alone fail
// to clear it, because it no longer touches the store at all. The flag and
// the retry machinery it drives (reconcileJobFiles, clearManifestRowsStaleIfGen)
// are untouched production code, so this fixture now raises the flag
// directly: it does not matter to that machinery why the flag is up, only
// that it is, and the discard still runs first so the fixture matches a real
// discard's effect on the file's policy.
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
		t.Fatal("fixture guard: expected the deferred recovery volume at index 1")
	}
	if err := q.MarkArticlesDone(job.ID, []string{"c2@x"}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}

	fs.failReplaceMani = true
	job.bumpFileSetGen()
	job.setManifestRowsStale(true)
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
	// Indices no longer move: content-2 stays at index 2, the discarded
	// recovery volume stays at index 1.
	for i, f := range stored {
		if f.idx != i || f.subject != m.FileSubject(i) {
			t.Errorf("row %d is %q, want index %d subject %q", f.idx, f.subject, i, m.FileSubject(i))
		}
	}
	if stored[1].fetch != FetchNever {
		t.Errorf("row 1 (%q) fetch policy = %d, want FetchNever after the discard was reconciled", stored[1].subject, stored[1].fetch)
	}
	if stored[0].deferred || stored[2].deferred {
		t.Error("a file that was never discarded reads as deferred after the reconcile")
	}
	if stored[2].subject != "content-2.bin" {
		t.Fatalf("fixture guard: expected content-2 at index 2, got %q", stored[2].subject)
	}
	if stored[2].articlesDone == "" || stored[2].articlesDone == "0000" {
		t.Errorf("content-2's row has articles_done=%q; its downloaded article was lost in the reconcile", stored[2].articlesDone)
	}
}

// reconcileJobFiles' own branches, driven directly rather than through
// Save, which only ever reaches the success path.
func TestReconcileJobFiles(t *testing.T) {
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
		q.reconcileJobFiles(t.Context(), []*Job{cloneJob(job)})

		if got := readJobFiles(t, db, job.ID); len(got) != len(before) {
			t.Errorf("rows changed for an unflagged job: %d -> %d", len(before), len(got))
		}
		if job.ManifestRowsStale() {
			t.Error("an unflagged job came back flagged")
		}
	})

	t.Run("marks a snapshot the job has outrun", func(t *testing.T) {
		real, dir, _ := setupResidencyTestStoreWithDB(t)
		q := New(WithStore(real), WithStateDir(dir))
		job, err := NewJob(renumberNZB(), AddOptions{Filename: "outrun.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("NewJob: %v", err)
		}
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		snap := cloneJob(job)

		// Something rebuilds the live job's file set after the clone was
		// taken. DiscardDeferredPar2 can no longer do this itself — it never
		// calls bumpFileSetGen, since it no longer rebuilds anything (see its
		// doc comment) — so this drives fileSetGen directly, which is the
		// only thing this branch of reconcileJobFiles actually reads.
		job.bumpFileSetGen()
		if snap.ManifestRowsStale() {
			t.Fatal("fixture guard: the snapshot is already flagged, so this proves nothing")
		}

		q.reconcileJobFiles(t.Context(), []*Job{snap})

		if !snap.ManifestRowsStale() {
			t.Error("a snapshot whose file set the job has rebuilt was left writable; updateTx would address the renumbered rows by their old indices")
		}
		if job.ManifestRowsStale() {
			t.Error("the live job was flagged; its own rows are current and its per-file state must keep persisting")
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

		q.reconcileJobFiles(t.Context(), []*Job{cloneJob(job)})

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
		q.reconcileJobFiles(t.Context(), []*Job{cloneJob(job)})

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

		q.reconcileJobFiles(t.Context(), []*Job{cloneJob(job)})

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

// TestCheckpoint_AfterAFailedDiscardDoesNotSpliceRows and
// TestUpdateBatch_StaleSnapshotAfterASuccessfulDiscard used to reproduce
// #310: updateTx rewrites job_files with UPDATE ... WHERE file_index = ?,
// taking every value from the live manifest and never touching the identity
// columns (subject, bytes, is_par2_recovery, date), which is safe only while
// the stored rows describe the same file set the manifest does. A discard
// that rebuilt and renumbered the manifest but failed to persist the rows to
// match — or a snapshot cloned before a renumbering discard and written
// after it — left the rows one file_index off from the manifest, so every
// subsequent checkpoint spliced a surviving file's state onto its
// pre-discard neighbour's row.
//
// This task removes the premise both tests shared, not just their fixture:
// DiscardDeferredPar2 no longer renumbers anything (see its doc comment), so
// file_index is now permanently stable across a discard and there is no
// neighbour for a row to be spliced onto — by construction, not by a check
// that could be defeated. A stale snapshot or a job flagged
// ManifestRowsStale can still cause a row's own per-file state (its policy,
// its downloaded bytes) to lag by one checkpoint tick, but that is ordinary,
// self-correcting staleness, indistinguishable from any other field racing a
// concurrent mutation, and not the cross-file corruption #310 was about.
// TestCheckpoint_ReconcilesOnceTheRewriteSucceeds and the ManifestRowsStale
// branches in TestReconcileJobFiles above already cover that ordinary
// staleness. No replacement for the splice-specific assertions is added
// here: there is no longer a splice to pin.
