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
		if job.Files[0].IsPar2Recovery {
			t.Error("content file wrongly classified as recovery volume")
		}
		if job.Files[1].IsPar2Recovery {
			t.Error("par2 index wrongly classified as recovery volume")
		}
		if !job.Files[2].IsPar2Recovery {
			t.Error("recovery volume not classified as IsPar2Recovery")
		}
		if job.Files[0].Deferred || job.Files[1].Deferred {
			t.Error("content/index must not be deferred")
		}
		if !job.Files[2].Deferred {
			t.Error("recovery volume should be deferred when OnDemandPar2 is on")
		}
	})

	t.Run("classifies but does not defer when disabled", func(t *testing.T) {
		job, err := NewJob(par2NZB(), AddOptions{Filename: "m.nzb", OnDemandPar2: false}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !job.Files[2].IsPar2Recovery {
			t.Error("recovery volume should still be classified when feature off")
		}
		if job.Files[2].Deferred {
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
	if snap.PendingArticles != 2 {
		t.Errorf("PendingArticles=%d, want 2 (recovery volume deferred)", snap.PendingArticles)
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
	if !snap.Par2Recovered {
		t.Error("Par2Recovered should be set after un-deferring")
	}
	if snap.PendingArticles != 3 {
		t.Errorf("PendingArticles=%d, want 3 after un-defer", snap.PendingArticles)
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
	if !snap.Par2Recovered {
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
	if err := q.UndeferRecoveryVolumes(job.ID, []int{len(job.Files)}); err == nil {
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
	if snap.Par2Recovered {
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

	initialTotalBytes := snap.TotalBytes
	deferredBytes := snap.Files[2].Bytes

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

	if len(snap.Files) != 2 { // movie.mkv + movie.par2
		t.Errorf("len(Files) = %d, want 2", len(snap.Files))
	}

	if snap.TotalBytes != initialTotalBytes-deferredBytes {
		t.Errorf("TotalBytes = %d, want %d", snap.TotalBytes, initialTotalBytes-deferredBytes)
	}

	if snap.RemainingBytes != snap.TotalBytes {
		t.Errorf("RemainingBytes = %d, want %d", snap.RemainingBytes, snap.TotalBytes)
	}
}
