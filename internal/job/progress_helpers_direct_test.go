package job

import "testing"

// Direct tests for five unexported helpers in progress.go.
//
// They are here because a comment-only edit to progress.go put every
// unexported helper in the file on check_test_alignment's bar, which is the
// gate working as designed — it examines the whole touched file, not the
// changed lines. Rather than dodge that, these are real assertions on
// behaviour worth pinning, and markNotDone's is load-bearing: its refusal is
// cited by docs/durability-contract.md as what keeps a permanently failed
// article permanently failed.

// TestMarkNotDone_RefusesAPermanentlyFailedArticle pins the guard the
// durability contract names. A failed article's done bit must survive
// markNotDone, or a restart would re-derive it as outstanding and re-fetch
// bytes that will never arrive.
func TestMarkNotDone_RefusesAPermanentlyFailedArticle(t *testing.T) {
	m := newManifest([]JobFile{{
		Subject:  "a.rar",
		Bytes:    300,
		Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}, {ID: "<2@x>", Bytes: 100}, {ID: "<3@x>", Bytes: 100}},
	}})
	p := newJobProgress(m)

	// Article 0: done via a successful path. markNotDone must undo it.
	p.done.Set(0)
	if !p.markNotDone(0) {
		t.Error("markNotDone(0) = false for a done, unfailed article; want true")
	}
	if p.done.Get(0) {
		t.Error("article 0 is still done after markNotDone")
	}

	// Article 1: permanently failed. markFailed sets done AND failed, and
	// markNotDone must refuse it.
	if !p.markFailed(m, 1) {
		t.Fatal("setup: markFailed(1) = false")
	}
	if p.markNotDone(1) {
		t.Error("markNotDone(1) = true for a permanently failed article; want false — " +
			"clearing done here would make the article look outstanding and re-fetch bytes " +
			"that will never arrive (docs/durability-contract.md)")
	}
	if !p.done.Get(1) {
		t.Error("markNotDone cleared a permanently failed article's done bit")
	}

	// Article 2: never done. Nothing to undo.
	if p.markNotDone(2) {
		t.Error("markNotDone(2) = true for an article that was never done; want false")
	}
}

// TestFileMetaFromManifest_ProjectsCountsBytesAndPar2 pins the projection
// newJobProgress depends on: per-file article counts, byte totals, and the
// par2 classification that sizeFigures later excludes on.
func TestFileMetaFromManifest_ProjectsCountsBytesAndPar2(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "movie.part1.rar", Bytes: 200, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}, {ID: "<2@x>", Bytes: 100}}},
		{Subject: "movie.vol00+01.par2", Bytes: 50, Articles: []JobArticle{{ID: "<3@x>", Bytes: 50}}, IsPar2Recovery: true},
	})

	got := fileMetaFromManifest(m)

	if len(got) != 2 {
		t.Fatalf("fileMetaFromManifest returned %d entries, want 2", len(got))
	}
	if got[0].ArticleCount != 2 || got[0].Bytes != 200 {
		t.Errorf("file 0 = {count:%d bytes:%d}, want {count:2 bytes:200}", got[0].ArticleCount, got[0].Bytes)
	}
	if got[0].IsPar2 {
		t.Error("file 0 (movie.part1.rar) classified as par2")
	}
	if got[1].ArticleCount != 1 || got[1].Bytes != 50 {
		t.Errorf("file 1 = {count:%d bytes:%d}, want {count:1 bytes:50}", got[1].ArticleCount, got[1].Bytes)
	}
	if !got[1].IsPar2 {
		t.Error("file 1 (movie.vol00+01.par2) not classified as par2 — sizeFigures excludes on this")
	}
}

// TestDerivedRemainingBytes_TracksCompletionAndFetchPolicy pins that the
// derived figure is the "remaining" half of sizeFigures: it starts at the
// fetched-file total, drops as files complete, and excludes a file the job is
// not fetching at all.
func TestDerivedRemainingBytes_TracksCompletionAndFetchPolicy(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}},
		{Subject: "b.rar", Bytes: 200, Articles: []JobArticle{{ID: "<2@x>", Bytes: 200}}},
	})
	p := newJobProgress(m)

	if got := p.derivedRemainingBytes(); got != 300 {
		t.Errorf("derivedRemainingBytes on a fresh job = %d, want 300", got)
	}

	p.files[0].Complete = true
	if got := p.derivedRemainingBytes(); got != 200 {
		t.Errorf("derivedRemainingBytes after file 0 completed = %d, want 200", got)
	}

	// A file the job is not fetching is not remaining work.
	p.files[1].Fetch = FetchNever
	if got := p.derivedRemainingBytes(); got != 0 {
		t.Errorf("derivedRemainingBytes with file 1 at FetchNever = %d, want 0", got)
	}
}

