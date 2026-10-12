package job

import (
	"fmt"
	"math"
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

// TestMarkArticleFailedCharge_ChargesTheUncoveredBytes pins #432 at the
// owner of the figure: a failure reported with an uncovered byte count charges
// that count, capped at the article's manifest size, and the rest of the
// article counts as downloaded. WholeArticle charges the whole size.
func TestMarkArticleFailedCharge_ChargesTheUncoveredBytes(t *testing.T) {
	cases := map[string]struct{ uncovered, want int64 }{
		"none":               {0, 0},
		"some":               {30, 30},
		"all":                {100, 100},
		"more than its size": {250, 100},
		"whole article":      {WholeArticle, 100},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			j := threeArticleJob(t)
			if err := j.MarkArticleFailed(2, tc.uncovered); err != nil {
				t.Fatalf("MarkArticleFailed: %v", err)
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

// TestMarkArticleFailedCharge_RefusesANegativeCount pins that a negative
// charge other than WholeArticle is an error and fails nothing.
func TestMarkArticleFailedCharge_RefusesANegativeCount(t *testing.T) {
	for _, charge := range []int64{WholeArticle - 1, math.MinInt64} {
		j := threeArticleJob(t)
		if err := j.MarkArticleFailed(2, charge); err == nil {
			t.Errorf("MarkArticleFailed(2, %d): nil error", charge)
		}
		if j.Progress().ArticleFailed(2) {
			t.Errorf("article 2 is failed after MarkArticleFailed(2, %d) was refused", charge)
		}
	}
}

// TestMarkArticleFailedCharge_EvictedChargeHoldsAtHydration pins the
// evicted branch: the bits are set without the manifest, and the charge has to
// travel with them, or the hydration's recompute charges the whole article.
func TestMarkArticleFailedCharge_EvictedChargeHoldsAtHydration(t *testing.T) {
	j, m := evictedThreeArticleJob(t)
	if err := j.MarkArticleFailed(2, 30); err != nil {
		t.Fatalf("MarkArticleFailed on an evicted job: %v", err)
	}
	if err := j.RestoreContent(m); err != nil {
		t.Fatalf("RestoreContent: %v", err)
	}
	checkFigures(t, j, 30)
}

// TestMarkArticleFailedCharge_ResetForgetsTheCharge pins that the charge
// leaves with the failed bit: a reload that returns the article to Outstanding
// unwinds exactly what it charged, and a later full failure of the same
// article charges its whole size.
func TestMarkArticleFailedCharge_ResetForgetsTheCharge(t *testing.T) {
	j := threeArticleJob(t)
	if err := j.MarkArticleFailed(2, 30); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}

	cleared, _ := j.ClearEmittedForReload(false)
	j.contentMu.Lock()
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

	if err := j.MarkArticleFailed(2, WholeArticle); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	checkFigures(t, j, 100)
}

// TestMarkArticleFailedCharge_SecondFailureKeepsTheFirstCharge pins that a
// charge is recorded only by the failure that set the failed bit. An article
// already failed whole, whose redelivery is then refused for overlap, keeps
// its full charge: a reduced one recorded afterwards would make recompute
// disagree with the live figure, and a reset would subtract less than was
// added.
func TestMarkArticleFailedCharge_SecondFailureKeepsTheFirstCharge(t *testing.T) {
	t.Run("resident", func(t *testing.T) {
		j := threeArticleJob(t)
		if err := j.MarkArticleFailed(2, WholeArticle); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
		if err := j.MarkArticleFailed(2, 10); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
		checkFigures(t, j, 100)

		j.contentMu.Lock()
		j.progress.recompute(j.manifest)
		j.contentMu.Unlock()
		checkFigures(t, j, 100)

		j.ClearEmittedForReload(false)
		j.contentMu.Lock()
		failed := j.progress.failedBytes
		j.contentMu.Unlock()
		if failed != 0 {
			t.Errorf("failedBytes after the reset = %d, want 0", failed)
		}
	})
	t.Run("evicted", func(t *testing.T) {
		j, m := evictedThreeArticleJob(t)
		if err := j.MarkArticleFailed(2, WholeArticle); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
		if err := j.MarkArticleFailed(2, 10); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
		if err := j.RestoreContent(m); err != nil {
			t.Fatalf("RestoreContent: %v", err)
		}
		checkFigures(t, j, 100)
	})
}

// TestClone_DoesNotShareTheChargeMap pins that a clone keeps its own charges
// when the original's are dropped.
func TestClone_DoesNotShareTheChargeMap(t *testing.T) {
	j := threeArticleJob(t)
	if err := j.MarkArticleFailed(2, 30); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	cp := j.progress.clone()
	j.progress.clearDone(0, 2)
	if got := cp.chargeOf(j.manifest, 2); got != 30 {
		t.Errorf("clone's charge for article 2 = %d after the original dropped it, want 30", got)
	}
}

// TestMarkArticleFailedCharge_ReleasesRecoveryOnlyForACharge pins that a
// failure charging nothing leaves held recovery volumes held: par2 has nothing
// to repair, so fetching them spends bandwidth for nothing. One that charges a
// byte releases them, as any permanent failure does.
func TestMarkArticleFailedCharge_ReleasesRecoveryOnlyForACharge(t *testing.T) {
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
			if err := j.MarkArticleFailed(0, tc.uncovered); err != nil {
				t.Fatalf("MarkArticleFailed: %v", err)
			}
			if got := j.progress.files[1].Fetch; got != tc.want {
				t.Errorf("recovery volume fetch policy = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCheckEarlyAbort_IgnoresARefusalThatChargesNothing pins #844: a refused
// article whose whole range other articles wrote is charged nothing, so it is
// resolved but not failed for the early-abort rate. A job whose every failure
// is such a refusal loses no bytes and must not be aborted as hopeless, on the
// live path, after a recompute, and when the failures land while the job is
// evicted and are folded in at hydration. A charge of one byte fails each
// article for the rate, which proves the fixture reaches the threshold.
func TestCheckEarlyAbort_IgnoresARefusalThatChargesNothing(t *testing.T) {
	const written, refused = 2, earlyAbortSample
	newJob := func(t *testing.T) (*Job, *Manifest) {
		t.Helper()
		arts := make([]JobArticle, written+refused)
		for i := range arts {
			arts[i] = JobArticle{ID: fmt.Sprintf("<r%d@x>", i), Bytes: 100, Number: i + 1}
		}
		m := NewManifest([]JobFile{{Subject: "data.bin", Bytes: int64(100 * len(arts)), Articles: arts}})
		j := New("refused", "refused", PolicyFromPP(3))
		if err := j.AttachContent(m); err != nil {
			t.Fatalf("AttachContent: %v", err)
		}
		for i := range written {
			markWritten(t, j, i)
		}
		return j, m
	}
	failAll := func(t *testing.T, j *Job, charge int64) {
		t.Helper()
		for i := written; i < written+refused; i++ {
			if err := j.MarkArticleFailed(i, charge); err != nil {
				t.Fatalf("MarkArticleFailed(%d, %d): %v", i, charge, err)
			}
		}
	}
	paths := map[string]func(t *testing.T, charge int64) *Job{
		"live": func(t *testing.T, charge int64) *Job {
			j, _ := newJob(t)
			failAll(t, j, charge)
			return j
		},
		"recomputed": func(t *testing.T, charge int64) *Job {
			j, _ := newJob(t)
			failAll(t, j, charge)
			j.contentMu.Lock()
			j.progress.recompute(j.manifest)
			j.contentMu.Unlock()
			return j
		},
		"failed while evicted": func(t *testing.T, charge int64) *Job {
			j, m := newJob(t)
			j.Evict()
			failAll(t, j, charge)
			if err := j.RestoreContent(m); err != nil {
				t.Fatalf("RestoreContent: %v", err)
			}
			return j
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				charge     int64
				wantFailed int
				wantAbort  bool
			}{{0, 0, false}, {1, refused, true}} {
				j := run(t, tc.charge)
				if got := j.Progress().ArticlesFailed(); got != tc.wantFailed {
					t.Errorf("charge %d: ArticlesFailed = %d, want %d", tc.charge, got, tc.wantFailed)
				}
				if got := j.CheckEarlyAbort(); got != tc.wantAbort {
					t.Errorf("charge %d: CheckEarlyAbort = %v after %d refusals each charged %d bytes, want %v",
						tc.charge, got, refused, tc.charge, tc.wantAbort)
				}
			}
		})
	}
}

// TestCheckEarlyAbort_AZeroChargeRefusalStaysResolved pins the other half of
// #844: a refusal charged nothing still counts in the early-abort rate's
// denominator. Ten whole-article failures and six zero-charge refusals, with
// nothing written, is a rate of 10/16, under the threshold; dropping the
// refusals from articlesResolved would make it 10/10 and abort the job. It runs
// on the live path, after a recompute, and with the failures landing while the
// job is evicted and folded in at hydration.
func TestCheckEarlyAbort_AZeroChargeRefusalStaysResolved(t *testing.T) {
	const failedWhole, refused = earlyAbortSample, 6
	newJob := func(t *testing.T) (*Job, *Manifest) {
		t.Helper()
		arts := make([]JobArticle, failedWhole+refused)
		for i := range arts {
			arts[i] = JobArticle{ID: fmt.Sprintf("<d%d@x>", i), Bytes: 100, Number: i + 1}
		}
		m := NewManifest([]JobFile{{Subject: "data.bin", Bytes: int64(100 * len(arts)), Articles: arts}})
		j := New("denominator", "denominator", PolicyFromPP(3))
		if err := j.AttachContent(m); err != nil {
			t.Fatalf("AttachContent: %v", err)
		}
		return j, m
	}
	failAll := func(t *testing.T, j *Job) {
		t.Helper()
		for i := range failedWhole + refused {
			charge := WholeArticle
			if i >= failedWhole {
				charge = 0
			}
			if err := j.MarkArticleFailed(i, charge); err != nil {
				t.Fatalf("MarkArticleFailed(%d, %d): %v", i, charge, err)
			}
		}
	}
	paths := map[string]func(t *testing.T) *Job{
		"live": func(t *testing.T) *Job {
			j, _ := newJob(t)
			failAll(t, j)
			return j
		},
		"recomputed": func(t *testing.T) *Job {
			j, _ := newJob(t)
			failAll(t, j)
			j.contentMu.Lock()
			j.progress.recompute(j.manifest)
			j.contentMu.Unlock()
			return j
		},
		"failed while evicted": func(t *testing.T) *Job {
			j, m := newJob(t)
			j.Evict()
			failAll(t, j)
			if err := j.RestoreContent(m); err != nil {
				t.Fatalf("RestoreContent: %v", err)
			}
			return j
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			j := run(t)
			if got := j.Progress().ArticlesFailed(); got != failedWhole {
				t.Errorf("ArticlesFailed = %d, want %d", got, failedWhole)
			}
			if j.CheckEarlyAbort() {
				t.Errorf("CheckEarlyAbort = true after %d whole failures and %d zero-charge refusals, "+
					"want false: the refusals left the rate's denominator", failedWhole, refused)
			}
		})
	}
}
