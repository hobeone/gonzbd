package queue

import (
	"testing"
)

// TestDiscardDeferredPar2_KeepsFileIndicesStable is the whole point of the
// change. Removing a non-final file renumbered every file_index after it, and
// job_files rows are keyed by that index, which is the root of #294, #308,
// #310, #315 and #317.
func TestDiscardDeferredPar2_KeepsFileIndicesStable(t *testing.T) {
	q := New()
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
		{Subject: "b.rar", Bytes: 2000, Articles: []JobArticle{{ID: "b1", Bytes: 2000}}},
	})
	job := &Job{ID: "discard-stable"}
	job.setResidency(m, newJobProgress(m))
	job.setScalarsFromManifest(m)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.jobs[0].progress.files[1].Fetch = FetchIfNeeded

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}

	gotM, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if gotM.NumFiles() != 3 {
		t.Fatalf("NumFiles = %d, want 3 — the file set must not shrink", gotM.NumFiles())
	}
	// The index of the file *after* the discarded one is what a rebuild moved.
	if got := gotM.FileSubject(2); got != "b.rar" {
		t.Errorf("file 2 = %q, want %q — indices after the discarded file moved", got, "b.rar")
	}
	if got := job.Progress().FileFetchPolicy(1); got != FetchNever {
		t.Errorf("file 1 policy = %d, want FetchNever", got)
	}
}

// TestDiscardDeferredPar2_LeavesFiguresUnchanged pins that the discard needs
// no accounting fixup. Both derived figures already exclude a non-fetched
// file, so moving FetchIfNeeded to FetchNever changes neither.
func TestDiscardDeferredPar2_LeavesFiguresUnchanged(t *testing.T) {
	q := New()
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	job := &Job{ID: "discard-figures"}
	job.setResidency(m, newJobProgress(m))
	job.setScalarsFromManifest(m)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.jobs[0].progress.files[1].Fetch = FetchIfNeeded

	beforeExpected := job.Progress().ExpectedBytes()
	beforeRemaining := job.Progress().RemainingBytes()

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}

	if got := job.Progress().ExpectedBytes(); got != beforeExpected {
		t.Errorf("ExpectedBytes moved across the discard: %d -> %d", beforeExpected, got)
	}
	if got := job.Progress().RemainingBytes(); got != beforeRemaining {
		t.Errorf("RemainingBytes moved across the discard: %d -> %d", beforeRemaining, got)
	}
	// TotalBytes is the immutable whole-manifest figure and must now stay put.
	if got, want := job.TotalBytes(), int64(1800); got != want {
		t.Errorf("TotalBytes = %d, want %d — the immutable total must not shrink", got, want)
	}
}

// TestDiscardDeferredPar2_LateFailureDoesNotResurrect is the hazard #318
// names. undeferRecoveryLocked runs on a first-time permanent failure; a
// discarded volume must not come back.
func TestDiscardDeferredPar2_LateFailureDoesNotResurrect(t *testing.T) {
	q := New()
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 2000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}, {ID: "a2", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	job := &Job{ID: "discard-no-resurrect"}
	job.setResidency(m, newJobProgress(m))
	job.setScalarsFromManifest(m)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.jobs[0].progress.files[1].Fetch = FetchIfNeeded
	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}

	ackFailed(t, q, job.ID, "a2")

	if got := job.Progress().FileFetchPolicy(1); got != FetchNever {
		t.Errorf("file 1 policy = %d, want FetchNever — a late failure resurrected a discarded volume", got)
	}
	var offered []string
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		offered = append(offered, a.MessageID)
		return true
	})
	for _, id := range offered {
		if id == "v1" {
			t.Error("a discarded recovery volume was offered for dispatch after a late failure")
		}
	}
}

// TestDiscardDeferredPar2_SurvivesRestart pins that the discard needs no
// special persistence. Nothing about the file set changed, so an ordinary
// checkpoint carries it, and the non-resident view built from job_files
// agrees with the resident one.
func TestDiscardDeferredPar2_SurvivesRestart(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "discard-restart", 2, 1)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.jobs[0].progress.files[1].Fetch = FetchIfNeeded
	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	metas, err := store.ArticleCountsByJob(t.Context())
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	got := metas[job.ID]
	if len(got) != 2 {
		t.Fatalf("stored %d files, want 2 — the row set must not shrink", len(got))
	}
	if got[1].Fetch != FetchNever {
		t.Errorf("stored fetch policy = %d, want FetchNever", got[1].Fetch)
	}

	// The non-resident reconstruction must agree with the resident figures.
	nonResident := newJobProgressSized(got)
	if a, b := nonResident.ExpectedBytes(), job.Progress().ExpectedBytes(); a != b {
		t.Errorf("non-resident ExpectedBytes = %d, resident = %d", a, b)
	}
	if a, b := nonResident.RemainingBytes(), job.Progress().RemainingBytes(); a != b {
		t.Errorf("non-resident RemainingBytes = %d, resident = %d", a, b)
	}
}
