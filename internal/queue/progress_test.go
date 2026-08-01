package queue

import (
	"strings"
	"testing"
)

// TestJobProgressArticleAccessorsOutOfRange pins the bounds-guard branch on
// ArticleDone/ArticleFailed/ArticleEmitted: a stale or corrupt article index
// (or a nil *JobProgress) must return false rather than panicking, even
// though the backing bitsets now do their own bounds check too.
func TestJobProgressArticleAccessorsOutOfRange(t *testing.T) {
	var nilP *JobProgress
	if nilP.ArticleDone(0) || nilP.ArticleFailed(0) || nilP.ArticleEmitted(0) {
		t.Error("nil *JobProgress accessors must return false")
	}

	m := newManifest([]JobFile{{Articles: []JobArticle{{ID: "a1", Bytes: 100}}}})
	p := newJobProgress(m)

	for _, i := range []int{-1, m.NumArticles(), m.NumArticles() + 10} {
		if p.ArticleDone(i) {
			t.Errorf("ArticleDone(%d) = true, want false out of range", i)
		}
		if p.ArticleFailed(i) {
			t.Errorf("ArticleFailed(%d) = true, want false out of range", i)
		}
		if p.ArticleEmitted(i) {
			t.Errorf("ArticleEmitted(%d) = true, want false out of range", i)
		}
	}
}

// TestRecomputePanicsOnArticleCountMismatch pins finding 2 from the final
// whole-branch review: JobProgress and Manifest are persisted as independent
// documents (Job.UnmarshalJSON assigns both from separate JSON keys with
// nothing reconciling their lengths) and independent SQLite rows. If they
// ever come back mismatched, every article-indexed write in markDone/
// markFailed would either silently no-op (bitset.Set/Clear are deliberately
// lenient — see TestBitsetOutOfRangeIsSafe) or write against the wrong
// article, corrupting byte accounting with no error anywhere. recompute must
// catch the mismatch loudly instead of quietly proceeding — mirroring how
// the file dimension of this same mismatch already panics via the
// p.files[fi] index below.
func TestRecomputePanicsOnArticleCountMismatch(t *testing.T) {
	m := newManifest([]JobFile{{Articles: []JobArticle{
		{ID: "a1", Bytes: 100},
		{ID: "a2", Bytes: 100},
		{ID: "a3", Bytes: 100},
	}}})
	// Undersized relative to m: as if the JobProgress side of an independent
	// pair of documents was sized for fewer articles than the Manifest.
	p := newJobProgress(newManifest([]JobFile{{Articles: []JobArticle{{ID: "a1", Bytes: 100}}}}))

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("recompute did not panic on an article-count mismatch between progress and manifest")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "mismatch") {
			t.Errorf("panic value = %v, want a message mentioning the mismatch", r)
		}
	}()
	p.recompute(m)
}
