package queue

import (
	"context"
	"testing"
)

func TestNewJobProgress_CarriesPerFileBytes(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "payload.rar", Bytes: 5000, Articles: []JobArticle{{ID: "a1", Bytes: 2500}, {ID: "a2", Bytes: 2500}}},
		{Subject: "payload.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "b1", Bytes: 800}}},
	})
	p := newJobProgress(m)

	if got, want := p.files[0].Bytes, int64(5000); got != want {
		t.Errorf("files[0].Bytes = %d, want %d", got, want)
	}
	if got, want := p.files[1].Bytes, int64(800); got != want {
		t.Errorf("files[1].Bytes = %d, want %d", got, want)
	}
}

// TestNewJobProgressSized_RoundTripsPerFileState builds a job, downloads
// part of it (one file fully complete via MarkFileComplete, one file
// partially downloaded, one file deferred), persists it, then reconstructs
// progress the non-resident way via ArticleCountsByJob and
// newJobProgressSized. It asserts the reconstruction's per-file
// BytesDownloaded/Complete/Deferred match what MarkArticlesDone/
// MarkFileComplete/the direct Deferred write actually stored, and that
// TotalRemainingBytes still agrees with the residency-parity guarantee
// TestTotalRemainingBytes_RestartReconstructsNonResident pins.
func TestNewJobProgressSized_RoundTripsPerFileState(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "sized-roundtrip", 3, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// File 0: fully downloaded and marked complete.
	if err := q.MarkArticlesDone(job.ID, []string{articleID(0, 0), articleID(0, 1)}); err != nil {
		t.Fatalf("MarkArticlesDone file 0: %v", err)
	}
	if err := q.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete file 0: %v", err)
	}

	// File 1: partially downloaded, not complete.
	if err := q.MarkArticlesDone(job.ID, []string{articleID(1, 0)}); err != nil {
		t.Fatalf("MarkArticlesDone file 1: %v", err)
	}

	// File 2: deferred, untouched otherwise.
	job.progress.files[2].Deferred = true

	if err := store.Update(t.Context(), job); err != nil {
		t.Fatalf("Update: %v", err)
	}

	metas, err := store.ArticleCountsByJob(t.Context())
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	sized := newJobProgressSized(metas[job.ID])

	if got, want := sized.files[0].Complete, true; got != want {
		t.Errorf("file 0 Complete = %v, want %v", got, want)
	}
	if got, want := sized.files[0].BytesDownloaded, job.progress.files[0].BytesDownloaded; got != want {
		t.Errorf("file 0 BytesDownloaded = %d, want %d", got, want)
	}
	if got, want := sized.files[1].Complete, false; got != want {
		t.Errorf("file 1 Complete = %v, want %v", got, want)
	}
	if got, want := sized.files[1].BytesDownloaded, job.progress.files[1].BytesDownloaded; got != want {
		t.Errorf("file 1 BytesDownloaded = %d, want %d", got, want)
	}
	if got, want := sized.files[2].Deferred, true; got != want {
		t.Errorf("file 2 Deferred = %v, want %v", got, want)
	}
	if got, want := sized.files[2].BytesDownloaded, int64(0); got != want {
		t.Errorf("file 2 BytesDownloaded = %d, want %d", got, want)
	}

	// sized.RemainingBytes() reads the still-maintained field, seeded in
	// newJobProgressSized as sum(Bytes-BytesDownloaded) over files that are
	// neither Complete nor Deferred — file 0 (complete) and file 2
	// (deferred) contribute nothing, file 1 contributes its undownloaded
	// half. This is deliberately not compared against the resident job's
	// own RemainingBytes(): the maintained counter on job.progress does not
	// yet exclude deferred files (that exclusion is what Task 2/3 of the
	// plan add), so the two are expected to disagree whenever a file is
	// deferred. The residency-parity property for the no-deferral case is
	// pinned separately by TestRemainingBytes_IdenticalResidentAndNonResident
	// once the derivation lands (plan Task 4).
	if got, want := sized.RemainingBytes(), int64(100_000); got != want {
		t.Errorf("sized.RemainingBytes() = %d, want %d (file1's undownloaded half only)", got, want)
	}
}

