package queue

import (
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// par2NZB builds a parsed NZB with one content file (idx 0), a par2 index
// file (idx 1), and one par2 recovery volume (idx 2), each with one article.
func par2NZB() *nzb.NZB {
	return &nzb.NZB{
		Files: []nzb.File{
			{Subject: `"movie.mkv" yEnc`, Bytes: 1000, Articles: []nzb.Article{{ID: "c@x", Bytes: 1000, Number: 1}}},
			{Subject: `"movie.par2" yEnc`, Bytes: 50, Articles: []nzb.Article{{ID: "i@x", Bytes: 50, Number: 1}}},
			{Subject: `"movie.vol000+01.par2" yEnc`, Bytes: 500, Articles: []nzb.Article{{ID: "v@x", Bytes: 500, Number: 1}}},
		},
	}
}

func TestNewJob_OnDemandPar2Classification(t *testing.T) {
	t.Run("defers recovery volume when enabled", func(t *testing.T) {
		job, err := NewJob(par2NZB(), AddOptions{Filename: "m.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		m, p := mustManifest(t, job), job.Progress()
		if m.FileIsPar2Recovery(0) {
			t.Error("content file wrongly classified as recovery volume")
		}
		if m.FileIsPar2Recovery(1) {
			t.Error("par2 index wrongly classified as recovery volume")
		}
		if !m.FileIsPar2Recovery(2) {
			t.Error("recovery volume not classified as IsPar2Recovery")
		}
		if p.FileFetchPolicy(0) != FetchAlways || p.FileFetchPolicy(1) != FetchAlways {
			t.Error("content/index must not be deferred")
		}
		if p.FileFetchPolicy(2) != FetchIfNeeded {
			t.Error("recovery volume should be deferred when OnDemandPar2 is on")
		}
	})

	t.Run("classifies but does not defer when disabled", func(t *testing.T) {
		job, err := NewJob(par2NZB(), AddOptions{Filename: "m.nzb", OnDemandPar2: false}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		m, p := mustManifest(t, job), job.Progress()
		if !m.FileIsPar2Recovery(2) {
			t.Error("recovery volume should still be classified when feature off")
		}
		if p.FileFetchPolicy(2) != FetchAlways {
			t.Error("recovery volume must not be deferred when OnDemandPar2 is off")
		}
	})
}

func TestOnDemandPar2_PendingAndCompletion(t *testing.T) {
	q := New()
	job, err := NewJob(par2NZB(), AddOptions{Filename: "m.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after add")

	snap := q.SnapshotJob(job.ID)
	// Only content + index are pending; the recovery volume is deferred.
	if snap.Progress().PendingArticles() != 2 {
		t.Errorf("PendingArticles=%d, want 2 (recovery volume deferred)", snap.Progress().PendingArticles())
	}
	if !snap.HasDeferredPar2() {
		t.Error("HasDeferredPar2 should report the deferred volume")
	}
	if idxs := snap.DeferredRecoveryIndices(); len(idxs) != 1 || idxs[0] != 2 {
		t.Errorf("DeferredRecoveryIndices=%v, want [2]", idxs)
	}

	// The deferred recovery article must never be handed to the dispatcher.
	var yielded []string
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		yielded = append(yielded, a.MessageID)
		return true
	})
	if len(yielded) != 2 {
		t.Errorf("dispatcher yielded %d articles (%v), want 2", len(yielded), yielded)
	}
	for _, id := range yielded {
		if id == "v@x" {
			t.Error("deferred recovery article was offered to the dispatcher")
		}
	}

	// Completing the non-deferred files makes the job complete even though the
	// recovery volume was never downloaded.
	ackDone(t, q, job.ID, "c@x", "i@x")
	verifyPending(t, q, "after content+index done")
	if err := q.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkFileComplete(job.ID, 1); err != nil {
		t.Fatal(err)
	}
	if snap := q.SnapshotJob(job.ID); !snap.IsComplete() {
		t.Error("IsComplete should be true when only deferred files remain")
	}
}

func TestUndeferRecoveryVolumes(t *testing.T) {
	q := New()
	job, err := NewJob(par2NZB(), AddOptions{Filename: "m.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}

	idxs := q.SnapshotJob(job.ID).DeferredRecoveryIndices()
	if err := q.UndeferRecoveryVolumes(job.ID, idxs); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after undefer")

	snap := q.SnapshotJob(job.ID)
	if snap.HasDeferredPar2() {
		t.Error("no files should remain deferred after un-deferring all")
	}
	if !snap.Progress().Par2Recovered() {
		t.Error("Par2Recovered should be set after un-deferring")
	}
	if snap.Progress().PendingArticles() != 3 {
		t.Errorf("PendingArticles=%d, want 3 after un-defer", snap.Progress().PendingArticles())
	}

	// The recovery article is now dispatchable.
	var found bool
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		if a.MessageID == "v@x" {
			found = true
		}
		return true
	})
	if !found {
		t.Error("recovery article should be dispatched after un-defer")
	}
}

func TestOnDemandPar2_EarlyUndeferOnFailure(t *testing.T) {
	q := New()
	job, err := NewJob(par2NZB(), AddOptions{Filename: "m.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}
	if !q.SnapshotJob(job.ID).HasDeferredPar2() {
		t.Fatal("precondition: recovery volume should start deferred")
	}

	// A content article permanently fails — proves repair will be needed.
	ackFailed(t, q, job.ID, "c@x")
	verifyPending(t, q, "after data-article failure")

	snap := q.SnapshotJob(job.ID)
	if snap.HasDeferredPar2() {
		t.Error("recovery volume should be released early after a data-article failure")
	}
	if !snap.Progress().Par2Recovered() {
		t.Error("Par2Recovered should be set after early un-defer")
	}
	var found bool
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		if a.MessageID == "v@x" {
			found = true
		}
		return true
	})
	if !found {
		t.Error("recovery article should be dispatchable after early un-defer")
	}
}

func TestUndeferRecoveryVolumes_Edges(t *testing.T) {
	q := New()
	if err := q.UndeferRecoveryVolumes("missing", []int{0}); err == nil {
		t.Error("expected ErrNotFound for missing job")
	}

	job, err := NewJob(par2NZB(), AddOptions{Filename: "m.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}
	// Out-of-range indices must return an error matching sibling file methods.
	if err := q.UndeferRecoveryVolumes(job.ID, []int{mustManifest(t, job).NumFiles()}); err == nil {
		t.Error("expected error for out-of-range fileIdx")
	} else if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error = %v, want 'out of range'", err)
	}
	if err := q.UndeferRecoveryVolumes(job.ID, []int{-1}); err == nil {
		t.Error("expected error for negative fileIdx")
	}

	// Non-deferred valid indices are no-ops: Par2Recovered stays
	// false and the volume stays deferred.
	if err := q.UndeferRecoveryVolumes(job.ID, []int{0}); err != nil {
		t.Fatal(err)
	}
	snap := q.SnapshotJob(job.ID)
	if snap.Progress().Par2Recovered() {
		t.Error("Par2Recovered should not be set when nothing was un-deferred")
	}
	if !snap.HasDeferredPar2() {
		t.Error("recovery volume should remain deferred after a no-op un-defer")
	}
}

// TestDiscardDeferredPar2 used to assert that the manifest shrank by exactly
// the deferred recovery volume's bytes, and that RemainingBytes then equaled
// the shrunk TotalBytes. This task removes the shrink itself: the file set
// and TotalBytes are now immutable across a discard, so the replacement
// assertions are the opposite — the file set does not move, and both derived
// size figures (which already exclude a non-fetched file, deferred or
// discarded alike) do not move either.
func TestDiscardDeferredPar2(t *testing.T) {
	q := New()
	job, err := NewJob(par2NZB(), AddOptions{Filename: "m.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}

	snap := q.SnapshotJob(job.ID)
	if !snap.HasDeferredPar2() {
		t.Fatal("expected deferred par2 files")
	}

	initialTotalBytes := mustManifest(t, snap).TotalBytes()
	initialRemaining := snap.Progress().RemainingBytes()

	if err := q.DiscardDeferredPar2("missing"); err == nil {
		t.Error("expected error for missing job")
	}

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatal(err)
	}

	snap = q.SnapshotJob(job.ID)
	if snap.HasDeferredPar2() {
		t.Error("expected no deferred (FetchIfNeeded) par2 files after discard")
	}

	if got := mustManifest(t, snap).NumFiles(); got != 3 {
		t.Errorf("NumFiles() = %d, want 3 — the file set must not shrink", got)
	}
	if got := snap.Progress().FileFetchPolicy(2); got != FetchNever {
		t.Errorf("file 2 policy = %d, want FetchNever", got)
	}

	if got := mustManifest(t, snap).TotalBytes(); got != initialTotalBytes {
		t.Errorf("TotalBytes = %d, want %d — the immutable total must not shrink", got, initialTotalBytes)
	}
	if got := snap.Progress().RemainingBytes(); got != initialRemaining {
		t.Errorf("RemainingBytes = %d, want %d — moving FetchIfNeeded to FetchNever changes nothing it counted", got, initialRemaining)
	}
}

// TestDiscardDeferredPar2_NoIndexShift replaces
// TestDiscardDeferredPar2_IndexShiftAndStaleness. The original exercised the
// case par2NZB()-based fixtures cannot: a deferred file positioned BEFORE a
// surviving one, downloaded partially first, so a rebuild-and-renumber bug
// (carried-over-and-adjusted vs. silently-reset) would be visible in the
// surviving file's global article index. This task removes the renumbering
// machinery outright, so the property worth pinning is now the opposite one:
// nothing shifts. Global article indices, per-file bytes, and the job's
// cached scalars (synced once at Add and never re-synced by a discard, since
// DiscardDeferredPar2 no longer calls setScalarsFromManifest) all stay
// exactly where they were.
func TestDiscardDeferredPar2_NoIndexShift(t *testing.T) {
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "content-1.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "c1@x", Bytes: 1000, Number: 1}}},
			{Subject: `"content.vol000+01.par2" yEnc`, Bytes: 500, Articles: []nzb.Article{{ID: "v@x", Bytes: 500, Number: 1}}},
			{Subject: "content-2.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "c2@x", Bytes: 1000, Number: 1}}},
		},
	}
	q := New()
	job, err := NewJob(parsed, AddOptions{Filename: "shift.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}

	// Precondition: sortJobFiles is a stable no-op for tier-1 files, so the
	// deferred recovery volume stays at index 1, sorting BEFORE content-2.
	m := mustManifest(t, job)
	if m.NumFiles() != 3 || !m.FileIsPar2Recovery(1) {
		t.Fatalf("precondition: expected recovery volume at index 1, got NumFiles=%d, file1IsRecovery=%v",
			m.NumFiles(), m.FileIsPar2Recovery(1))
	}

	oldTotalBytes := m.TotalBytes()
	oldPar2Bytes := m.RecoveryBytes()
	oldPar2Files := m.RecoveryFiles()

	// Partially download the job before discarding: mark content-2's article
	// done at its global index 2 (after content-1's 1 article and the
	// deferred volume's 1 article).
	ackDone(t, q, job.ID, "c2@x")
	oldRemaining := q.SnapshotJob(job.ID).Progress().RemainingBytes()

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatal(err)
	}

	snap := q.SnapshotJob(job.ID)
	newM, newP := mustManifest(t, snap), snap.Progress()

	if newM.NumFiles() != 3 {
		t.Fatalf("NumFiles() = %d, want 3 — the file set must not shrink", newM.NumFiles())
	}
	if newM.FileSubject(0) != "content-1.bin" || newM.FileSubject(2) != "content-2.bin" {
		t.Fatalf("unexpected file order: %q, %q, %q", newM.FileSubject(0), newM.FileSubject(1), newM.FileSubject(2))
	}

	// content-2's article, done pre-discard at global index 2, must still
	// read Done at index 2 — nothing shifts, so there is no new index to
	// check it at.
	lo, _ := newM.FileRange(2)
	if !newP.ArticleDone(lo) {
		t.Error("content-2's article lost its Done state across the discard")
	}
	if got := newP.FileFetchPolicy(1); got != FetchNever {
		t.Errorf("file 1 policy = %d, want FetchNever", got)
	}

	// Par2Bytes/Par2Files are untouched — there was never a rebuild to carry
	// them across.
	if newM.RecoveryBytes() != oldPar2Bytes {
		t.Errorf("Par2Bytes = %d, want %d (unchanged)", newM.RecoveryBytes(), oldPar2Bytes)
	}
	if newM.RecoveryFiles() != oldPar2Files {
		t.Errorf("Par2Files = %d, want %d (unchanged)", newM.RecoveryFiles(), oldPar2Files)
	}

	// RemainingBytes must be unchanged by the discard: deferred files already
	// contribute nothing to it (derivedRemainingBytes skips anything that is
	// not FetchAlways), so moving FetchIfNeeded to FetchNever changes nothing
	// that was ever counted.
	if newP.RemainingBytes() != oldRemaining {
		t.Errorf("RemainingBytes = %d, want %d (unchanged: deferred/never-fetched bytes were never counted)",
			newP.RemainingBytes(), oldRemaining)
	}

	// The job-level scalar cache is synced once at Add, and
	// DiscardDeferredPar2 no longer calls setScalarsFromManifest — it has no
	// rebuilt manifest to sync from — so it must still agree with the
	// untouched manifest, not have moved to some new value.
	if got := snap.TotalBytes(); got != oldTotalBytes {
		t.Errorf("Job.TotalBytes() = %d, want %d (unchanged)", got, oldTotalBytes)
	}
	if got := snap.NumFiles(); got != newM.NumFiles() {
		t.Errorf("Job.NumFiles() = %d, want %d", got, newM.NumFiles())
	}
	if got := snap.NumArticles(); got != newM.NumArticles() {
		t.Errorf("Job.NumArticles() = %d, want %d", got, newM.NumArticles())
	}
	if got := snap.RecoveryBytes(); got != oldPar2Bytes {
		t.Errorf("Job.RecoveryBytes() = %d, want %d (unchanged)", got, oldPar2Bytes)
	}
	if got := snap.RecoveryFiles(); got != oldPar2Files {
		t.Errorf("Job.RecoveryFiles() = %d, want %d (unchanged)", got, oldPar2Files)
	}
}

// TestDiscardDeferredPar2_NoOpWhenNothingDeferred pins that discard remains a
// pure no-op (no manifest/progress swap, no dirty flag) when there is
// nothing to discard — matching today's code, which gates its entire
// mutation block on discardedBytes > 0.
func TestDiscardDeferredPar2_NoOpWhenNothingDeferred(t *testing.T) {
	q := New()
	job, err := NewJob(par2NZB(), AddOptions{Filename: "m.nzb", OnDemandPar2: false}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := q.Save(dir); err != nil {
		t.Fatal(err)
	}
	if q.IsDirty() {
		t.Fatal("precondition: queue should not be dirty right after Save")
	}

	manifestBefore := mustManifest(t, job)
	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatal(err)
	}

	if q.IsDirty() {
		t.Error("DiscardDeferredPar2 with nothing deferred must not mark the queue dirty")
	}
	if mustManifest(t, job) != manifestBefore {
		t.Error("DiscardDeferredPar2 with nothing deferred must not replace the manifest pointer")
	}
}
