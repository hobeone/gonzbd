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
			if _, err := d.BlockUnwanted(j, true); err != nil {
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

// TestPersistIfChanged_NeverWritesABlockedStateWithTheApprovingResumesIntent
// lands the user's approving resume right after the persist's read of the
// job. The row written must be one moment's view: the Blocked state with the
// pause, or the Approved state with the run, never Blocked with IntentRun,
// which a restart reads as a fail-action filing that is owed.
func TestPersistIfChanged_NeverWritesABlockedStateWithTheApprovingResumesIntent(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(context.Background(), j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if moved, err := d.BlockUnwanted(j, true); err != nil || !moved {
		t.Fatalf("BlockUnwanted = (%v, %v)", moved, err)
	}
	once := true
	d.persistReadHook = func() {
		if once {
			once = false
			if err := d.ResumeJobByUser("a"); err != nil {
				t.Errorf("ResumeJobByUser: %v", err)
			}
		}
	}
	if err := d.persistIfChanged(context.Background(), j); err != nil {
		t.Fatalf("persistIfChanged: %v", err)
	}
	p, ok := st.row("a")
	if !ok {
		t.Fatal("no row was written")
	}
	if p.Header.Unwanted == unwanted.StateBlocked && p.Intent == job.IntentRun {
		t.Fatalf("persisted row = Blocked with IntentRun: a torn read of the header and the intent")
	}
}

// TestResumeJobByUser_RacingBlockUnwantedNeverLeavesABlockedJobRunning is the
// user-resume twin: BlockUnwanted starts after ResumeJobByUser has decided
// (the job was not blocked, so there was nothing to approve) and before it
// sets the intent. They serialise on d.mu, so the block lands after the
// resume, the job stays Blocked and its pause stands.
func TestResumeJobByUser_RacingBlockUnwantedNeverLeavesABlockedJobRunning(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(context.Background(), j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	var wg sync.WaitGroup
	d.resumeDecidedHook = func() {
		wg.Go(func() {
			if _, err := d.BlockUnwanted(j, true); err != nil {
				t.Errorf("BlockUnwanted: %v", err)
			}
		})
		time.Sleep(100 * time.Millisecond)
	}
	if err := d.ResumeJobByUser("a"); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	wg.Wait()

	if st, _ := d.UnwantedState("a"); st != unwanted.StateBlocked {
		t.Fatalf("UnwantedState = %d, want blocked", st)
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause: the job is blocked and running", in)
	}
}