// TestSizeFigures_ExcludesDifferOnCompleteOnly pins the relationship the
// helper's own comment says is the reason for one walk rather than two: both
// figures skip a file the job is not fetching, and ONLY remaining also skips a
// complete one. Computed apart, that is a convention; here it is asserted.
func TestSizeFigures_ExcludesDifferOnCompleteOnly(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}},
		{Subject: "b.rar", Bytes: 200, Articles: []JobArticle{{ID: "<2@x>", Bytes: 200}}},
		{Subject: "c.rar", Bytes: 400, Articles: []JobArticle{{ID: "<3@x>", Bytes: 400}}},
	})
	p := newJobProgress(m)

	expected, remaining := p.sizeFigures()
	if expected != 700 || remaining != 700 {
		t.Fatalf("fresh job: sizeFigures() = (%d, %d), want (700, 700)", expected, remaining)
	}

	// Complete: still expected, no longer remaining.
	p.files[0].Complete = true
	expected, remaining = p.sizeFigures()
	if expected != 700 {
		t.Errorf("expected = %d after a file completed, want 700 — a complete file is still "+
			"part of what the job set out to fetch", expected)
	}
	if remaining != 600 {
		t.Errorf("remaining = %d after file 0 completed, want 600", remaining)
	}

	// Not fetching: dropped from BOTH.
	p.files[1].Fetch = FetchNever
	expected, remaining = p.sizeFigures()
	if expected != 500 || remaining != 400 {
		t.Errorf("with file 1 at FetchNever: sizeFigures() = (%d, %d), want (500, 400) — a file "+
			"the job is not fetching leaves both figures", expected, remaining)
	}

	// Partial download reduces remaining but not expected.
	p.files[2].BytesDownloaded = 150
	expected, remaining = p.sizeFigures()
	if expected != 500 || remaining != 250 {
		t.Errorf("with 150 of file 2 downloaded: sizeFigures() = (%d, %d), want (500, 250)", expected, remaining)
	}
}

// TestDescribesSameJobAs_GuardsBothDimensions pins the predicate that keeps a
// mismatched (manifest, progress) pair out of RestoreContent. It is what makes
// a structurally-valid-but-inconsistent pair an error rather than a panic in
// recompute (#278), so both dimensions and the nil cases matter.
func TestDescribesSameJobAs_GuardsBothDimensions(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}},
		{Subject: "b.rar", Bytes: 100, Articles: []JobArticle{{ID: "<2@x>", Bytes: 100}}},
	})
	p := newJobProgress(m)

	if !p.describesSameJobAs(m) {
		t.Fatal("a progress built from m does not describe m")
	}

	// Article-count mismatch, in both directions.
	bigger := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}},
		{Subject: "b.rar", Bytes: 200, Articles: []JobArticle{{ID: "<2@x>", Bytes: 100}, {ID: "<3@x>", Bytes: 100}}},
	})
	if p.describesSameJobAs(bigger) {
		t.Error("progress sized for 2 articles accepted a 3-article manifest")
	}
	if newJobProgress(bigger).describesSameJobAs(m) {
		t.Error("progress sized for 3 articles accepted a 2-article manifest — the guard must " +
			"reject an OVERSIZED progress too, which is the direction the old code tolerated")
	}

	// File-count mismatch at the same article count.
	oneFile := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 200, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}, {ID: "<2@x>", Bytes: 100}}},
	})
	if p.describesSameJobAs(oneFile) {
		t.Error("progress for 2 files accepted a 1-file manifest with the same article count")
	}

	// Nil on either side is not a match — this is what keeps a null manifest
	// out of recompute rather than letting it panic there.
	if p.describesSameJobAs(nil) {
		t.Error("describesSameJobAs(nil) = true")
	}
	var nilProgress *JobProgress
	if nilProgress.describesSameJobAs(m) {
		t.Error("(*JobProgress)(nil).describesSameJobAs(m) = true")
	}
}

// TestPar2ReleaseReason_SetAndClear pins the pair as each other's inverse.
// They are trivial, and the point of testing them together is that the
// accessor reports the round trip rather than either setter in isolation.
func TestPar2ReleaseReason_SetAndClear(t *testing.T) {
	m := newManifest([]JobFile{{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}}})
	p := newJobProgress(m)

	if got := p.Par2ReleaseReason(); got != "" {
		t.Errorf("a fresh JobProgress reports par2 release reason %q, want empty", got)
	}

	p.setPar2ReleaseReason("permanent article download failure detected on active queue")
	if got := p.Par2ReleaseReason(); got != "permanent article download failure detected on active queue" {
		t.Errorf("Par2ReleaseReason() = %q after set", got)
	}

	p.clearPar2ReleaseReason()
	if got := p.Par2ReleaseReason(); got != "" {
		t.Errorf("Par2ReleaseReason() = %q after clear, want empty", got)
	}
}
