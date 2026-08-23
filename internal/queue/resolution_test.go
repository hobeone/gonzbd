package queue

import (
	"context"
	"testing"
)

// TestRunRangesForJob_ReadsOnlyTheNamedJobsSpans pins the narrow read the
// resident restore path makes.
//
// Only the two index columns, because article resolution needs nothing else —
// offset, length and crc32 answer the truncate bound, the overlap check and
// the whole-file CRC, and none of those questions is asked here. Selecting
// them anyway would make the boot read wider for no consumer.
func TestRunRangesForJob_ReadsOnlyTheNamedJobsSpans(t *testing.T) {
	store, _, _ := setupResidencyTestStoreWithDB(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO durable_runs (job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32)
		VALUES ('job-a',0,0,3,0,400,1), ('job-a',1,4,4,0,100,2), ('job-b',0,0,9,0,1000,3)`,
	); err != nil {
		t.Fatal(err)
	}

	got, err := store.runRangesForJob(ctx, "job-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d spans for job-a, want 2 (%+v)", len(got), got)
	}
	// Ordered by the primary key, so (file 0, offset 0) precedes (file 1).
	if got[0] != (artRange{First: 0, Last: 3}) || got[1] != (artRange{First: 4, Last: 4}) {
		t.Errorf("spans = %+v, want [{0 3} {4 4}]", got)
	}
	other, err := store.runRangesForJob(ctx, "job-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].Last != 9 {
		t.Errorf("job-b's spans = %+v, want one covering [0,9] — the read must be scoped "+
			"to the job, or every job derives every other job's articles as done", other)
	}
}

// TestApplyResolution_MarksFailedBeforeDone pins the ORDER, which is the only
// decision this helper makes and the one #300 turned on.
//
// markFailed early-returns once the article is already done, so marking done
// first would restore a permanently failed article as a successful one.
// JobProgress.recompute then derives failedBytes from the bits, and the job
// comes back reporting full health while a retry finds nothing to refetch.
//
// It is also bounded to the file's own range: a helper that walked the whole
// job would mark another file's articles from this file's row.
func TestApplyResolution_MarksFailedBeforeDone(t *testing.T) {
	const perFile = 2
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "apply-resolution", 2, perFile)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Article 0 done, article 1 done AND failed, articles 2-3 (file 1)
	// resolved too — but only file 0's range is applied.
	done := []bool{true, true, true, true}
	failed := []bool{false, true, false, false}
	applyResolution(job, done, failed, 0, perFile)

	p := job.Progress()
	if !p.ArticleDone(0) || p.ArticleFailed(0) {
		t.Errorf("article 0: done=%v failed=%v, want done and not failed",
			p.ArticleDone(0), p.ArticleFailed(0))
	}
	if !p.ArticleDone(1) || !p.ArticleFailed(1) {
		t.Errorf("article 1: done=%v failed=%v, want both — marking done first makes "+
			"markFailed early-return and a failed article comes back successful (#300)",
			p.ArticleDone(1), p.ArticleFailed(1))
	}
	for i := perFile; i < 2*perFile; i++ {
		if p.ArticleDone(i) {
			t.Errorf("article %d of file 1 was resolved by file 0's range", i)
		}
	}

	// A resolution shorter than the range is not an index panic: it reads as
	// "nothing recorded for the tail", the safe direction under S3.
	applyResolution(job, []bool{true}, nil, perFile, 2*perFile)
	if p.ArticleDone(perFile + 1) {
		t.Error("an article past the end of the resolution slice was marked done")
	}
}

// TestFillResolution_DerivesFileLocalFlagsWithoutAManifest pins the boot
// path's whole trick: it converts GLOBAL article indices into the FILE-LOCAL
// flags FileMeta carries, using nothing but the per-file article counts it
// already holds.
//
// The manifest derives the same offsets by accumulating counts in file order,
// so the running sum reproduces it — and a boot path that skipped the
// conversion would place every file's flags at the job's first article.
func TestFillResolution_DerivesFileLocalFlagsWithoutAManifest(t *testing.T) {
	store, _, _ := setupResidencyTestStoreWithDB(t)
	ctx := context.Background()
	// File 0 owns global articles 0-1, file 1 owns 2-4. One run covers
	// article 3 (file 1's ordinal 1); article 4 failed permanently.
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO durable_runs (job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32)
		VALUES ('job-a',1,3,3,0,100,1)`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFailedArticles(ctx, "job-a", []int32{4}); err != nil {
		t.Fatal(err)
	}
	// A row for a job the caller is not asking about, so the sweep's scoping
	// is exercised: it reads both tables whole rather than per job.
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO durable_runs (job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32)
		VALUES ('job-gone',0,0,0,0,100,1)`); err != nil {
		t.Fatal(err)
	}

	result := map[string][]FileMeta{
		"job-a": {{ArticleCount: 2}, {ArticleCount: 3}},
	}
	if err := store.fillResolution(ctx, result); err != nil {
		t.Fatal(err)
	}

	f0, f1 := result["job-a"][0], result["job-a"][1]
	if len(f0.Done) != 2 || f0.Done[0] || f0.Done[1] {
		t.Errorf("file 0 done = %v, want both false — its articles are 0-1 and nothing "+
			"covers them", f0.Done)
	}
	if len(f1.Done) != 3 {
		t.Fatalf("file 1 done = %v, want three flags", f1.Done)
	}
	if f1.Done[0] {
		t.Error("file 1's ordinal 0 (global article 2) was marked done; nothing covers it")
	}
	if !f1.Done[1] {
		t.Error("file 1's ordinal 1 (global article 3) is not done, although a run covers " +
			"it — the global-to-file-local conversion placed the flag somewhere else")
	}
	if !f1.Done[2] || !f1.Failed[2] {
		t.Errorf("file 1's ordinal 2 (global article 4) = done %v failed %v, want both — "+
			"failed implies done, and both consumers read Failed only inside the Done "+
			"branch", f1.Done[2], f1.Failed[2])
	}

	// An empty result must not run the sweeps at all: Load calls this for
	// every restart, including one with no jobs.
	if err := store.fillResolution(ctx, map[string][]FileMeta{}); err != nil {
		t.Errorf("an empty result reported an error: %v", err)
	}
	// A job whose files hold no articles has nothing to derive and must not
	// index into a zero-length slice.
	empty := map[string][]FileMeta{"job-a": {{ArticleCount: 0}}}
	if err := store.fillResolution(ctx, empty); err != nil {
		t.Errorf("a zero-article job reported an error: %v", err)
	}
	if empty["job-a"][0].Done != nil {
		t.Errorf("a zero-article file got flags: %v", empty["job-a"][0].Done)
	}
}
