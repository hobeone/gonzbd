package queue

import "testing"

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