// TestNewJobProgressSized_ClampsRemainingPerFileNotPerJob pins that an
// over-downloaded file's clamp is applied per file, not netted against the
// rest of the job's files.
//
// The deleted Store.RemainingBytesByJob computed SUM(bytes-bytes_downloaded)
// per job and clamped the job-wide total at 0 — TestSQLiteStore_RemainingBytesByJob
// (now also deleted) pinned exactly that for a single over-downloaded file.
// newJobProgressSized clamps per file instead: a file whose
// bytes_downloaded exceeds its bytes contributes zero, independent of any
// other file's shortfall. This is a deliberate semantics change, not an
// accidental coverage drop — it is exactly what the FileProgress-derived
// RemainingBytes (plan Task 2/3) computes for the same state, and aligning
// the seed with it now is the point of this refactor. For a job with one
// file over-downloaded by 50 and another under-downloaded by 30, the old
// job-total clamp would have reported 0 (50-30=20 short of the total, but
// the negative from file 0 ate into file 1's real remainder); the per-file
// clamp reports 30, because file 0 independently contributes 0 rather than
// -50.
func TestNewJobProgressSized_ClampsRemainingPerFileNotPerJob(t *testing.T) {
	store, dir, db := setupResidencyTestStoreWithDB(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "clamp-per-file", 2, 1)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// file 0 (100_000 bytes): over-downloaded by 50 — must contribute 0,
	// never a negative "remaining".
	if _, err := db.ExecContext(t.Context(),
		"UPDATE job_files SET bytes_downloaded = ? WHERE job_id = ? AND file_index = 0",
		100_050, job.ID); err != nil {
		t.Fatalf("UPDATE file 0 bytes_downloaded: %v", err)
	}
	// file 1 (100_000 bytes): genuinely under-downloaded by 30.
	if _, err := db.ExecContext(t.Context(),
		"UPDATE job_files SET bytes_downloaded = ? WHERE job_id = ? AND file_index = 1",
		99_970, job.ID); err != nil {
		t.Fatalf("UPDATE file 1 bytes_downloaded: %v", err)
	}

	metas, err := store.ArticleCountsByJob(t.Context())
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	sized := newJobProgressSized(metas[job.ID])

	if got, want := sized.RemainingBytes(), int64(30); got != want {
		t.Errorf("RemainingBytes() = %d, want %d (file 0's over-download must clamp to 0 per file, "+
			"not net against file 1's real 30-byte shortfall)", got, want)
	}
}

func TestMarkFailed_AccumulatesPerFileFailedBytes(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 1500}, {ID: "a2", Bytes: 1500}}},
		{Subject: "b.rar", Bytes: 2000, Articles: []JobArticle{{ID: "b1", Bytes: 2000}}},
	})
	p := newJobProgress(m)

	p.markDone(m, 0)
	p.markFailed(m, 1)
	p.markFailed(m, 2)

	if got, want := p.files[0].FailedBytes, int64(1500); got != want {
		t.Errorf("file 0 FailedBytes = %d, want %d", got, want)
	}
	if got, want := p.files[1].FailedBytes, int64(2000); got != want {
		t.Errorf("file 1 FailedBytes = %d, want %d", got, want)
	}
	if got, want := p.files[0].BytesDownloaded, int64(1500); got != want {
		t.Errorf("failed bytes leaked into BytesDownloaded: got %d, want %d", got, want)
	}
	// Per-file failed bytes must sum to the job-level counter, or the two
	// disagree the moment Task 3 derives remaining from the per-file side.
	if got, want := p.files[0].FailedBytes+p.files[1].FailedBytes, p.failedBytes; got != want {
		t.Errorf("per-file sum = %d, job-level failedBytes = %d", got, want)
	}
}

