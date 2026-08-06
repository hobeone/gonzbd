package queue

import "testing"

// TestDeferredRecoveryIndices_ExcludesDiscarded pins the resurrection guard.
// undeferRecoveryLocked walks this list on any first-time permanent article
// failure, so a discarded volume appearing here re-downloads exactly what the
// CRC oracle proved unnecessary. Widening the predicate to `!= FetchAlways`
// must fail this test.
func TestDeferredRecoveryIndices_ExcludesDiscarded(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
		{Subject: "a.vol001+02.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v2", Bytes: 800}}},
	})
	p := newJobProgress(m)
	p.files[1].Fetch = FetchIfNeeded
	p.files[2].Fetch = FetchNever

	got := p.DeferredRecoveryIndices()
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("DeferredRecoveryIndices() = %v, want [1] — a discarded volume must never be offered for un-deferral", got)
	}
}

// TestHasDeferredPar2_FalseOnceEverythingIsDecided pins the release gate. The
// par2 release path re-runs full CRC verification while this reports true, so
// a discarded volume counted as held costs a verification pass per completion
// event for the rest of the job.
func TestHasDeferredPar2_FalseOnceEverythingIsDecided(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	p := newJobProgress(m)

	p.files[1].Fetch = FetchIfNeeded
	if !p.HasDeferredPar2() {
		t.Error("HasDeferredPar2() = false with a volume awaiting the verdict, want true")
	}
	p.files[1].Fetch = FetchNever
	if p.HasDeferredPar2() {
		t.Error("HasDeferredPar2() = true with every volume decided, want false — the release path would re-verify on every completion event")
	}
}

// TestSizeFigures_ExcludesDiscarded pins that a discarded volume leaves both
// derived figures, not just remaining. Expected must drop it too, or the
// downloaded identity in internal/app/history_helper.go over-reports.
func TestSizeFigures_ExcludesDiscarded(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	p := newJobProgress(m)
	p.files[1].Fetch = FetchNever

	expected, remaining := p.sizeFigures()
	if expected != 1000 {
		t.Errorf("expected = %d, want 1000 (a discarded volume is not part of what the job will fetch)", expected)
	}
	if remaining != 1000 {
		t.Errorf("remaining = %d, want 1000", remaining)
	}
}
