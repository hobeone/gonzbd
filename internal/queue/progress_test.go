package queue

import (
	"strings"
	"testing"
	"time"
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

// TestIsJobStamp_MatchesTheWireForm pins the bound that #464 turns on.
//
// The sub-second case is the one that matters: it is the value the rejected
// t.After(time.Unix(0,0)) formulation would have admitted, and it still
// encodes to the integer 0 that the store reads as "absent".
func TestIsJobStamp_MatchesTheWireForm(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   time.Time
		want bool
	}{
		{"zero value", time.Time{}, false},
		{"the epoch itself", time.Unix(0, 0), false},
		{"one second before the epoch", time.Unix(-1, 0), false},
		{"half a second after the epoch", time.Unix(0, 500000000), false},
		{"one second after the epoch", time.Unix(1, 0), true},
		{"a plausible now", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isJobStamp(tc.in); got != tc.want {
				t.Errorf("isJobStamp(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestSetDownloadStampOnce_FirstWinsAndRefusesNonStamps covers both setters.
//
// The table is parameterised by the getter as well as the setter: that is what
// catches a setter miswired to write its sibling's field, which a table keyed
// on the setter alone would pass.
func TestSetDownloadStampOnce_FirstWinsAndRefusesNonStamps(t *testing.T) {
	t.Parallel()

	real := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name    string
		set     func(*JobProgress, time.Time) bool
		get     func(*JobProgress) time.Time
		sibling func(*JobProgress) time.Time
	}{
		{"started", (*JobProgress).setDownloadStartedOnce,
			(*JobProgress).DownloadStarted, (*JobProgress).DownloadFinished},
		{"finished", (*JobProgress).setDownloadFinishedOnce,
			(*JobProgress).DownloadFinished, (*JobProgress).DownloadStarted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := &JobProgress{}
			if !tc.set(p, real) {
				t.Fatal("first real stamp was refused")
			}
			if !tc.get(p).Equal(real) {
				t.Fatalf("stamp = %v, want %v", tc.get(p), real)
			}
			// Asserting the target field alone would pass a setter that wrote
			// BOTH fields, which is a plausible copy-paste result given how
			// alike the two bodies are.
			if !tc.sibling(p).IsZero() {
				t.Errorf("the sibling field was also written: %v", tc.sibling(p))
			}
			if tc.set(p, real.Add(time.Hour)) {
				t.Error("second stamp took; first-wins was not enforced")
			}
			if !tc.get(p).Equal(real) {
				t.Errorf("stamp moved to %v", tc.get(p))
			}

			// A refusal must not consume the slot.
			q := &JobProgress{}
			if tc.set(q, time.Unix(0, 0)) {
				t.Error("epoch zero was accepted")
			}
			if !tc.get(q).IsZero() {
				t.Errorf("epoch zero was stored: %v", tc.get(q))
			}
			if !tc.set(q, real) {
				t.Error("the refusal consumed the first-wins slot")
			}
		})
	}
}

func TestClearDownloadStamps_ClearsBothFields(t *testing.T) {
	t.Parallel()

	real := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	p := &JobProgress{downloadStarted: real, downloadFinished: real.Add(time.Hour)}
	p.clearDownloadStamps()
	if !p.downloadStarted.IsZero() || !p.downloadFinished.IsZero() {
		t.Errorf("after clear: started=%v finished=%v, want both zero",
			p.downloadStarted, p.downloadFinished)
	}
}

func TestRestoreDownloadStamps_FiltersEachFieldIndependently(t *testing.T) {
	t.Parallel()

	real := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                      string
		started, finished         time.Time
		wantStarted, wantFinished time.Time
	}{
		{"both real", real, real, real, real},
		{"started fails the rule", time.Unix(0, 0), real, time.Time{}, real},
		{"finished fails the rule", real, time.Unix(0, 0), real, time.Time{}},
		{"both fail", time.Unix(0, 0), time.Time{}, time.Time{}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &JobProgress{}
			p.restoreDownloadStamps(tc.started, tc.finished)
			if !p.downloadStarted.Equal(tc.wantStarted) {
				t.Errorf("started = %v, want %v", p.downloadStarted, tc.wantStarted)
			}
			if !p.downloadFinished.Equal(tc.wantFinished) {
				t.Errorf("finished = %v, want %v", p.downloadFinished, tc.wantFinished)
			}
		})
	}
}