// TestFileFailedBytes_GuardBranches exercises FileFailedBytes's bounds
// guard directly through the exported accessor — TestMarkFailed_
// AccumulatesPerFileFailedBytes above only ever reads p.files[fi].
// FailedBytes, the unexported field, so the accessor's out-of-range branch
// (shared shape with FileBytesDownloaded's guard) had never actually run.
// The nil-receiver branch is covered separately by
// TestJobProgress_ExportedReadersAreNilSafe.
func TestFileFailedBytes_GuardBranches(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1500, Articles: []JobArticle{{ID: "a1", Bytes: 1500}}},
	})
	p := newJobProgress(m)
	p.markFailed(m, 0)

	for _, fi := range []int{-1, len(p.files), len(p.files) + 10} {
		if got := p.FileFailedBytes(fi); got != 0 {
			t.Errorf("FileFailedBytes(%d) = %d, want 0 out of range", fi, got)
		}
	}
	if got, want := p.FileFailedBytes(0), int64(1500); got != want {
		t.Errorf("FileFailedBytes(0) = %d, want %d", got, want)
	}
}

func TestResetForReload_ReturnsFailedBytesToTheFile(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 3000}}},
	})
	p := newJobProgress(m)

	p.markFailed(m, 0)
	if got, want := p.files[0].FailedBytes, int64(3000); got != want {
		t.Fatalf("FailedBytes after markFailed = %d, want %d", got, want)
	}

	p.resetForReload(m, 0)
	if got, want := p.files[0].FailedBytes, int64(0); got != want {
		t.Errorf("FailedBytes after resetForReload = %d, want %d", got, want)
	}
	if got, want := p.failedBytes, int64(0); got != want {
		t.Errorf("job-level failedBytes after resetForReload = %d, want %d", got, want)
	}
}

func TestRestoreJobProgress_CarriesPerFileBytes(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "restore-bytes", 3, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	restored := &Job{ID: job.ID, manifest: m, progress: newJobProgress(m)}
	for fi := range restored.progress.files {
		restored.progress.files[fi].Bytes = 0 // prove the restore sets it, not the constructor
	}

	if err := store.RestoreJobProgress(t.Context(), restored); err != nil {
		t.Fatalf("RestoreJobProgress: %v", err)
	}
	for fi := range m.NumFiles() {
		if got, want := restored.progress.files[fi].Bytes, m.FileBytes(fi); got != want {
			t.Errorf("restored files[%d].Bytes = %d, want %d", fi, got, want)
		}
	}
}

// TestFailedBytes_NotDoubledByHydration pins the Task 2 review defect:
// newJobProgressSized seeds job-level failedBytes from job_files while a
// job is non-resident, and RestoreJobProgress then replays per-article
// state through markFailed on top of that seed rather than onto a fresh
// JobProgress, so the two stack.
//
// Driven through Queue.SnapshotJob rather than an unexported-field write:
// the reloaded job comes back non-resident (StatusQueued, per
// makeMultiFileJob's default) with progress already seeded from job_files,
// and SnapshotJob's hydrateSnapshot is the real production path that reads
// the manifest back off disk and calls Store.RestoreJobProgress against
// that same seeded progress — precisely the seed-plus-replay this test
// targets, reached through a public entry point.
func TestFailedBytes_NotDoubledByHydration(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	job := makeMultiFileJob(t, "failed-bytes-hydrate", 2, 2)
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	job.Progress().markFailed(m, 0)
	job.Progress().markDone(m, 1)
	want := job.Progress().FailedBytes()
	if want == 0 {
		t.Fatal("fixture produced no failed bytes; the test would pass vacuously")
	}
	if err := store.Add(context.Background(), job); err != nil {
		t.Fatalf("add: %v", err)
	}

	reloaded, err := Load(dir, WithStore(store))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Seeded from job_files while non-resident.
	if got := reloaded.byID[job.ID].Progress().FailedBytes(); got != want {
		t.Fatalf("non-resident FailedBytes = %d, want %d", got, want)
	}

	// Hydrating replays per-article state on top of that seed. The total
	// must not move: it is the same job, only more of it is in memory.
	snap := reloaded.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if snap.manifest == nil {
		t.Fatal("fixture not exercising the bug: SnapshotJob did not hydrate a manifest")
	}
	if got := snap.Progress().FailedBytes(); got != want {
		t.Errorf("FailedBytes after hydration = %d, want %d (seed and replay stacked)", got, want)
	}
	// The per-file values must still sum to the job-level total.
	var sum int64
	for fi := range snap.Progress().files {
		sum += snap.Progress().files[fi].FailedBytes
	}
	if sum != want {
		t.Errorf("per-file FailedBytes sum = %d, job-level = %d", sum, want)
	}
}

