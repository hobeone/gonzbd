package app

import (
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestHandleArticlesUnwritten_ReturnsTheArticlesToOutstanding pins the half of
// the handling the fault routing does not cover.
//
// A dispatched article carries a set Emitted bit. When its write fails the
// bytes are not on disk and the article has to be fetched again, but
// ForEachUnfinishedArticle skips a set Emitted bit and nothing else on this
// path clears it: no Drain reports the article, no AckPermanentFailure names
// it, and eviction keeps job.progress, so pausing and resuming does not clear
// it either. Without this the article is stranded for the life of the process
// while the job reports it as work in flight.
//
// It takes a SET, and that is the finding it exists for: a batch failure rolls
// back every article in a coalesced run, or everything after the write that
// failed in a drain, and the old single-index signature could report only one
// of them. The rest were left neither Done, nor Failed, nor Outstanding.
func TestHandleArticlesUnwritten_ReturnsTheArticlesToOutstanding(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	for _, idx := range []int32{0, 1} {
		if err := application.queue.MarkArticleEmittedByIdx(job.ID, idx); err != nil {
			t.Fatalf("MarkArticleEmittedByIdx(%d): %v", idx, err)
		}
	}
	// Grounding: without this the assertions below pass on a fixture that
	// never reached the state under test.
	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Progress().ArticleEmitted(0) || !snap.Progress().ArticleEmitted(1) {
		t.Fatal("fixture never emitted both articles, so it cannot observe the bits being cleared")
	}

	application.handleArticlesUnwritten(job.ID, 0, []int32{0, 1})

	snap, err = application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int32{0, 1} {
		if snap.Progress().ArticleEmitted(int(idx)) {
			t.Errorf("article %d is still Emitted after its write failed. "+
				"ForEachUnfinishedArticle skips a set Emitted bit, so it is never "+
				"re-dispatched and the job cannot finish", idx)
		}
		if snap.Progress().ArticleFailed(int(idx)) {
			t.Errorf("article %d was marked failed by a STORAGE fault, which A1 forbids: "+
				"a full disk is not evidence about the article's availability", idx)
		}
	}
}

// TestHandleArticlesUnwritten_SurvivesAJobThatHasLeftTheQueue covers the
// branch where the clear fails.
//
// Ordinary rather than exceptional: the assembler's worker is a separate
// goroutine, so a roll-back can be routed after the job it belongs to has been
// cancelled or moved to history. There is nothing left to re-dispatch and
// nothing to recover, so the handler must not panic or propagate — but it must
// not be silent either (A2), which is why the branch exists rather than the
// error being discarded at the call.
func TestHandleArticlesUnwritten_SurvivesAJobThatHasLeftTheQueue(t *testing.T) {
	application, _ := newDurabilityTestApp(t, 1, 2)

	// No job with this ID was ever added, which is the same state the queue
	// presents after one is removed.
	application.handleArticlesUnwritten("job-that-never-existed", 0, []int32{0, 1})
}

// TestHandleWriteFault_RoutesOnPermanence pins the branch that decides whether
// the job can ever recover, on the same R18 rule the barrier uses.
func TestHandleWriteFault_RoutesOnPermanence(t *testing.T) {
	t.Run("retryable stalls", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 2)
		f := storagefault.Classify("write", "/d/a.bin", syscall.ENOSPC)
		if f.Permanent {
			t.Fatalf("fixture error is permanent, so this subtest cannot observe the retryable branch")
		}

		application.handleWriteFault(job.ID, 0, f)

		snap, err := application.queue.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Status != constants.StatusPaused {
			t.Errorf("status = %v after a retryable write fault, want paused — a job that "+
				"keeps dispatching into a device that cannot take them turns one fault "+
				"into a flood", snap.Status)
		}
	})

	t.Run("permanent fails", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 2)
		f := storagefault.Classify("write", "/d/a.bin", syscall.EROFS)
		if !f.Permanent {
			t.Fatalf("fixture error is retryable, so this subtest cannot observe the permanent branch")
		}

		application.handleWriteFault(job.ID, 0, f)

		snap, err := application.queue.Get(job.ID)
		if err == nil && snap.Status == constants.StatusPaused {
			t.Error("a permanent fault only paused the job; R20 says it is not " +
				"re-evaluated, so pausing leaves it parked forever with no path out")
		}
	})
}

// TestHandleArticleRejected_AcksThePermanentFailure pins the app half of the
// rejection path: the assembler refuses the article, and this is what turns
// that refusal into a resolved article.
//
// Both consequences are asserted because they are separate defects. Without
// the ack the article keeps its Emitted bit, which is NOT Outstanding —
// ForEachUnfinishedArticle skips a set Emitted bit — so nothing re-dispatches
// it and the job never reaches completion. And its bytes are never charged as
// failed, so the "beyond repair" gate compares a healthy-looking failed-byte
// count against par2's recovery budget and lets a job proceed to a repair that
// cannot succeed.
func TestHandleArticleRejected_AcksThePermanentFailure(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	if err := application.queue.MarkArticleEmittedByIdx(job.ID, 1); err != nil {
		t.Fatalf("MarkArticleEmittedByIdx: %v", err)
	}
	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Progress().ArticleEmitted(1) {
		t.Fatal("fixture never emitted article 1, so it cannot observe the article being resolved")
	}
	before := snap.Progress().FailedBytes()

	application.handleArticleRejected(job.ID, 0, 1, "negative offset")

	snap, err = application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Progress().ArticleFailed(1) {
		t.Error("the rejected article is not marked failed; its Emitted bit is still set, " +
			"so ForEachUnfinishedArticle skips it and nothing ever re-dispatches it")
	}
	if got := snap.Progress().FailedBytes(); got <= before {
		t.Errorf("FailedBytes = %d, was %d — a rejected article's bytes must be charged, "+
			"or the beyond-repair gate weighs them against par2's budget as if they had arrived",
			got, before)
	}
}

// TestHandleArticleRejected_SurvivesAJobThatHasLeftTheQueue covers the branch
// where the ack fails.
//
// It is ordinary rather than exceptional: the assembler's worker is a separate
// goroutine, so a rejection can be routed after the job it belongs to has been
// cancelled or moved to history. There is nothing left to record against and
// nothing to recover, so the handler must not panic or propagate — but it must
// not be silent either (A2), which is why the branch exists rather than the
// error being discarded at the call.
func TestHandleArticleRejected_SurvivesAJobThatHasLeftTheQueue(t *testing.T) {
	application, _ := newDurabilityTestApp(t, 1, 2)

	// No job with this ID was ever added, which is the same state the queue
	// presents after one is removed.
	application.handleArticleRejected("job-that-never-existed", 0, 1, "negative offset")
}
