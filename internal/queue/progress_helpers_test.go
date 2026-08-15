package queue

import "testing"

// ---------- markEmitted / clearEmitted ----------

// TestMarkEmittedClearEmitted_Pair pins the matched-pair contract between
// markEmitted and clearEmitted on JobProgress.Pending/pendingArticles:
// markEmitted moves an article out of "pending" only the first time (it is a
// no-op once Emitted or Done), and clearEmitted only restores it to pending
// when the article has not since completed.
func TestMarkEmittedClearEmitted_Pair(t *testing.T) {
	m := newManifest([]JobFile{{Articles: []JobArticle{
		{ID: "a0", Bytes: 100},
		{ID: "a1", Bytes: 100},
		{ID: "a2", Bytes: 100},
	}}})
	p := newJobProgress(m)
	// recompute is not needed to seed the counters — newJobProgress sets
	// pendingArticles now that it delegates to newJobProgressSized — but it
	// is what production runs before these helpers ever see the progress, so
	// the fixture matches that state rather than a constructor-fresh one.
	p.recompute(m)

	if got := p.files[0].Pending; got != 3 {
		t.Fatalf("fixture: Pending = %d, want 3", got)
	}
	if got := p.pendingArticles; got != 3 {
		t.Fatalf("fixture: pendingArticles = %d, want 3", got)
	}

	// markEmitted on a fresh article moves it out of pending.
	p.markEmitted(m, 0)
	if !p.emitted.Get(0) {
		t.Error("article 0 should be Emitted after markEmitted")
	}
	if got := p.files[0].Pending; got != 2 {
		t.Errorf("Pending after first markEmitted = %d, want 2", got)
	}
	if got := p.pendingArticles; got != 2 {
		t.Errorf("pendingArticles after first markEmitted = %d, want 2", got)
	}

	// markEmitted is idempotent: calling it again on an already-Emitted
	// article must not double-decrement the counters.
	p.markEmitted(m, 0)
	if got := p.files[0].Pending; got != 2 {
		t.Errorf("Pending after second markEmitted (idempotence) = %d, want 2", got)
	}
	if got := p.pendingArticles; got != 2 {
		t.Errorf("pendingArticles after second markEmitted (idempotence) = %d, want 2", got)
	}

	// markEmitted on an already-Done article is a no-op: mark article 1 done
	// directly (bypassing markDone's own bookkeeping) to isolate markEmitted's
	// own Done guard.
	p.done.Set(1)
	p.markEmitted(m, 1)
	if p.emitted.Get(1) {
		t.Error("markEmitted set Emitted on an already-Done article; it must be a no-op")
	}
	if got := p.files[0].Pending; got != 2 {
		t.Errorf("Pending after markEmitted on a Done article = %d, want unchanged 2", got)
	}

	// clearEmitted restores a still-pending (not Done) article to pending.
	p.clearEmitted(m, 0)
	if p.emitted.Get(0) {
		t.Error("article 0 should not be Emitted after clearEmitted")
	}
	if got := p.files[0].Pending; got != 3 {
		t.Errorf("Pending after clearEmitted = %d, want 3", got)
	}
	if got := p.pendingArticles; got != 3 {
		t.Errorf("pendingArticles after clearEmitted = %d, want 3", got)
	}

	// clearEmitted on an article that was never Emitted is a no-op.
	p.clearEmitted(m, 2)
	if got := p.files[0].Pending; got != 3 {
		t.Errorf("Pending after clearEmitted on a never-emitted article = %d, want unchanged 3", got)
	}
	if got := p.pendingArticles; got != 3 {
		t.Errorf("pendingArticles after clearEmitted on a never-emitted article = %d, want unchanged 3", got)
	}

	// clearEmitted on an article that is both Emitted and Done (the state a
	// result-in-flight article is in the instant markDone runs, before
	// markDone's own p.emitted.Clear(i)) must clear the flag without
	// restoring Pending — the article already completed, so it must not be
	// re-offered as pending. Constructed directly rather than through
	// markDone/markEmitted, which never leave this combination standing, to
	// isolate clearEmitted's own boundary check.
	p.emitted.Set(2)
	p.done.Set(2)
	pendingBefore := p.files[0].Pending
	totalBefore := p.pendingArticles
	p.clearEmitted(m, 2)
	if p.emitted.Get(2) {
		t.Error("clearEmitted did not clear Emitted on a Done article")
	}
	if got := p.files[0].Pending; got != pendingBefore {
		t.Errorf("Pending changed on clearEmitted of a Done article: got %d, want unchanged %d", got, pendingBefore)
	}
	if got := p.pendingArticles; got != totalBefore {
		t.Errorf("pendingArticles changed on clearEmitted of a Done article: got %d, want unchanged %d", got, totalBefore)
	}
}

