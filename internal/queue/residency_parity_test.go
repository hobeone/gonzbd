package queue

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// commitBarrierExtents writes the Class B extents a durability.Barrier would
// have committed for job's currently-durable articles.
//
// It stands in for Task 10's barrier cadence, which does not exist yet, and it
// goes through the real durability.SQLiteExtentStore rather than writing rows
// by hand — so what the non-resident read path reads back is what production
// will actually have written, not a shape invented by the test.
//
// A file's durable bits are its articles that are done and not failed, which
// is exactly what Barrier.buildExtent sets: it charges bytes on the 0->1
// transition of an article the fsync covered, and a failed article never
// reaches a Drain. BytesDurable is the sum over those same articles, so the
// bitmap and the aggregate cannot disagree here for the same reason they
// cannot in the barrier.
func commitBarrierExtents(t *testing.T, db *sql.DB, job *Job) {
	t.Helper()
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("commitBarrierExtents: manifest: %v", err)
	}
	p := job.Progress()
	exts := make([]durability.FileExtent, 0, m.NumFiles())
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		bm := durability.NewBitmap(hi - lo)
		var bytesDurable int64
		for i := lo; i < hi; i++ {
			if p.ArticleDone(i) && !p.ArticleFailed(i) {
				bm.Set(i - lo)
				bytesDurable += int64(m.ArticleBytes(i))
			}
		}
		exts = append(exts, durability.FileExtent{
			FileIdx:      int32(fi), //nolint:gosec // G115: file counts are far below int32
			Durable:      bm,
			BytesDurable: bytesDurable,
		})
	}
	if err := durability.NewSQLiteExtentStore(db).Commit(context.Background(), job.ID, exts); err != nil {
		t.Fatalf("commitBarrierExtents: commit: %v", err)
	}
}

// TestRemainingBytes_IdenticalResidentAndNonResident is the acceptance
// property this refactor exists to establish: RemainingBytes, ExpectedBytes,
// and FailedBytes must report the same figure for the same job whether or
// not its manifest is resident. Every earlier attempt at deriving remaining
// bytes failed exactly this check.
//
// The fixture mixes all four kinds of file state that could make the two
// construction paths diverge: file 0 is partially downloaded (exercises
// BytesDownloaded), file 1 has a permanently failed article (exercises
// FailedBytes), file 2 is deferred (exercises the Deferred exclusion shared
// by all three figures), and file 3 is fully downloaded and marked Complete
// (exercises the Complete exclusion RemainingBytes applies but ExpectedBytes
// does not — derivedRemainingBytes reads five FileProgress fields in total,
// and a fixture missing any one of them would let a construction path that
// dropped that field's copy pass unnoticed). A job that stayed fully fresh,
// or that never exercised one of the four, would let the two paths agree by
// accident; this fixture does not let that happen.
//
// The non-resident side is built exactly the way Load reconstructs a job
// that restarts beyond maxActive: newJobProgressSized fed directly from
// Store.ArticleCountsByJob, with no hand-adjustment of any field afterward.
// A test that poked BytesDownloaded/FailedBytes itself would prove nothing
// about production — this reads back only what the store actually persisted.
//
// It now takes TWO persistence paths to reproduce the resident figures, and
// that is the point of the fixture rather than an accident of it: the failed
// bytes come from job_files.failed_bytes, written beside the articles_done
// bits they sum, and the downloaded bytes from file_extents.bytes_durable,
// written only by the barrier after its fsync. Either one missing shows up
// here as a divergence.
func TestRemainingBytes_IdenticalResidentAndNonResident(t *testing.T) {
	store, dir, db := setupResidencyTestStoreWithDB(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "residency-parity", 4, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}

	// File 0: partially downloaded so the figure is not simply the total.
	lo0, _ := m.FileRange(0)
	job.progress.markDone(m, lo0)
	// File 1: one article permanently failed.
	lo1, _ := m.FileRange(1)
	job.progress.markFailed(m, lo1)
	// File 2: deferred, untouched otherwise.
	job.progress.files[2].Fetch = FetchIfNeeded
	// File 3: only partially downloaded, then marked complete directly (the
	// production path MarkFileComplete allows — assembly can finish, e.g. via
	// repair, without every article having gone through the download path).
	// Marking it complete while bytes are still outstanding is deliberate: if
	// the two sides disagreed about Complete, file 3's leftover bytes would
	// show up as nonzero remaining on whichever side dropped the flag. A file
	// that was fully downloaded before being marked complete would already
	// read zero remaining bytes either way, and the Complete copy could be
	// silently dropped without this test noticing.
	lo3, _ := m.FileRange(3)
	job.progress.markDone(m, lo3)
	if err := q.MarkFileComplete(job.ID, 3); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}

	if err := store.Update(t.Context(), job); err != nil {
		t.Fatalf("Update: %v", err)
	}
	commitBarrierExtents(t, db, job)

	resident := job.Progress()

	// Guard the fixture: if any of these four effects is missing, the
	// equivalence checked below would pass vacuously.
	if resident.FileBytesDownloaded(0) == 0 {
		t.Fatal("fixture produced no downloaded bytes; the test would pass vacuously")
	}
	if resident.FailedBytes() == 0 {
		t.Fatal("fixture produced no failed bytes; the test would pass vacuously")
	}
	if resident.FileFetchPolicy(2) != FetchIfNeeded {
		t.Fatal("fixture not exercising a deferred file")
	}
	if !resident.FileComplete(3) {
		t.Fatal("fixture not exercising a complete file")
	}

	residentRemaining := resident.RemainingBytes()
	residentExpected := resident.ExpectedBytes()
	residentFailed := resident.FailedBytes()

	metas, err := store.ArticleCountsByJob(t.Context())
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	nonResident := newJobProgressSized(metas[job.ID])

	if got, want := nonResident.RemainingBytes(), residentRemaining; got != want {
		t.Errorf("non-resident RemainingBytes = %d, resident = %d", got, want)
	}
	if got, want := nonResident.ExpectedBytes(), residentExpected; got != want {
		t.Errorf("non-resident ExpectedBytes = %d, resident = %d", got, want)
	}
	if got, want := nonResident.FailedBytes(), residentFailed; got != want {
		t.Errorf("non-resident FailedBytes = %d, resident = %d", got, want)
	}
}

