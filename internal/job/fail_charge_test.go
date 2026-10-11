package job

import (
	"testing"
)

// threeArticleJob is one 300-byte file of three 100-byte articles, no par2,
// with articles 0 and 1 written.
func threeArticleJob(t *testing.T) *Job {
	t.Helper()
	m := NewManifest([]JobFile{{Subject: "data.bin", Bytes: 300, Articles: []JobArticle{
		{ID: "<a0@x>", Bytes: 100, Number: 1},
		{ID: "<a1@x>", Bytes: 100, Number: 2},
		{ID: "<a2@x>", Bytes: 100, Number: 3},
	}}})
	j := New("charge", "charge", PolicyFromPP(3))
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	markWritten(t, j, 0)
	markWritten(t, j, 1)
	return j
}

// checkFigures asserts the failed and remaining figures of a threeArticleJob.
// Remaining is zero once article 2 is resolved: the part of its size that is
// not charged is on disk, written by another article, so nothing is left to
// fetch.
func checkFigures(t *testing.T, j *Job, wantFailed int64) {
	t.Helper()
	if got := j.FailedBytes(); got != wantFailed {
		t.Errorf("FailedBytes = %d, want %d", got, wantFailed)
	}
	if got := j.ContentFailedBytes(); got != wantFailed {
		t.Errorf("ContentFailedBytes = %d, want %d", got, wantFailed)
	}
	if got := j.Progress().FileFailedBytes(0); got != wantFailed {
		t.Errorf("FileFailedBytes(0) = %d, want %d", got, wantFailed)
	}
	if got := j.RemainingBytes(); got != 0 {
		t.Errorf("RemainingBytes = %d, want 0 — every article is resolved", got)
	}
}

// TestMarkArticleFailedUncovered_ChargesTheUncoveredBytes pins #432 at the
// owner of the figure: a failure reported with an uncovered byte count charges
// that count, capped at the article's manifest size, and the rest of the
// article counts as downloaded.
func TestMarkArticleFailedUncovered_ChargesTheUncoveredBytes(t *testing.T) {
	cases := map[string]struct{ uncovered, want int64 }{
		"none":               {0, 0},
		"some":               {30, 30},
		"all":                {100, 100},
		"more than its size": {250, 100},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			j := threeArticleJob(t)
			if err := j.MarkArticleFailedUncovered(2, tc.uncovered); err != nil {
				t.Fatalf("MarkArticleFailedUncovered: %v", err)
			}
			if !j.Progress().ArticleFailed(2) {
				t.Fatal("article 2 is not failed")
			}
			checkFigures(t, j, tc.want)

			// A recompute derives the same figures from the bits: the charge is
			// a property of the failed article, not of the call that failed it.
			j.contentMu.Lock()
			j.progress.recompute(j.manifest)
			j.contentMu.Unlock()
			checkFigures(t, j, tc.want)
		})
	}
}

// TestMarkArticleFailedUncovered_RefusesANegativeCount pins that a negative
// count is an error and fails nothing.
func TestMarkArticleFailedUncovered_RefusesANegativeCount(t *testing.T) {
	j := threeArticleJob(t)
	if err := j.MarkArticleFailedUncovered(2, -1); err == nil {
		t.Error("MarkArticleFailedUncovered(2, -1): nil error")
	}
	if j.Progress().ArticleFailed(2) {
		t.Error("article 2 is failed after a refused call")
	}
}

// TestMarkArticleFailedUncovered_EvictedChargeHoldsAtHydration pins the
// evicted branch: the bits are set without the manifest, and the charge has to
// travel with them, or the hydration's recompute charges the whole article.
func TestMarkArticleFailedUncovered_EvictedChargeHoldsAtHydration(t *testing.T) {
	j, m := evictedThreeArticleJob(t)
	if err := j.MarkArticleFailedUncovered(2, 30); err != nil {
		t.Fatalf("MarkArticleFailedUncovered on an evicted job: %v", err)
	}
	if err := j.RestoreContent(m); err != nil {
		t.Fatalf("RestoreContent: %v", err)
	}
	checkFigures(t, j, 30)
}

