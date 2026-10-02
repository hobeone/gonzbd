package app

import (
	"testing"
)

// TestPauseJob_FetchResultsLandingAfterThePauseAreRecorded pins what keeping a
// paused job resident buys. The pause returns the Fetching job's lease while
// articles it dispatched are still in flight; a write that a barrier then acks,
// and an article the assembler then rejects, must both be recorded on the job,
// with its counters, as they would be for a job still downloading.
func TestPauseJob_FetchResultsLandingAfterThePauseAreRecorded(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	ctx := t.Context()
	d := application.dispatcher
	d.Tick(ctx)
	d.Tick(ctx)
	if !j.HoldsLease() {
		t.Fatal("fixture: the job does not hold a lease, so the pause has none to return")
	}
	// Written before the pause, acked after it: the in-flight success.
	writeFixtureArticle(t, application, j.ID(), 0, 0)

	if err := d.PauseJob(j.ID()); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	d.Tick(ctx)
	if j.HoldsLease() {
		t.Fatal("fixture: the paused job kept its lease, so this is not the case under test")
	}

	application.checkpointJob(ctx, j.ID())
	if !j.Progress().ArticleDone(0) {
		t.Error("an article written before the pause and acked after it is not Done: " +
			"the barrier's ack met a job whose manifest the pause evicted")
	}

	application.handleArticleRejected(j.ID(), 0, 1, "negative offset")
	if got := j.Progress().ArticlesFailed(); got != 1 {
		t.Errorf("ArticlesFailed = %d after a rejection landed on the paused job, want 1: "+
			"the failure reached a job with no manifest to maintain its counters", got)
	}
}
