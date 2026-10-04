package dispatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestResumeJob_RacingBlockUnwantedNeverLeavesABlockedJobRunning drives the
// interleaving deterministically: BlockUnwanted starts after ResumeJob has
// decided to resume and before it sets the intent. The two serialise on d.mu,
// so the block lands after the resume and its pause stands.
func TestResumeJob_RacingBlockUnwantedNeverLeavesABlockedJobRunning(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(context.Background(), j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	d.resumeDecidedHook = func() {
		wg.Go(func() {
			if _, err := d.BlockUnwanted("a", true); err != nil {
				t.Errorf("BlockUnwanted: %v", err)
			}
		})
		// Time for the goroutine to reach d.mu; an unserialised ResumeJob
		// would let it finish inside this window.
		time.Sleep(100 * time.Millisecond)
	}
	if err := d.ResumeJob("a"); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	wg.Wait()

	if st, _ := d.UnwantedState("a"); st != unwanted.StateBlocked {
		t.Fatalf("UnwantedState = %d, want blocked", st)
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause: the job is blocked and running", in)
	}
}