// TestMarkArticleFailedUncovered_ResetForgetsTheCharge pins that the charge
// leaves with the failed bit: a reload that returns the article to Outstanding
// unwinds exactly what it charged, and a later full failure of the same
// article charges its whole size.
func TestMarkArticleFailedUncovered_ResetForgetsTheCharge(t *testing.T) {
	j := threeArticleJob(t)
	if err := j.MarkArticleFailedUncovered(2, 30); err != nil {
		t.Fatalf("MarkArticleFailedUncovered: %v", err)
	}

	// resetForReload maintains the figures itself, ahead of the recompute
	// ClearEmittedForReload runs after it, so it is checked on its own.
	j.contentMu.Lock()
	cleared, _ := j.progress.resetForReload(j.manifest, 0, true)
	failed, downloaded := j.progress.failedBytes, j.progress.files[0].BytesDownloaded
	j.contentMu.Unlock()
	if len(cleared) != 1 || cleared[0] != 2 {
		t.Fatalf("resetForReload cleared %v, want [2]", cleared)
	}
	if failed != 0 {
		t.Errorf("failedBytes after the reset = %d, want 0", failed)
	}
	if downloaded != 200 {
		t.Errorf("BytesDownloaded after the reset = %d, want 200 (articles 0 and 1)", downloaded)
	}

	if err := j.MarkArticleFailed(2); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	checkFigures(t, j, 100)
}

// TestClone_DoesNotShareTheChargeMap pins that a clone keeps its own charges
// when the original's are dropped.
func TestClone_DoesNotShareTheChargeMap(t *testing.T) {
	j := threeArticleJob(t)
	if err := j.MarkArticleFailedUncovered(2, 30); err != nil {
		t.Fatalf("MarkArticleFailedUncovered: %v", err)
	}
	cp := j.progress.clone()
	j.progress.clearDone(0, 2)
	if got := cp.chargeOf(j.manifest, 2); got != 30 {
		t.Errorf("clone's charge for article 2 = %d after the original dropped it, want 30", got)
	}
}

// TestMarkArticleFailedUncovered_ReleasesRecoveryOnlyForACharge pins that a
// failure charging nothing leaves held recovery volumes held: par2 has nothing
// to repair, so fetching them spends bandwidth for nothing. One that charges a
// byte releases them, as any permanent failure does.
func TestMarkArticleFailedUncovered_ReleasesRecoveryOnlyForACharge(t *testing.T) {
	newFixture := func(t *testing.T) *Job {
		t.Helper()
		m := NewManifest([]JobFile{
			{Subject: "data", Bytes: 100, Articles: []JobArticle{{ID: "<d0@x>", Bytes: 100, Number: 1}}},
			{Subject: "vol01+02.par2", Bytes: 100, IsPar2Recovery: true,
				Articles: []JobArticle{{ID: "<p0@x>", Bytes: 100, Number: 1}}},
		})
		j := New("j", "j", PolicyFromPP(1))
		if err := j.AttachContent(m); err != nil {
			t.Fatalf("AttachContent: %v", err)
		}
		j.progress.files[1].Fetch = FetchIfNeeded
		return j
	}
	cases := map[string]struct {
		uncovered int64
		want      FetchPolicy
	}{
		"nothing uncovered holds the volume": {0, FetchIfNeeded},
		"a byte uncovered releases it":       {1, FetchAlways},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			j := newFixture(t)
			if err := j.MarkArticleFailedUncovered(0, tc.uncovered); err != nil {
				t.Fatalf("MarkArticleFailedUncovered: %v", err)
			}
			if got := j.progress.files[1].Fetch; got != tc.want {
				t.Errorf("recovery volume fetch policy = %v, want %v", got, tc.want)
			}
		})
	}
}