// TestNewJobProgress_MatchesSizedConstruction characterises newJobProgress's
// existing behaviour before it is refactored to delegate to
// newJobProgressSized via fileMetaFromManifest, so the delegation in the
// next step is provably behaviour-preserving.
func TestNewJobProgress_MatchesSizedConstruction(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 1500}, {ID: "a2", Bytes: 1500}}},
		{Subject: "b.rar", Bytes: 2000, Articles: []JobArticle{{ID: "b1", Bytes: 2000}}},
		{Subject: "c.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	p := newJobProgress(m)

	if got, want := p.done.Len(), m.NumArticles(); got != want {
		t.Errorf("done bitset sized %d, want %d", got, want)
	}
	if got, want := len(p.files), m.NumFiles(); got != want {
		t.Errorf("files sized %d, want %d", got, want)
	}
	if got, want := p.RemainingBytes(), m.TotalBytes(); got != want {
		t.Errorf("RemainingBytes() = %d, want m.TotalBytes() = %d", got, want)
	}
	for fi := range p.files {
		lo, hi := m.FileRange(fi)
		if got, want := p.files[fi].Pending, hi-lo; got != want {
			t.Errorf("file %d Pending = %d, want %d", fi, got, want)
		}
		if got, want := p.files[fi].Bytes, m.FileBytes(fi); got != want {
			t.Errorf("file %d Bytes = %d, want %d", fi, got, want)
		}
	}

	// Pin the projection itself, not only newJobProgress's downstream use of
	// it: fileMetaFromManifest must carry the right ArticleCount/Bytes per
	// file, and everything else must read zero for a fresh manifest — no
	// articles are downloaded, failed, complete, or deferred yet.
	metas := fileMetaFromManifest(m)
	if got, want := len(metas), m.NumFiles(); got != want {
		t.Fatalf("fileMetaFromManifest returned %d entries, want %d", got, want)
	}
	for fi, fm := range metas {
		lo, hi := m.FileRange(fi)
		if got, want := fm.ArticleCount, hi-lo; got != want {
			t.Errorf("fileMetaFromManifest[%d].ArticleCount = %d, want %d", fi, got, want)
		}
		if got, want := fm.Bytes, m.FileBytes(fi); got != want {
			t.Errorf("fileMetaFromManifest[%d].Bytes = %d, want %d", fi, got, want)
		}
		if fm.BytesDownloaded != 0 {
			t.Errorf("fileMetaFromManifest[%d].BytesDownloaded = %d, want 0", fi, fm.BytesDownloaded)
		}
		if fm.FailedBytes != 0 {
			t.Errorf("fileMetaFromManifest[%d].FailedBytes = %d, want 0", fi, fm.FailedBytes)
		}
		if fm.Complete {
			t.Errorf("fileMetaFromManifest[%d].Complete = true, want false", fi)
		}
		if fm.Deferred {
			t.Errorf("fileMetaFromManifest[%d].Deferred = true, want false", fi)
		}
	}
}

func TestFailedBytes_SurvivesRestartNonResident(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	job := makeMultiFileJob(t, "failed-bytes-residency", 2, 2)
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	job.Progress().markFailed(m, 0)
	job.Progress().markDone(m, 1)
	wantFailed := job.Progress().FailedBytes()
	wantRemaining := job.Progress().RemainingBytes()
	if wantFailed == 0 {
		t.Fatal("fixture produced no failed bytes; the test would pass vacuously")
	}
	if err := store.Add(context.Background(), job); err != nil {
		t.Fatalf("add: %v", err)
	}

	reloaded, err := Load(dir, WithStore(store))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reloaded.byID[job.ID].Progress()
	if got.FailedBytes() != wantFailed {
		t.Errorf("FailedBytes across restart: got %d, want %d", got.FailedBytes(), wantFailed)
	}
	if got.RemainingBytes() != wantRemaining {
		t.Errorf("RemainingBytes across restart: got %d, want %d", got.RemainingBytes(), wantRemaining)
	}
}

