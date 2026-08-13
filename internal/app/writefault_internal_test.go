package app

import (
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestHandleWriteFault_ReturnsTheArticleToOutstanding pins the half of the
// handler the fault routing does not cover.
//
// A dispatched article carries a set Emitted bit. When its write fails the
// bytes are not on disk and the article has to be fetched again, but
// ForEachUnfinishedArticle skips a set Emitted bit and nothing else on this
// path clears it: no Drain reports the article, no AckPermanentFailure names
// it, and eviction keeps job.progress, so pausing and resuming does not clear
// it either. Without this the article is stranded for the life of the process
// while the job reports it as work in flight.
func TestHandleWriteFault_ReturnsTheArticleToOutstanding(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	if err := application.queue.MarkArticleEmittedByIdx(job.ID, 1); err != nil {
		t.Fatalf("MarkArticleEmittedByIdx: %v", err)
	}
	// Grounding: without this the assertion below passes on a fixture that
	// never reached the state under test.
	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Progress().ArticleEmitted(1) {
		t.Fatal("fixture never emitted article 1, so it cannot observe the bit being cleared")
	}

	application.handleWriteFault(job.ID, 0, 1, storagefault.Classify("write", "/d/a.bin", syscall.ENOSPC))

	snap, err = application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Progress().ArticleEmitted(1) {
		t.Error("article 1 is still Emitted after its write failed. " +
			"ForEachUnfinishedArticle skips a set Emitted bit, so it is never " +
			"re-dispatched and the job cannot finish")
	}
	if snap.Progress().ArticleFailed(1) {
		t.Error("article 1 was marked failed by a STORAGE fault, which A1 forbids: " +
			"a full disk is not evidence about the article's availability")
	}
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

		application.handleWriteFault(job.ID, 0, 0, f)

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

		application.handleWriteFault(job.ID, 0, 0, f)

		snap, err := application.queue.Get(job.ID)
		if err == nil && snap.Status == constants.StatusPaused {
			t.Error("a permanent fault only paused the job; R20 says it is not " +
				"re-evaluated, so pausing leaves it parked forever with no path out")
		}
	})
}
