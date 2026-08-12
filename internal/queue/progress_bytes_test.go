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
	job.progress.files[2].Fetch = FetchIfNeeded

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
	if got, want := sized.files[1].Complete, false; got != want {
		t.Errorf("file 1 Complete = %v, want %v", got, want)
	}
	if got, want := sized.files[2].Fetch, FetchIfNeeded; got != want {
		t.Errorf("file 2 Fetch = %v, want %v", got, want)
	}
	// sized.RemainingBytes() runs the same derivation as the resident job's
	// RemainingBytes(): sum(Bytes-BytesDownloaded-FailedBytes) over files
	// that are neither Complete nor Deferred. File 0 (complete) and file 2
	// (deferred) contribute nothing, so only file 1 does — and it
	// contributes its whole size, not the half still outstanding, because a
	// non-resident job no longer carries per-file downloaded bytes. Restoring
	// them from the durable extents is the later half of this work; until
	// then a non-resident job over-reports what is left, which is the
	// direction that cannot cause a job to finish early.
	if got, want := sized.RemainingBytes(), int64(200_000); got != want {
		t.Errorf("sized.RemainingBytes() = %d, want %d (file 1's whole size)", got, want)
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
	// Per-file failed bytes must sum to the job-level counter, since
	// derivedRemainingBytes derives remaining from the per-file side and
	// the two would silently disagree otherwise.
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

// TestFailedBytes_NotDoubledByHydration pins a hydration defect a review
// caught: newJobProgressSized used to seed job-level failedBytes from
// job_files while a job was non-resident, and RestoreJobProgress then replays
// per-article state through markFailed on top of that seed rather than onto a
// fresh JobProgress, so the two stacked.
//
// job_files.failed_bytes is back, so the seed is back, and this is the test
// that says the defect did not come back with it. What makes the restored seed
// safe is that RestoreJobProgress ends in JobProgress.recompute, which ASSIGNS
// BytesDownloaded, FailedBytes and the job-level failedBytes from the manifest
// and the article bitmaps rather than adding to them — so the replay
// supersedes the seed wholesale (S4) instead of stacking on it. Both halves
// are asserted below: the seed must be visible while non-resident, and the
// total must land on the fixture's figure exactly once after hydration.
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
	// Seeded from job_files.failed_bytes: residency parity requires the
	// non-resident figure to match, not to read zero.
	if got := reloaded.byID[job.ID].Progress().FailedBytes(); got != want {
		t.Fatalf("non-resident FailedBytes = %d, want %d (seeded from job_files.failed_bytes)", got, want)
	}

	// Hydrating replays per-article state. The total must land on the
	// fixture's figure exactly once — neither doubled nor left at the
	// unseeded zero.
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
	// file is complete or deferred yet. It no longer carries downloaded or
	// failed byte counts at all; those left job_files with #306/#337.
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
		if fm.Complete {
			t.Errorf("fileMetaFromManifest[%d].Complete = true, want false", fi)
		}
		if fm.Fetch != FetchAlways {
			t.Errorf("fileMetaFromManifest[%d].Fetch = %v, want FetchAlways", fi, fm.Fetch)
		}
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
	p.files[3].Fetch = FetchIfNeeded

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
	p.files[1].Fetch = FetchIfNeeded

	expected, remaining := p.ExpectedBytes(), p.RemainingBytes()
	if expected != remaining {
		t.Errorf("nothing downloaded but expected (%d) != remaining (%d); a queue row would show non-zero progress", expected, remaining)
	}

	// Un-deferring must move both together, with no fixup call.
	p.files[1].Fetch = FetchAlways
	if got, want := p.ExpectedBytes(), int64(11_000); got != want {
		t.Errorf("ExpectedBytes() after un-defer = %d, want %d", got, want)
	}
	if p.ExpectedBytes() != p.RemainingBytes() {
		t.Errorf("after un-defer, expected (%d) != remaining (%d)", p.ExpectedBytes(), p.RemainingBytes())
	}
}

func TestDerivedRemaining_ExcludesDeferredFiles(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 3000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	p := newJobProgress(m)
	p.files[1].Fetch = FetchIfNeeded

	if got, want := p.derivedRemainingBytes(), int64(3000); got != want {
		t.Errorf("deferred volume counted: got %d, want %d", got, want)
	}

	p.files[1].Fetch = FetchAlways
	if got, want := p.derivedRemainingBytes(), int64(3800); got != want {
		t.Errorf("un-deferred volume not counted: got %d, want %d", got, want)
	}
}