// TestDerivedRemaining_MatchesHandComputedValues pins RemainingBytes()
// (derivedRemainingBytes) against expected values computed by hand from the
// fixture at each stage, now that there is no maintained counter left to
// compare against. Fixture: file a.rar = 3000 bytes (a1=1500, a2=1500),
// file b.rar = 2000 bytes (b1=2000); total = 5000.
//
//   - fresh: nothing resolved. remaining = 3000 + 2000 = 5000.
//   - one article done: a1 (1500) downloaded.
//     remaining = (3000-1500) + 2000 = 1500 + 2000 = 3500.
//   - first file complete: a2 (1500) also downloaded, and a.rar's Complete
//     flag is set, excluding it entirely regardless of its own byte math
//     (which would otherwise be 3000-1500-1500=0 anyway).
//     remaining = 0 (a.rar excluded) + 2000 (b.rar untouched) = 2000.
//   - second file failed: b1 (2000) fails, adding 2000 to b.rar's
//     FailedBytes. remaining = 0 (a.rar excluded) + (2000-0-2000) = 0.
func TestDerivedRemaining_MatchesHandComputedValues(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 1500}, {ID: "a2", Bytes: 1500}}},
		{Subject: "b.rar", Bytes: 2000, Articles: []JobArticle{{ID: "b1", Bytes: 2000}}},
	})
	p := newJobProgress(m)

	check := func(stage string, want int64) {
		t.Helper()
		if got := p.RemainingBytes(); got != want {
			t.Errorf("%s: RemainingBytes() = %d, want %d", stage, got, want)
		}
	}

	check("fresh", 5000)

	p.markDone(m, 0)
	check("one article done", 3500)

	p.markDone(m, 1)
	p.files[0].Complete = true
	check("first file complete", 2000)

	p.markFailed(m, 2)
	check("second file failed", 0)
}

// TestExpectedBytes_ClosesTheDownloadedIdentity pins the property every
// consumer of these figures depends on: downloaded = expected - failed -
// remaining, for each kind of file a job can hold at once.
func TestExpectedBytes_ClosesTheDownloadedIdentity(t *testing.T) {
	m := newManifest([]JobFile{
		// Fully downloaded and complete.
		{Subject: "done.rar", Bytes: 2000, Articles: []JobArticle{{ID: "d1", Bytes: 2000}}},
		// Half downloaded, still going.
		{Subject: "partial.rar", Bytes: 2000, Articles: []JobArticle{{ID: "p1", Bytes: 1000}, {ID: "p2", Bytes: 1000}}},
		// One article permanently failed.
		{Subject: "failed.rar", Bytes: 1000, Articles: []JobArticle{{ID: "f1", Bytes: 1000}}},
		// Deferred recovery volume: never dispatched.
		{Subject: "x.vol000+01.par2", Bytes: 500, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 500}}},
	})
	p := newJobProgress(m)
	p.files[3].Deferred = true

	p.markDone(m, 0)
	p.files[0].Complete = true
	p.markDone(m, 1)
	p.markFailed(m, 3)

	// expected excludes only the deferred volume: 2000+2000+1000 = 5000.
	if got, want := p.ExpectedBytes(), int64(5000); got != want {
		t.Errorf("ExpectedBytes() = %d, want %d (deferred volume must not count)", got, want)
	}
	// remaining: done contributes 0 (Complete), partial 2000-1000 = 1000,
	// failed 1000-0-1000 = 0, deferred 0.
	if got, want := p.RemainingBytes(), int64(1000); got != want {
		t.Errorf("RemainingBytes() = %d, want %d", got, want)
	}
	if got, want := p.FailedBytes(), int64(1000); got != want {
		t.Errorf("FailedBytes() = %d, want %d", got, want)
	}
	// The identity every consumer relies on. Bytes actually fetched:
	// 2000 (done) + 1000 (partial's first article) = 3000.
	downloaded := p.ExpectedBytes() - p.FailedBytes() - p.RemainingBytes()
	if want := int64(3000); downloaded != want {
		t.Errorf("downloaded identity = %d, want %d", downloaded, want)
	}
}