// ---------- resetForReload ----------

// TestResetForReload_FailedArticle pins the branch that matters: an article
// that had permanently failed is reset to retryable — Done and Failed both
// cleared, and its bytes unwound from both the per-file FailedBytes-style
// total (via markFailed's bookkeeping) and remainingBytes. Emitted must also
// be cleared, matching the not-failed path.
func TestResetForReload_FailedArticle(t *testing.T) {
	// dupcomment:ok the two resetForReload fixtures state the same constraint
	// because they set up the same manifest for the same reason. The third
	// copy of this block was a real defect and is corrected below.
	//
	// Bytes is the sum of the articles' bytes, as internal/nzb's parser
	// always makes it. RemainingBytes derives from the file's own size, so a
	// fixture leaving it zero would clamp the figure to zero and make the
	// assertions below pass no matter what resetForReload did.
	m := newManifest([]JobFile{{Bytes: 200, Articles: []JobArticle{
		{ID: "a0", Bytes: 100},
		{ID: "a1", Bytes: 100},
	}}})
	p := newJobProgress(m)
	p.markFailed(m, 0)
	p.emitted.Set(0) // simulate a reload racing an in-flight (re-)dispatch

	failedBytesBefore := p.failedBytes
	remainingBefore := p.RemainingBytes()
	if failedBytesBefore == 0 {
		t.Fatal("fixture: markFailed should have recorded failed bytes")
	}

	p.resetForReload(m, 0)

	if p.emitted.Get(0) {
		t.Error("Emitted was not cleared")
	}
	if p.done.Get(0) {
		t.Error("Done was not cleared on a reset failed article")
	}
	if p.failed.Get(0) {
		t.Error("Failed was not cleared on a reset failed article")
	}
	wantFailedBytes := failedBytesBefore - int64(m.ArticleBytes(0))
	if p.failedBytes != wantFailedBytes {
		t.Errorf("failedBytes = %d, want %d (unwound by article 0's bytes)", p.failedBytes, wantFailedBytes)
	}
	wantRemaining := remainingBefore + int64(m.ArticleBytes(0))
	if got := p.RemainingBytes(); got != wantRemaining {
		t.Errorf("RemainingBytes() = %d, want %d (restored by article 0's bytes)", got, wantRemaining)
	}
}

// TestResetForReload_NotFailedArticle pins the other side: an article that
// was never Failed only has its transient Emitted flag cleared — Done,
// failedBytes, and RemainingBytes must be untouched, including for a Done
// (successfully completed) article, which must not be reopened by a reload.
func TestResetForReload_NotFailedArticle(t *testing.T) {
	// dupcomment:ok the two resetForReload fixtures state the same
	// constraint because they set up the same manifest for the same reason.
	//
	// Bytes is the sum of the articles' bytes, as internal/nzb's parser
	// always makes it. RemainingBytes derives from the file's own size, so a
	// fixture leaving it zero would clamp the figure to zero and make the
	// assertions below pass no matter what resetForReload did.
	m := newManifest([]JobFile{{Bytes: 200, Articles: []JobArticle{
		{ID: "a0", Bytes: 100},
		{ID: "a1", Bytes: 100},
	}}})
	p := newJobProgress(m)
	p.markDone(m, 0) // successfully completed, not failed
	p.emitted.Set(0)

	failedBytesBefore := p.failedBytes
	remainingBefore := p.RemainingBytes()
	doneBefore := p.done.Get(0)

	p.resetForReload(m, 0)

	if p.emitted.Get(0) {
		t.Error("Emitted was not cleared")
	}
	if p.done.Get(0) != doneBefore {
		t.Errorf("Done changed on a reset non-failed article: got %v, want unchanged %v", p.done.Get(0), doneBefore)
	}
	if p.failedBytes != failedBytesBefore {
		t.Errorf("failedBytes changed on a reset non-failed article: got %d, want unchanged %d", p.failedBytes, failedBytesBefore)
	}
	if got := p.RemainingBytes(); got != remainingBefore {
		t.Errorf("RemainingBytes() changed on a reset non-failed article: got %d, want unchanged %d", got, remainingBefore)
	}
}

// ---------- clone ----------

