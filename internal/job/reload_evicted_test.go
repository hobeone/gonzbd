package job

import (
	"slices"
	"testing"
)

// TestClearEmittedForReload_ReoffersEmittedArticles pins that a reload
// returns an emitted article to the counters as well as clearing its bit: a
// file whose every outstanding article was emitted has Pending 0, and
// ForEachUnfinishedArticle skips such a file.
func TestClearEmittedForReload_ReoffersEmittedArticles(t *testing.T) {
	t.Parallel()
	j, m := newHeldVolumeJob(t)
	for i := range 5 {
		if err := j.MarkArticleEmitted(i); err != nil {
			t.Fatalf("MarkArticleEmitted(%d): %v", i, err)
		}
	}
	j.ClearEmittedForReload(false)
	checkPendingMatchesRecompute(t, j, m, 5)
	if got := offered(t, j); !slices.Equal(got, []int32{0, 1, 2, 3, 4}) {
		t.Errorf("offered %v after the reload, want every article", got)
	}
}

// TestClearEmittedForReload_UnfailsAnEvictedJob pins #904: a reload returns
// the failed articles of an evicted job's incomplete files to Outstanding, as
// it does a resident job's, and leaves a complete file's failure in place. The
// counters follow at the next hydration.
func TestClearEmittedForReload_UnfailsAnEvictedJob(t *testing.T) {
	t.Parallel()
	j, m := newHeldVolumeJob(t)
	for _, i := range []int{0, 1, 2} {
		if err := j.MarkArticleEmitted(i); err != nil {
			t.Fatalf("MarkArticleEmitted(%d): %v", i, err)
		}
	}
	markWritten(t, j, 3)
	markWritten(t, j, 4)
	if err := j.MarkFileComplete(1); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	j.Evict()
	if err := j.MarkArticleFailed(0, WholeArticle); err != nil {
		t.Fatalf("MarkArticleFailed(0): %v", err)
	}
	if err := j.MarkArticleFailed(2, WholeArticle); err != nil {
		t.Fatalf("MarkArticleFailed(2): %v", err)
	}

	cleared, retained := j.ClearEmittedForReload(false)
	if !slices.Equal(cleared, []int32{0}) || !slices.Equal(retained, []int32{2}) {
		t.Errorf("cleared %v, retained %v; want [0] and [2]", cleared, retained)
	}
	p := j.Progress()
	if p.ArticleFailed(0) || p.ArticleDone(0) {
		t.Errorf("article 0 failed=%v done=%v after the reload, want Outstanding", p.ArticleFailed(0), p.ArticleDone(0))
	}
	if !p.ArticleFailed(2) {
		t.Error("article 2, in a complete file, was un-failed")
	}

	if err := j.RestoreContent(m); err != nil {
		t.Fatalf("RestoreContent: %v", err)
	}
	checkPendingMatchesRecompute(t, j, m, 2)
	if got := offered(t, j); !slices.Equal(got, []int32{0, 1}) {
		t.Errorf("offered %v, want [0 1]: article 0 un-failed and article 1's emitted bit cleared", got)
	}
	if got, want := j.FailedBytes(), int64(100); got != want {
		t.Errorf("FailedBytes = %d, want %d: only article 2's failure stands", got, want)
	}
}