// TestSizeFigures_SharedAndDivergentPredicates pins the relationship the two
// figures have to keep, which is the reason they are computed in one walk:
// both skip Deferred files, and only remaining also skips Complete ones.
//
// Asserting the two accessors separately would not catch a drift between
// them — each would still look self-consistent. The interesting assertions
// here are the per-file contributions, since that is where a predicate moving
// on one figure but not the other shows up.
func TestSizeFigures_SharedAndDivergentPredicates(t *testing.T) {
	m := newManifest([]JobFile{
		// Partly downloaded: contributes to both.
		{Subject: "partial.rar", Bytes: 1000, Articles: []JobArticle{{ID: "p1", Bytes: 500}, {ID: "p2", Bytes: 500}}},
		// Complete: contributes its whole size to expected, nothing to
		// remaining. This is the divergent half of the predicate.
		{Subject: "complete.rar", Bytes: 1000, Articles: []JobArticle{{ID: "c1", Bytes: 1000}}},
		// Deferred: contributes to neither. This is the shared half.
		{Subject: "x.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
		// Wholly failed: counted as expected, but nothing is left to fetch.
		{Subject: "failed.rar", Bytes: 600, Articles: []JobArticle{{ID: "f1", Bytes: 600}}},
	})
	p := newJobProgress(m)

	p.markDone(m, 0) // partial.rar: half of it
	p.markDone(m, 2) // complete.rar: all of it
	p.files[1].Complete = true
	p.files[2].Fetch = FetchIfNeeded
	p.markFailed(m, 4) // failed.rar

	expected, remaining := p.sizeFigures()

	// 1000 + 1000 + 600; the deferred 800 is excluded from both.
	if want := int64(2600); expected != want {
		t.Errorf("expected = %d, want %d (deferred volume must be excluded)", expected, want)
	}
	// Only partial.rar has anything outstanding: 1000 - 500.
	if want := int64(500); remaining != want {
		t.Errorf("remaining = %d, want %d", remaining, want)
	}
	// The complete file is what separates the two figures here: drop it from
	// expected and expected would read 1600, while remaining is unmoved. It
	// does NOT separate them in the other direction — its single article is
	// downloaded, so its residual is already zero and the Complete skip in
	// the remaining half is inert against this fixture. That half is pinned
	// by TestSizeFigures_RemainingSkipsCompleteFileWithUnresolvedArticles,
	// which needs a state this one cannot express.
	if expected-remaining != 2100 {
		t.Errorf("expected-remaining = %d, want 2100", expected-remaining)
	}

	// The exported accessors must be these same two figures, or a caller
	// pairing them is back to reading figures from two different walks.
	if got := p.ExpectedBytes(); got != expected {
		t.Errorf("ExpectedBytes() = %d, want %d", got, expected)
	}
	if got := p.RemainingBytes(); got != remaining {
		t.Errorf("RemainingBytes() = %d, want %d", got, remaining)
	}

	// And the identity every consumer depends on still closes: bytes actually
	// fetched are partial.rar's 500 plus complete.rar's 1000.
	if downloaded := expected - p.FailedBytes() - remaining; downloaded != 1500 {
		t.Errorf("downloaded identity = %d, want 1500", downloaded)
	}
}

// TestSizeFigures_RemainingSkipsCompleteFileWithUnresolvedArticles pins the
// half of the divergent predicate that arithmetic alone does not reach.
//
// For a complete file, `Bytes - BytesDownloaded - FailedBytes` is normally
// already zero — every article is resolved, and the parser normalizes
// JobFile.Bytes to the exact sum of its articles' bytes. Against such a file
// the Complete skip changes nothing, so a fixture built from one cannot tell
// whether the skip is there.
//
// The skip earns its place in the state below: Complete set while an article
// is still unresolved. That is not a state internal/queue prevents — it is
// prevented one package over, by the assembler flushing its pending Done and
// Failed acks *before* firing OnFileComplete, precisely so nothing observes a
// complete file whose articles have not been marked yet (see
// internal/assembler/assembler.go, handleCompletedFile). A guard whose
// precondition is held by an ordering barrier in another package is exactly
// the kind that needs a test here: the barrier can be refactored by someone
// who never reads this file.
//
// Without the skip, remaining would report 500 bytes still to fetch for a
// file the job has already finished with, and the queue would never reach
// zero remaining.
func TestSizeFigures_RemainingSkipsCompleteFileWithUnresolvedArticles(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "done.rar", Bytes: 1000, Articles: []JobArticle{{ID: "d1", Bytes: 500}, {ID: "d2", Bytes: 500}}},
	})
	p := newJobProgress(m)

	// One article resolved, one not, and the file marked complete anyway.
	p.markDone(m, 0)
	p.files[0].Complete = true

	expected, remaining := p.sizeFigures()
	if want := int64(1000); expected != want {
		t.Errorf("expected = %d, want %d (a complete file is still part of what the job set out to fetch)", expected, want)
	}
	if want := int64(0); remaining != want {
		t.Errorf("remaining = %d, want %d (a complete file has nothing left to fetch, whatever its articles say)", remaining, want)
	}
}
