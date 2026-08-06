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
		if p.FileDeferred(0) || p.FileDeferred(1) {
			t.Error("content/index must not be deferred")
		}
		if !p.FileDeferred(2) {
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
		if p.FileDeferred(2) {
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
	if err := q.MarkArticlesDone(job.ID, []string{"c@x", "i@x"}); err != nil {
		t.Fatal(err)
	}
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
	if _, err := q.MarkArticlesFailed(job.ID, []string{"c@x"}); err != nil {
		t.Fatal(err)
	}
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
	deferredBytes := mustManifest(t, snap).FileBytes(2)

	if err := q.DiscardDeferredPar2("missing"); err == nil {
		t.Error("expected error for missing job")
	}

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatal(err)
	}

	snap = q.SnapshotJob(job.ID)
	if snap.HasDeferredPar2() {
		t.Error("expected no deferred par2 files after discard")
	}

	if mustManifest(t, snap).NumFiles() != 2 { // movie.mkv + movie.par2
		t.Errorf("NumFiles() = %d, want 2", mustManifest(t, snap).NumFiles())
	}

	if mustManifest(t, snap).TotalBytes() != initialTotalBytes-deferredBytes {
		t.Errorf("TotalBytes = %d, want %d", mustManifest(t, snap).TotalBytes(), initialTotalBytes-deferredBytes)
	}

	if snap.Progress().RemainingBytes() != mustManifest(t, snap).TotalBytes() {
		t.Errorf("RemainingBytes = %d, want %d", snap.Progress().RemainingBytes(), mustManifest(t, snap).TotalBytes())
	}
}

// TestDiscardDeferredPar2_IndexShiftAndStaleness exercises the case
// par2NZB()-based fixtures cannot: par2NZB's deferred recovery volume
// always sorts last (all three files are tier-1/non-RAR, and sortJobFiles
// is a stable no-op for tier-1), so discarding it never shifts any
// surviving article's global index. This fixture instead places the
// deferred file BEFORE a surviving file
// ([content-1, recovery-volume(deferred), content-2]), and the job is
// partially downloaded before the discard, so this test can distinguish
// "carried over and adjusted" from "reset to the new total" — a
// zero-download job produces the same RemainingBytes either way.
func TestDiscardDeferredPar2_IndexShiftAndStaleness(t *testing.T) {
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

	oldPar2Bytes := m.Par2Bytes()
	oldPar2Files := m.Par2Files()

	// Partially download the job before discarding: mark content-2's article
	// done. Its pre-discard global index is 2 (after content-1's 1 article
	// and the deferred volume's 1 article); post-discard it must shift to 1.
	if err := q.MarkArticlesDone(job.ID, []string{"c2@x"}); err != nil {
		t.Fatal(err)
	}
	oldRemaining := q.SnapshotJob(job.ID).Progress().RemainingBytes()

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatal(err)
	}

	snap := q.SnapshotJob(job.ID)
	newM, newP := mustManifest(t, snap), snap.Progress()

	if newM.NumFiles() != 2 {
		t.Fatalf("NumFiles() = %d, want 2", newM.NumFiles())
	}
	if newM.FileSubject(0) != "content-1.bin" || newM.FileSubject(1) != "content-2.bin" {
		t.Fatalf("unexpected surviving file order: %q, %q", newM.FileSubject(0), newM.FileSubject(1))
	}

	// The shift bug: content-2's article, done pre-discard at global index 2,
	// must still read Done at its new, shifted global index (1).
	lo, _ := newM.FileRange(1)
	if !newP.ArticleDone(lo) {
		t.Error("content-2's article lost its Done state across the index shift")
	}

	// Par2Bytes/Par2Files must stay exactly as stale as today's code leaves
	// them: carried over unchanged, not recomputed against the reduced set.
	if newM.Par2Bytes() != oldPar2Bytes {
		t.Errorf("Par2Bytes = %d, want %d (carried over unchanged)", newM.Par2Bytes(), oldPar2Bytes)
	}
	if newM.Par2Files() != oldPar2Files {
		t.Errorf("Par2Files = %d, want %d (carried over unchanged)", newM.Par2Files(), oldPar2Files)
	}

	// RemainingBytes must be unchanged by the discard, not TotalBytes() of
	// the reduced manifest — a zero-download job can't distinguish these,
	// which is why this fixture downloads first. Deferred files already
	// contribute nothing to RemainingBytes (derivedRemainingBytes skips
	// them), so removing one changes nothing that was ever counted.
	if newP.RemainingBytes() != oldRemaining {
		t.Errorf("RemainingBytes = %d, want %d (unchanged: deferred bytes were never counted)",
			newP.RemainingBytes(), oldRemaining)
	}

	// The job-level scalar cache (TotalBytes/NumFiles/NumArticles) must be
	// re-synced to the rebuilt, smaller manifest, not left at the pre-discard
	// totals: DiscardDeferredPar2 is the one operation that legitimately
	// changes a job's manifest after Add, and a caller reading the cached
	// scalars (the entire point of promoting them onto Job) must never see a
	// value the live manifest has already moved past. Par2Bytes/Par2Files are
	// asserted equal to the deliberately-stale manifest values above, not
	// recomputed independently.
	if got := snap.TotalBytes(); got != newM.TotalBytes() {
		t.Errorf("Job.TotalBytes() = %d, want %d (synced to rebuilt manifest)", got, newM.TotalBytes())
	}
	if got := snap.NumFiles(); got != newM.NumFiles() {
		t.Errorf("Job.NumFiles() = %d, want %d (synced to rebuilt manifest)", got, newM.NumFiles())
	}
	if got := snap.NumArticles(); got != newM.NumArticles() {
		t.Errorf("Job.NumArticles() = %d, want %d (synced to rebuilt manifest)", got, newM.NumArticles())
	}
	if got := snap.Par2Bytes(); got != oldPar2Bytes {
		t.Errorf("Job.Par2Bytes() = %d, want %d (carried over unchanged)", got, oldPar2Bytes)
	}
	if got := snap.Par2Files(); got != oldPar2Files {
		t.Errorf("Job.Par2Files() = %d, want %d (carried over unchanged)", got, oldPar2Files)
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