// TestExpectedBytes_FreshOnDemandJobReportsZeroProgress pins the
// user-visible symptom directly: the percentage a queue row shows for a job
// whose recovery volumes are deferred and whose content is untouched.
func TestExpectedBytes_FreshOnDemandJobReportsZeroProgress(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "content.rar", Bytes: 10_000, Articles: []JobArticle{{ID: "c1", Bytes: 10_000}}},
		{Subject: "content.vol000+01.par2", Bytes: 1_000, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 1_000}}},
	})
	p := newJobProgress(m)
	p.files[1].Deferred = true

	expected, remaining := p.ExpectedBytes(), p.RemainingBytes()
	if expected != remaining {
		t.Errorf("nothing downloaded but expected (%d) != remaining (%d); a queue row would show non-zero progress", expected, remaining)
	}

	// Un-deferring must move both together, with no fixup call.
	p.files[1].Deferred = false
	if got, want := p.ExpectedBytes(), int64(11_000); got != want {
		t.Errorf("ExpectedBytes() after un-defer = %d, want %d", got, want)
	}
	if p.ExpectedBytes() != p.RemainingBytes() {
		t.Errorf("after un-defer, expected (%d) != remaining (%d)", p.ExpectedBytes(), p.RemainingBytes())
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
func TestRemainingBytes_IdenticalResidentAndNonResident(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "residency-parity", 4, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// File 0: partially downloaded so the figure is not simply the total.
	if err := q.MarkArticlesDone(job.ID, []string{articleID(0, 0)}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	// File 1: one article permanently failed.
	if _, err := q.MarkArticlesFailed(job.ID, []string{articleID(1, 0)}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	// File 2: deferred, untouched otherwise.
	job.progress.files[2].Deferred = true
	// File 3: only partially downloaded, then marked complete directly (the
	// production path MarkFileComplete allows — assembly can finish, e.g. via
	// repair, without every article having gone through the download path).
	// Marking it complete while bytes are still outstanding is deliberate: if
	// the two sides disagreed about Complete, file 3's leftover bytes would
	// show up as nonzero remaining on whichever side dropped the flag. A file
	// that was fully downloaded before being marked complete would already
	// read zero remaining bytes either way, and the Complete copy could be
	// silently dropped without this test noticing.
	if err := q.MarkArticlesDone(job.ID, []string{articleID(3, 0)}); err != nil {
		t.Fatalf("MarkArticlesDone file 3: %v", err)
	}
	if err := q.MarkFileComplete(job.ID, 3); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}

	if err := store.Update(t.Context(), job); err != nil {
		t.Fatalf("Update: %v", err)
	}

	resident := job.Progress()

	// Guard the fixture: if any of these four effects is missing, the
	// equivalence checked below would pass vacuously.
	if resident.FileBytesDownloaded(0) == 0 {
		t.Fatal("fixture produced no downloaded bytes; the test would pass vacuously")
	}
	if resident.FailedBytes() == 0 {
		t.Fatal("fixture produced no failed bytes; the test would pass vacuously")
	}
	if !resident.FileDeferred(2) {
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

func TestDerivedRemaining_ExcludesDeferredFiles(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 3000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	p := newJobProgress(m)
	p.files[1].Deferred = true

	if got, want := p.derivedRemainingBytes(), int64(3000); got != want {
		t.Errorf("deferred volume counted: got %d, want %d", got, want)
	}

	p.files[1].Deferred = false
	if got, want := p.derivedRemainingBytes(), int64(3800); got != want {
		t.Errorf("un-deferred volume not counted: got %d, want %d", got, want)
	}
}
