package queue

import (
	"context"
	"testing"
)

// TestClearFailedArticlesByIdx_RemovesOnlyTheNamedArticles pins the property
// the whole-job form cannot provide: a reversal that is a strict subset of the
// job's stored rows leaves the rest in place.
//
// The unnamed row surviving is the point. #426 lets ClearAllEmitted retain a
// failed article whose file is Complete, and if its row went with the others
// the next restart would load it as outstanding work on a file
// ForEachUnfinishedArticle skips.
func TestClearFailedArticlesByIdx_RemovesOnlyTheNamedArticles(t *testing.T) {
	store, _, _ := setupResidencyTestStoreWithDB(t)
	ctx := context.Background()

	if err := store.RecordFailedArticles(ctx, "job-a", []int32{1, 4, 9}); err != nil {
		t.Fatalf("RecordFailedArticles: %v", err)
	}
	if err := store.RecordFailedArticles(ctx, "job-b", []int32{4}); err != nil {
		t.Fatalf("RecordFailedArticles(job-b): %v", err)
	}

	if err := store.ClearFailedArticlesByIdx(ctx, "job-a", []int32{1, 9}); err != nil {
		t.Fatalf("ClearFailedArticlesByIdx: %v", err)
	}

	got, err := store.failedArticlesForJob(ctx, "job-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 4 {
		t.Errorf("job-a failed articles = %v, want [4]: only the named indices may be removed", got)
	}

	// Scoped to one job, like the wholesale form — article 4 is stored for
	// both jobs, so a delete keyed on art_idx alone would take job-b's too.
	if got, err := store.failedArticlesForJob(ctx, "job-b"); err != nil || len(got) != 1 {
		t.Errorf("job-b failed articles = %v (err %v), want [4] left alone", got, err)
	}

	// Idempotent: an index with no stored row is not an error. ClearAllEmitted
	// runs on every reload and cannot know what a previous one already removed.
	if err := store.ClearFailedArticlesByIdx(ctx, "job-a", []int32{1, 77}); err != nil {
		t.Errorf("clearing an already-absent index reported an error: %v", err)
	}

	// An empty batch must not open a transaction it has nothing to write in,
	// matching RecordFailedArticles. This is the common case: most reloads
	// reset nothing.
	if err := store.ClearFailedArticlesByIdx(ctx, "job-a", nil); err != nil {
		t.Errorf("an empty batch reported an error: %v", err)
	}
}

// TestClearFailedArticlesByIdx_ChunksBeyondTheHostParameterLimit exercises
// more indices than SQLite's 999-parameter chunk so the loop runs more than
// once.
//
// The assertion that matters is the retained one. A "delete everything except
// these" formulation passes a single-chunk test and fails here, because the
// second statement's NOT IN would delete the rows the first chunk preserved —
// which is why the method enumerates what to remove.
func TestClearFailedArticlesByIdx_ChunksBeyondTheHostParameterLimit(t *testing.T) {
	store, _, _ := setupResidencyTestStoreWithDB(t)
	ctx := context.Background()

	const total = 2500
	all := make([]int32, 0, total)
	for i := range int32(total) {
		all = append(all, i)
	}
	if err := store.RecordFailedArticles(ctx, "job-a", all); err != nil {
		t.Fatalf("RecordFailedArticles: %v", err)
	}

	// Clear everything except two, one inside the first chunk and one past it.
	keep := map[int32]bool{5: true, 1500: true}
	drop := make([]int32, 0, total)
	for _, idx := range all {
		if !keep[idx] {
			drop = append(drop, idx)
		}
	}
	if err := store.ClearFailedArticlesByIdx(ctx, "job-a", drop); err != nil {
		t.Fatalf("ClearFailedArticlesByIdx: %v", err)
	}

	got, err := store.failedArticlesForJob(ctx, "job-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("job-a has %d failed articles after a chunked clear, want 2 (%v)", len(got), got)
	}
	for _, idx := range got {
		if !keep[idx] {
			t.Errorf("article %d survived but was named for removal", idx)
		}
	}
}

// TestClearAllEmitted_KeepsTheStoredRowForARetainedFailedArticle is the half
// of #426 that only shows up after a restart.
//
// The in-memory guard keeps the article Failed; if the reload still dropped
// every failed_articles row for the job, the record would disagree with memory
// immediately and the next restart would load the article as outstanding —
// re-stranding it on a file ForEachUnfinishedArticle skips, and understating
// failedBytes so the Early Health Gate sees a job healthier than it is.
func TestClearAllEmitted_KeepsTheStoredRowForARetainedFailedArticle(t *testing.T) {
	store, _, _ := setupResidencyTestStoreWithDB(t)
	ctx := context.Background()

	q := New(WithStore(store))
	if err := q.Add(makeTestJob("j1", 1, 3)); err != nil {
		t.Fatal(err)
	}
	ackDone(t, q, "j1", artID(0, 0))
	ackFailed(t, q, "j1", artID(0, 1))
	ackFailed(t, q, "j1", artID(0, 2))

	stored, err := store.failedArticlesForJob(ctx, "j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("fixture: job has %d stored failed articles, want 2 (%v)", len(stored), stored)
	}

	// File 0 completes short, carrying both permanent failures.
	if err := q.MarkFileComplete("j1", 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}

	q.ClearAllEmitted()

	stored, err = store.failedArticlesForJob(ctx, "j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Errorf("stored failed articles = %v, want both retained: the reload kept them "+
			"failed in memory, so dropping their rows would let the next restart "+
			"reload them as outstanding work on a Complete file", stored)
	}
}