// TestFailedBytes_SurvivesRestartNonResident is the end-to-end half of the
// same property: a real restart, through Load, of a job that stays
// non-resident afterwards.
//
// It is the test that could not pass while the failed-byte cache lived in
// file_extents.bytes_failed, because nothing ever wrote that column — the
// barrier commits only what its fsync made true, and a permanently failed
// article never decodes, so it has no Class A record for the barrier to
// derive one from. The figure now caches in job_files.failed_bytes, beside
// the articles_done bits it sums and written by the same statement.
func TestFailedBytes_SurvivesRestartNonResident(t *testing.T) {
	store, dir, db := setupResidencyTestStoreWithDB(t)
	job := makeMultiFileJob(t, "failed-bytes-residency", 2, 2)
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	job.Progress().markFailed(m, 0)
	job.Progress().markDone(m, 1)
	wantFailed := job.Progress().FailedBytes()
	wantRemaining := job.Progress().RemainingBytes()
	wantDownloaded := job.Progress().FileBytesDownloaded(0)
	if wantFailed == 0 {
		t.Fatal("fixture produced no failed bytes; the test would pass vacuously")
	}
	if wantDownloaded == 0 {
		t.Fatal("fixture produced no downloaded bytes; RemainingBytes would not discriminate")
	}
	if err := store.Add(context.Background(), job); err != nil {
		t.Fatalf("add: %v", err)
	}
	commitBarrierExtents(t, db, job)

	reloaded, err := Load(dir, WithStore(store))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reloaded.byID[job.ID].Progress()
	if reloaded.byID[job.ID].manifest != nil {
		t.Fatal("fixture not exercising the non-resident path: the job came back resident")
	}
	if got.FailedBytes() != wantFailed {
		t.Errorf("FailedBytes across restart: got %d, want %d", got.FailedBytes(), wantFailed)
	}
	if got.RemainingBytes() != wantRemaining {
		t.Errorf("RemainingBytes across restart: got %d, want %d", got.RemainingBytes(), wantRemaining)
	}
}
