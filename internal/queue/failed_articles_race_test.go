package queue

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
)

// waitForFailedBit blocks until article i of jobID is marked failed in memory,
// reading under q.mu the way every other in-queue reader does.
//
// It is the observable proof that AckPermanentFailure has finished its locked
// section — the mark and the generation capture happen in the same hold — and
// so is past the point where a reversal can be seen by it.
func waitForFailedBit(t *testing.T, q *Queue, jobID string, i int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.RLock()
		job, ok := q.byID[jobID]
		marked := ok && job.progress != nil && job.progress.failed.Get(i)
		q.mu.RUnlock()
		if marked {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("article %d of %s was never marked failed in memory", i, jobID)
}

// TestAckPermanentFailure_DoesNotOutliveAResetThatOvertookIt pins the ordering
// half of the failed_articles write.
//
// Both the write and its reversal run BELOW q.mu, because the project bans I/O
// under a lock. That leaves them free to interleave: the ack marks an article
// failed in memory and unlocks, a reversal then locks, clears the bit, unlocks
// and DELETEs, and only afterwards does the ack's INSERT land. The row that
// survives has no in-memory bit behind it, and the next RestoreJobProgress
// re-derives the article as Failed+Done from it — so it is never fetched
// again. ClearAllEmitted's own doc names acks in flight across a downloader
// reload as the case it exists for, so this is the designed-for interleaving
// rather than an exotic one.
//
// The fixture drives that interleaving deterministically instead of racing for
// it. Holding failedPersistMu parks the ack at exactly the point the reversal
// would overtake it; bumping the generation stands in for a reversal that has
// already completed its own DELETE, which is the state the ack must detect.
// Releasing the mutex then lets the ack decide with the same information it
// would have in production.
//
// Without the generation check the ack cannot tell, and the INSERT lands.
func TestAckPermanentFailure_DoesNotOutliveAResetThatOvertookIt(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	j := makeMultiFileJob(t, "ack-race", 1, 2)
	j.ID = "ack-race"
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Park the ack's store call. It reaches this only after its locked
	// section, so the mark and the generation capture are already done.
	q.failedPersistMu.Lock()
	done := make(chan error, 1)
	go func() { done <- q.AckPermanentFailure("ack-race", []int32{1}) }()
	waitForFailedBit(t, q, "ack-race", 1)

	// A reversal runs to completion while the write is parked: it cleared the
	// bit under q.mu, bumped the generation there, and its DELETE has already
	// run against a table that held nothing yet.
	q.failedGen.Add(1)
	q.failedPersistMu.Unlock()

	if err := <-done; err != nil {
		t.Fatalf("AckPermanentFailure: %v", err)
	}
	got, err := store.failedArticlesForJob(context.Background(), "ack-race")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("%v survives a reset that overtook the write. The matching in-memory "+
			"bit is already cleared, so the next RestoreJobProgress re-derives the "+
			"article as Failed+Done and it is never fetched again", got)
	}
}

// TestReversalsBumpTheReloadGeneration pins the other half: the check above is
// inert unless every reversal actually moves the counter it reads.
//
// Both in-queue reversals are covered, and they are the two that can race an
// ack at all — a job being reset here is a job the downloader may still be
// acking against. Application.RetryHistoryJob, the third site, resets a job
// rebuilt outside the queue and before Queue.Add, so no ack can be in flight
// for it and it needs no generation of its own.
func TestReversalsBumpTheReloadGeneration(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	j := makeMultiFileJob(t, "gen-bump", 1, 2)
	j.ID = "gen-bump"
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	before := q.failedGen.Load()
	q.ClearAllEmitted()
	afterClear := q.failedGen.Load()
	if afterClear == before {
		t.Error("ClearAllEmitted did not move the reload generation, so an ack " +
			"in flight across a downloader reload cannot tell its rows are stale")
	}

	if err := q.SetStatus("gen-bump", constants.StatusFailed); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := q.Retry("gen-bump"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if q.failedGen.Load() == afterClear {
		t.Error("Queue.Retry did not move the reload generation, so an ack in " +
			"flight when the user retries a job can re-record the very articles " +
			"the retry cleared")
	}
}
