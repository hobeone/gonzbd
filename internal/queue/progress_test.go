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

// Every exported JobProgress reader is nil-safe, and callers rely on it
// rather than guarding at each site: buildSlot renders a queue slot straight
// from these, and Queue.TotalRemainingBytes sums RemainingBytes across the
// whole queue from a 1 Hz ticker goroutine that carries no recover().
//
// The guards are individually one comparison and collectively the reason a
// nil progress degrades to zeros instead of killing the process. Nothing
// exercised the nil branch before, so each guard could have been deleted
// with every test still green.
func TestJobProgress_ExportedReadersAreNilSafe(t *testing.T) {
	t.Parallel()
	var p *JobProgress

	// Each call must return the zero value rather than panicking. The
	// assertions are deliberately shallow — the property under test is "does
	// not panic on a nil receiver", not what the zero value happens to be.
	checks := map[string]func(){
		"ArticleDone":         func() { _ = p.ArticleDone(0) },
		"ArticleFailed":       func() { _ = p.ArticleFailed(0) },
		"ArticleEmitted":      func() { _ = p.ArticleEmitted(0) },
		"FileComplete":        func() { _ = p.FileComplete(0) },
		"FileFetchPolicy":     func() { _ = p.FileFetchPolicy(0) },
		"FilePending":         func() { _ = p.FilePending(0) },
		"FileBytesDownloaded": func() { _ = p.FileBytesDownloaded(0) },
		"FileFailedBytes":     func() { _ = p.FileFailedBytes(0) },

		"FileFilename":       func() { _ = p.FileFilename(0) },
		"FileAssembledCRC32": func() { _ = p.FileAssembledCRC32(0) },
		"PendingArticles":    func() { _ = p.PendingArticles() },
		"ArticlesResolved":   func() { _ = p.ArticlesResolved() },
		"ArticlesFailed":     func() { _ = p.ArticlesFailed() },
		"EarlyAborted":       func() { _ = p.EarlyAborted() },
		"FailedBytes":        func() { _ = p.FailedBytes() },
		"RemainingBytes":     func() { _ = p.RemainingBytes() },
		"ServerStats":        func() { _ = p.ServerStats() },
		"DownloadStarted":    func() { _ = p.DownloadStarted() },
		"DownloadFinished":   func() { _ = p.DownloadFinished() },
		"Par2Recovered":      func() { _ = p.Par2Recovered() },
		"Par2ReleaseReason":  func() { _ = p.Par2ReleaseReason() },
		"HasDeferredPar2":    func() { _ = p.HasDeferredPar2() },
	}

	for name, call := range checks {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s() panicked on a nil receiver: %v", name, r)
				}
			}()
			call()
		})
	}

	// Spot-check the two the reporting paths actually sum and render, so a
	// guard that returned something other than the zero value would not slip
	// through the panic-only checks above.
	if got := p.RemainingBytes(); got != 0 {
		t.Errorf("RemainingBytes() on nil = %d, want 0", got)
	}
	if got := p.PendingArticles(); got != 0 {
		t.Errorf("PendingArticles() on nil = %d, want 0", got)
	}
}