// TestJobProgressClone_DeepCopy pins that clone produces an independent
// copy: mutating the original afterward (bitsets, files slice, serverStats
// map) must not move the clone. cloneJob relies on this to let a saveStore
// snapshot be read outside q.mu while the live job keeps mutating.
func TestJobProgressClone_DeepCopy(t *testing.T) {
	// Bytes is the sum of the articles' bytes, as internal/nzb's parser
	// always makes it. RemainingBytes derives from the file's own size, so a
	// fixture leaving it zero would clamp the figure to zero and make the
	// deep-copy assertions below pass against a shallow clone, every field
	// they compare reading zero on both sides.
	//
	// This block was a verbatim copy of the two resetForReload fixtures above
	// and still named resetForReload, which this test does not call.
	m := newManifest([]JobFile{{Bytes: 200, Articles: []JobArticle{
		{ID: "a0", Bytes: 100},
		{ID: "a1", Bytes: 100},
	}}})
	p := newJobProgress(m)
	p.done.Set(0)
	p.files[0].Filename = "original.bin"
	p.serverStats = map[string]int64{"srv1": 5}

	cp := p.clone()

	// Sanity: the clone starts out equal.
	if !cp.done.Get(0) {
		t.Fatal("fixture: clone should start with article 0 Done")
	}
	if cp.files[0].Filename != "original.bin" {
		t.Fatal("fixture: clone should start with the same filename")
	}
	if cp.serverStats["srv1"] != 5 {
		t.Fatal("fixture: clone should start with the same serverStats")
	}

	// Mutate the original after cloning.
	p.done.Set(1)
	p.files[0].Filename = "mutated.bin"
	p.serverStats["srv1"] = 999
	p.serverStats["srv2"] = 1

	if cp.done.Get(1) {
		t.Error("clone's done bitset moved when the original's did — not a deep copy")
	}
	if cp.files[0].Filename != "original.bin" {
		t.Errorf("clone's files slice moved when the original's did: got %q, want %q", cp.files[0].Filename, "original.bin")
	}
	if cp.serverStats["srv1"] != 5 {
		t.Errorf("clone's serverStats moved when the original's did: got %d, want 5", cp.serverStats["srv1"])
	}
	if _, ok := cp.serverStats["srv2"]; ok {
		t.Error("clone's serverStats gained a key added to the original after cloning")
	}
}

// ---------- isEarlyAbort ----------

// TestIsEarlyAbort_Boundaries covers both sides of each condition in the
// early-abort heuristic: already-fired short-circuit, the resolved-count
// floor, and the failure-rate threshold.
func TestIsEarlyAbort_Boundaries(t *testing.T) {
	tests := []struct {
		name             string
		earlyAborted     bool
		articlesResolved int
		articlesFailed   int
		want             bool
	}{
		{
			name:             "already aborted stays false regardless of rate",
			earlyAborted:     true,
			articlesResolved: earlyAbortSample,
			articlesFailed:   earlyAbortSample,
			want:             false,
		},
		{
			name:             "below sample floor is false even at 100% failure",
			articlesResolved: earlyAbortSample - 1,
			articlesFailed:   earlyAbortSample - 1,
			want:             false,
		},
		{
			name:             "at sample floor, rate below threshold is false",
			articlesResolved: earlyAbortSample,
			articlesFailed:   earlyAbortSample - 3, // 70% < 80% threshold
			want:             false,
		},
		{
			name:             "at sample floor, rate exactly at threshold triggers",
			articlesResolved: earlyAbortSample,
			articlesFailed:   int(earlyAbortThreshold * earlyAbortSample), // exactly 80%
			want:             true,
		},
		{
			name:             "above sample floor, rate above threshold triggers",
			articlesResolved: earlyAbortSample + 10,
			articlesFailed:   earlyAbortSample + 9, // well above 80%
			want:             true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &JobProgress{
				earlyAborted:     tc.earlyAborted,
				articlesResolved: tc.articlesResolved,
				articlesFailed:   tc.articlesFailed,
			}
			got := p.isEarlyAbort()
			if got != tc.want {
				t.Errorf("isEarlyAbort() = %v, want %v", got, tc.want)
			}
			if tc.want && !p.earlyAborted {
				t.Error("isEarlyAbort() returned true but did not set earlyAborted, so it would fire again on the next call")
			}
		})
	}

	// A job that trips the threshold must not trip again on a second call:
	// the earlyAborted flag it sets is exactly what the first test case
	// above short-circuits on.
	p := &JobProgress{articlesResolved: earlyAbortSample, articlesFailed: earlyAbortSample}
	if !p.isEarlyAbort() {
		t.Fatal("fixture: first call should trigger early abort")
	}
	if p.isEarlyAbort() {
		t.Error("isEarlyAbort() fired a second time after already aborting")
	}
}
