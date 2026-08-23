package queue

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// This file's fixture writes the durable runs a durability.Barrier would have
// recorded for a job's currently-done articles — see commitBarrierRuns below.
//
// It stands in for the application's barrier cadence rather than starting one,
// and it goes through the real durability.SQLiteRunStore rather than writing
// rows by hand — so what the non-resident read path reads back is what
// production will actually have written, not a shape invented by the test.
//
// A run covers an article that is done and not failed, which is exactly what
// the barrier records: it hands Commit the articles a completed fsync covered,
// and a failed article never reaches a Drain.
//
// Lengths are in DECODED bytes, via decodedBytesOf, because that is the unit
// the barrier records: WrittenArticle.Length is the payload written to disk.
// Manifest.ArticleBytes is the NZB `bytes` attribute, which counts the ENCODED
// article and runs a few percent higher. Using the encoded figure here would
// make this fixture agree with the resident path by construction and hide any
// consumer that confuses the two — which is exactly what it did until #365.
//
// decodedBytesOf models the yEnc overhead that separates an article's encoded
// size from the payload the assembler writes: escapes and line endings put the
// encoded form roughly 2% above the decoded one. The exact ratio does not
// matter — only that it is not 1, so a figure carried in the wrong unit cannot
// pass for the right one.
func decodedBytesOf(encoded int) int64 {
	return int64(encoded) * 100 / 102
}

// commitBarrierRuns records what a barrier would have recorded for the job's
// currently-done articles, so a non-resident reload derives the same
// per-article state a resident job holds.
//
// Offsets are synthesised from the articles' decoded sizes in index order,
// which is what makes contiguous articles merge the way real ones do.
func commitBarrierRuns(t *testing.T, db *sql.DB, job *Job) {
	t.Helper()
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("commitBarrierRuns: manifest: %v", err)
	}
	p := job.Progress()
	var arts []durability.DurableArticle
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		var off int64
		for i := lo; i < hi; i++ {
			n := decodedBytesOf(m.ArticleBytes(i))
			if p.ArticleDone(i) && !p.ArticleFailed(i) {
				arts = append(arts, durability.DurableArticle{
					FileIdx: int32(fi), //nolint:gosec // G115: file counts are far below int32
					ArtIdx:  int32(i),  //nolint:gosec // G115: article counts are far below int32
					Offset:  off,
					Length:  int32(n), //nolint:gosec // G115: a decoded article is far below int32 bytes
				})
			}
			off += n
		}
	}
	if err := durability.NewSQLiteRunStore(db).Commit(context.Background(), job.ID, arts); err != nil {
		t.Fatalf("commitBarrierRuns: commit: %v", err)
	}
}

// recordDurability persists the WHOLE of a job's per-article state the way
// production does: the durable runs a barrier would have recorded for its
// successful articles, and a failed_articles row for each permanently failed
// one.
//
// Every fixture that mutates progress in memory and then expects a reload to
// see it needs this, and that is a real consequence of the durable-runs
// design rather than test scaffolding. Article resolution is no longer a
// column the queue re-serialises on Update — it is DERIVED from two records
// with different owners, and only one of them (failed_articles) belongs to the
// queue at all. A fixture that calls Update alone persists no resolution,
// which is exactly what production does when no barrier has run.
//
// It reaches through the queue's own store for the failed half rather than
// writing rows by hand, so the fixture cannot drift from what
// AckPermanentFailure actually writes.
func recordDurability(t *testing.T, store *SQLiteStore, job *Job) {
	t.Helper()
	commitBarrierRuns(t, store.db, job)

	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("recordDurability: manifest: %v", err)
	}
	p := job.Progress()
	var failed []int32
	for i := range m.NumArticles() {
		if p.ArticleFailed(i) {
			failed = append(failed, int32(i)) //nolint:gosec // G115: article counts are far below int32
		}
	}
	if err := store.RecordFailedArticles(context.Background(), job.ID, failed); err != nil {
		t.Fatalf("recordDurability: failed articles: %v", err)
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
// Both byte figures come from job_files, each caching a sum over the article
// resolution the durable runs carry, and the fixture records those runs in
// DECODED bytes — the unit the barrier actually records. That is the point of
// the fixture rather than an accident of it: a consumer that reached into the
// record for the downloaded figure would find a number in the wrong unit, and
// diverge here. It read exactly that way until #365, and this test could not
// see it while the fixture seeded encoded bytes into a decoded column.
func TestRemainingBytes_IdenticalResidentAndNonResident(t *testing.T) {
	store, dir, _ := setupResidencyTestStoreWithDB(t)
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
	recordDurability(t, store, job)

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

	// The ARTICLE counts, on the same terms as the bytes above.
	//
	// These lagged the byte figures: caching bytes_downloaded and failed_bytes
	// made the byte side agree at both residencies while the article side
	// still reported every article as still to fetch, so the two disagreed in
	// kind. The queue listing reads PendingArticles as ArticlesRemaining
	// without hydrating, so after a restart a half-downloaded Queued or Paused
	// job showed its full article count until it was promoted.
	if got, want := nonResident.PendingArticles(), resident.PendingArticles(); got != want {
		t.Errorf("non-resident PendingArticles = %d, resident = %d — the queue listing "+
			"serves this as articles_remaining without hydrating, so the figure jumps "+
			"the moment the job is promoted", got, want)
	}
	if got, want := nonResident.ArticlesResolved(), resident.ArticlesResolved(); got != want {
		t.Errorf("non-resident ArticlesResolved = %d, resident = %d", got, want)
	}
	if got, want := nonResident.ArticlesFailed(), resident.ArticlesFailed(); got != want {
		t.Errorf("non-resident ArticlesFailed = %d, resident = %d", got, want)
	}
	// Per file as well as per job, because the job total can agree while the
	// bits sit against the wrong files: the global index each file's bits are
	// placed at is derived here from a running sum rather than read from a
	// manifest, and an off-by-one file would cancel out in the total.
	for fi := range 4 {
		if got, want := nonResident.FilePending(fi), resident.FilePending(fi); got != want {
			t.Errorf("file %d: non-resident Pending = %d, resident = %d — the file's "+
				"articles were placed at the wrong global index", fi, got, want)
		}
	}
	// And the bits themselves, which is what makes the placement assertion
	// above more than an arithmetic coincidence.
	for i := range m.NumArticles() {
		if got, want := nonResident.ArticleDone(i), resident.ArticleDone(i); got != want {
			t.Errorf("article %d: non-resident done = %v, resident = %v", i, got, want)
		}
		if got, want := nonResident.ArticleFailed(i), resident.ArticleFailed(i); got != want {
			t.Errorf("article %d: non-resident failed = %v, resident = %v", i, got, want)
		}
	}
}

// TestFailedBytes_SurvivesRestartNonResident is the end-to-end half of the
// same property: a real restart, through Load, of a job that stays
// non-resident afterwards.
//
// It is the test that could not pass while the failed-byte cache lived on the
// durability record, because nothing ever wrote that column — the barrier
// commits only what its fsync made true, and a permanently failed article
// never decodes, so it is never written and no durable run covers it. The
// figure caches in job_files.failed_bytes instead, written by the one
// statement that updates the file's row.
func TestFailedBytes_SurvivesRestartNonResident(t *testing.T) {
	store, dir, _ := setupResidencyTestStoreWithDB(t)
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
	recordDurability(t, store, job)

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
